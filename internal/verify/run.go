package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goadesign/surla/verify/candidate"
	"github.com/goadesign/surla/verify/suite"
	"goa.design/goa-ai/eval"
)

type (
	// Plan says what to verify: an exact candidate commit, the binaries to
	// build from it, and an exact evaluator suite commit.
	Plan struct {
		// CandidateRepo is the git repository holding the candidate.
		CandidateRepo string
		// CandidateCommit is the full commit ID to build, in the repository's
		// object format. Branch names and other revision expressions are
		// rejected.
		CandidateCommit string
		// CandidateModule is the Go module directory inside the candidate
		// repository that the binaries are built from, such as
		// "examples/orders" or ".". After symbolic links are resolved it must
		// still be inside the checkout.
		CandidateModule string
		// Binaries maps each artifact name to its package path inside
		// CandidateModule, such as "./cmd/orders". A name is the artifact ID
		// the suite receives as --artifact name=path: it must be non-empty,
		// not "." or "..", and contain no "/", "\\", "=" or NUL. Each package
		// path is "." or starts with "./", and after symbolic links are
		// resolved it must name a directory inside CandidateModule.
		Binaries map[string]string
		// SuiteRepo is the git repository holding the evaluator suite.
		SuiteRepo string
		// SuiteCommit is the full ID of the suite commit to run.
		SuiteCommit string
		// SuiteModule is the suite's Go module directory inside SuiteRepo.
		// Like CandidateModule, it must resolve to a directory inside the
		// checkout.
		SuiteModule string
		// SuiteCommand is the suite command's package path inside
		// SuiteModule, following the suite command contract. Like a candidate
		// package path, it is "." or starts with "./" and must resolve to a
		// directory inside SuiteModule.
		SuiteCommand string
		// RequestRevision identifies the confirmed request being verified.
		RequestRevision string
		// WorkDir is a private directory for clones, builds and evidence. It
		// must not be shared with the candidate's builder. It is required and
		// must be missing or empty, so everything a build can read under it was
		// written by this run; a relative path is made absolute against the
		// current directory.
		WorkDir string
		// ReportDir receives the report, named by its digest.
		ReportDir string
	}

	// Stage names the step of Run that stopped a verification.
	Stage string

	// StageError is every error Run returns: the step that failed, a summary
	// that is safe to show builders, and the full error. Err may quote command
	// output and must stay with the evaluator.
	StageError struct {
		// Stage is the step that failed.
		Stage Stage
		// Summary describes the failure in one sentence without command output.
		Summary string
		// Err is the full error.
		Err error
	}

	// Prepared is what Prepare built: the binding of the exact commits and
	// candidate binary digests, the suite program, and the --artifact flags
	// that hand the built candidate binaries to it.
	Prepared struct {
		// Binding names the candidate and suite commits, the request revision
		// and every built binary's digest. Its SuiteID and Environment are
		// empty until the suite describes itself.
		Binding Binding
		// SuiteBin is the built suite program.
		SuiteBin string
		// ArtifactFlags are the suite's --artifact name=path arguments, in
		// binary-name order.
		ArtifactFlags []string
		// EvidenceDir holds every preparation command's output.
		EvidenceDir string
		// runner runs Run's later suite commands with the same evidence
		// directory.
		runner *runner
	}

	// runner runs the verifier's commands with file-backed output.
	runner struct {
		// evidenceDir receives every command's stdout and stderr.
		evidenceDir string
	}

	// buildToolchain is what a sandboxed candidate build runs with.
	buildToolchain struct {
		// goroot is the resolved toolchain the candidate module selects.
		goroot string
		// modCache is the module cache holding the candidate's dependencies.
		modCache string
		// workDir is the build's private HOME, TMPDIR and GOCACHE.
		workDir string
	}
)

const (
	// StageCheckout covers the plan, the work directory and the candidate and
	// suite checkouts.
	StageCheckout Stage = "checkout"
	// StageBuild covers preparing and building the candidate and the suite.
	StageBuild Stage = "build"
	// StageDescribe is the suite's --describe.
	StageDescribe Stage = "describe"
	// StageSuite is running the suite against the candidate.
	StageSuite Stage = "suite"
	// StageReport is assembling and saving the report.
	StageReport Stage = "report"

	// holdoutTag marks a scenario whose details builders must never see.
	holdoutTag = "holdout"
	// maxErrorExcerpt bounds how much of a failed command's output an error
	// message quotes; the full output stays in the evidence directory.
	maxErrorExcerpt = 4 << 10
	// suiteStopGrace is how long the suite gets to stop its candidates and
	// report after SIGTERM before it is killed.
	suiteStopGrace = 30 * time.Second
	// summaryPlan is the builder-safe summary of an invalid verification plan
	// or work directory, which the operator configures.
	summaryPlan = "The operator's verification configuration is invalid."
	// summaryCandidateBuild is the builder-safe summary of a candidate that
	// did not build.
	summaryCandidateBuild = "The candidate did not build."
	// summarySuiteBuild is the builder-safe summary of an evaluator suite that
	// did not build.
	summarySuiteBuild = "The evaluator suite did not build."
	// sandboxExit71 is the exit status (EX_OSERR) /usr/bin/sandbox-exec returns
	// when macOS refuses to apply a sandbox profile (for example inside a nested
	// restricted coding-agent shell), and also when a sandboxed child exits 71.
	sandboxExit71 = 71
	// summarySandboxExit71 is the builder-safe summary returned when a
	// non-canceled sandboxed build command in Prepare exits with status 71.
	summarySandboxExit71 = "The sandboxed command exited with status 71. If Surla is running inside a restricted coding-agent shell, its sandbox may have been blocked; run Surla from an unrestricted host shell."
)

// commitID matches a full commit ID in either Git object format: 40 hex digits
// for SHA-1 repositories, 64 for SHA-256 ones. The checkout then requires HEAD
// to equal it, which only the full ID in the repository's own format can.
var commitID = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// IsCommitID reports whether commit is a full commit ID in either Git object
// format: 40 lowercase hex digits for SHA-1 repositories, 64 for SHA-256 ones.
func IsCommitID(commit string) bool {
	return commitID.MatchString(commit)
}

// Run verifies a plan. Prepare first clones the candidate at its exact commit
// and builds its binaries into WorkDir/candidate-bin, then clones the suite at
// its exact commit and builds it into WorkDir/suite-bin. Run then asks the
// suite for its declaration, runs it against the built binaries, and returns
// the CP4 report bound to everything that was tested. The report is also
// written to ReportDir as <digest>.json.
//
// Both builds go through candidate.Build: an OS sandbox in which the build
// reads nothing but its source tree, the toolchain, the module cache and its
// own directories under WorkDir, writes nothing but those directories, and has
// no network. The candidate's source tree is its module; the suite's is its
// whole checkout, so it may replace sibling modules there. A committed link or
// replace directive can reach only those trees: the bound commit, the trusted
// toolchain and checksum-verified modules, and directories this run created
// empty, because Run refuses a WorkDir that already has contents. The suite is
// checked out only after the candidate is built. Every command's
// output goes to files under WorkDir/evidence; errors quote only a bounded
// excerpt.
//
// Run starts its builds, the suite and the candidates the suite runs in the
// caller's process group and does not stop processes they leave behind: its
// caller must run it inside an owning group that it stops afterwards, as
// `surla verify-run execute` does under its supervisor.
//
// Every error is a *StageError naming the step that failed and a summary
// safe to show builders; its Err may quote command output and stays with the
// evaluator. A suite that produces no decodable eval.Report is an error.
// Otherwise the report keeps the suite's evidence and exit status: a suite
// that ran and failed, or that exited abnormally after writing partial
// results, yields a report that does not pass.
func Run(ctx context.Context, p Plan) (Report, error) {
	prep, err := Prepare(ctx, p)
	if err != nil {
		return Report{}, err
	}
	r, binding, suiteBin, artifactFlags := prep.runner, prep.Binding, prep.SuiteBin, prep.ArtifactFlags
	declared, err := r.describe(ctx, suiteBin)
	if err != nil {
		return Report{}, stageError(StageDescribe, "The evaluator suite did not describe its scenarios.", err)
	}
	binding.SuiteID, binding.Environment = declared.ID, declared.Environment
	evidence, exit, err := r.runSuite(ctx, suiteBin, artifactFlags)
	if err != nil {
		return Report{}, stageError(StageSuite, "The evaluator suite ended without a report.", err)
	}
	report, err := Assemble(binding, declared, evidence, exit)
	if err != nil {
		return Report{}, stageError(StageReport, "The evaluator suite's report does not match its declaration or the binaries it was given.", err)
	}
	if err := writeReport(p.ReportDir, report); err != nil {
		return Report{}, stageError(StageReport, "Saving the verification report failed.", err)
	}
	return report, nil
}

// Prepare runs the trusted preparation Run starts with: it checks the plan,
// clones the candidate at its exact commit and builds its binaries into
// WorkDir/candidate-bin, then clones the suite at its exact commit and builds
// it into WorkDir/suite-bin, with the same sandbox, containment and evidence
// rules Run documents. Run calls it before describing and running the suite;
// a live CP3 target calls it before starting the suite in --live mode, so
// both build exactly the same way. The caller owns the process group, as for
// Run. Every error is a *StageError.
func Prepare(ctx context.Context, p Plan) (Prepared, error) {
	for name, commit := range map[string]string{"candidate": p.CandidateCommit, "suite": p.SuiteCommit} {
		if !IsCommitID(commit) {
			return Prepared{}, stageError(StageCheckout, summaryPlan, fmt.Errorf("%s commit %q is not a full commit ID", name, commit))
		}
	}
	for name, pkg := range p.Binaries {
		if err := ValidateArtifactName(name); err != nil {
			return Prepared{}, stageError(StageCheckout, summaryPlan, err)
		}
		if err := validatePackagePath(pkg); err != nil {
			return Prepared{}, stageError(StageCheckout, summaryPlan, fmt.Errorf("binary %q: %w", name, err))
		}
	}
	if err := validatePackagePath(p.SuiteCommand); err != nil {
		return Prepared{}, stageError(StageCheckout, summaryPlan, fmt.Errorf("suite command: %w", err))
	}
	if p.WorkDir == "" {
		return Prepared{}, stageError(StageCheckout, summaryPlan, errors.New("work directory is required"))
	}
	workDir, err := filepath.Abs(p.WorkDir)
	if err != nil {
		return Prepared{}, stageError(StageCheckout, summaryPlan, fmt.Errorf("work directory: %w", err))
	}
	p.WorkDir = workDir
	switch entries, err := os.ReadDir(p.WorkDir); {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Prepared{}, stageError(StageCheckout, summaryPlan, fmt.Errorf("work directory: %w", err))
	case len(entries) > 0:
		return Prepared{}, stageError(StageCheckout, summaryPlan, fmt.Errorf("work directory %s must be missing or empty", p.WorkDir))
	}
	evidenceDir := filepath.Join(p.WorkDir, "evidence")
	candidateBinDir := filepath.Join(p.WorkDir, "candidate-bin")
	suiteBinDir := filepath.Join(p.WorkDir, "suite-bin")
	for _, dir := range []string{evidenceDir, candidateBinDir, suiteBinDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Prepared{}, stageError(StageCheckout, summaryPlan, fmt.Errorf("create %s: %w", dir, err))
		}
	}
	r := &runner{evidenceDir: evidenceDir}
	binding := Binding{
		CandidateCommit: p.CandidateCommit,
		RequestRevision: p.RequestRevision,
		SuiteCommit:     p.SuiteCommit,
		Artifacts:       map[string]string{},
	}

	candidateDir := filepath.Join(p.WorkDir, "candidate")
	if err := r.checkout(ctx, "candidate", p.CandidateRepo, p.CandidateCommit, candidateDir); err != nil {
		return Prepared{}, stageError(StageCheckout, "Checking out the candidate commit failed.", err)
	}
	moduleDir, err := resolveInside(candidateDir, p.CandidateModule)
	if err != nil {
		return Prepared{}, stageError(StageCheckout, "The candidate commit does not contain the configured module.", fmt.Errorf("candidate module %q: %w", p.CandidateModule, err))
	}
	for name, pkg := range p.Binaries {
		if _, err := resolveInside(moduleDir, pkg); err != nil {
			return Prepared{}, stageError(StageCheckout, "The candidate commit does not contain a configured binary package.", fmt.Errorf("binary %q package %q: %w", name, pkg, err))
		}
	}
	toolchain, err := r.prepareBuild(ctx, "candidate", moduleDir, p.WorkDir)
	if err != nil {
		return Prepared{}, stageError(StageBuild, summaryCandidateBuild, err)
	}
	var artifactFlags []string
	for _, name := range slices.Sorted(maps.Keys(p.Binaries)) {
		out := filepath.Join(candidateBinDir, name)
		logFile := filepath.Join(r.evidenceDir, "build-"+name+".log")
		if err := candidate.Build(ctx, candidate.BuildSpec{
			SourceDir: moduleDir, ModuleDir: moduleDir, Package: p.Binaries[name], Out: out,
			GOROOT: toolchain.goroot, ModCache: toolchain.modCache, WorkDir: toolchain.workDir,
			LogFile: logFile,
		}, (*exec.Cmd).Run); err != nil {
			// The private error carries the end of the compiler's output and
			// the log that holds all of it; the summary builders see does not.
			return Prepared{}, stageError(StageBuild, buildSummary(ctx, err, summaryCandidateBuild),
				fmt.Errorf("build candidate %s: %w%s", name, err, excerptFile(logFile, "build output")))
		}
		digest, err := sha256File(out)
		if err != nil {
			return Prepared{}, stageError(StageBuild, summaryCandidateBuild, err)
		}
		binding.Artifacts[name] = digest
		artifactFlags = append(artifactFlags, "--artifact", name+"="+out)
	}

	suiteDir := filepath.Join(p.WorkDir, "suite")
	if err := r.checkout(ctx, "suite", p.SuiteRepo, p.SuiteCommit, suiteDir); err != nil {
		return Prepared{}, stageError(StageCheckout, "Checking out the evaluator suite failed.", err)
	}
	suiteModuleDir, err := resolveInside(suiteDir, p.SuiteModule)
	if err != nil {
		return Prepared{}, stageError(StageCheckout, summaryPlan, fmt.Errorf("suite module %q: %w", p.SuiteModule, err))
	}
	if _, err := resolveInside(suiteModuleDir, p.SuiteCommand); err != nil {
		return Prepared{}, stageError(StageCheckout, summaryPlan, fmt.Errorf("suite command %q: %w", p.SuiteCommand, err))
	}
	suiteToolchain, err := r.prepareBuild(ctx, "suite", suiteModuleDir, p.WorkDir)
	if err != nil {
		return Prepared{}, stageError(StageBuild, summarySuiteBuild, err)
	}
	// The suite builds in the same sandbox as the candidate, readable only
	// within its own checkout: its module may replace sibling modules there,
	// but no link or replace can pull in source outside SuiteCommit.
	suiteBin := filepath.Join(suiteBinDir, "suite")
	suiteLog := filepath.Join(r.evidenceDir, "build-suite.log")
	if err := candidate.Build(ctx, candidate.BuildSpec{
		SourceDir: suiteDir, ModuleDir: suiteModuleDir, Package: p.SuiteCommand, Out: suiteBin,
		GOROOT: suiteToolchain.goroot, ModCache: suiteToolchain.modCache, WorkDir: suiteToolchain.workDir,
		LogFile: suiteLog,
	}, (*exec.Cmd).Run); err != nil {
		// As for the candidate: compiler output goes only into the private error.
		return Prepared{}, stageError(StageBuild, buildSummary(ctx, err, summarySuiteBuild),
			fmt.Errorf("build suite: %w%s", err, excerptFile(suiteLog, "build output")))
	}
	return Prepared{
		Binding:       binding,
		SuiteBin:      suiteBin,
		ArtifactFlags: artifactFlags,
		EvidenceDir:   evidenceDir,
		runner:        r,
	}, nil
}

// Error returns the step, the summary and the full error.
func (e *StageError) Error() string {
	return fmt.Sprintf("%s: %s %v", e.Stage, e.Summary, e.Err)
}

// Unwrap returns the full error.
func (e *StageError) Unwrap() error {
	return e.Err
}

// resolveInside resolves rel, a path relative to the directory root, following
// every symbolic link in root and rel, and returns the resolved path. It fails
// when the path does not exist or is not root itself or inside it, so a
// committed link cannot point a build at source outside the commit the report
// names.
func resolveInside(root, rel string) (string, error) {
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", root, err)
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(base, rel))
	if err != nil {
		return "", err
	}
	inner, err := filepath.Rel(base, dir)
	if err != nil || !filepath.IsLocal(inner) {
		return "", fmt.Errorf("resolves to %s, outside %s", dir, base)
	}
	return dir, nil
}

// validatePackagePath rejects a Go package argument that is not "." or a
// "./"-relative directory, such as a standard library import path or a go
// build flag, before it reaches `go build`.
func validatePackagePath(pkg string) error {
	if pkg != "." && !strings.HasPrefix(pkg, "./") {
		return fmt.Errorf("package path %q must be \".\" or start with \"./\"", pkg)
	}
	return nil
}

// ValidateArtifactName returns an error unless name can serve both as the
// artifact ID passed to the suite as --artifact name=path and as one file name
// inside WorkDir/candidate-bin and WorkDir/evidence: non-empty, not . or ..,
// and free of /, \, = and NUL.
func ValidateArtifactName(name string) error {
	switch {
	case name == "", name == ".", name == "..":
		return fmt.Errorf("binary name %q is not a valid artifact name", name)
	case strings.ContainsAny(name, "/\\=\x00"):
		return fmt.Errorf("binary name %q must not contain /, \\, = or NUL", name)
	}
	return nil
}

// checkout clones repo into dir and checks out commit, detached. The source
// repository is only read; clone hooks and templates are disabled.
func (r *runner) checkout(ctx context.Context, label, repo, commit, dir string) error {
	if err := r.run(ctx, "clone-"+label, "", nil, "git", "-c", "core.hooksPath=/dev/null",
		"clone", "--quiet", "--no-checkout", "--template=", repo, dir); err != nil {
		return fmt.Errorf("check out %s: %w", label, err)
	}
	if err := r.run(ctx, "checkout-"+label, dir, nil, "git", "-c", "core.hooksPath=/dev/null",
		"-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", commit+"^{commit}"); err != nil {
		return fmt.Errorf("check out %s: %w", label, err)
	}
	if err := r.run(ctx, "head-"+label, dir, nil, "git", "rev-parse", "HEAD"); err != nil {
		return fmt.Errorf("check out %s: %w", label, err)
	}
	head, err := os.ReadFile(filepath.Join(r.evidenceDir, "head-"+label+".stdout"))
	if err != nil {
		return fmt.Errorf("read %s head: %w", label, err)
	}
	if got := strings.TrimSpace(string(head)); got != commit {
		return fmt.Errorf("check out %s: HEAD is %s, want %s", label, got, commit)
	}
	return nil
}

// describe runs the suite's --describe and converts it into the declared
// scenario list.
func (r *runner) describe(ctx context.Context, suiteBin string) (Suite, error) {
	if err := r.run(ctx, "describe", "", nil, suiteBin, "--describe"); err != nil {
		return Suite{}, fmt.Errorf("describe suite: %w", err)
	}
	var d suite.Declaration
	if err := decodeFile(filepath.Join(r.evidenceDir, "describe.stdout"), &d); err != nil {
		return Suite{}, fmt.Errorf("decode suite declaration: %w", err)
	}
	s := Suite{ID: d.ID, Environment: d.Environment}
	for _, sc := range d.Scenarios {
		s.Scenarios = append(s.Scenarios, Scenario{ID: sc.ID, Holdout: slices.Contains(sc.Tags, holdoutTag)})
	}
	return s, nil
}

// runSuite runs the suite and returns its eval.Report and exit status. The
// suite and the candidates it starts run in the caller's process group, which
// stops and reaps them (surla verify-run supervise owns that group).
// Cancellation sends SIGTERM to the suite so it can stop its candidates and
// report what it has; it is killed after suiteStopGrace. A report is decoded
// whatever the outcome; a canceled or abnormal end gets exit status -1 and
// never passes. Only a missing or undecodable report is an error.
func (r *runner) runSuite(ctx context.Context, suiteBin string, artifactFlags []string) (eval.Report, int, error) {
	stdout, stderr, err := r.outputs("suite")
	if err != nil {
		return eval.Report{}, 0, err
	}
	defer stdout.Close()
	defer stderr.Close()
	cmd := exec.CommandContext(ctx, suiteBin, artifactFlags...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// Cancellation asks the suite to stop its candidates and report; it gets
	// suiteStopGrace before it is killed. The suite and whatever it starts
	// stay in the caller's process group, which owns their cleanup.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = suiteStopGrace
	runErr := cmd.Run()
	var report eval.Report
	decodeErr := decodeFile(filepath.Join(r.evidenceDir, "suite.stdout"), &report)
	exit := 0
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &exitErr):
		exit = exitErr.ExitCode()
	default:
		// Canceled or otherwise not a clean exit: whatever the suite reported
		// is kept, and the verification can never pass.
		exit = -1
	}
	if decodeErr != nil {
		return eval.Report{}, 0, fmt.Errorf("suite ended (%v) without a decodable report: %w%s", runErr, decodeErr, r.excerpt("suite"))
	}
	return report, exit, nil
}

// run runs one command with its stdout and stderr in evidence files named by
// label. env, when non-nil, replaces the inherited environment.
func (r *runner) run(ctx context.Context, label, dir string, env []string, name string, args ...string) error {
	stdout, stderr, err := r.outputs(label)
	if err != nil {
		return err
	}
	defer stdout.Close()
	defer stderr.Close()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w%s", label, err, r.excerpt(label))
	}
	return nil
}

// outputs creates the evidence files for one command.
func (r *runner) outputs(label string) (*os.File, *os.File, error) {
	stdout, err := os.Create(filepath.Join(r.evidenceDir, label+".stdout"))
	if err != nil {
		return nil, nil, fmt.Errorf("create evidence file: %w", err)
	}
	stderr, err := os.Create(filepath.Join(r.evidenceDir, label+".stderr"))
	if err != nil {
		_ = stdout.Close()
		return nil, nil, fmt.Errorf("create evidence file: %w", err)
	}
	return stdout, stderr, nil
}

// excerpt returns the end of a command's stderr for an error message, noting
// truncation and where the full output is.
func (r *runner) excerpt(label string) string {
	return excerptFile(filepath.Join(r.evidenceDir, label+".stderr"), "stderr")
}

// prepareBuild resolves the toolchain the module at moduleDir selects and
// downloads its module dependencies, outside the sandbox, for the build named
// label ("candidate" or "suite"). These steps fetch checksum-verified modules
// and official toolchains and compile or execute no checked-out code; the
// sandboxed build afterwards runs with no network. Evidence files and the
// build home are named by label so the two builds never share them.
func (r *runner) prepareBuild(ctx context.Context, label, moduleDir, workDir string) (buildToolchain, error) {
	if err := r.run(ctx, "go-mod-download-"+label, moduleDir, nil, "go", "mod", "download"); err != nil {
		return buildToolchain{}, fmt.Errorf("download %s modules: %w", label, err)
	}
	if err := r.run(ctx, "go-env-"+label, moduleDir, nil, "go", "env", "GOROOT", "GOMODCACHE"); err != nil {
		return buildToolchain{}, fmt.Errorf("resolve %s toolchain: %w", label, err)
	}
	out, err := os.ReadFile(filepath.Join(r.evidenceDir, "go-env-"+label+".stdout"))
	if err != nil {
		return buildToolchain{}, fmt.Errorf("read toolchain: %w", err)
	}
	lines := strings.Fields(string(out))
	if len(lines) != 2 {
		return buildToolchain{}, fmt.Errorf("unexpected go env output %q", out)
	}
	t := buildToolchain{goroot: lines[0], modCache: lines[1], workDir: filepath.Join(workDir, "build-home-"+label)}
	if err := os.MkdirAll(t.workDir, 0o700); err != nil {
		return buildToolchain{}, fmt.Errorf("create build home: %w", err)
	}
	return t, nil
}

// excerptFile returns the end of the evidence file at path, described as
// what, for an error message: a note of truncation, the file's path, and its
// last maxErrorExcerpt bytes. It returns "" when the file is missing, empty
// or unreadable, so an error never points at output that was not written.
func excerptFile(path, what string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}
	start := max(info.Size()-maxErrorExcerpt, 0)
	b := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(b, start); err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	note := ""
	if start > 0 {
		note = " (last " + strconv.Itoa(maxErrorExcerpt) + " bytes)"
	}
	return "\n" + what + note + ", full output in " + path + ":\n" + strings.TrimSpace(string(b))
}

// decodeFile decodes one JSON value from a file.
func decodeFile(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewDecoder(f).Decode(v)
}

// writeReport saves the report as <ReportDir>/<digest>.json.
func writeReport(dir string, r Report) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create report directory: %w", err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	return os.WriteFile(filepath.Join(dir, r.Digest+".json"), b, 0o600)
}

// sha256File returns the lowercase hex SHA-256 of a file.
func sha256File(path string) (string, error) {
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

// buildSummary returns summarySandboxExit71 when a non-canceled sandboxed
// build command exited with status 71, and defaultSummary otherwise.
func buildSummary(ctx context.Context, err error, defaultSummary string) string {
	if ctx.Err() == nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == sandboxExit71 {
			return summarySandboxExit71
		}
	}
	return defaultSummary
}

// stageError wraps err as the *StageError Run returns.
func stageError(stage Stage, summary string, err error) error {
	return &StageError{Stage: stage, Summary: summary, Err: err}
}
