package main

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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// verificationFile is the JSON form of the operator's CP4 configuration that
// `surla serve -verification-config` reads. Every field is required.
type verificationFile struct {
	// RunsDir holds one private directory per verification run.
	RunsDir string `json:"runs_dir"`
	// ReportDir receives every full report, named by its digest.
	ReportDir string `json:"report_dir"`
	// CandidateRepo is the git repository builders push candidates to.
	CandidateRepo string `json:"candidate_repo"`
	// CandidateModule is the Go module directory inside CandidateRepo.
	CandidateModule string `json:"candidate_module"`
	// Binaries maps each candidate binary name to its package path.
	Binaries map[string]string `json:"binaries"`
	// SuiteRepo is the git repository of the evaluator suite.
	SuiteRepo string `json:"suite_repo"`
	// SuiteCommit is the exact evaluator suite commit every run uses.
	SuiteCommit string `json:"suite_commit"`
	// SuiteModule is the suite's Go module directory inside SuiteRepo.
	SuiteModule string `json:"suite_module"`
	// SuiteCommand is the suite command's package path inside SuiteModule.
	SuiteCommand string `json:"suite_command"`
	// Timeout is how long a run may take, as a Go duration such as "30m".
	Timeout string `json:"timeout"`
}

// cappedBuffer collects at most limit bytes of one git query's output stream.
type cappedBuffer struct {
	// buf holds the bytes kept so far.
	buf bytes.Buffer
	// limit is the most bytes buf may hold.
	limit int
	// keepPrefix makes writes past limit succeed and be dropped, keeping the
	// first limit bytes, instead of failing the command.
	keepPrefix bool
	// overflow reports that the stream exceeded limit.
	overflow bool
}

// defaultProjectVerifyTimeout is the CP4 verification timeout derived for
// `surla serve -project <dir>`, matching the starter README.
const defaultProjectVerifyTimeout = 30 * time.Minute

// gitReadTimeout bounds each read-only git query that `surla serve -project`
// makes at startup, so an unresponsive repository stops startup with an error.
const gitReadTimeout = 30 * time.Second

// gitReadMaxOutput bounds the stdout of each startup git query. The largest
// is a committed go.mod file; a larger one stops startup with an error.
const gitReadMaxOutput = 1 << 20

// gitReadMaxStderr bounds how much of a failed git query's stderr is kept for
// the startup error message.
const gitReadMaxStderr = 4 << 10

// runVerifyRun runs one internal role started by verifyrun: `surla verify-run
// supervise <dir>` or `surla verify-run execute <dir>` for a verification run
// (verifyrun.Start), or `surla verify-run live <dir>` for a CP3 live target
// (verifyrun.LaunchLive). Only verifyrun starts these; they are not listed in
// the usage text.
func runVerifyRun(args []string) error {
	if len(args) != 2 {
		return errors.New("verify-run takes a role (supervise, execute or live) and a run directory")
	}
	self, err := selfCommand()
	if err != nil {
		return err
	}
	switch role, dir := args[0], args[1]; role {
	case "supervise":
		return verifyrun.Supervise(dir, self)
	case "execute":
		return verifyrun.Execute(dir, verify.Run, os.Stdout, os.Stderr)
	case "live":
		return verifyrun.Live(dir, verify.Prepare, os.Stdout, os.Stderr)
	default:
		return fmt.Errorf("unknown verify-run role %q", role)
	}
}

// runVerify builds the candidate commit in the OS sandbox and runs the
// evaluator suite commit against it, through the same supervised processes
// verify_change uses. -work-dir keeps its meaning: it receives the clones,
// the built binaries (candidate-bin/<name>) and the evidence, and is left in
// place. The run's own bookkeeping lives in a temporary directory removed
// when the command ends. It waits for the run and writes the JSON report to
// stdout: the full report, or its builder view with -builder-view. stdout
// holds only that JSON; after it, one human-readable summary line goes to
// stderr (see writeVerifySummary), which with -builder-view names holdouts
// only as holdout-<n>. The full report is also saved in -report-dir. A report that did not pass is still
// printed, and the command then fails so the exit status is 1. A canceled or
// timed-out run prints the partial report its suite saved while stopping, if
// any, and fails with the reason. Any other run that ends without a report
// prints nothing and fails with where it stopped and the full error. SIGINT,
// SIGTERM and SIGHUP cancel the run, and -timeout, when given, ends it.
func runVerify(args []string) error {
	plan, timeout, builderView, err := parseVerifyFlags(args)
	if err != nil {
		return err
	}
	self, err := selfCommand()
	if err != nil {
		return err
	}
	// Signals are caught before the run starts so a Ctrl-C or a closed
	// terminal always cancels it instead of leaving it running on its own.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	root, err := os.MkdirTemp("", "surla-verify-run-*")
	if err != nil {
		return fmt.Errorf("create run directory: %w", err)
	}
	defer os.RemoveAll(root)
	runID := verifyrun.NewRunID()
	supervisor, err := verifyrun.Start(root, verifyrun.Request{
		RunID: runID, Owner: "cli", AcceptedAt: time.Now().UTC(), Timeout: timeout, Plan: plan,
	}, self)
	if err != nil {
		return err
	}
	go forwardCancel(signals, supervisor)
	if err := verifyrun.Wait(root, runID); err != nil {
		return err
	}
	_, state, err := verifyrun.Read(root, runID)
	if err != nil {
		return err
	}
	if state.Kind == verifyrun.StateNotCompleted && state.Result.PartialReportDigest != "" {
		partial, err := verifyrun.LoadReport(plan.ReportDir, state.Result.PartialReportDigest)
		if err != nil {
			return err
		}
		// The summary follows the JSON, so it stays visible on a terminal.
		err = writeVerifyReport(os.Stdout, partial, builderView)
		writeVerifySummary(os.Stderr, partial, builderView)
		if err != nil {
			return fmt.Errorf("verification %s: %s (%w)", state.Result.Failure, state.Result.Summary, err)
		}
	}
	if state.Kind != verifyrun.StateCompleted {
		// The CLI's caller is the operator, so the full error is shown.
		detail, err := os.ReadFile(filepath.Join(root, runID, "executor.log"))
		if err != nil {
			return fmt.Errorf("read executor log: %w", err)
		}
		return fmt.Errorf("verification %s at stage %s: %s\n%s",
			state.Result.Failure, state.Result.Stage, state.Result.Summary, strings.TrimSpace(string(detail)))
	}
	err = writeVerifyReport(os.Stdout, state.Report, builderView)
	writeVerifySummary(os.Stderr, state.Report, builderView)
	return err
}

// writeVerifySummary writes one human-readable line about report to w, which
// runVerify passes as stderr so stdout stays the report's JSON alone. The line
// gives the verdict, the scenario counts, the scenarios that did not pass and
// the report digest. The verdict is the report's own: a run stopped after its
// suite passed keeps an all-passing partial report, yet the command still
// fails as interrupted. Each scenario ID is Go-quoted, because suites may declare
// IDs that contain spaces or line breaks and the summary must stay one line.
// With builderView it names scenarios as the builder view does, so a holdout
// appears only as holdout-<n>. A failed write is ignored:
// the summary only repeats what the JSON report already says.
func writeVerifySummary(w io.Writer, report verify.Report, builderView bool) {
	view := report.ForBuilders()
	var failing []string
	if builderView {
		for _, s := range view.Scenarios {
			if s.Outcome != verify.OutcomePassed {
				failing = append(failing, strconv.Quote(s.ID))
			}
		}
	} else {
		for _, s := range report.Scenarios {
			if s.Outcome != verify.OutcomePassed {
				failing = append(failing, strconv.Quote(s.ID))
			}
		}
	}
	verdict := "passed"
	if !report.Passed {
		verdict = "did not pass"
	}
	c := report.Counts
	line := fmt.Sprintf("surla: verification report %s: %d/%d scenarios passed; failed: %d, not run: %d, harness errors: %d",
		verdict, c.Passed, c.Required, c.Failed, c.NotRun, c.HarnessError)
	if view.SuiteFailed {
		line += "; the suite run itself failed"
	}
	if len(failing) > 0 {
		line += "; not passing: " + strings.Join(failing, ", ")
	}
	fmt.Fprintf(w, "%s (report %s)\n", line, report.Digest)
}

// resolveVerificationConfig selects and loads the operator's CP3/CP4 target
// configuration from either -project or -verification-config, rejecting
// supplying both flags at once.
func resolveVerificationConfig(cfg serveConfig) (*controlplane.VerificationConfig, error) {
	if cfg.projectSet && cfg.verificationPath != "" {
		return nil, errors.New("-project and -verification-config cannot be used together")
	}
	if cfg.projectSet {
		if strings.TrimSpace(cfg.projectDir) == "" {
			return nil, errors.New("-project requires a directory")
		}
		return loadProjectVerificationConfig(cfg.projectDir)
	}
	return loadVerificationConfig(cfg.verificationPath)
}

// loadProjectVerificationConfig derives a VerificationConfig from the starter
// layout committed at HEAD in projectDir, pinning SuiteCommit to that HEAD
// commit and placing RunsDir and ReportDir under <projectDir>/.surla/.
func loadProjectVerificationConfig(projectDir string) (*controlplane.VerificationConfig, error) {
	info, err := os.Stat(projectDir)
	if err != nil {
		return nil, fmt.Errorf("read -project %s: %w", projectDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("-project %s is not a directory", projectDir)
	}
	// EvalSymlinks keeps a relative path relative, and Git reports an absolute
	// repository root, so resolve the directory from the working directory
	// first: -project . names the directory surla serve started in.
	absProject, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, fmt.Errorf("resolve -project %s: %w", projectDir, err)
	}
	projectRoot, err := filepath.EvalSymlinks(absProject)
	if err != nil {
		return nil, fmt.Errorf("resolve -project %s: %w", projectDir, err)
	}
	topOut, err := runGitRead(projectRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("-project %s is not inside a git repository (run git init && git add -A && git commit -m 'Initial scaffold' first): %w",
			projectDir, err)
	}
	repoRoot, err := filepath.EvalSymlinks(strings.TrimSpace(string(topOut)))
	if err != nil {
		return nil, fmt.Errorf("resolve git repository root for -project %s: %w", projectDir, err)
	}
	relProject, err := filepath.Rel(repoRoot, projectRoot)
	if err != nil || (relProject != "." && !filepath.IsLocal(relProject)) {
		return nil, fmt.Errorf("-project %s lies outside git repository %s", projectDir, repoRoot)
	}
	headOut, err := runGitRead(repoRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, fmt.Errorf("-project %s has no committed HEAD (run git add -A && git commit -m 'Initial scaffold' first): %w",
			projectDir, err)
	}
	suiteCommit := strings.TrimSpace(string(headOut))

	relSlash := filepath.ToSlash(relProject)
	candidateModule := "."
	suiteModule := "eval"
	prefix := ""
	if relSlash != "." {
		candidateModule = relSlash
		suiteModule = relSlash + "/eval"
		prefix = relSlash + "/"
	}
	rootGoMod, err := readGitBlob(repoRoot, suiteCommit, prefix+"go.mod", projectDir)
	if err != nil {
		return nil, err
	}
	evalGoMod, err := readGitBlob(repoRoot, suiteCommit, prefix+"eval/go.mod", projectDir)
	if err != nil {
		return nil, err
	}
	layout, err := scaffold.StarterLayout(rootGoMod, evalGoMod)
	if err != nil {
		return nil, fmt.Errorf("-project %s does not match the surla init starter layout at HEAD (%s): %w (commit the starter layout or pass -verification-config)",
			projectDir, suiteCommit, err)
	}
	cmdTree := prefix + strings.TrimPrefix(layout.CandidatePackage, "./")
	if err := requireGitTree(repoRoot, suiteCommit, cmdTree, projectDir); err != nil {
		return nil, err
	}
	evalCmdTree := prefix + layout.SuiteDir + "/" + strings.TrimPrefix(layout.SuiteCommand, "./")
	if err := requireGitTree(repoRoot, suiteCommit, evalCmdTree, projectDir); err != nil {
		return nil, err
	}
	self, err := selfCommand()
	if err != nil {
		return nil, err
	}
	return &controlplane.VerificationConfig{
		Timeout: defaultProjectVerifyTimeout,
		RunsDir: filepath.Join(projectRoot, ".surla", "runs"),
		Plan: verify.Plan{
			CandidateRepo:   repoRoot,
			CandidateModule: candidateModule,
			Binaries:        map[string]string{layout.Service: layout.CandidatePackage},
			SuiteRepo:       repoRoot,
			SuiteCommit:     suiteCommit,
			SuiteModule:     suiteModule,
			SuiteCommand:    layout.SuiteCommand,
			ReportDir:       filepath.Join(projectRoot, "reports"),
		},
		Command: self,
	}, nil
}

// readGitBlob reads a committed regular-file blob at <commit>:<treePath> in
// repoRoot, rejecting missing paths, symlinks, and directories.
func readGitBlob(repoRoot, commit, treePath, projectDir string) ([]byte, error) {
	modeOut, err := runGitRead(repoRoot, "ls-tree", commit, "--", treePath)
	if err != nil || len(bytes.TrimSpace(modeOut)) == 0 {
		return nil, fmt.Errorf("-project %s is missing committed %s at HEAD (%s) (commit the starter layout or pass -verification-config)",
			projectDir, treePath, commit)
	}
	fields := strings.Fields(string(modeOut))
	if len(fields) < 2 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
		return nil, fmt.Errorf("-project %s: committed %s at HEAD (%s) must be a regular file", projectDir, treePath, commit)
	}
	return runGitRead(repoRoot, "cat-file", "blob", commit+":"+treePath)
}

// requireGitTree checks that <commit>:<treePath> exists as a committed
// directory in repoRoot.
func requireGitTree(repoRoot, commit, treePath, projectDir string) error {
	out, err := runGitRead(repoRoot, "cat-file", "-t", commit+":"+treePath)
	if err != nil || strings.TrimSpace(string(out)) != "tree" {
		return fmt.Errorf("-project %s is missing committed directory %s at HEAD (%s) (commit the starter layout or pass -verification-config)",
			projectDir, treePath, commit)
	}
	return nil
}

// runGitRead runs one read-only git query in dir and returns its stdout. It
// removes every inherited GIT_* variable, so dir alone selects the repository;
// ignores replace refs, so a commit ID names its real content; disables a
// configured file-system monitor and hooks, so no repository program runs;
// takes no optional locks; and stops after gitReadTimeout.
func runGitRead(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitReadTimeout)
	defer cancel()
	cmdArgs := append([]string{"-C", dir, "--no-optional-locks", "--no-replace-objects",
		"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = cleanGitEnv()
	stdout := &cappedBuffer{limit: gitReadMaxOutput}
	stderr := &cappedBuffer{limit: gitReadMaxStderr, keepPrefix: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if stdout.overflow {
		return nil, fmt.Errorf("git %s printed more than %d bytes", args[0], gitReadMaxOutput)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.buf.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w", msg, err)
		}
		return nil, err
	}
	return stdout.buf.Bytes(), nil
}

// cleanGitEnv returns os.Environ without any GIT_* variable, such as GIT_DIR,
// GIT_OBJECT_DIRECTORY or GIT_CONFIG_PARAMETERS, that could make git read a
// different repository, objects or configuration than the -project
// directory's own, plus settings that disable terminal prompts and the
// system-wide configuration file.
func cleanGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
}

// loadVerificationConfig strictly decodes the operator's verification
// configuration at path into a controlplane.VerificationConfig whose runs
// re-execute this surla executable. An empty path means verification is not
// configured.
func loadVerificationConfig(path string) (*controlplane.VerificationConfig, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read -verification-config: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var f verificationFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode -verification-config %s: %w", path, err)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode -verification-config %s: data after the configuration", path)
	}
	var missing []string
	for _, field := range []struct{ name, value string }{
		{"runs_dir", f.RunsDir}, {"report_dir", f.ReportDir}, {"candidate_repo", f.CandidateRepo},
		{"candidate_module", f.CandidateModule}, {"suite_repo", f.SuiteRepo}, {"suite_commit", f.SuiteCommit},
		{"suite_module", f.SuiteModule}, {"suite_command", f.SuiteCommand}, {"timeout", f.Timeout},
	} {
		if field.value == "" {
			missing = append(missing, field.name)
		}
	}
	if len(f.Binaries) == 0 {
		missing = append(missing, "binaries")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("-verification-config %s is missing %s", path, strings.Join(missing, ", "))
	}
	timeout, err := time.ParseDuration(f.Timeout)
	if err != nil || timeout <= 0 {
		return nil, fmt.Errorf("-verification-config %s: timeout %q must be a positive duration such as 30m", path, f.Timeout)
	}
	self, err := selfCommand()
	if err != nil {
		return nil, err
	}
	return &controlplane.VerificationConfig{
		Timeout: timeout,
		RunsDir: f.RunsDir,
		Plan: verify.Plan{
			CandidateRepo: f.CandidateRepo, CandidateModule: f.CandidateModule, Binaries: f.Binaries,
			SuiteRepo: f.SuiteRepo, SuiteCommit: f.SuiteCommit, SuiteModule: f.SuiteModule,
			SuiteCommand: f.SuiteCommand, ReportDir: f.ReportDir,
		},
		Command: self,
	}, nil
}

// forwardCancel asks the run's supervisor to cancel the run when the CLI
// receives SIGINT or SIGTERM. The supervisor runs in its own session, so it
// does not receive the terminal's signals itself.
func forwardCancel(signals <-chan os.Signal, supervisor int) {
	if _, ok := <-signals; !ok {
		return
	}
	if err := syscall.Kill(supervisor, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		fmt.Fprintf(os.Stderr, "surla: cancel verification: %v\n", err)
	}
}

// selfCommand returns the argument list that runs this surla executable.
func selfCommand() ([]string, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate the surla executable: %w", err)
	}
	return []string{exe}, nil
}

// Write appends p up to the buffer's limit. Past the limit it records the
// overflow and, unless keepPrefix is set, fails so the git command stops
// instead of producing output nobody will read.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.overflow = true
		if !c.keepPrefix {
			return 0, errors.New("output limit exceeded")
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}
