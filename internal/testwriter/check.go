// This file prepares the required tests in a working copy, judges the copy
// against a Story, and records an accepted copy as a commit.
package testwriter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/genpublish"
	"github.com/goadesign/surla/internal/procgroup"
	"github.com/goadesign/surla/verify/candidate"
	"github.com/goadesign/surla/verify/suite"
)

type (
	// Suite locates the project's evaluator suite: the Go module holding the
	// evaluation design, its generated gen/ folder and the test writer's
	// scenario hooks, and the suite command in it that follows Surla's suite
	// command contract (verify/suite). A surla init starter has Module "eval"
	// and Command "./cmd/<name>-evals".
	Suite struct {
		// Module is the module's slash-separated path relative to the
		// project folder.
		Module string
		// Command is the suite command's package relative to Module: "." or
		// a path starting with "./".
		Command string
	}

	// Checker prepares and checks test writers' working copies. Its methods
	// may be called concurrently, but not for the same working copy.
	Checker struct {
		// evaluator generates the required tests; its owner closes it.
		evaluator *designeval.Evaluator
		// workDir is the owner's private folder, outside every project, that
		// holds each check's temporary folder and the build cache checks
		// share. The owner removes it.
		workDir string
	}

	// Verdict is Check's judgment of one working copy against a Story.
	Verdict struct {
		// Findings lists what the test writer must change, in the order the
		// check found them; it is empty exactly when the copy is accepted.
		Findings []Finding
		// copy is the working copy that was judged.
		copy WorkingCopy
		// tree is the git tree that was built and described: the copy's
		// files, as a commit would hold them, with the generated folder
		// written again from the Story. It is set exactly when Findings is
		// empty; Record commits it.
		tree string
	}

	// Finding is one reason a working copy is not accepted, worded for the
	// test writer.
	Finding struct {
		// Kind is what failed.
		Kind FindingKind
		// Message says what failed and how, quoting the tool's output, at
		// most maxFindingBytes long.
		Message string
	}

	// FindingKind names the step of the check that failed.
	FindingKind string

	// toolchain is the Go toolchain and module cache a check builds with.
	toolchain struct {
		// goroot is the resolved toolchain root.
		goroot string
		// modCache is the module cache the toolchain downloads into.
		modCache string
	}
)

const (
	// FindingGeneration means the required tests could not be generated from
	// the Story with the copy's suite module, for example because its design
	// does not compile or does not declare exactly one evaluation suite.
	FindingGeneration FindingKind = "generation"
	// FindingDependencies means the suite module's dependencies could not be
	// downloaded from its go.mod and go.sum.
	FindingDependencies FindingKind = "dependencies"
	// FindingBuild means the suite command does not compile, for example
	// because a required hook method is missing. The message quotes the
	// compiler, which names the method.
	FindingBuild FindingKind = "build"
	// FindingListing means the suite command's --describe failed or did not
	// print exactly one suite declaration on standard output.
	FindingListing FindingKind = "listing"
	// FindingMissingScenario means the suite command does not declare a
	// scenario the Story requires. The message names the scenario, its flow
	// and its hook method.
	FindingMissingScenario FindingKind = "missing_scenario"

	// maxFindingBytes bounds one finding's message.
	maxFindingBytes = 8 << 10
	// maxDeclarationBytes bounds what --describe may print.
	maxDeclarationBytes = 1 << 20
	// buildStopGrace bounds how long a canceled build gets to exit, and how
	// long its process group may take to empty.
	buildStopGrace = 10 * time.Second
)

// NewChecker returns a Checker that generates with evaluator and works in
// workDir, a private folder its owner keeps outside every project and
// removes after the Checker's last call. Nothing is created until a call
// needs it.
func NewChecker(evaluator *designeval.Evaluator, workDir string) *Checker {
	return &Checker{evaluator: evaluator, workDir: workDir}
}

// Record commits an accepted verdict's tree, exactly the files Check built
// and described, on the working copy's branch with message and the user's own
// git identity, with hooks disabled, and returns the commit. The message is
// the commit message; it carries no attribution. When the branch already
// holds that tree, as when the copy did not change since the last record, it
// returns the branch's commit and makes none. Edits made in the copy after
// the check are not recorded; they show as changes in the copy. A verdict
// with findings or not made by Check, or an empty message, is refused.
func Record(ctx context.Context, v Verdict, message string) (string, error) {
	if !v.Accepted() {
		return "", errors.New("record: the verdict has findings; only an accepted working copy is recorded")
	}
	if v.tree == "" {
		return "", errors.New("record: the verdict did not come from Check")
	}
	if strings.TrimSpace(message) == "" {
		return "", errors.New("record: the commit message is empty")
	}
	return commitTree(ctx, v.copy, v.tree, message)
}

// Prepare writes the required tests for stories into the working copy: it
// runs the design evaluator's story generator on the copy's suite module and
// replaces the module's gen/ folder (and its AGENTS_QUICKSTART.md) with the
// result. It returns the required scenarios, one per finished flow, named by
// the generator. The project folder is not touched. A generation failure
// wraps the evaluator's error, from which designeval.GenerationDiagnostic
// derives the caller-safe text; on a platform without the sandbox it wraps
// candidate.ErrIsolationUnavailable.
func (c *Checker) Prepare(
	ctx context.Context, wc WorkingCopy, s Suite, stories []designeval.StorySummary,
) (_ []designeval.StoryScenario, err error) {
	if err := errors.Join(wc.opened(), s.validate()); err != nil {
		return nil, err
	}
	moduleDir := filepath.Join(wc.project, filepath.FromSlash(s.Module))
	cleanDir, root, err := designeval.OpenModuleRoot(moduleDir)
	if err != nil {
		return nil, fmt.Errorf("open the suite module %s: %w", s.Module, err)
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	files, err := designeval.ReadModuleSnapshotFromRoot(cleanDir, root)
	if err != nil {
		return nil, fmt.Errorf("read the suite module %s: %w", s.Module, err)
	}
	gen, err := c.evaluator.GenerateWithStories(ctx, files, stories)
	if err != nil {
		return nil, fmt.Errorf("generate the required tests: %w", err)
	}
	if err := publish(ctx, root, gen.Files); err != nil {
		return nil, err
	}
	return gen.Included, nil
}

// Check judges the working copy against stories without trusting its author.
// It snapshots the copy's files as a commit would hold them into a private
// folder, writes the suite module's generated folder there again from the
// stories (so the author cannot change what is required), downloads the
// module's dependencies, compiles the suite command in the verify/candidate
// build sandbox, lists the scenarios it declares by running its --describe in
// the candidate sandbox, reading standard output alone, and compares them
// with the required ones by the generator's own names.
//
// The verdict is accepted, or carries findings for the first step the
// author's files failed: generation, dependencies, the build (quoting the
// compiler, which names a missing hook method), the listing, or each required
// scenario not declared. The working copy itself is not written. Errors are
// failures that say nothing about the author's work: the platform has no
// sandbox (candidate.ErrIsolationUnavailable), ctx ended, or git, the file
// system or the build's process cleanup failed. ctx bounds the whole check,
// including a --describe that never exits.
func (c *Checker) Check(
	ctx context.Context, wc WorkingCopy, s Suite, stories []designeval.StorySummary,
) (_ Verdict, err error) {
	if err := errors.Join(wc.opened(), s.validate()); err != nil {
		return Verdict{}, err
	}
	if err := os.MkdirAll(c.workDir, 0o700); err != nil {
		return Verdict{}, fmt.Errorf("create the check folder: %w", err)
	}
	tmp, err := os.MkdirTemp(c.workDir, "check-*")
	if err != nil {
		return Verdict{}, fmt.Errorf("create the check folder: %w", err)
	}
	keep := false
	defer func() {
		if keep {
			err = fmt.Errorf("%w; %s is kept because build processes may still use it", err, tmp)
			return
		}
		err = errors.Join(err, candidate.RemoveStore(filepath.Join(tmp, "store")), os.RemoveAll(tmp))
	}()
	index, src := filepath.Join(tmp, "index"), filepath.Join(tmp, "src")
	if err := stageCopy(ctx, wc, index, src); err != nil {
		return Verdict{}, err
	}
	rel, err := filepath.Rel(wc.dir, wc.project)
	if err != nil {
		return Verdict{}, err
	}
	moduleDir := filepath.Join(src, rel, filepath.FromSlash(s.Module))
	required, findings, err := c.regenerate(ctx, moduleDir, src, stories)
	if err != nil {
		return Verdict{}, err
	}
	if len(findings) > 0 {
		return Verdict{Findings: findings, copy: wc}, nil
	}
	tree, err := writeTree(ctx, wc, index, src)
	if err != nil {
		return Verdict{}, err
	}
	declared, findings, err := c.listScenarios(ctx, tmp, src, moduleDir, s.Command, &keep)
	if err != nil {
		return Verdict{}, err
	}
	if len(findings) > 0 {
		return Verdict{Findings: findings, copy: wc}, nil
	}
	for _, r := range required {
		if !slices.Contains(declared, r.Scenario) {
			findings = append(findings, Finding{Kind: FindingMissingScenario, Message: fmt.Sprintf(
				"The suite command does not declare scenario %q, which the Story requires for flow %q of story %q: "+
					"its --describe must list every scenario of the generated suite, whose hook %s runs it.",
				r.Scenario, r.Key, r.Story, r.Hook)})
		}
	}
	if len(findings) > 0 {
		return Verdict{Findings: findings, copy: wc}, nil
	}
	return Verdict{copy: wc, tree: tree}, nil
}

// Accepted reports whether the working copy passed the check.
func (v Verdict) Accepted() bool {
	return len(v.Findings) == 0
}

// publish replaces the gen/ folder of the module open at root with files. A
// publication warning, such as a staging folder left by an earlier run, is an
// error: the folder may not hold exactly the generated files.
func publish(ctx context.Context, root *os.Root, files map[string][]byte) error {
	res, err := genpublish.Publish(ctx, root, files)
	if err != nil {
		return fmt.Errorf("write the generated folder: %w", err)
	}
	if len(res.Warnings) > 0 {
		return fmt.Errorf("write the generated folder: %s", strings.Join(res.Warnings, "; "))
	}
	return nil
}

// runBuild runs a check's build in its own process group, the build's owning
// boundary: whatever ends it, the compiler, assembler and linker it started
// are killed and waited for. A group that does not empty is reported with the
// build's own error and marked by stuck.
func runBuild(stuck *bool) func(*exec.Cmd) error {
	return func(cmd *exec.Cmd) error {
		runErr, stopErr := procgroup.Run(cmd, syscall.SIGKILL, buildStopGrace, buildStopGrace)
		if stopErr != nil {
			*stuck = true
			return errors.Join(fmt.Errorf("build processes did not exit: %w", stopErr), runErr)
		}
		return runErr
	}
}

// goEnv is the environment of the go commands a check runs outside the
// sandbox: the user's module download settings, with cgo, workspaces,
// toolchain switching and injected go flags turned off.
func goEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"),
		"CGO_ENABLED=0", "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=",
	}
	for _, name := range []string{"GOMODCACHE", "GOPROXY", "GOSUMDB", "GOPRIVATE", "GONOPROXY", "GONOSUMDB"} {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// forAuthor words message for the test writer: the check's private snapshot
// folder src is removed from the paths it quotes, so they read relative to
// the working copy, and it is cut to at most maxFindingBytes on a character
// boundary, saying how much was left out.
func forAuthor(message, src string) string {
	s := strings.TrimSpace(strings.ReplaceAll(message, src+string(filepath.Separator), ""))
	if len(s) <= maxFindingBytes {
		return s
	}
	const more = "\n… (%d more bytes)"
	k := maxFindingBytes - len(more) - 8
	for k > 0 && !utf8.RuneStart(s[k]) {
		k--
	}
	return s[:k] + fmt.Sprintf(more, len(s)-k)
}

// isFailure reports whether err from generating, building or running the
// author's code says nothing about the author's work: ctx ended or the
// platform has no sandbox.
func isFailure(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, candidate.ErrIsolationUnavailable)
}

// decodeDeclaration decodes exactly one suite.Declaration from out: unknown
// fields and anything after it are errors.
func decodeDeclaration(out []byte) (suite.Declaration, error) {
	dec := json.NewDecoder(bytes.NewReader(out))
	dec.DisallowUnknownFields()
	var d suite.Declaration
	if err := dec.Decode(&d); err != nil {
		return suite.Declaration{}, err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return suite.Declaration{}, errors.New("unexpected data after the declaration")
	}
	return d, nil
}

// resolveToolchain resolves the local Go toolchain's root and module cache,
// from dir, outside any module, so the author's go.mod cannot select another
// toolchain.
func resolveToolchain(ctx context.Context, dir string) (toolchain, error) {
	cmd := exec.CommandContext(ctx, "go", "env", "GOROOT", "GOMODCACHE")
	cmd.Dir, cmd.Env = dir, goEnv()
	out, err := cmd.Output()
	if err != nil {
		return toolchain{}, fmt.Errorf("resolve go env GOROOT GOMODCACHE: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return toolchain{}, fmt.Errorf("unexpected go env output %q", out)
	}
	goroot, err := abspath.Eval(lines[0])
	if err != nil {
		return toolchain{}, fmt.Errorf("resolve the Go toolchain: %w", err)
	}
	return toolchain{goroot: goroot, modCache: lines[1]}, nil
}

// regenerate writes the generated folder of the snapshot's suite module at
// moduleDir again from stories and returns the required scenarios. A module
// that cannot be read or generated is a finding; src is removed from quoted
// paths.
func (c *Checker) regenerate(
	ctx context.Context, moduleDir, src string, stories []designeval.StorySummary,
) (_ []designeval.StoryScenario, _ []Finding, err error) {
	finding := func(message string) []Finding {
		return []Finding{{Kind: FindingGeneration, Message: forAuthor(message, src)}}
	}
	cleanDir, root, err := designeval.OpenModuleRoot(moduleDir)
	if err != nil {
		return nil, finding(fmt.Sprintf("The suite module could not be opened: %v", err)), nil
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	files, err := designeval.ReadModuleSnapshotFromRoot(cleanDir, root)
	if _, ok := files["go.mod"]; err == nil && !ok {
		err = errors.New("it has no go.mod")
	}
	if err != nil {
		return nil, finding(fmt.Sprintf("The suite module could not be read: %v", err)), nil
	}
	gen, err := c.evaluator.GenerateWithStories(ctx, files, stories)
	if err != nil {
		if isFailure(ctx, err) {
			return nil, nil, fmt.Errorf("generate the required tests: %w", err)
		}
		return nil, finding(designeval.GenerationDiagnostic(err)), nil
	}
	if err := publish(ctx, root, gen.Files); err != nil {
		return nil, nil, err
	}
	return gen.Included, nil, nil
}

// listScenarios builds the suite command of the snapshot module at moduleDir in
// the build sandbox, with src as the only source it may read, runs its
// --describe in the candidate sandbox, and returns the scenario IDs it
// declares, or findings when the module's dependencies, its build or its
// listing fail. tmp is the check's private folder; keep is set when build
// processes may still use it.
func (c *Checker) listScenarios(
	ctx context.Context, tmp, src, moduleDir, command string, keep *bool,
) ([]string, []Finding, error) {
	finding := func(kind FindingKind, message string) []Finding {
		return []Finding{{Kind: kind, Message: forAuthor(message, src)}}
	}
	tc, err := resolveToolchain(ctx, tmp)
	if err != nil {
		return nil, nil, err
	}
	download := exec.CommandContext(ctx, filepath.Join(tc.goroot, "bin", "go"), "mod", "download")
	download.Dir, download.Env = moduleDir, goEnv()
	// The download's output is withheld from the finding: it runs outside
	// the sandbox with the user's proxy settings, which it may print.
	if err := download.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("download the suite module's dependencies: %w", err)
		}
		return nil, finding(FindingDependencies, "The suite module's dependencies could not be downloaded "+
			"(go mod download failed). Check that its go.mod and go.sum list every module it imports, "+
			"for example by running go mod tidy in it."), nil
	}
	modCache, err := abspath.Eval(tc.modCache)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve the module cache: %w", err)
	}
	buildHome := filepath.Join(c.workDir, "build-home")
	binDir := filepath.Join(tmp, "bin")
	for _, d := range []string{buildHome, binDir, filepath.Join(tmp, "state")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, nil, fmt.Errorf("create %s: %w", d, err)
		}
	}
	bin, buildLog := filepath.Join(binDir, "suite"), filepath.Join(tmp, "build.log")
	err = candidate.Build(ctx, candidate.BuildSpec{
		SourceDir: src, ModuleDir: moduleDir, Package: command, Out: bin,
		GOROOT: tc.goroot, ModCache: modCache, WorkDir: buildHome, LogFile: buildLog,
	}, runBuild(keep))
	if err != nil {
		if *keep || isFailure(ctx, err) {
			return nil, nil, fmt.Errorf("build the suite command: %w", err)
		}
		out, readErr := os.ReadFile(buildLog)
		if readErr != nil {
			return nil, nil, errors.Join(fmt.Errorf("build the suite command: %w", err), readErr)
		}
		return nil, finding(FindingBuild, "The suite command does not compile:\n"+string(out)), nil
	}
	art, err := candidate.Pin(filepath.Join(tmp, "store"), bin)
	if err != nil {
		return nil, nil, fmt.Errorf("pin the suite command: %w", err)
	}
	stderrLog := filepath.Join(tmp, "describe.log")
	out, err := candidate.Output(ctx, art, candidate.Spec{
		Args: []string{"--describe"}, StateDir: filepath.Join(tmp, "state"), LogFile: stderrLog,
	}, maxDeclarationBytes)
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case isFailure(ctx, err):
		return nil, nil, fmt.Errorf("list the declared scenarios: %w", err)
	case errors.Is(err, candidate.ErrOutputTooLarge):
		return nil, finding(FindingListing, fmt.Sprintf(
			"The suite command's --describe printed more than %d bytes on standard output.", maxDeclarationBytes)), nil
	case errors.As(err, &exitErr):
		stderr, readErr := os.ReadFile(stderrLog)
		if readErr != nil {
			return nil, nil, errors.Join(fmt.Errorf("list the declared scenarios: %w", err), readErr)
		}
		return nil, finding(FindingListing, fmt.Sprintf("The suite command's --describe exited with status %d. "+
			"Its standard error:\n%s", exitErr.ExitCode(), stderr)), nil
	default:
		return nil, nil, fmt.Errorf("list the declared scenarios: %w", err)
	}
	d, err := decodeDeclaration(out)
	if err != nil {
		return nil, finding(FindingListing, "The suite command's --describe did not print one suite declaration "+
			"on standard output: "+err.Error()), nil
	}
	ids := make([]string, 0, len(d.Scenarios))
	for _, sc := range d.Scenarios {
		ids = append(ids, sc.ID)
	}
	return ids, nil, nil
}

// validate requires a local module path and a single-package command path.
func (s Suite) validate() error {
	if s.Module == "" || !filepath.IsLocal(filepath.FromSlash(s.Module)) || path.Clean(s.Module) != s.Module {
		return fmt.Errorf("suite module %q is not a clean path inside the project", s.Module)
	}
	if c := s.Command; (c != "." && !strings.HasPrefix(c, "./")) || !filepath.IsLocal(filepath.FromSlash(c)) ||
		strings.Contains(c, "...") {
		return fmt.Errorf("suite command %q is not one package path inside the suite module", s.Command)
	}
	return nil
}
