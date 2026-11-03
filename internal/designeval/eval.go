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

// errEvaluatorClosed is the cause of an evaluation canceled by Close, and
// the error Evaluate returns after Close.
var errEvaluatorClosed = errors.New("design evaluator is closed")

// NewEvaluator returns an Evaluator. It runs no command and creates no file.
func NewEvaluator() *Evaluator {
	ctx, cancel := context.WithCancel(context.Background())
	e := &Evaluator{ctx: ctx, cancel: cancel}
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
func (e *Evaluator) Evaluate(ctx context.Context, files map[string]string) (_ Result, err error) {
	workDir, err := e.admit()
	if err != nil {
		return Result{}, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(e.ctx, func() { cancel(errEvaluatorClosed) })
	// This runs after evaluate's own deferred cleanup of its temporary
	// directory, and marks the evaluation finished only after everything it
	// started has been released.
	defer func() {
		stop()
		cancel(nil)
		e.finish(err)
	}()
	return evaluate(ctx, files, workDir)
}

// Close cancels the evaluations in progress, waits until each has released
// its files, and removes the shared workspace. If any evaluation's build
// processes could not be confirmed stopped, it keeps the workspace and its
// error names it. Every call, including concurrent ones, returns the result
// of that one cleanup once it has finished.
func (e *Evaluator) Close() error {
	return e.closeResult()
}

// Error describes the build processes left running.
func (e *stuckBuildError) Error() string {
	return "build processes did not exit: " + e.err.Error()
}

// Unwrap returns procgroup.Run's stop error.
func (e *stuckBuildError) Unwrap() error {
	return e.err
}

// evaluate runs one evaluation in a temporary directory of its own, with
// workDir as the build's shared HOME, TMPDIR and GOCACHE.
func evaluate(ctx context.Context, files map[string]string, workDir string) (_ Result, err error) {
	target, err := validateSnapshot(files)
	if err != nil {
		return Result{}, err
	}
	evalMod, err := evaluatorGoMod()
	if err != nil {
		return Result{}, err
	}
	tc, err := resolveToolchainEnv(ctx)
	if err != nil {
		return Result{}, err
	}

	tmpDir, err := os.MkdirTemp("", "surla-designeval-*")
	if err != nil {
		return Result{}, fmt.Errorf("create temp dir: %w", err)
	}
	storeDir := filepath.Join(tmpDir, "store")
	defer func() { err = releaseTemp(tmpDir, storeDir, err) }()

	buildDir := filepath.Join(tmpDir, "build")
	outDir := filepath.Join(tmpDir, "out")
	stateDir := filepath.Join(tmpDir, "state")
	logDir := filepath.Join(tmpDir, "logs")
	for _, d := range []string{buildDir, outDir, storeDir, stateDir, logDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return Result{}, fmt.Errorf("mkdir %q: %w", d, err)
		}
	}
	if err := stageSnapshot(filepath.Join(buildDir, "target"), files); err != nil {
		return Result{}, err
	}
	designImport := strconv.Quote(target.Module.Mod.Path + "/design")
	harnessMain := strings.ReplaceAll(harnessMainTemplate, "{{DESIGN_IMPORT}}", designImport)
	harnessMain = strings.ReplaceAll(harnessMain, "{{GEN_IMPORT}}", strconv.Quote(target.Module.Mod.Path+"/gen"))
	if err := os.WriteFile(filepath.Join(buildDir, "main.go"), []byte(harnessMain), 0o600); err != nil {
		return Result{}, fmt.Errorf("write evaluator main.go: %w", err)
	}
	gocmd := goCommand{bin: filepath.Join(tc.goroot, "bin", "go")}
	if err := prepareHarnessModule(ctx, gocmd, buildDir, target, evalMod, logDir); err != nil {
		return Result{}, err
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
			return Result{}, fmt.Errorf("compile Goa design: %w", err)
		}
		logBytes, readErr := os.ReadFile(buildLog)
		if readErr != nil {
			return Result{}, fmt.Errorf("compile Goa design: %w", err)
		}
		return Result{}, fmt.Errorf("compile Goa design: %w (%s)", err, strings.TrimSpace(string(logBytes)))
	}

	art, err := candidate.Pin(storeDir, rawBin)
	if err != nil {
		return Result{}, fmt.Errorf("pin design evaluator binary: %w", err)
	}

	outFile := filepath.Join(stateDir, "evaluated.json")
	logFile := filepath.Join(tmpDir, "designeval.log")
	proc, err := candidate.Exec(ctx, art, candidate.Spec{
		Args:     []string{"-out", outFile},
		StateDir: stateDir,
		LogFile:  logFile,
	})
	if err != nil {
		return Result{}, fmt.Errorf("exec sandboxed design evaluator: %w", err)
	}
	if waitErr := proc.Wait(); waitErr != nil {
		excerpt, _, exErr := proc.Excerpt(16 << 10)
		if exErr != nil {
			return Result{}, fmt.Errorf("evaluate Goa design in sandbox: %w", waitErr)
		}
		return Result{}, fmt.Errorf("evaluate Goa design in sandbox: %w (%s)", waitErr, strings.TrimSpace(excerpt))
	}

	rawJSON, err := os.ReadFile(outFile)
	if err != nil {
		return Result{}, fmt.Errorf("read evaluated design output: %w", err)
	}
	var res Result
	if err := json.Unmarshal(rawJSON, &res); err != nil {
		return Result{}, fmt.Errorf("decode evaluated Goa design: %w", err)
	}
	return res, nil
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
		dir, err := os.MkdirTemp("", "surla-designeval-work-*")
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
