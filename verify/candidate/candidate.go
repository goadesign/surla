// Package candidate is the only place Surla executes candidate code. Exec
// starts one digest-pinned candidate binary as a single sandboxed process: it
// sees only its explicit environment, reads and writes only its own state
// directory (plus the system libraries needed to load), connects only to the
// loopback ports it was assigned, listens only on its assigned port, and cannot
// create child processes. Output runs one such process to completion and
// returns its standard output apart from its diagnostics. When the platform
// cannot enforce that sandbox, Exec and Output fail with
// ErrIsolationUnavailable and never run the candidate unsandboxed.
//
// Callers such as an evaluator suite keep their own process lifecycle (start,
// kill, restart); the sandbox applies only to the started process, never to the
// trusted caller.
package candidate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/goadesign/surla/internal/abspath"
	"goa.design/goa-ai/eval"
)

type (
	// Artifact is one candidate binary pinned into verifier-owned, read-only
	// storage. Only Pin creates Artifacts, so an Artifact always names a copy
	// the candidate's builder never wrote and the verifier never rewrites.
	Artifact struct {
		// path is the absolute, symlink-free path of the pinned copy.
		path string
		// digest is the lowercase hex SHA-256 of the pinned copy's bytes.
		digest string
	}

	// Spec describes how to run one candidate process.
	Spec struct {
		// Args are the command-line arguments after the binary name.
		Args []string
		// Env is the complete environment as KEY=VALUE entries; nothing is
		// inherited from the caller.
		Env []string
		// StateDir is the only directory the process may read and write. It
		// must exist and is also the process's HOME and TMPDIR.
		StateDir string
		// ConnectPorts are the loopback TCP ports the process may connect to.
		ConnectPorts []int
		// ListenPort is the loopback TCP port the process may listen on, or
		// zero when it must not listen.
		ListenPort int
		// LogFile receives the process's diagnostics: its combined stdout and
		// stderr under Exec, only its stderr under Output. It is owned by the
		// caller and must lie outside StateDir so the candidate cannot rewrite
		// its own diagnostics. Exec and Output create or truncate it.
		LogFile string
	}

	// Process is a running sandboxed candidate process. Its output goes to
	// the caller's log file; the verifier keeps none of it in memory.
	Process struct {
		// cmd is the started command.
		cmd *exec.Cmd
		// logFile is the path of the process's output file.
		logFile string
		// done is closed after the process exited.
		done chan struct{}
		// waitErr is the process's exit error, valid after done is closed.
		waitErr error
	}

	// limitedBuffer keeps a candidate's standard output for Output, up to a
	// fixed size. Only the copy into it writes; Output reads it after the
	// process and that copy have finished.
	limitedBuffer struct {
		// buf holds the bytes written so far.
		buf bytes.Buffer
		// limit is the most bytes buf may hold.
		limit int
		// overflow is set once a write would have exceeded limit.
		overflow bool
	}
)

// sandboxExec is the macOS sandbox launcher. It applies the profile and then
// executes the candidate in place, so the candidate keeps the started PID.
const sandboxExec = "/usr/bin/sandbox-exec"

// ErrIsolationUnavailable reports that this platform cannot enforce the
// candidate sandbox, so the candidate must not run.
var ErrIsolationUnavailable = errors.New("candidate isolation is unavailable on this platform")

// ErrDigestMismatch reports that the binary on disk is not the pinned artifact.
var ErrDigestMismatch = errors.New("candidate binary does not match its pinned digest")

// ErrOutputTooLarge reports a candidate whose standard output exceeded the
// limit its Output caller set; the output read so far is discarded.
var ErrOutputTooLarge = errors.New("candidate standard output exceeds its limit")

// Pin copies the binary at src into store, the verifier's own artifact
// directory, as <store>/<sha256>/<name> with read-only permissions, and returns
// the pinned Artifact. The digest is computed over the bytes written to the
// pinned copy, so later changes to src cannot affect it. Pinning the same bytes
// twice reuses the existing copy after re-checking its digest.
func Pin(store, src string) (Artifact, error) {
	in, err := os.Open(src)
	if err != nil {
		return Artifact{}, fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	if err := os.MkdirAll(store, 0o700); err != nil {
		return Artifact{}, fmt.Errorf("create artifact store: %w", err)
	}
	tmp, err := os.CreateTemp(store, ".pin-*")
	if err != nil {
		return Artifact{}, fmt.Errorf("create pinned copy: %w", err)
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), in); err != nil {
		_ = tmp.Close()
		return Artifact{}, fmt.Errorf("copy %s: %w", src, err)
	}
	if err := tmp.Close(); err != nil {
		return Artifact{}, fmt.Errorf("close pinned copy: %w", err)
	}
	digest := hex.EncodeToString(h.Sum(nil))
	dir := filepath.Join(store, digest)
	path := filepath.Join(dir, filepath.Base(src))
	if _, err := os.Stat(path); err == nil {
		got, err := fileDigest(path)
		if err != nil || got != digest {
			return Artifact{}, fmt.Errorf("%w: existing pinned copy %s", ErrDigestMismatch, path)
		}
		return resolvedArtifact(path, digest)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Artifact{}, fmt.Errorf("create pinned directory: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o500); err != nil {
		return Artifact{}, fmt.Errorf("protect pinned copy: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return Artifact{}, fmt.Errorf("place pinned copy: %w", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		return Artifact{}, fmt.Errorf("protect pinned directory: %w", err)
	}
	return resolvedArtifact(path, digest)
}

// RemoveStore deletes an artifact store created by Pin, restoring write
// permission on its read-only directories first. Callers remove the store
// once every process started from its artifacts has exited.
func RemoveStore(store string) error {
	err := filepath.WalkDir(store, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("unprotect artifact store: %w", err)
	}
	if err := os.RemoveAll(store); err != nil {
		return fmt.Errorf("remove artifact store: %w", err)
	}
	return nil
}

// Digest returns the pinned binary's lowercase hex SHA-256.
func (a Artifact) Digest() string {
	return a.digest
}

// Evidence returns the record an evaluator suite attaches to a scenario result
// to state that the scenario ran this pinned binary. name is the binary's name
// in the verification plan: the name in `surla verify -binary name=package`,
// which the suite receives as `--artifact name=path`. surla verify accepts a
// scenario only when its result carries one such record for every bound
// binary, with the digest the verifier measured. For example, the binary bound
// as orders with digest 9f2c… yields
// eval.Artifact{Name: "candidate:orders", URI: "sha256:9f2c…"}.
func (a Artifact) Evidence(name string) eval.Artifact {
	return eval.Artifact{Name: "candidate:" + name, URI: "sha256:" + a.digest}
}

// Exec verifies the artifact's digest and starts it in the sandbox described by
// spec. Relative paths resolve from the caller's working directory before the
// candidate changes into its state directory. The process is killed when ctx ends.
func Exec(ctx context.Context, a Artifact, spec Spec) (*Process, error) {
	return start(ctx, a, spec, nil)
}

// Output runs the artifact to completion in the same sandbox as Exec and
// returns what it printed on standard output, so the caller reads a program's
// answer exactly instead of picking it out of a log: spec.LogFile receives
// only standard error. Standard output may hold at most maxBytes; one byte
// more stops the read and fails with ErrOutputTooLarge, which the candidate
// sees as a closed pipe. A candidate that exits unsuccessfully returns its
// standard output with the exit error, an *exec.ExitError. Every Exec
// admission error, including ErrIsolationUnavailable, is returned before the
// candidate starts. The process is killed when ctx ends.
//
// Output suits short programs whose answer fits in memory, such as an
// evaluator suite's --describe.
func Output(ctx context.Context, a Artifact, spec Spec, maxBytes int) ([]byte, error) {
	out := &limitedBuffer{limit: maxBytes}
	p, err := start(ctx, a, spec, out)
	if err != nil {
		return nil, err
	}
	waitErr := p.Wait()
	if out.overflow {
		return nil, fmt.Errorf("%w of %d bytes", ErrOutputTooLarge, maxBytes)
	}
	return out.buf.Bytes(), waitErr
}

// ExcerptFile returns at most maxBytes from the end of the log file at path,
// such as a Spec.LogFile after Exec or Output, and whether earlier output was
// left out. It reads only those bytes, however large the file is, so a
// candidate that writes without limit to its log cannot make the caller hold
// it in memory.
func ExcerptFile(path string, maxBytes int64) (string, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, fmt.Errorf("open log file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", false, fmt.Errorf("stat log file: %w", err)
	}
	start := max(info.Size()-maxBytes, 0)
	b := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(b, start); err != nil && !errors.Is(err, io.EOF) {
		return "", false, fmt.Errorf("read log file: %w", err)
	}
	return string(b), start > 0, nil
}

// start verifies the artifact and starts it in the sandbox described by spec.
// stdout receives standard output when non-nil; otherwise standard output joins
// standard error in spec.LogFile.
func start(ctx context.Context, a Artifact, spec Spec, stdout io.Writer) (*Process, error) {
	if runtime.GOOS != "darwin" {
		return nil, ErrIsolationUnavailable
	}
	if _, err := os.Stat(sandboxExec); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIsolationUnavailable, err)
	}
	if a.path == "" {
		return nil, errors.New("artifact was not pinned")
	}
	digest, err := fileDigest(a.path)
	if err != nil {
		return nil, err
	}
	if digest != a.digest {
		return nil, fmt.Errorf("%w: %s is %s, pinned %s", ErrDigestMismatch, a.path, digest, a.digest)
	}
	stateDir, err := abspath.Eval(spec.StateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	binary := a.path
	logPath, err := abspath.EvalMissing(spec.LogFile)
	if err != nil {
		return nil, fmt.Errorf("resolve log file %q: %w", spec.LogFile, err)
	}
	inside, err := abspath.Within(stateDir, logPath)
	if err != nil {
		return nil, fmt.Errorf("check log file outside candidate state directory: %w", err)
	}
	if inside {
		return nil, fmt.Errorf("log file %s must lie outside the candidate state directory", logPath)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file: %w", err)
	}
	profile := Profile(binary, stateDir, spec.ConnectPorts, spec.ListenPort)
	cmd := exec.CommandContext(ctx, sandboxExec, append([]string{"-p", profile, binary}, spec.Args...)...)
	cmd.Env = append([]string{"HOME=" + stateDir, "TMPDIR=" + stateDir}, spec.Env...)
	cmd.Dir = stateDir
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if stdout != nil {
		cmd.Stdout = stdout
	}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("start candidate %s: %w", binary, err)
	}
	p := &Process{cmd: cmd, logFile: logPath, done: make(chan struct{})}
	go func() {
		p.waitErr = cmd.Wait()
		_ = logFile.Close()
		close(p.done)
	}()
	return p, nil
}

// BuildSpec describes one sandboxed compile of candidate source.
type BuildSpec struct {
	// SourceDir is the declared source tree the build may read, such as the
	// candidate module or a whole checkout whose modules replace each other.
	// Besides SourceDir, the build can read only GOROOT, ModCache, WorkDir,
	// the output directory and system libraries; a symbolic link or local
	// replace directive that resolves anywhere else is denied.
	SourceDir string
	// ModuleDir is the directory of the module being built. It must resolve
	// inside SourceDir.
	ModuleDir string
	// Package is the package to build, relative to ModuleDir, such as
	// "./cmd/orders".
	Package string
	// Out is the output binary path; its directory is writable.
	Out string
	// GOROOT is the resolved Go toolchain root the build runs with. Its bin
	// and tool binaries are the only programs the build may execute.
	GOROOT string
	// ModCache is the already populated module cache, read-only.
	ModCache string
	// WorkDir is a private writable directory for TMPDIR and GOCACHE.
	WorkDir string
	// LogFile receives the build's output; it must lie outside ModuleDir.
	LogFile string
}

// resolvedBuild is a BuildSpec whose directories resolveBuild has resolved to
// symlink-free paths, with Out inside outDir. Only resolveBuild creates one.
type resolvedBuild struct {
	// spec holds the resolved paths; spec.Out is the absolute output file.
	spec BuildSpec
	// outDir is the resolved output directory, the only place besides
	// spec.WorkDir that the build may write.
	outDir string
}

// Build compiles source in the build sandbox. The Go toolchain may start its
// own compiler, assembler and linker, but the build can read only SourceDir,
// the toolchain, the read-only module cache, its own work and output
// directories, and system libraries; write only its work and output
// directories; and use no network. Directives in candidate source, such as an
// assembly #include of an outside file, and links or replace directives that
// leave SourceDir are denied by the operating system. It fails with
// ErrIsolationUnavailable where the sandbox cannot be enforced.
//
// Besides SourceDir, the build trusts GOROOT and ModCache as fixed inputs: the
// selected toolchain, and module versions verified against their checksums
// when they were downloaded. A source file linked into either reads bytes the
// build could already read by importing that toolchain package or module
// version, so such a link grants nothing beyond that trust. Build does not
// check that WorkDir and the output directory are fresh: a caller that relies
// on them holding only this build's output must create them empty.
//
// run runs the prepared build command to completion. The caller decides which
// process group owns the compiler, assembler and linker the build starts: a
// verification passes (*exec.Cmd).Run so they stay in the verification's
// group, which its supervisor stops; a caller without such a group passes a
// runner that gives the build its own group and stops it.
func Build(ctx context.Context, spec BuildSpec, run func(*exec.Cmd) error) error {
	b, err := resolveBuild(spec)
	if err != nil {
		return err
	}
	if err := b.goRun(ctx, run, b.spec.Out, "build", "-trimpath"); err != nil {
		return fmt.Errorf("sandboxed build of %s: %w", spec.Package, err)
	}
	return nil
}

// BuildTest compiles the tests of one candidate package into a single test
// binary at spec.Out, in the same build sandbox, environment and process-group
// contract as Build. It runs `go test -c -trimpath -vet=off`, so go vet
// findings do not fail it, and a compile error in the package or its tests
// fails it with the toolchain output in spec.LogFile. Test-only imports must
// already be in go.mod, go.sum and ModCache, as for any Build.
//
// spec.Package must name one package directory inside ModuleDir: "." or a
// path starting with "./" and containing no "..." pattern. Anything else, such
// as an import path, "all", or a value the go command would read as a flag
// such as -o or -c, is rejected before the toolchain runs.
//
// spec.Out must not exist when BuildTest is called. BuildTest compiles to a
// fresh staging path in the output directory and links the result to spec.Out
// only when the toolchain produced a test binary, so other files written
// during the build cannot be mistaken for the test binary or overwritten by
// it. BuildTest returns true when it wrote the test binary, a regular file at
// spec.Out. A package without _test.go files is not an error: BuildTest
// returns false and a nil error and writes no test binary.
//
// The caller runs the result only as a candidate: pin it with Pin and start
// it with Exec, passing test flags such as -test.run and -test.timeout in
// Spec.Args. Exec's sandbox forbids child processes, network access beyond
// the assigned loopback ports, and file access outside Spec.StateDir beyond
// what Exec documents, so a test that needs any of them fails inside the
// sandbox instead of escaping it.
func BuildTest(ctx context.Context, spec BuildSpec, run func(*exec.Cmd) error) (bool, error) {
	if pkg := spec.Package; (pkg != "." && !strings.HasPrefix(pkg, "./")) || !filepath.IsLocal(pkg) || strings.Contains(pkg, "...") {
		return false, fmt.Errorf("test build package %q is not one package path", spec.Package)
	}
	b, err := resolveBuild(spec)
	if err != nil {
		return false, err
	}
	pkgDir := filepath.Join(b.spec.ModuleDir, filepath.FromSlash(spec.Package))
	switch pkgInfo, err := os.Stat(pkgDir); {
	case err != nil:
		return false, fmt.Errorf("inspect test package directory %s: %w", spec.Package, err)
	case !pkgInfo.IsDir():
		return false, fmt.Errorf("test build package %q is not a directory", spec.Package)
	}
	out := b.spec.Out
	switch _, err := os.Lstat(out); {
	case err == nil:
		return false, fmt.Errorf("test binary path %s must not exist before the build", out)
	case !errors.Is(err, os.ErrNotExist):
		return false, fmt.Errorf("inspect test binary path %s: %w", out, err)
	}
	stageDir, err := os.MkdirTemp(b.outDir, ".buildtest-*")
	if err != nil {
		return false, fmt.Errorf("create test build staging directory: %w", err)
	}
	defer os.RemoveAll(stageDir)
	stageOut := filepath.Join(stageDir, filepath.Base(out))
	if err := b.goRun(ctx, run, stageOut, "test", "-c", "-trimpath", "-vet=off"); err != nil {
		return false, fmt.Errorf("sandboxed test build of %s: %w", spec.Package, err)
	}
	info, err := os.Lstat(stageOut)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("inspect test binary %s: %w", out, err)
	case !info.Mode().IsRegular():
		return false, fmt.Errorf("test binary %s is not a regular file", out)
	}
	if err := os.Link(stageOut, out); err != nil {
		return false, fmt.Errorf("publish test binary %s: %w", out, err)
	}
	return true, nil
}

// BuildProfile renders the build sandbox profile. Paths must be absolute and
// free of symlinks.
func BuildProfile(sourceDir, goroot, modCache, workDir, outDir string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n(allow process-fork)\n(allow sysctl-read)\n(allow signal (target self))\n")
	fmt.Fprintf(&b, "(allow process-exec (subpath %s))\n", quote(goroot))
	fmt.Fprintf(&b, "(allow file-read* (literal \"/\") (subpath \"/usr/lib\") (subpath \"/System\") (subpath \"/private/var/db/dyld\") "+
		"(literal \"/dev/null\") (literal \"/dev/urandom\") (literal \"/dev/random\") "+
		"(subpath %s) (subpath %s) (subpath %s) (subpath %s) (subpath %s))\n",
		quote(sourceDir), quote(goroot), quote(modCache), quote(workDir), quote(outDir))
	fmt.Fprintf(&b, "(allow file-write* (subpath %s) (subpath %s) (literal \"/dev/null\"))\n", quote(workDir), quote(outDir))
	b.WriteString("(allow file-read-metadata (literal \"/var\") (literal \"/tmp\") (literal \"/etc\")")
	for _, root := range []string{sourceDir, goroot, modCache, workDir, outDir} {
		for dir := filepath.Dir(root); dir != "/"; dir = filepath.Dir(dir) {
			fmt.Fprintf(&b, " (literal %s)", quote(dir))
		}
	}
	b.WriteString(")\n")
	return b.String()
}

// Profile renders the sandbox profile for one candidate process. Paths must be
// absolute and already free of symlinks.
func Profile(binary, stateDir string, connectPorts []int, listenPort int) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(deny default)\n")
	fmt.Fprintf(&b, "(allow process-exec (literal %s))\n", quote(binary))
	// The standard macOS top-level symlinks must be resolvable so callers can
	// use /var/... and /tmp/... paths; this reveals only that they exist.
	b.WriteString("(allow sysctl-read)\n(allow file-read-metadata (literal \"/var\") (literal \"/tmp\") (literal \"/etc\")")
	// Libraries such as SQLite stat every directory on the path to their
	// files; the state directory's ancestors are visible as metadata only.
	for dir := filepath.Dir(stateDir); dir != "/"; dir = filepath.Dir(dir) {
		fmt.Fprintf(&b, " (literal %s)", quote(dir))
	}
	b.WriteString(")\n")
	fmt.Fprintf(&b, "(allow file-read* (literal \"/\") (literal %s) (subpath \"/usr/lib\") (subpath \"/System\") "+
		"(subpath \"/private/var/db/dyld\") (subpath %s) (literal \"/dev/null\") (literal \"/dev/urandom\") (literal \"/dev/random\"))\n",
		quote(binary), quote(stateDir))
	fmt.Fprintf(&b, "(allow file-write* (subpath %s) (literal \"/dev/null\"))\n", quote(stateDir))
	for _, port := range connectPorts {
		fmt.Fprintf(&b, "(allow network-outbound (remote ip \"localhost:%d\"))\n", port)
	}
	if listenPort != 0 {
		fmt.Fprintf(&b, "(allow network-bind (local ip \"localhost:%d\"))\n", listenPort)
		fmt.Fprintf(&b, "(allow network-inbound (local ip \"localhost:%d\"))\n", listenPort)
	}
	return b.String()
}

// Kill sends SIGKILL to the candidate process and waits for it to exit. The
// sandbox forbids child processes, so the started PID is the whole process
// tree.
func (p *Process) Kill() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill candidate: %w", err)
	}
	<-p.done
	return nil
}

// Wait waits for the process to exit and returns its exit error.
func (p *Process) Wait() error {
	<-p.done
	return p.waitErr
}

// Done is closed once the process exited.
func (p *Process) Done() <-chan struct{} {
	return p.done
}

// Excerpt returns at most maxBytes from the end of the process's output so
// far, and whether earlier output was left out.
func (p *Process) Excerpt(maxBytes int64) (string, bool, error) {
	return ExcerptFile(p.logFile, maxBytes)
}

// Write keeps p, or fails without keeping any of it when buf would then exceed
// limit. The failure stops the copy from the candidate's standard output.
func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len()+len(p) > b.limit {
		b.overflow = true
		return 0, ErrOutputTooLarge
	}
	return b.buf.Write(p)
}

// fileDigest returns the lowercase hex SHA-256 of the file's bytes.
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// resolveBuild checks that the build sandbox is available and resolves every
// directory in spec to its symlink-free path, placing Out in the resolved
// output directory. The sandbox profile is written in those resolved paths, so
// the toolchain sees the same paths the profile allows.
func resolveBuild(spec BuildSpec) (resolvedBuild, error) {
	if runtime.GOOS != "darwin" {
		return resolvedBuild{}, ErrIsolationUnavailable
	}
	if _, err := os.Stat(sandboxExec); err != nil {
		return resolvedBuild{}, fmt.Errorf("%w: %v", ErrIsolationUnavailable, err)
	}
	paths := map[string]*string{
		"source": &spec.SourceDir, "module": &spec.ModuleDir, "goroot": &spec.GOROOT,
		"modcache": &spec.ModCache, "work": &spec.WorkDir,
	}
	for name, p := range paths {
		resolved, err := abspath.Eval(*p)
		if err != nil {
			return resolvedBuild{}, fmt.Errorf("resolve %s directory: %w", name, err)
		}
		*p = resolved
	}
	if rel, err := filepath.Rel(spec.SourceDir, spec.ModuleDir); err != nil || !filepath.IsLocal(rel) {
		return resolvedBuild{}, fmt.Errorf("module directory %s is outside source directory %s",
			spec.ModuleDir, spec.SourceDir)
	}
	if spec.Out == "" {
		return resolvedBuild{}, fmt.Errorf("resolve output directory: %w", os.ErrNotExist)
	}
	absOut, err := filepath.Abs(spec.Out)
	if err != nil {
		return resolvedBuild{}, fmt.Errorf("resolve output directory: %w", err)
	}
	outDir, err := abspath.Eval(filepath.Dir(absOut))
	if err != nil {
		return resolvedBuild{}, fmt.Errorf("resolve output directory: %w", err)
	}
	spec.Out = filepath.Join(outDir, filepath.Base(absOut))
	return resolvedBuild{spec: spec, outDir: outDir}, nil
}

// resolvedArtifact returns the Artifact for a pinned copy with an absolute,
// symlink-free path, so a caller changing directories cannot change the binary
// the sandbox profile permits or the process executes.
func resolvedArtifact(path, digest string) (Artifact, error) {
	resolved, err := abspath.Eval(path)
	if err != nil {
		return Artifact{}, fmt.Errorf("resolve pinned copy: %w", err)
	}
	return Artifact{path: resolved, digest: digest}, nil
}

// quote renders s as a sandbox profile string literal.
func quote(s string) string {
	return strconv.Quote(s)
}

// goRun runs `go <args> -o <outPath> <Package>` from ModuleDir in the build
// sandbox, which may write only WorkDir and the output directory, with the
// toolchain's output in LogFile. It returns run's error unwrapped, an
// "open build log" error when LogFile cannot be opened, or an error when
// opening LogFile resolves to the same file as spec.Out. The fixed
// environment keeps the build offline (GOPROXY=off), on the selected toolchain
// (GOTOOLCHAIN=local), unable to change go.mod (-mod=readonly), and free of
// cgo, with TMPDIR and GOCACHE inside WorkDir and no HOME, so os.UserConfigDir
// leaves Go telemetry disabled without starting its detached child process.
func (b resolvedBuild) goRun(
	ctx context.Context,
	run func(*exec.Cmd) error,
	outPath string,
	args ...string,
) error {
	spec := b.spec
	_, outStatErr := os.Lstat(spec.Out)
	logFile, err := os.OpenFile(spec.LogFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open build log: %w", err)
	}
	defer logFile.Close()
	logInfo, err := logFile.Stat()
	if err != nil {
		return fmt.Errorf("stat build log: %w", err)
	}
	if outInfo, err := os.Stat(spec.Out); err == nil && os.SameFile(logInfo, outInfo) {
		aliasErr := fmt.Errorf("build log %s must not be the output binary %s", spec.LogFile, spec.Out)
		if errors.Is(outStatErr, os.ErrNotExist) {
			if rmErr := os.Remove(spec.Out); rmErr != nil {
				return errors.Join(aliasErr, rmErr)
			}
		}
		return aliasErr
	}
	goBin := filepath.Join(spec.GOROOT, "bin", "go")
	profile := BuildProfile(spec.SourceDir, spec.GOROOT, spec.ModCache, spec.WorkDir, b.outDir)
	cmdArgs := append([]string{"-p", profile, goBin}, args...)
	cmd := exec.CommandContext(ctx, sandboxExec, append(cmdArgs, "-o", outPath, spec.Package)...)
	cmd.Dir = spec.ModuleDir
	cmd.Env = []string{
		"PATH=" + filepath.Join(spec.GOROOT, "bin") + ":/usr/bin:/bin",
		"TMPDIR=" + spec.WorkDir,
		"GOROOT=" + spec.GOROOT,
		"GOCACHE=" + filepath.Join(spec.WorkDir, "gocache"),
		"GOMODCACHE=" + spec.ModCache,
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=readonly",
		"GOPROXY=off",
		"GONOSUMDB=*",
		"CGO_ENABLED=0",
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	return run(cmd)
}
