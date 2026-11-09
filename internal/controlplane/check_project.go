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
	"path/filepath"
	"strings"
	"sync"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/verify"
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
		// dir is <CheckWorkDir>/<id>.
		dir string
		// grantRoot is the live grant the copy reproduces.
		grantRoot string
		// workspaceDir is the active workspace inside the grant.
		workspaceDir string
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
// Linux containers, never changing the workspace.
func (c *ControlPlane) CheckProject(ctx context.Context) (*gensurla.CheckProjectResult, error) {
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
	wsDir, dev, ino := c.currentWorkspaceTarget()
	if wsDir == "" {
		return nil, gensurla.MakeNotImplemented(errors.New("no workspace directory is bound to this control plane"))
	}
	physWS, wsRoot, err := openModuleRootWithIdentity(wsDir, dev, ino)
	if err != nil {
		return nil, err
	}
	_ = wsRoot.Close()
	roots, err := verify.WorkspaceBoundaryRoots(ctx, physWS)
	if err != nil {
		return nil, fmt.Errorf("resolve the workspace's source boundary: %w", err)
	}
	op := &checkOperation{c: c, id: newOperationID(), workspaceDir: physWS, grantRoot: outermost(roots)}
	op.dir = filepath.Join(c.checkWorkDir, op.id)
	for _, d := range []string{op.dir, op.path("src"), op.path("modcache"), op.path("docker-config")} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return nil, fmt.Errorf("create check directory %s: %w", d, err)
		}
	}
	if op.out, err = newCapWriter(op.path(outputFile)); err != nil {
		return nil, err
	}
	c.checkOps.Add(1)
	op.copy, err = copyGrant(op.grantRoot, op.workspaceDir, op.path("src"), c.importedReports)
	if err != nil {
		var changed *errSourceChanged
		if errors.As(err, &changed) {
			return nil, op.finish(err)
		}
		return nil, op.finish(fmt.Errorf("copy the workspace: %w", err))
	}
	for _, p := range op.copy.skipped {
		fmt.Fprintf(op.out, "note: %s is not a regular file, directory or symlink and was not copied\n", p)
	}
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
	if code != 0 {
		return moduleOutcome{dir: mod, step: step, exitCode: code, start: start, end: end}, false, nil
	}
	return moduleOutcome{}, true, nil
}

// finish closes the output, then removes the operation's copy, module cache
// and work directories once no container still pins them, keeping the
// output for read_check_output. It returns cause joined with any cleanup
// failure; an unconfirmed container removal keeps the whole directory.
func (op *checkOperation) finish(cause error) error {
	defer op.c.checkOps.Done()
	closeErr := op.out.close()
	if len(op.owner.Containers) > 0 {
		return errors.Join(cause, closeErr, fmt.Errorf("kept %s because container %s may still exist; remove it with docker rm -f %s", op.dir, op.owner.Containers[0], op.owner.Containers[0]))
	}
	return errors.Join(cause, closeErr, removeOperationScratch(op.dir))
}

// message is the bounded result message: the modules tested, then the
// first failure and the end of its output.
func (op *checkOperation) message(failures []moduleOutcome) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tested %d module(s): %s.", len(op.copy.modules), strings.Join(op.copy.modules, ", "))
	if len(failures) == 0 {
		b.WriteString(" All passed.")
		return op.withGap(b.String())
	}
	f := failures[0]
	switch f.step {
	case "download":
		fmt.Fprintf(&b, " Dependencies for %s could not be prepared (go list exited %d). If Go reports a missing go.sum or go.work.sum entry, or a go.mod update, call update_dependencies and check again.", f.dir, f.exitCode)
	default:
		fmt.Fprintf(&b, " %s failed: go %s exited %d.", f.dir, f.step, f.exitCode)
	}
	if len(failures) > 1 {
		fmt.Fprintf(&b, " %d other module step(s) also failed.", len(failures)-1)
	}
	b.WriteString(" Read the full output with read_check_output(op_id). Last output:\n")
	return op.withGap(b.String() + op.out.tail(f.start, f.end, maxCheckMessageBytes-b.Len()-256))
}

// withGap appends the capture-limit note when output was dropped.
func (op *checkOperation) withGap(s string) string {
	if op.out.dropped > 0 {
		s += fmt.Sprintf("\nOutput after byte %d was not captured: %d bytes dropped.", op.out.limit, op.out.dropped)
	}
	return s
}

// moduleGrantRel returns the workspace module mod as a grant-relative slash
// path.
func (op *checkOperation) moduleGrantRel(mod string) string {
	rel, _ := filepath.Rel(op.grantRoot, filepath.Join(op.workspaceDir, filepath.FromSlash(mod)))
	return filepath.ToSlash(rel)
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

// recoverCheckOperations retries the removal of every container a kept
// operation recorded, using that operation's recorded endpoint, and
// removes the scratch directories of operations whose containers are all
// confirmed gone. Directories with unconfirmed containers stay.
func (c *ControlPlane) recoverCheckOperations() error {
	if c.checker == nil || c.checkWorkDir == "" {
		return nil
	}
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

// validateCheckWorkSeparation fails unless checkWorkDir lies outside every
// protected workspace root, so a builder can never write into, or read, an
// operation's copy, module cache or output through the workspace.
func validateCheckWorkSeparation(checkWorkDir string, protectedDirs ...string) error {
	phys, err := abspath.EvalMissing(checkWorkDir)
	if err != nil {
		return fmt.Errorf("resolve check work directory %s: %w", checkWorkDir, err)
	}
	for _, ws := range protectedDirs {
		if ws == "" {
			continue
		}
		inside, err := abspath.Within(ws, phys)
		if err != nil {
			return err
		}
		if inside {
			return fmt.Errorf("check work directory %s must be outside workspace %s", checkWorkDir, ws)
		}
	}
	return nil
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
	// dropped counts bytes past limit.
	dropped int64
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

// Write implements io.Writer, never failing the stream: bytes past the
// limit are counted as dropped.
func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	keep := min(int64(len(p)), max(w.limit-w.captured, 0))
	if keep > 0 {
		n, err := w.f.Write(p[:keep])
		w.captured += int64(n)
		if err != nil {
			return len(p), nil
		}
	}
	w.dropped += int64(len(p)) - keep
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
	return string(buf[:k])
}

// close closes the file and records the byte counts for read_check_output.
func (w *capWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	raw, _ := json.Marshal(outputMeta{Captured: w.captured, Dropped: w.dropped})
	return errors.Join(w.f.Close(), os.WriteFile(w.metaPath, raw, 0o600))
}

// outputMeta is the saved byte accounting of one operation's output.
type outputMeta struct {
	// Captured is the number of bytes in output.log.
	Captured int64 `json:"captured"`
	// Dropped is the number of bytes past the capture limit.
	Dropped int64 `json:"dropped"`
}

var _ io.Writer = (*capWriter)(nil)
