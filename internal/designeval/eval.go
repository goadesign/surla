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

	// toolchainEnv holds the resolved Go toolchain root and module cache
	// directory for candidate.Build. Each Evaluate call creates its own build
	// workspace.
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

var (
	// resolveToolchainEnv caches the resolved GOROOT and GOMODCACHE used by
	// candidate.Build. It creates no directory.
	resolveToolchainEnv = sync.OnceValues(func() (toolchainEnv, error) {
		out, err := exec.Command("go", "env", "GOROOT", "GOMODCACHE").Output()
		if err != nil {
			return toolchainEnv{}, fmt.Errorf("resolve go env GOROOT GOMODCACHE: %w", err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) < 2 || strings.TrimSpace(lines[0]) == "" || strings.TrimSpace(lines[1]) == "" {
			return toolchainEnv{}, fmt.Errorf("unexpected go env output %q", string(out))
		}
		return toolchainEnv{
			goroot:   strings.TrimSpace(lines[0]),
			modCache: strings.TrimSpace(lines[1]),
		}, nil
	})
)

// Evaluate evaluates the Goa/Goa-AI/Model design in a module snapshot and
// returns its declarations, C4 topology, agent policies and tool contracts.
//
// files is the module's exact content keyed by module-relative path (see
// validateSnapshot). The snapshot is staged byte for byte; no host path it
// names is read. Its dependencies are resolved only from its go.mod and
// go.sum, and evaluation fails rather than use different versions (see
// prepareHarnessModule). The evaluator program is then compiled with
// candidate.Build and run with candidate.Exec, so neither compiling nor
// running the design can read evaluator files or reach the network.
func Evaluate(ctx context.Context, files map[string]string) (_ Result, err error) {
	target, err := validateSnapshot(files)
	if err != nil {
		return Result{}, err
	}
	evalMod, err := evaluatorGoMod()
	if err != nil {
		return Result{}, err
	}
	tc, err := resolveToolchainEnv()
	if err != nil {
		return Result{}, err
	}

	tmpDir, err := os.MkdirTemp("", "surla-designeval-*")
	if err != nil {
		return Result{}, fmt.Errorf("create temp dir: %w", err)
	}
	storeDir := filepath.Join(tmpDir, "store")
	defer func() { err = releaseTemp(tmpDir, storeDir, err) }()

	// work is the evaluator build's private HOME, TMPDIR and GOCACHE. It
	// lives inside this call's temporary directory, so releaseTemp removes it
	// with the rest, or keeps it when build processes may still use it.
	workDir := filepath.Join(tmpDir, "work")
	buildDir := filepath.Join(tmpDir, "build")
	outDir := filepath.Join(tmpDir, "out")
	stateDir := filepath.Join(tmpDir, "state")
	logDir := filepath.Join(tmpDir, "logs")
	for _, d := range []string{workDir, buildDir, outDir, storeDir, stateDir, logDir} {
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

// Error describes the build processes left running.
func (e *stuckBuildError) Error() string {
	return "build processes did not exit: " + e.err.Error()
}

// Unwrap returns procgroup.Run's stop error.
func (e *stuckBuildError) Unwrap() error {
	return e.err
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
