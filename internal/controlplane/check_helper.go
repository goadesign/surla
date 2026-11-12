// This file runs each check_project and update_dependencies operation in a
// check helper process, so the operation's containers are removed and its
// record and output finished even when surla serve dies.
//
// surla serve binds the workspace, creates the operation directory and takes
// an exclusive flock on it, then starts the helper the composition root
// bound (CheckHelper, `surla check-run`) in its own session:
//
//	stdin   one JSON checkHelperSpec; stdin then stays open, and its end
//	        (serve canceled the operation or exited) cancels the operation
//	stdout  one JSON checkHelperResult, written once the directory is final
//	stderr  diagnostics; serve keeps the first maxCheckMessageBytes
//	fd 3    the locked operation directory, held until the helper exits
//	fd 4    the live grant directory serve verified
//	fd 5    the identity-verified workspace directory
//
// The helper is the only writer of the operation's owner.json, output and
// scratch while it runs. Recovery skips a directory whose lock is held, so
// it acts only once the helper has exited.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	goa "goa.design/goa/v3/pkg"
	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/verify/candidate"
)

type (
	// CheckHelper returns a new, unstarted command that runs this program's
	// check helper role, which calls RunCheckHelper with the program's own
	// standard streams. The control plane sets the command's standard
	// streams, extra descriptors and session before starting it, so the
	// returned command must not set them; it inherits the server's
	// environment unless it sets one. The composition root binds it: surla
	// serve binds `surla check-run`. New requires one with a Checker.
	CheckHelper func() *exec.Cmd

	// checkHelperSpec is what surla serve sends a check helper on stdin: the
	// operation to run and the facts serve verified when it bound the
	// workspace. The helper checks its inherited descriptors against it.
	checkHelperSpec struct {
		// Tool is checkToolProject or checkToolDependencies.
		Tool string `json:"tool"`
		// OpID is the operation ID, the name of Dir.
		OpID string `json:"op_id"`
		// Dir is the absolute operation directory, descriptor 3.
		Dir string `json:"dir"`
		// GrantRoot is the kernel's path of the live grant, descriptor 4.
		GrantRoot string `json:"grant_root"`
		// WorkspaceRel is the workspace's slash path inside the grant.
		WorkspaceRel string `json:"workspace_rel"`
		// WorkspaceDev and WorkspaceIno identify the workspace, descriptor 5.
		WorkspaceDev int32  `json:"workspace_dev"`
		WorkspaceIno uint64 `json:"workspace_ino"`
		// GoProxyEnv is the operator's module proxy settings.
		GoProxyEnv []string `json:"go_proxy_env"`
		// CheckTimeout is the operator's -check-timeout, named in messages.
		CheckTimeout time.Duration `json:"check_timeout"`
		// TraceContext carries the W3C trace context of serve's request span,
		// so the helper's spans are its children.
		TraceContext map[string]string `json:"trace_context"`
	}

	// checkHelperResult is the line a check helper writes on stdout. Exactly
	// one field is set: the tool's result, or the error the caller gets.
	checkHelperResult struct {
		// Check is check_project's result.
		Check *gensurla.CheckProjectResult `json:"check,omitempty"`
		// Update is update_dependencies' result.
		Update *gensurla.UpdateDependenciesResult `json:"update,omitempty"`
		// Error is the operation's caller-facing error.
		Error *checkHelperError `json:"error,omitempty"`
	}

	// checkHelperError is a caller-facing error carried from the helper to
	// serve: a named service error keeps its name and flags, so HTTP and
	// MCP report it as before.
	checkHelperError struct {
		// Name is the service error name, or empty for an unnamed error.
		Name string `json:"name"`
		// Message is the complete error text, including the output ID.
		Message string `json:"message"`
		// Timeout, Temporary and Fault are the service error's flags.
		Timeout   bool `json:"timeout"`
		Temporary bool `json:"temporary"`
		Fault     bool `json:"fault"`
	}

	// boundedBuffer keeps the first n bytes written to it and accepts the
	// rest without keeping it, so a helper's diagnostics never block it.
	boundedBuffer struct {
		// buf holds the kept bytes.
		buf bytes.Buffer
		// n is how many bytes buf may hold.
		n int
	}
)

const (
	// checkToolProject and checkToolDependencies name the operations a
	// check helper runs.
	checkToolProject      = "check_project"
	checkToolDependencies = "update_dependencies"
	// checkHelperDirFD, checkHelperGrantFD and checkHelperWorkspaceFD are
	// the descriptors a check helper inherits: the locked operation
	// directory, the live grant and the workspace.
	checkHelperDirFD       = 3
	checkHelperGrantFD     = 4
	checkHelperWorkspaceFD = 5
)

// errServeReleased is the cancellation cause a check helper records when
// its stdin ends: surla serve canceled the operation or exited.
var errServeReleased = errors.New("surla serve canceled the operation or exited")

// RunCheckHelper runs as `surla check-run`, the check helper process of one
// check_project or update_dependencies operation. runner resolves the
// engine; tp receives the helper's spans. It reads the spec from stdin,
// verifies the descriptors it inherited, and runs the operation; when stdin
// ends before the operation does, the operation is canceled: no container
// is started and no manifest file begun afterwards, every container is
// removed or kept recorded, and the output and record are finished. It
// writes one result to stdout and returns, which releases the operation
// directory's lock. It returns an error, and the process should exit
// non-zero, only when the spec or descriptors are unusable, before any
// docker command, or when the result could not be written.
func RunCheckHelper(runner *candidate.ContainerRunner, tp trace.TracerProvider, stdin io.Reader, stdout io.Writer) error {
	// A write to a closed stdout or stderr must not kill the helper before
	// it has cleaned up after a server that died; it fails instead.
	signal.Ignore(syscall.SIGPIPE)
	dec := json.NewDecoder(stdin)
	dec.DisallowUnknownFields()
	var spec checkHelperSpec
	if err := dec.Decode(&spec); err != nil {
		return fmt.Errorf("read the check helper spec: %w", err)
	}
	if err := spec.validate(); err != nil {
		return err
	}
	op, dir, err := openCheckHelperOperation(spec, runner)
	if err != nil {
		return err
	}
	// Closing the directory, last, releases the operation's lock.
	defer dir.Close()

	ctx := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier(spec.TraceContext))
	ctx, stopSignals := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	go watchServe(io.MultiReader(dec.Buffered(), stdin), cancel)

	ctx, span := tp.Tracer("surla/controlplane").Start(ctx, "surla.check.helper", trace.WithAttributes(
		attribute.String("surla.check.tool", spec.Tool),
		attribute.String("surla.check.op_id", spec.OpID),
	))
	var result checkHelperResult
	switch spec.Tool {
	case checkToolProject:
		result.Check, err = op.checkProject(ctx)
	case checkToolDependencies:
		result.Update, err = op.updateDependencies(ctx)
	}
	if err != nil {
		result = checkHelperResult{Error: newCheckHelperError(err)}
	}
	if cause := context.Cause(ctx); cause != nil {
		span.AddEvent("operation_canceled", trace.WithAttributes(attribute.String("surla.check.reason", cause.Error())))
	}
	endCheckSpan(span, err)
	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode the check result: %w", err)
	}
	if _, err := stdout.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write the check result: %w", err)
	}
	return nil
}

// Write keeps what fits and reports every byte as written.
func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := b.n - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// lockNewOperationDir creates the operation directory dir and returns it
// open with an exclusive flock held, which surla serve hands to the check
// helper. A directory that cannot be locked is removed.
func lockNewOperationDir(dir string) (*os.File, error) {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create check directory %s: %w", dir, err)
	}
	fd, err := unix.Open(dir, openDirFlags, 0)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open check directory %s: %w", dir, err), os.Remove(dir))
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("lock check directory %s: %w", dir, err), os.Remove(dir), unix.Close(fd))
	}
	return os.NewFile(uintptr(fd), dir), nil
}

// dupDirectory returns a close-on-exec duplicate of the held directory fd,
// which exec gives a child as one of its extra descriptors.
func dupDirectory(fd int, name string) (*os.File, error) {
	dup, err := unix.FcntlInt(uintptr(fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate the held %s directory: %w", name, err)
	}
	return os.NewFile(uintptr(dup), name), nil
}

// openCheckHelperOperation binds the descriptors a check helper inherited
// to spec: it marks them close-on-exec, so no docker command inherits the
// lock or the live directories, and checks that descriptor 3 is spec.Dir,
// 4 is spec.GrantRoot and 5 is the workspace spec identifies. It returns
// the operation and the directory whose closing releases its lock.
func openCheckHelperOperation(spec checkHelperSpec, runner *candidate.ContainerRunner) (*checkOperation, *os.File, error) {
	for _, fd := range []int{checkHelperDirFD, checkHelperGrantFD, checkHelperWorkspaceFD} {
		syscall.CloseOnExec(fd)
	}
	var held, named, ws unix.Stat_t
	if err := errors.Join(unix.Fstat(checkHelperDirFD, &held), unix.Lstat(spec.Dir, &named), unix.Fstat(checkHelperWorkspaceFD, &ws)); err != nil {
		return nil, nil, fmt.Errorf("inspect the check helper's descriptors: %w", err)
	}
	grantRoot, err := fdPath(checkHelperGrantFD)
	switch {
	case err != nil:
		return nil, nil, fmt.Errorf("resolve the check helper's grant descriptor: %w", err)
	case held.Dev != named.Dev || held.Ino != named.Ino:
		return nil, nil, fmt.Errorf("descriptor %d is not the operation directory %s", checkHelperDirFD, spec.Dir)
	case grantRoot != spec.GrantRoot:
		return nil, nil, fmt.Errorf("descriptor %d is %s, not the grant %s", checkHelperGrantFD, grantRoot, spec.GrantRoot)
	case ws.Dev != spec.WorkspaceDev || ws.Ino != spec.WorkspaceIno:
		return nil, nil, fmt.Errorf("descriptor %d is not the verified workspace", checkHelperWorkspaceFD)
	}
	op := &checkOperation{
		id:           spec.OpID,
		dir:          spec.Dir,
		grantRoot:    spec.GrantRoot,
		grantFD:      checkHelperGrantFD,
		workspaceFD:  checkHelperWorkspaceFD,
		workspaceID:  fileID{dev: spec.WorkspaceDev, ino: spec.WorkspaceIno},
		workspaceRel: spec.WorkspaceRel,
		checker:      runner,
		checkTimeout: spec.CheckTimeout,
		goProxyEnv:   spec.GoProxyEnv,
	}
	return op, os.NewFile(checkHelperDirFD, spec.Dir), nil
}

// watchServe reads the rest of the helper's stdin, which surla serve never
// writes to after the spec, and cancels the operation when it ends: serve
// closed it to cancel the operation, or exited. A read error ends it the
// same way.
func watchServe(rest io.Reader, cancel context.CancelCauseFunc) {
	_, err := io.Copy(io.Discard, rest)
	cancel(errors.Join(errServeReleased, err))
}

// decodeCheckHelperResult strictly decodes the helper's stdout as one
// result for tool: exactly one field set, the tool's own result valid for
// its generated contract, or an error with a message.
func decodeCheckHelperResult(raw []byte, tool string) (checkHelperResult, error) {
	var r checkHelperResult
	if err := decodeStrictObject(raw, &r); err != nil {
		return checkHelperResult{}, fmt.Errorf("decode the check result: %w", err)
	}
	switch {
	case r.Error != nil && r.Check == nil && r.Update == nil && r.Error.Message != "":
		return r, nil
	case r.Check != nil && r.Update == nil && r.Error == nil && tool == checkToolProject:
		return r, gensurla.ValidateCheckProjectResultValue(r.Check)
	case r.Update != nil && r.Check == nil && r.Error == nil && tool == checkToolDependencies:
		return r, gensurla.ValidateUpdateDependenciesResultValue(r.Update)
	}
	return checkHelperResult{}, fmt.Errorf("the check result %s is not one %s result or error", raw, tool)
}

// newCheckHelperError carries err to serve, keeping a named service error's
// name and flags.
func newCheckHelperError(err error) *checkHelperError {
	e := &checkHelperError{Message: err.Error()}
	var serviceErr *goa.ServiceError
	if errors.As(err, &serviceErr) {
		e.Name, e.Timeout, e.Temporary, e.Fault = serviceErr.Name, serviceErr.Timeout, serviceErr.Temporary, serviceErr.Fault
	}
	return e
}

// runCheckHelper runs tool for the active workspace in a new check helper:
// it binds the workspace's live grant, creates and locks the operation
// directory, starts the helper, and returns its result. The helper's error
// result is returned as the caller-facing error. Binding failures are the
// errors they were before helpers existed (not_implemented, unprocessable).
func (c *ControlPlane) runCheckHelper(ctx context.Context, tool string) (res checkHelperResult, retErr error) {
	wsDir, dev, ino := c.currentWorkspaceTarget()
	if wsDir == "" {
		return checkHelperResult{}, gensurla.MakeNotImplemented(errors.New("no workspace directory is bound to this control plane"))
	}
	_, wsRoot, err := openModuleRootWithIdentity(wsDir, dev, ino)
	if err != nil {
		return checkHelperResult{}, err
	}
	op := &checkOperation{id: newOperationID(), grantFD: -1, workspaceFD: -1}
	if err := errors.Join(op.openLive(wsRoot, c.protectedDirs), wsRoot.Close()); err != nil {
		return checkHelperResult{}, errors.Join(err, op.closeLive())
	}
	// The helper receives its own copies of the held directories.
	defer func() { retErr = errors.Join(retErr, op.closeLive()) }()
	op.dir = filepath.Join(c.checkWorkDir, op.id)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("surla.check.op_id", op.id))
	lock, err := lockNewOperationDir(op.dir)
	if err != nil {
		return checkHelperResult{}, err
	}
	spec := checkHelperSpec{
		Tool:         tool,
		OpID:         op.id,
		Dir:          op.dir,
		GrantRoot:    op.grantRoot,
		WorkspaceRel: op.workspaceRel,
		WorkspaceDev: op.workspaceID.dev,
		WorkspaceIno: op.workspaceID.ino,
		GoProxyEnv:   c.goProxyEnv,
		CheckTimeout: c.checkTimeout,
		TraceContext: map[string]string{},
	}
	propagation.TraceContext{}.Inject(ctx, propagation.MapCarrier(spec.TraceContext))
	return c.startCheckHelper(ctx, spec, lock, op)
}

// startCheckHelper starts the helper for spec, handing it lock and copies
// of op's held directories, sends the spec, closes the helper's stdin when
// ctx ends, and waits for it to exit. The helper bounds every phase of its
// own cleanup, so the wait ends once it has finished. A helper that ends
// without a valid result is an error naming the operation's output.
func (c *ControlPlane) startCheckHelper(ctx context.Context, spec checkHelperSpec, lock *os.File, op *checkOperation) (checkHelperResult, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return checkHelperResult{}, errors.Join(err, os.Remove(spec.Dir), lock.Close())
	}
	grant, err := dupDirectory(op.grantFD, "grant")
	if err != nil {
		return checkHelperResult{}, errors.Join(err, os.Remove(spec.Dir), lock.Close())
	}
	workspace, err := dupDirectory(op.workspaceFD, "workspace")
	if err != nil {
		return checkHelperResult{}, errors.Join(err, grant.Close(), os.Remove(spec.Dir), lock.Close())
	}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return checkHelperResult{}, errors.Join(err, grant.Close(), workspace.Close(), os.Remove(spec.Dir), lock.Close())
	}
	var stdout bytes.Buffer
	stderr := &boundedBuffer{n: maxCheckMessageBytes}
	cmd := c.checkHelper()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, &stdout, stderr
	cmd.ExtraFiles = []*os.File{lock, grant, workspace}
	if err := cmd.Start(); err != nil {
		return checkHelperResult{}, errors.Join(fmt.Errorf("start the check helper: %w", err), os.Remove(spec.Dir),
			lock.Close(), grant.Close(), workspace.Close(), stdinR.Close(), stdinW.Close())
	}
	// From here the helper alone holds the operation's lock.
	closeErr := errors.Join(lock.Close(), grant.Close(), workspace.Close(), stdinR.Close())
	closeStdin := sync.OnceValue(stdinW.Close)
	_, writeErr := stdinW.Write(append(raw, '\n'))
	stop := context.AfterFunc(ctx, func() {
		if err := closeStdin(); err != nil {
			trace.SpanFromContext(ctx).RecordError(err)
		}
	})
	waitErr := cmd.Wait()
	stop()
	result, err := decodeCheckHelperResult(stdout.Bytes(), spec.Tool)
	if err != nil {
		return checkHelperResult{}, fmt.Errorf("the check helper of operation %s ended without a result: %w: %s\nRead saved output with read_check_output(op_id: %q); output may be incomplete.",
			spec.OpID, errors.Join(err, waitErr, writeErr, closeErr, closeStdin()), stderr.buf.String(), spec.OpID)
	}
	if err := errors.Join(waitErr, writeErr, closeErr, closeStdin()); err != nil {
		trace.SpanFromContext(ctx).RecordError(err)
	}
	if result.Error != nil {
		return checkHelperResult{}, result.Error.err()
	}
	return result, nil
}

// validate checks the spec's structure before the helper acts on it.
func (s checkHelperSpec) validate() error {
	switch {
	case s.Tool != checkToolProject && s.Tool != checkToolDependencies:
		return fmt.Errorf("check helper tool %q is not %s or %s", s.Tool, checkToolProject, checkToolDependencies)
	case !opIDPattern.MatchString(s.OpID) || !filepath.IsAbs(s.Dir) || filepath.Base(s.Dir) != s.OpID:
		return fmt.Errorf("check helper operation %q does not name its directory %q", s.OpID, s.Dir)
	case !filepath.IsAbs(s.GrantRoot):
		return fmt.Errorf("check helper grant %q is not absolute", s.GrantRoot)
	case s.WorkspaceRel != "." && !filepath.IsLocal(s.WorkspaceRel):
		return fmt.Errorf("check helper workspace %q is not inside the grant", s.WorkspaceRel)
	case s.CheckTimeout <= 0:
		return fmt.Errorf("check helper timeout %s must be positive", s.CheckTimeout)
	}
	return nil
}

// err returns the caller-facing error e carries: a goa service error when
// it has a name, so transports map it as the helper's operation did.
func (e *checkHelperError) err() error {
	if e.Name == "" {
		return errors.New(e.Message)
	}
	return goa.NewServiceError(errors.New(e.Message), e.Name, e.Timeout, e.Temporary, e.Fault)
}
