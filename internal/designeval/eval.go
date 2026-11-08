// Package designeval evaluates Go/Goa/Goa-AI/Model design sources by compiling a
// standalone evaluator binary and executing it inside verify/candidate.Exec's
// OS-enforced single-process sandbox. Registered generators return source bytes;
// the evaluator hashes those bytes and reads Goa-AI's existing tool catalogs.
// Protobuf comparisons include Goa's .proto sources, without invoking protoc or
// claiming that generated source equality proves runtime compatibility.
package designeval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goadesign/surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/procgroup"
	"github.com/goadesign/surla/verify/candidate"
	"golang.org/x/mod/modfile"
)

type (
	// Result holds the canonical declarations, C4 topology, agent policies, and
	// tool contracts extracted from an evaluated Goa/Goa-AI/Model design.
	Result struct {
		// SystemName is the evaluated API name, Model system name, or primary service name.
		SystemName string `json:"system_name"`
		// Declarations contains generated source digests under Generated:<path>
		// and domain, Model, and agent-policy declaration records under scoped names.
		Declarations map[string]string `json:"declarations"`
		// Containers lists the C4 containers derived from evaluated Services, Agents, Toolsets, and Model containers.
		Containers []*gensurla.C4ContainerItem `json:"containers"`
		// Edges lists the validated directed contract edges between C4 containers.
		Edges []*gensurla.C4EdgeItem `json:"edges"`
		// AgentPolicies lists the evaluated Goa-AI RunPolicy settings per agent.
		AgentPolicies []*gensurla.AgentPolicyItem `json:"agent_policies"`
		// Tools lists the public tools selected by each agent, with compiled input
		// choices; it does not claim which binding the application installs.
		Tools []*gensurla.ToolContractItem `json:"tools"`
	}

	// Evaluator evaluates design snapshots for one owner, such as a
	// ControlPlane or one `surla preview` or `surla diff` command. Its
	// evaluations share one private build workspace (the evaluator build's
	// HOME, TMPDIR and GOCACHE), so later evaluations reuse compiled
	// dependencies. The workspace is created by the first admitted Evaluate,
	// never by NewEvaluator, and Close removes it. Evaluate may be called
	// concurrently. After Close, Evaluate fails and creates nothing. The owner
	// must call Close; an unclosed Evaluator leaves its workspace behind.
	Evaluator struct {
		// ctx ends when Close starts; every admitted evaluation runs under it
		// and under its caller's context.
		ctx context.Context
		// cancel ends ctx.
		cancel context.CancelFunc
		// closeResult runs the cleanup once and returns its result to every
		// Close call.
		closeResult func() error
		// active counts admitted evaluations that have not finished their own
		// cleanup. Add is called only under mu while closed is false.
		active sync.WaitGroup
		// mu guards workDir, closed and unsafe. It is never held across a
		// command, a build or a wait.
		mu sync.Mutex
		// workDir is the shared build workspace, empty until first use.
		workDir string
		// tempParentDir is the owner's temporary storage parent; empty uses
		// the host default for embedded callers without a workspace.
		tempParentDir string
		// closed is set when Close starts; later evaluations are refused.
		closed bool
		// unsafe is set, and never cleared, once an evaluation's build
		// processes could not be confirmed stopped: they may still use
		// workDir, so Close keeps it.
		unsafe bool
	}

	// toolchainEnv holds the Go toolchain root and module cache one
	// evaluation resolves and uses for both module resolution and the build.
	toolchainEnv struct {
		// goroot is the resolved Go toolchain root.
		goroot string
		// modCache is the read-only Go module cache directory.
		modCache string
	}

	// generatedFile is one entry of the generation program's sources.json.
	generatedFile struct {
		// Path is the module-relative, slash-separated path.
		Path string `json:"path"`
		// Bytes is the file's content.
		Bytes []byte `json:"bytes"`
	}

	// buildError reports a harness program that did not compile. Its message
	// carries the compiler's diagnostics unchanged.
	buildError struct {
		// err is the compile error, with the build log.
		err error
	}

	// stuckBuildError reports an evaluator build whose process group did not
	// empty after the build ended. Its processes may still use the evaluation's
	// temporary directory, so Evaluate keeps it.
	stuckBuildError struct {
		// err is procgroup.Run's stop error.
		err error
	}
)

// buildStopGrace bounds how long the evaluator build gets to exit after it
// is killed, and how long its process group may take to empty.
const buildStopGrace = 10 * time.Second

// completeSourcesGoa is the first goa.design/goa/v3 version that provides
// generator.CompleteSources, which Generate needs.
const completeSourcesGoa = "v3.35.1-0.20261103205041-35e789e8f9b1"

// sandboxExit71 is the exit status (EX_OSERR) /usr/bin/sandbox-exec returns
// when macOS refuses to apply a sandbox profile (for example inside a nested
// restricted coding-agent shell), and also when the sandboxed child itself
// calls os.Exit(71).
const sandboxExit71 = 71

// exit71Guidance is appended to a non-canceled sandboxed build or evaluation
// error whose command exited with status 71. It states the observed exit code
// and gives conditional recovery advice without claiming the sandbox failed to
// start.
const exit71Guidance = "The sandboxed command exited with status 71. If Surla is running inside a restricted coding-agent shell, its sandbox may have been blocked; run Surla from an unrestricted host shell."

// errEvaluatorClosed is the cause of an evaluation canceled by Close, and
// the error Evaluate returns after Close.
var errEvaluatorClosed = errors.New("design evaluator is closed")

// NewEvaluator returns an Evaluator. It runs no command and creates no file.
func NewEvaluator() *Evaluator {
	return NewEvaluatorInDir("")
}

// NewEvaluatorInDir returns an evaluator whose compiler workspace and
// evaluation directories are created under tempParentDir, never the process's
// TMPDIR. Filesystem hosts pass a parent outside their builder's write grant.
// An empty parent preserves NewEvaluator's host-default behavior. Construction
// creates nothing; the first operation creates the parent if needed, and Close
// removes only this evaluator's temporary directories, keeping unsafe builds.
func NewEvaluatorInDir(tempParentDir string) *Evaluator {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Evaluator{ctx: ctx, cancel: cancel, tempParentDir: tempParentDir}
	e.closeResult = sync.OnceValue(e.close)
	return e
}

// Evaluate evaluates the Goa/Goa-AI/Model design in a module snapshot and
// returns its declarations, C4 topology, agent policies and tool contracts.
// It runs until it finishes, ctx ends, or Close is called, and fails at once
// after Close.
//
// files is the module's exact content keyed by module-relative path (see
// validateSnapshot). The snapshot is staged byte for byte; no host path it
// names is read. Its dependencies are resolved only from its go.mod and
// go.sum, and evaluation fails rather than use different versions (see
// prepareHarnessModule). The evaluator program is then compiled with
// candidate.Build and run with candidate.Exec, so neither compiling nor
// running the design can read evaluator files or reach the network.
func (e *Evaluator) Evaluate(ctx context.Context, files map[string]string) (Result, error) {
	var res Result
	err := e.admitted(ctx, func(ctx context.Context, workDir string) error {
		var err error
		res, err = evaluate(ctx, files, workDir)
		return err
	})
	return res, err
}

// Generate returns every path and its bytes that `goa gen` writes for the
// design in a module snapshot, including gen/goa.json, with each generated
// file's header naming "$ surla gen". It compiles and runs a generation
// program inside the same sandbox as Evaluate, with the target module's own
// dependency versions, and calls Goa's generator.CompleteSources, which fails
// when a generated file would be changed after it is written, such as a gRPC
// design's protobuf output.
//
// The target must select a goa.design/goa/v3 that provides CompleteSources.
// When the generation program does not compile, the error keeps the
// compiler's diagnostics and names the Goa module the build selected, so the
// caller can tell whether the module needs a newer Goa. Like Evaluate, it
// shares the evaluator's workspace and fails at once after Close.
func (e *Evaluator) Generate(ctx context.Context, files map[string]string) (map[string][]byte, error) {
	var out map[string][]byte
	err := e.admitted(ctx, func(ctx context.Context, workDir string) error {
		var err error
		out, err = generate(ctx, files, workDir)
		return err
	})
	return out, err
}

// Close cancels the evaluations in progress, waits until each has released
// its files, and removes the shared workspace. If any evaluation's build
// processes could not be confirmed stopped, it keeps the workspace and its
// error names it. Every call, including concurrent ones, returns the result
// of that one cleanup once it has finished.
func (e *Evaluator) Close() error {
	return e.closeResult()
}

// Summary returns a one-line count of what r declares, such as
// `"orders": 3 container(s), 2 boundary edge(s), 1 agent policy(s), 4 tool contract(s)`.
// The system name is Go-quoted, so the line stays a single line whatever the
// design names its system. `surla preview` prints it to stderr, and
// preview_architecture returns it as its banner.
func (r Result) Summary() string {
	return fmt.Sprintf(
		"%q: %d container(s), %d boundary edge(s), %d agent policy(s), %d tool contract(s)",
		r.SystemName, len(r.Containers), len(r.Edges), len(r.AgentPolicies), len(r.Tools),
	)
}

// Error returns the compile error and its diagnostics.
func (e *buildError) Error() string {
	return e.err.Error()
}

// Unwrap returns the compile error.
func (e *buildError) Unwrap() error {
	return e.err
}

// Error describes the build processes left running.
func (e *stuckBuildError) Error() string {
	return "build processes did not exit: " + e.err.Error()
}

// Unwrap returns procgroup.Run's stop error.
func (e *stuckBuildError) Unwrap() error {
	return e.err
}

// generate runs one generation in a temporary directory of its own, with
// workDir as the build's shared HOME, TMPDIR and GOCACHE.
func generate(ctx context.Context, files map[string]string, workDir string) (map[string][]byte, error) {
	var out map[string][]byte
	goa, err := runSandboxed(ctx, files, workDir, generateMainTemplate,
		func(stateDir string) []string {
			// One literal argument, which Goa copies into generated headers.
			return []string{"-sources", filepath.Join(stateDir, "sources.json"), "--cmd=$ surla gen"}
		},
		func(stateDir string) error {
			var err error
			out, err = readSources(stateDir)
			return err
		})
	// A build failure leads with the compiler's own error, whatever caused
	// it: a design that doesn't compile, a cancelled build, or a Goa without
	// generator.CompleteSources. The recovery for that last case follows,
	// conditional on the compiler output, because a versioned replacement
	// can lack the function whatever its version string.
	if build := (*buildError)(nil); errors.As(err, &build) {
		return nil, fmt.Errorf("%w\nIf that output shows that generator.CompleteSources is undefined, the "+
			"goa.design/goa/v3 this module builds with (%s) lacks it; surla gen needs %s or later. Update the "+
			"module's goa.design/goa/v3 requirement, or its replace directive, and go.sum, then run surla gen again",
			err, describe(goa), completeSourcesGoa)
	}
	return out, err
}

// readSources reads the generation program's sources.json from stateDir. The
// program is untrusted, so the file is opened through a root confined to
// stateDir, must be a regular file, and is decoded strictly: unknown fields,
// trailing data, and repeated paths are errors.
func readSources(stateDir string) (_ map[string][]byte, err error) {
	root, err := os.OpenRoot(stateDir)
	if err != nil {
		return nil, fmt.Errorf("open generation output: %w", err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	info, err := root.Lstat("sources.json")
	if err != nil {
		return nil, fmt.Errorf("read generation output: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("generation output sources.json is not a regular file (%s)", info.Mode().Type())
	}
	f, err := root.Open("sources.json")
	if err != nil {
		return nil, fmt.Errorf("read generation output: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var listed []generatedFile
	if err := dec.Decode(&listed); err != nil {
		return nil, fmt.Errorf("decode generation output: %w", err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, errors.New("decode generation output: unexpected data after the file list")
	}
	out := make(map[string][]byte, len(listed))
	for _, g := range listed {
		if _, dup := out[g.Path]; dup {
			return nil, fmt.Errorf("decode generation output: %s is listed twice", g.Path)
		}
		out[g.Path] = g.Bytes
	}
	return out, nil
}

// evaluate runs one evaluation in a temporary directory of its own, with
// workDir as the build's shared HOME, TMPDIR and GOCACHE.
func evaluate(ctx context.Context, files map[string]string, workDir string) (Result, error) {
	var res Result
	_, err := runSandboxed(ctx, files, workDir, harnessMainTemplate,
		func(stateDir string) []string {
			return []string{"-out", filepath.Join(stateDir, "evaluated.json")}
		},
		func(stateDir string) error {
			rawJSON, err := os.ReadFile(filepath.Join(stateDir, "evaluated.json"))
			if err != nil {
				return fmt.Errorf("read evaluated design output: %w", err)
			}
			if err := json.Unmarshal(rawJSON, &res); err != nil {
				return fmt.Errorf("decode evaluated Goa design: %w", err)
			}
			return nil
		})
	return res, err
}

// runSandboxed stages the module snapshot, compiles program (a harness
// template whose {{DESIGN_IMPORT}} and {{GEN_IMPORT}} name the target's
// packages) with candidate.Build against the target's own dependency
// versions, and runs it with candidate.Exec and args(stateDir). collect then
// reads the program's output from stateDir. Everything happens in a temporary
// directory of its own, with workDir as the build's shared HOME, TMPDIR and
// GOCACHE; releaseTemp removes it, or keeps it after a stuck build. It
// returns the Goa module the build selected, and a *buildError when the
// program does not compile.
func runSandboxed(ctx context.Context, files map[string]string, workDir, program string, args func(stateDir string) []string, collect func(stateDir string) error) (goa listedModule, err error) {
	target, err := validateSnapshot(files)
	if err != nil {
		return listedModule{}, err
	}
	evalMod, err := evaluatorGoMod()
	if err != nil {
		return listedModule{}, err
	}
	tc, err := resolveToolchainEnv(ctx)
	if err != nil {
		return listedModule{}, err
	}

	tmpDir, err := os.MkdirTemp(filepath.Dir(workDir), "surla-designeval-*")
	if err != nil {
		return listedModule{}, fmt.Errorf("create temp dir: %w", err)
	}
	storeDir := filepath.Join(tmpDir, "store")
	defer func() { err = releaseTemp(tmpDir, storeDir, err) }()

	buildDir := filepath.Join(tmpDir, "build")
	outDir := filepath.Join(tmpDir, "out")
	stateDir := filepath.Join(tmpDir, "state")
	logDir := filepath.Join(tmpDir, "logs")
	for _, d := range []string{buildDir, outDir, storeDir, stateDir, logDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return listedModule{}, fmt.Errorf("mkdir %q: %w", d, err)
		}
	}
	if err := stageSnapshot(filepath.Join(buildDir, "target"), files); err != nil {
		return listedModule{}, err
	}
	main := strings.ReplaceAll(program, "{{DESIGN_IMPORT}}", strconv.Quote(target.Module.Mod.Path+"/design"))
	main = strings.ReplaceAll(main, "{{GEN_IMPORT}}", strconv.Quote(target.Module.Mod.Path+"/gen"))
	if err := os.WriteFile(filepath.Join(buildDir, "main.go"), []byte(main), 0o600); err != nil {
		return listedModule{}, fmt.Errorf("write evaluator main.go: %w", err)
	}
	gocmd := goCommand{bin: filepath.Join(tc.goroot, "bin", "go")}
	goa, err = prepareHarnessModule(ctx, gocmd, buildDir, target, evalMod, logDir)
	if err != nil {
		return listedModule{}, err
	}

	rawBin := filepath.Join(outDir, "designeval-bin")
	buildLog := filepath.Join(tmpDir, "build.log")
	if err := candidate.Build(ctx, candidate.BuildSpec{
		SourceDir: buildDir,
		ModuleDir: buildDir,
		Package:   ".",
		Out:       rawBin,
		GOROOT:    tc.goroot,
		ModCache:  tc.modCache,
		WorkDir:   workDir,
		LogFile:   buildLog,
	}, runBuild); err != nil {
		// A stuck build's processes may still be writing its log.
		if stuck := (*stuckBuildError)(nil); errors.As(err, &stuck) {
			return goa, fmt.Errorf("compile Goa design: %w", err)
		}
		logBytes, readErr := os.ReadFile(buildLog)
		if readErr != nil {
			return goa, &buildError{err: withExit71Guidance(ctx, fmt.Errorf("compile Goa design: %w", err))}
		}
		return goa, &buildError{err: withExit71Guidance(ctx, fmt.Errorf("compile Goa design: %w (%s)", err, strings.TrimSpace(string(logBytes))))}
	}

	art, err := candidate.Pin(storeDir, rawBin)
	if err != nil {
		return goa, fmt.Errorf("pin design evaluator binary: %w", err)
	}
	proc, err := candidate.Exec(ctx, art, candidate.Spec{
		Args:     args(stateDir),
		StateDir: stateDir,
		LogFile:  filepath.Join(tmpDir, "designeval.log"),
	})
	if err != nil {
		return goa, fmt.Errorf("exec sandboxed design evaluator: %w", err)
	}
	if waitErr := proc.Wait(); waitErr != nil {
		excerpt, _, exErr := proc.Excerpt(16 << 10)
		if exErr != nil {
			return goa, withExit71Guidance(ctx, fmt.Errorf("evaluate Goa design in sandbox: %w", waitErr))
		}
		return goa, withExit71Guidance(ctx, fmt.Errorf("evaluate Goa design in sandbox: %w (%s)", waitErr, strings.TrimSpace(excerpt)))
	}
	return goa, collect(stateDir)
}

// withExit71Guidance appends exit71Guidance to err when ctx was not canceled
// and the sandboxed command exited with status 71.
func withExit71Guidance(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == sandboxExit71 {
		return fmt.Errorf("%w\n%s", err, exit71Guidance)
	}
	return err
}

// resolveToolchainEnv resolves the Go toolchain root and module cache for one
// evaluation with the go command on PATH. ctx bounds it.
func resolveToolchainEnv(ctx context.Context) (toolchainEnv, error) {
	out, err := exec.CommandContext(ctx, "go", "env", "GOROOT", "GOMODCACHE").Output()
	if err != nil {
		return toolchainEnv{}, fmt.Errorf("resolve go env GOROOT GOMODCACHE: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) == "" || strings.TrimSpace(lines[1]) == "" {
		return toolchainEnv{}, fmt.Errorf("unexpected go env output %q", string(out))
	}
	return toolchainEnv{goroot: strings.TrimSpace(lines[0]), modCache: strings.TrimSpace(lines[1])}, nil
}

// evaluatorGoMod returns the go.mod Surla was built from (surla.GoMod), which
// supplies the generator versions a target that does not require them is
// evaluated with. It is compiled into the binary, so the result is the same
// whatever directory surla runs in.
func evaluatorGoMod() (*modfile.File, error) {
	mod, err := modfile.Parse("go.mod", []byte(surla.GoMod()), nil)
	if err != nil {
		return nil, fmt.Errorf("parse evaluator go.mod: %w", err)
	}
	return mod, nil
}

// runBuild runs the evaluator build in its own process group, the owning
// boundary of this build: whatever ends it, the compiler, assembler and
// linker it started are killed and waited for before Evaluate continues. A
// group that does not empty is a *stuckBuildError, reported together with
// the build's own error.
func runBuild(cmd *exec.Cmd) error {
	runErr, stopErr := procgroup.Run(cmd, syscall.SIGKILL, buildStopGrace, buildStopGrace)
	if stopErr != nil {
		return errors.Join(&stuckBuildError{err: stopErr}, runErr)
	}
	return runErr
}

// releaseTemp owns the evaluation's temporary directory once Evaluate ends
// with err. It removes the artifact store and then the whole directory and
// reports their removal errors with err, unless err contains a
// *stuckBuildError: build processes may then still use the directory, so it
// is kept and err names it. The sandboxed evaluator needs no such check: its
// sandbox forbids child processes and Evaluate waits for it before returning.
func releaseTemp(tmpDir, storeDir string, err error) error {
	if stuck := (*stuckBuildError)(nil); errors.As(err, &stuck) {
		return fmt.Errorf("%w; the temporary directory %s is kept because build processes may still use it", err, tmpDir)
	}
	return errors.Join(err, candidate.RemoveStore(storeDir), os.RemoveAll(tmpDir))
}

// admitted runs op as one admitted evaluation: it registers it, runs it with
// a context that also ends when Close starts, and, once op has released its
// own files, releases the context and records whether its build got stuck.
func (e *Evaluator) admitted(ctx context.Context, op func(ctx context.Context, workDir string) error) (err error) {
	workDir, err := e.admit()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(e.ctx, func() { cancel(errEvaluatorClosed) })
	// This runs after op's own deferred cleanup of its temporary directory,
	// and marks the evaluation finished only after everything it started has
	// been released.
	defer func() {
		stop()
		cancel(nil)
		e.finish(err)
	}()
	return op(ctx, workDir)
}

// admit registers one evaluation and returns the shared workspace, creating
// it on first use. It fails once Close has started; a failure to create the
// workspace is returned and not remembered.
func (e *Evaluator) admit() (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return "", errEvaluatorClosed
	}
	if e.workDir == "" {
		if e.tempParentDir != "" {
			if err := os.MkdirAll(e.tempParentDir, 0o700); err != nil {
				return "", fmt.Errorf("create design evaluator parent: %w", err)
			}
		}
		dir, err := os.MkdirTemp(e.tempParentDir, "surla-designeval-work-*")
		if err != nil {
			return "", fmt.Errorf("create design evaluator workspace: %w", err)
		}
		e.workDir = dir
	}
	e.active.Add(1)
	return e.workDir, nil
}

// finish ends one admitted evaluation that returned err, after it released
// its own files and context: a stuck build marks the evaluator unsafe for
// good, and only then does the evaluation stop counting as active.
func (e *Evaluator) finish(err error) {
	if stuck := (*stuckBuildError)(nil); errors.As(err, &stuck) {
		e.mu.Lock()
		e.unsafe = true
		e.mu.Unlock()
	}
	e.active.Done()
}

// close is the one cleanup Close runs: it refuses new evaluations, cancels
// the ones in progress, waits for them, and then removes the workspace or,
// when it may still be in use, keeps it and says so.
func (e *Evaluator) close() error {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.cancel()
	e.active.Wait()
	switch {
	case e.unsafe:
		return fmt.Errorf("design evaluator workspace %s is kept because build processes may still use it", e.workDir)
	case e.workDir == "":
		return nil
	default:
		return os.RemoveAll(e.workDir)
	}
}
