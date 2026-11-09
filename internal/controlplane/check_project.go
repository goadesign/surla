// This file implements check_project: copy the active workspace's live grant
// into a per-operation directory and run go test ./... for every workspace
// module in Linux containers, after downloading dependencies against a
// read-only copy. Each operation owns its directory, containers and output.
package controlplane

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sys/unix"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/verify/candidate"
)

type (
	// checkOperation is one check_project or update_dependencies call: its
	// private directory, its engine and the containers it starts. Only the
	// goroutine serving the call uses it.
	checkOperation struct {
		// c is the control plane serving the call.
		c *ControlPlane
		// id is the random operation ID, also the directory name.
		id string
		// dir is <cards dir>/check-work/<id>.
		dir string
		// grantRoot is the live grant's path when the operation started. It
		// names the source mount point inside containers and nothing else.
		grantRoot string
		// grantFD holds the live grant directory open for the whole
		// operation, reached from the identity-verified workspace root. The
		// copy reads through it, so a path component replaced mid-operation
		// cannot redirect it.
		grantFD int
		// workspaceFD holds the identity-verified workspace open for the
		// whole operation; update_dependencies writes workspace manifests
		// through it.
		workspaceFD int
		// workspaceID is the verified workspace's identity; the copy refuses
		// a workspace path that resolves to anything else.
		workspaceID fileID
		// workspaceRel is the workspace's slash path inside the grant, "."
		// when they are the same directory.
		workspaceRel string
		// copy is the result of copying the grant into dir/src.
		copy grantCopy
		// engine is the operation's resolved local Docker engine.
		engine *candidate.Engine
		// out captures every container's output.
		out *capWriter
		// owner is the recovery record kept in dir/owner.json.
		owner checkOwner
		// containers counts containers created so far, for unique names.
		containers int
	}

	// checkOwner is the operation's recovery record: the engine every command
	// targeted and the containers not yet confirmed removed. Close and the
	// next startup retry removal with exactly this endpoint.
	checkOwner struct {
		// Docker is the docker executable the operation used.
		Docker string `json:"docker"`
		// Host is the resolved local engine endpoint.
		Host string `json:"host"`
		// Containers are names whose removal is not yet confirmed.
		Containers []string `json:"containers"`
	}

	// moduleOutcome is one module's result within an operation.
	moduleOutcome struct {
		// dir is the workspace-relative module directory.
		dir string
		// step names what failed: "download", "test" or "tidy".
		step string
		// exitCode is the failing container's exit code.
		exitCode int
		// start and end delimit the failing container's output in output.log.
		start, end int64
	}
)

const (
	// maxCheckMessageBytes bounds one MCP result's message.
	maxCheckMessageBytes = 16 << 10
	// ownerFile is the operation's recovery record.
	ownerFile = "owner.json"
	// outputFile is the operation's captured output.
	outputFile = "output.log"
	// outputMetaFile records captured and dropped byte counts.
	outputMetaFile = "output.json"
	// containerModCache is the module cache path inside containers.
	containerModCache = "/gomodcache"
	// containerWork is the scratch directory inside containers.
	containerWork = "/work"
)

// CheckProject copies the active workspace's live files into a protected
// per-operation copy and runs go test ./... for every Go module in it in
// Linux containers, never changing the workspace. It requires an
// authenticated principal.
func (c *ControlPlane) CheckProject(ctx context.Context) (res *gensurla.CheckProjectResult, retErr error) {
	if _, ok := PrincipalFromContext(ctx); !ok {
		return nil, errors.New("authenticated principal is required")
	}
	ctx, span := otel.Tracer("surla/controlplane").Start(ctx, "surla.controlplane.check_project")
	defer func() { endCheckSpan(span, retErr) }()
	ctx, cancel := c.checkDeadline(ctx)
	defer cancel()
	op, err := c.startCheckOperation(ctx)
	if err != nil {
		return nil, err
	}
	var failures []moduleOutcome
	for _, mod := range op.copy.modules {
		if f, ok, err := op.runGo(ctx, mod, "download", "bridge", true, op.c.goProxyEnv, "list", "-deps", "-test", "./..."); err != nil {
			return nil, op.finish(err)
		} else if !ok {
			failures = append(failures, f)
			continue
		}
		if f, ok, err := op.runGo(ctx, mod, "test", "none", false, []string{"GOPROXY=off"}, "test", "./..."); err != nil {
			return nil, op.finish(err)
		} else if !ok {
			failures = append(failures, f)
		}
	}
	if err := op.finish(nil); err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.Bool("surla.check.passed", len(failures) == 0))
	return &gensurla.CheckProjectResult{
		OpID:    op.id,
		Passed:  len(failures) == 0,
		Message: op.message(failures),
	}, nil
}

// startCheckOperation binds the active workspace, creates the operation
// directory, copies the grant and opens the engine. Errors are already
// the caller-facing service errors.
func (c *ControlPlane) startCheckOperation(ctx context.Context) (*checkOperation, error) {
	if c.checker == nil || c.checkWorkDir == "" {
		return nil, gensurla.MakeNotImplemented(errors.New("check_project is not configured on this server"))
	}
	if err := c.admitCheckOperation(); err != nil {
		return nil, err
	}
	// From here the operation is registered: every return either calls
	// op.finish, which ends the registration, or abort.
	abort := func(err error) (*checkOperation, error) {
		c.checkOps.Done()
		return nil, err
	}
	wsDir, dev, ino := c.currentWorkspaceTarget()
	if wsDir == "" {
		return abort(gensurla.MakeNotImplemented(errors.New("no workspace directory is bound to this control plane")))
	}
	_, wsRoot, err := openModuleRootWithIdentity(wsDir, dev, ino)
	if err != nil {
		return abort(err)
	}
	op := &checkOperation{c: c, id: newOperationID(), grantFD: -1, workspaceFD: -1}
	if err := errors.Join(op.openLive(wsRoot, c.protectedDirs), wsRoot.Close()); err != nil {
		return abort(errors.Join(err, op.closeLive()))
	}
	op.dir = filepath.Join(c.checkWorkDir, op.id)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("surla.check.op_id", op.id))
	for _, d := range []string{op.dir, op.path("src"), op.path("modcache"), op.path("docker-config")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return abort(errors.Join(fmt.Errorf("create check directory %s: %w", d, err), op.closeLive()))
		}
	}
	if op.out, err = newCapWriter(op.path(outputFile)); err != nil {
		return abort(errors.Join(err, op.closeLive()))
	}
	op.copy, err = copyGrant(ctx, op.grantFD, op.workspaceRel, op.workspaceID, op.path("src"), c.importedReports)
	if err != nil {
		var changed *errSourceChanged
		switch {
		case ctx.Err() != nil:
			return nil, op.finish(fmt.Errorf("check exceeded -check-timeout (%s) or was canceled while copying the workspace: %w", c.checkTimeout, err))
		case errors.As(err, &changed):
			return nil, op.finish(err)
		}
		return nil, op.finish(fmt.Errorf("copy the workspace: %w", err))
	}
	for _, p := range op.copy.skipped {
		fmt.Fprintf(op.out, "note: %s is not a regular file, directory or symlink and was not copied\n", p)
	}
	fmt.Fprintf(op.out, "==> modules: %s\n", strings.Join(op.copy.modules, ", "))
	trace.SpanFromContext(ctx).SetAttributes(attribute.Int("surla.check.modules", len(op.copy.modules)))
	if len(op.copy.modules) == 0 {
		return nil, op.finish(gensurla.MakeUnprocessable(errors.New("the workspace contains no Go module (no go.mod)")))
	}
	engine, err := c.checker.Open(ctx, op.path("docker-config"))
	if err == nil {
		op.engine = engine
		op.owner = checkOwner{Docker: engine.Docker(), Host: engine.Host()}
		err = engine.EnsureImage(ctx)
	}
	if err != nil {
		if errors.Is(err, candidate.ErrEngineUnavailable) {
			return nil, op.finish(gensurla.MakeUnavailable(err))
		}
		return nil, op.finish(err)
	}
	return op, nil
}

// admitCheckOperation registers a new operation with checkOps, or refuses it
// once Close has started. The check and the registration happen under a
// read lock of workspaceMu, which Close takes exclusively after canceling
// serverCtx: an operation admitted before that point is registered before
// Close waits on checkOps, and every later one sees serverCtx ended. Admitted
// operations still run concurrently.
func (c *ControlPlane) admitCheckOperation() error {
	c.workspaceMu.RLock()
	defer c.workspaceMu.RUnlock()
	if c.serverCtx.Err() != nil {
		return gensurla.MakeUnavailable(errors.New("the server is shutting down; call again on a running server"))
	}
	c.checkOps.Add(1)
	return nil
}

// checkDeadline bounds a whole check or dependency operation (copy, image
// pull, every container) by the operator's -check-timeout, and cancels it
// when the server closes. The request context can only shorten it.
func (c *ControlPlane) checkDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(ctx, c.checkTimeout)
	stop := context.AfterFunc(c.serverCtx, cancel)
	return ctx, func() { stop(); cancel() }
}

// runGo runs one go command for the workspace module mod in a new container
// and reports whether it exited 0. A non-zero exit is an outcome, not an
// error; err reports engine failures, the deadline and unconfirmed cleanup.
func (op *checkOperation) runGo(ctx context.Context, mod, step, network string, srcReadOnly bool, extraEnv []string, args ...string) (moduleOutcome, bool, error) {
	name := fmt.Sprintf("surla-check-%s-%d", op.id, op.containers)
	work := op.path(fmt.Sprintf("work-%d", op.containers))
	op.containers++
	if err := os.MkdirAll(filepath.Join(work, "tmp"), 0o700); err != nil {
		return moduleOutcome{}, false, fmt.Errorf("create container work directory: %w", err)
	}
	op.owner.Containers = append(op.owner.Containers, name)
	if err := op.writeOwner(); err != nil {
		return moduleOutcome{}, false, err
	}
	env := append([]string{
		"HOME=" + containerWork,
		"TMPDIR=" + containerWork + "/tmp",
		"GOCACHE=" + containerWork + "/gocache",
		"GOPATH=" + containerWork + "/gopath",
		"GOMODCACHE=" + containerModCache,
		"GOTOOLCHAIN=auto",
		"GOTELEMETRY=off",
		"PATH=/usr/local/go/bin:/usr/bin:/bin",
	}, extraEnv...)
	spec := candidate.ContainerSpec{
		Name:      name,
		Operation: op.id,
		Network:   network,
		User:      fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		WorkDir:   filepath.Join(op.grantRoot, filepath.FromSlash(op.moduleGrantRel(mod))),
		Mounts: []candidate.Mount{
			{Source: op.path("src"), Target: op.grantRoot, ReadOnly: srcReadOnly},
			{Source: op.path("modcache"), Target: containerModCache, ReadOnly: step == "test"},
			{Source: work, Target: containerWork},
		},
		Env:  env,
		Args: args,
	}
	fmt.Fprintf(op.out, "==> %s %s: go %s\n", step, mod, strings.Join(args, " "))
	start := op.out.size()
	code, err := op.engine.Run(ctx, spec, op.out)
	end := op.out.size()
	var cleanup *candidate.CleanupError
	if errors.As(err, &cleanup) {
		// The container may still exist and pin this directory; owner.json
		// keeps its name for Close and the next startup.
		return moduleOutcome{}, false, err
	}
	// Run removed the container, so it no longer pins anything.
	op.owner.Containers = op.owner.Containers[:len(op.owner.Containers)-1]
	if werr := op.writeOwner(); werr != nil {
		return moduleOutcome{}, false, errors.Join(err, werr)
	}
	if err != nil {
		if ctx.Err() != nil {
			return moduleOutcome{}, false, fmt.Errorf("check exceeded -check-timeout (%s) or was canceled during %s of %s: %w", op.c.checkTimeout, step, mod, err)
		}
		return moduleOutcome{}, false, err
	}
	fmt.Fprintf(op.out, "<== %s %s: exit %d\n", step, mod, code)
	trace.SpanFromContext(ctx).AddEvent("go_command_exited", trace.WithAttributes(
		attribute.String("surla.check.module", mod),
		attribute.String("surla.check.step", step),
		attribute.Int("surla.check.exit_code", code),
	))
	if code != 0 {
		return moduleOutcome{dir: mod, step: step, exitCode: code, start: start, end: end}, false, nil
	}
	return moduleOutcome{}, true, nil
}

// finish releases the grant, closes the output, then removes the
// operation's copy, module cache and work directories once no container
// still pins them, keeping the output for read_check_output. It returns
// cause joined with any capture or cleanup failure; an unconfirmed container
// removal keeps the whole directory.
func (op *checkOperation) finish(cause error) error {
	defer op.c.checkOps.Done()
	closeErr := errors.Join(op.out.close(), op.closeLive())
	if len(op.owner.Containers) > 0 {
		return errors.Join(cause, closeErr, fmt.Errorf("kept %s because container %s may still exist; remove it with docker rm -f %s", op.dir, op.owner.Containers[0], op.owner.Containers[0]))
	}
	return errors.Join(cause, closeErr, removeOperationScratch(op.dir))
}

// message is the result message: the modules tested, then the first
// failure and the end of its output, bounded as a whole.
func (op *checkOperation) message(failures []moduleOutcome) string {
	head := fmt.Sprintf("tested %d module(s): ", len(op.copy.modules))
	list := strings.Join(op.copy.modules, ", ")
	if len(failures) == 0 {
		return op.boundedMessage(head, list, ". All passed.", nil)
	}
	f := failures[0]
	var b strings.Builder
	switch f.step {
	case "download":
		fmt.Fprintf(&b, ". Dependencies for %s could not be prepared (go list exited %d). If Go reports a missing go.sum or go.work.sum entry, or a go.mod update, call update_dependencies and check again.", f.dir, f.exitCode)
	default:
		fmt.Fprintf(&b, ". %s failed: go %s exited %d.", f.dir, f.step, f.exitCode)
	}
	if len(failures) > 1 {
		fmt.Fprintf(&b, " %d other module step(s) also failed.", len(failures)-1)
	}
	b.WriteString(" Read the full output with read_check_output(op_id). Last output:\n")
	return op.boundedMessage(head, list, b.String(), &f)
}

// boundedMessage assembles a result message of at most
// maxCheckMessageBytes from, in order: head, list, body, the end of the
// failed step's output when failed is non-nil, and the capture-gap note.
// head and body are short fixed sentences; list (module names, written
// files, refusal notes) and the output tail vary. list is cut to the room
// left, or to a quarter of it when output follows, with a pointer to
// read_check_output, which holds the full list; the tail gets the rest.
func (op *checkOperation) boundedMessage(head, list, body string, failed *moduleOutcome) string {
	gap := op.gapNote()
	room := maxCheckMessageBytes - len(head) - len(body) - len(gap)
	if failed == nil {
		return head + clipText(list, room) + body + gap
	}
	list = clipText(list, room/4)
	return head + list + body + op.out.tail(failed.start, failed.end, room-len(list)) + gap
}

// gapNote states the output that was not captured, or is empty when the
// capture is complete.
func (op *checkOperation) gapNote() string {
	if op.out.dropped == 0 {
		return ""
	}
	return fmt.Sprintf("\nOutput after byte %d was not captured: %d bytes dropped.", op.out.captured, op.out.dropped)
}

// moduleGrantRel returns the workspace module mod as a grant-relative slash
// path.
func (op *checkOperation) moduleGrantRel(mod string) string {
	return path.Join(op.workspaceRel, mod)
}

// path returns a path inside the operation directory.
func (op *checkOperation) path(name string) string {
	return filepath.Join(op.dir, name)
}

// writeOwner saves the recovery record before a container is created and
// after one is confirmed removed.
func (op *checkOperation) writeOwner() error {
	raw, err := json.Marshal(op.owner)
	if err != nil {
		return err
	}
	tmp := op.path(ownerFile + ".tmp")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", ownerFile, err)
	}
	return os.Rename(tmp, op.path(ownerFile))
}

// recoverCheckOperations runs when no operation of this server is active
// (in New and in Close after every operation returned); the card store's
// exclusive lock guarantees no other server shares checkWorkDir. For each
// operation directory it retries the removal of every container the
// operation recorded, using that operation's recorded endpoint, and removes
// the scratch of operations whose containers are all confirmed gone. An
// operation with no owner.json never created a container (the record is
// saved before each create), so its scratch is removed directly. The
// captured output always stays; directories with unconfirmed containers
// stay whole. Failures are recorded on the recovery span and returned.
func (c *ControlPlane) recoverCheckOperations() (retErr error) {
	if c.checker == nil || c.checkWorkDir == "" {
		return nil
	}
	_, span := otel.Tracer("surla/controlplane").Start(context.Background(), "surla.controlplane.recover_check_operations")
	defer func() { endCheckSpan(span, retErr) }()
	entries, err := os.ReadDir(c.checkWorkDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	for _, e := range entries {
		if !e.IsDir() || !opIDPattern.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(c.checkWorkDir, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, ownerFile))
		if errors.Is(err, os.ErrNotExist) {
			span.AddEvent("ownerless_scratch_removed", trace.WithAttributes(attribute.String("surla.check.op_id", e.Name())))
			errs = append(errs, removeOperationScratch(dir))
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		var owner checkOwner
		if err := json.Unmarshal(raw, &owner); err != nil {
			errs = append(errs, fmt.Errorf("decode %s: %w", filepath.Join(dir, ownerFile), err))
			continue
		}
		engine := c.checker.Reopen(owner.Docker, owner.Host, filepath.Join(dir, "docker-config"))
		span.AddEvent("retained_containers_retried", trace.WithAttributes(
			attribute.String("surla.check.op_id", e.Name()),
			attribute.Int("surla.check.containers", len(owner.Containers)),
		))
		var left []string
		for _, name := range owner.Containers {
			if err := engine.Remove(name); err != nil {
				left = append(left, name)
				errs = append(errs, err)
			}
		}
		owner.Containers = left
		if len(left) == 0 {
			errs = append(errs, removeOperationScratch(dir))
		}
		if raw, err := json.Marshal(owner); err == nil {
			errs = append(errs, os.WriteFile(filepath.Join(dir, ownerFile), raw, 0o600))
		}
	}
	return errors.Join(errs...)
}

// removeOperationScratch removes everything in an operation directory except
// its captured output, restoring write permission first because Go makes
// module cache directories read-only.
func removeOperationScratch(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.Name() == outputFile || e.Name() == outputMetaFile || e.Name() == ownerFile {
			continue
		}
		p := filepath.Join(dir, e.Name())
		_ = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
		errs = append(errs, os.RemoveAll(p))
	}
	return errors.Join(errs...)
}

// openLive binds the operation to the live source. The grant is the
// outermost source root this server accepted at startup (the initial
// workspace or its enclosing Git working tree), never re-derived from Git
// metadata that may have changed since; openLive opens it directly. It also
// holds the identity-verified workspace wsRoot. The kernel's stored paths of
// both held directories give one spelling for each, whatever spelling the
// operator or Git used, so the workspace must lie inside the grant by those
// names; a workspace moved elsewhere is refused. The workspace's identity is
// recorded so the copy and every publication either act on the verified
// directories or refuse.
func (op *checkOperation) openLive(wsRoot *os.Root, accepted []string) error {
	ws, err := wsRoot.Open(".")
	if err != nil {
		return fmt.Errorf("open the workspace: %w", err)
	}
	op.workspaceFD, err = unix.Openat(int(ws.Fd()), ".", openDirFlags, 0)
	if err := errors.Join(err, ws.Close()); err != nil {
		return fmt.Errorf("hold the workspace: %w", err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(op.workspaceFD, &st); err != nil {
		return fmt.Errorf("hold the workspace: %w", err)
	}
	op.workspaceID = fileID{dev: st.Dev, ino: st.Ino}
	accepted0 := outermost(accepted)
	if op.grantFD, err = unix.Open(accepted0, openDirFlags, 0); err != nil {
		return fmt.Errorf("open the source directory %s: %w", accepted0, err)
	}
	if op.grantRoot, err = fdPath(op.grantFD); err != nil {
		return fmt.Errorf("resolve the source directory %s: %w", accepted0, err)
	}
	wsPath, err := fdPath(op.workspaceFD)
	if err != nil {
		return fmt.Errorf("resolve the workspace: %w", err)
	}
	rel, err := filepath.Rel(op.grantRoot, wsPath)
	if err != nil || !filepath.IsLocal(rel) && rel != "." {
		return gensurla.MakeUnprocessable(fmt.Errorf("workspace %s is outside %s, the source directory this server accepted at startup; if the workspace moved, move it back or restart surla serve from it", wsPath, op.grantRoot))
	}
	op.workspaceRel = filepath.ToSlash(rel)
	return nil
}

// liveDirAt opens the grant-relative directory rel of the live source
// without following a link. A directory inside the workspace is reached from
// the held workspace, so a replaced workspace path is never written; one
// outside it (a go.work above the workspace) from the held grant.
func (op *checkOperation) liveDirAt(rel string) (int, error) {
	if op.workspaceRel == "." {
		return openDirAt(op.workspaceFD, rel)
	}
	if rel == op.workspaceRel {
		return openDirAt(op.workspaceFD, ".")
	}
	if inner, ok := strings.CutPrefix(rel, op.workspaceRel+"/"); ok {
		return openDirAt(op.workspaceFD, inner)
	}
	return openDirAt(op.grantFD, rel)
}

// closeLive releases the held workspace and grant.
func (op *checkOperation) closeLive() error {
	err := errors.Join(closeFD(op.workspaceFD), closeFD(op.grantFD))
	op.workspaceFD, op.grantFD = -1, -1
	return err
}

// closeFD closes fd when it is open (non-negative).
func closeFD(fd int) error {
	if fd < 0 {
		return nil
	}
	return unix.Close(fd)
}

// clipText returns s cut to at most n bytes on a UTF-8 boundary, ending with
// a pointer to the full text in read_check_output when anything was cut.
func clipText(s string, n int) string {
	const more = " … (cut; read_check_output(op_id) has the full list)"
	if len(s) <= n {
		return s
	}
	if n < len(more) {
		return ""
	}
	k := n - len(more)
	for k > 0 && !utf8.RuneStart(s[k]) {
		k--
	}
	return s[:k] + more
}

// endCheckSpan records a failed operation's error with error status and
// ends span. A failing go test is an outcome, not an error.
func endCheckSpan(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "check operation failed")
	}
	span.End()
}

// newOperationID returns a random 32-hex operation ID.
func newOperationID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// outermost returns the root among roots that contains every other root:
// the enclosing Git working tree when there is one.
func outermost(roots []string) string {
	best := ""
	for _, r := range roots {
		if best == "" || len(r) < len(best) {
			best = r
		}
	}
	return best
}

// capWriter writes captured output to a file up to a fixed limit, counting
// every byte past it as dropped so the gap is stated, never hidden.
type capWriter struct {
	// mu serializes writes from docker's stdout and stderr streams.
	mu sync.Mutex
	// f is the output file.
	f *os.File
	// limit is maxCheckLogBytes.
	limit int64
	// captured counts bytes written to f.
	captured int64
	// dropped counts bytes that were not captured: past limit, or after a
	// write failure.
	dropped int64
	// err is the first write failure; nothing is written after it and
	// close reports it.
	err error
	// metaPath is where close records the counts.
	metaPath string
}

// maxCheckLogBytes bounds one operation's retained output on operator disk.
const maxCheckLogBytes = 64 << 20

// newCapWriter creates the output file at path.
func newCapWriter(path string) (*capWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create check output: %w", err)
	}
	return &capWriter{f: f, limit: maxCheckLogBytes, metaPath: filepath.Join(filepath.Dir(path), outputMetaFile)}, nil
}

// Write implements io.Writer, never failing the stream, so the container's
// output is drained to the end: bytes past the limit, and every byte after a
// write failure, are counted as dropped, and close reports the failure.
func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := int64(0)
	if keep := min(int64(len(p)), max(w.limit-w.captured, 0)); keep > 0 && w.err == nil {
		n, err := w.f.Write(p[:keep])
		written = int64(n)
		w.err = err
	}
	w.captured += written
	w.dropped += int64(len(p)) - written
	return len(p), nil
}

// size returns the bytes captured so far.
func (w *capWriter) size() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.captured
}

// tail returns at most n bytes ending at end, starting no earlier than
// start, read back from the output file.
func (w *capWriter) tail(start, end int64, n int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	from := max(start, end-int64(max(n, 0)))
	buf := make([]byte, end-from)
	r, err := os.Open(w.f.Name())
	if err != nil {
		return ""
	}
	defer r.Close()
	k, _ := r.ReadAt(buf, from)
	buf = buf[:k]
	if from > start {
		// Start at a character boundary rather than inside the character
		// the cut would split.
		for skipped := 0; skipped < utf8.UTFMax-1 && len(buf) > 0 && !utf8.RuneStart(buf[0]); skipped++ {
			buf = buf[1:]
		}
	}
	return string(buf)
}

// close closes the file, records the byte counts for read_check_output,
// and returns the first write failure, if any.
func (w *capWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var captureErr error
	if w.err != nil {
		captureErr = fmt.Errorf("capture check output after byte %d (%d bytes not captured): %w", w.captured, w.dropped, w.err)
	}
	raw, err := json.Marshal(outputMeta{Captured: w.captured, Dropped: w.dropped})
	if err != nil {
		return errors.Join(captureErr, w.f.Close(), err)
	}
	return errors.Join(captureErr, w.f.Close(), os.WriteFile(w.metaPath, raw, 0o600))
}

// outputMeta is the saved byte accounting of one operation's output.
type outputMeta struct {
	// Captured is the number of bytes in output.log.
	Captured int64 `json:"captured"`
	// Dropped is the number of bytes past the capture limit.
	Dropped int64 `json:"dropped"`
}

var _ io.Writer = (*capWriter)(nil)
