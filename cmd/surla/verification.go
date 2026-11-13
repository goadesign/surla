package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// verificationFile is the JSON form of the operator's CP4 configuration that
// `surla verification set` validates. Every field is required.
type verificationFile struct {
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

// cappedBuffer collects one git query's output stream, failing the query once
// the stream exceeds limit bytes.
type cappedBuffer struct {
	// buf holds the bytes written so far, never more than limit.
	buf bytes.Buffer
	// limit is the most bytes the stream may carry.
	limit int
	// overflow reports that the stream exceeded limit.
	overflow bool
}

// defaultProjectVerifyTimeout is the CP4 verification timeout derived for
// the project server, matching the starter README.
const defaultProjectVerifyTimeout = 30 * time.Minute

// gitReadTimeout bounds all the read-only git queries the project server
// makes at startup together, so an unresponsive repository stops startup with
// an error.
const gitReadTimeout = 30 * time.Second

// gitReadMaxOutput bounds the stdout of each startup git query. The largest
// is a committed go.mod file; a larger one stops startup with an error.
const gitReadMaxOutput = 1 << 20

// gitReadMaxStderr bounds the stderr of each startup git query, which a
// failed query's error message quotes.
const gitReadMaxStderr = 4 << 10

// Write appends p, or records the overflow and fails when p would take the
// stream past limit, so the git command stops instead of producing output
// nobody will read.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.limit {
		c.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	return c.buf.Write(p)
}

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
// stdout: its builder view by default, or the full report with
// -builder-view=false. stdout holds only that JSON; after it, one summary line goes to
// stderr (see writeVerifySummary), which in the builder view names holdouts
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

// starterVerificationAtHEAD derives a VerificationConfig from the starter
// layout committed at HEAD in workspaceDir and places RunsDir at runsDir and
// ReportDir at reportDir. Every git read shares ctx's deadline.
func starterVerificationAtHEAD(
	ctx context.Context,
	workspaceDir, runsDir, reportDir string,
) (*controlplane.VerificationConfig, error) {
	repoRoot, prefix, err := resolveStarterRepo(ctx, workspaceDir)
	if err != nil {
		return nil, err
	}
	headOut, err := runGitRead(ctx, repoRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, fmt.Errorf(
			"project %s has no committed HEAD (run git add -A && git commit -m 'Initial scaffold' first, or initialize a starter project with init_project): %w",
			workspaceDir,
			err,
		)
	}
	suiteCommit := strings.TrimSpace(string(headOut))
	return starterVerificationFromRepoCommit(ctx, workspaceDir, repoRoot, prefix, suiteCommit, runsDir, reportDir)
}

// starterVerificationAtCommit derives a VerificationConfig from the starter
// layout committed at pinnedCommit in workspaceDir and places RunsDir at
// runsDir and ReportDir at reportDir.
func starterVerificationAtCommit(
	ctx context.Context,
	workspaceDir, pinnedCommit, runsDir, reportDir string,
) (*controlplane.VerificationConfig, error) {
	if !verify.IsCommitID(pinnedCommit) {
		return nil, fmt.Errorf("project %s: suite commit %q is not a full commit ID", workspaceDir, pinnedCommit)
	}
	repoRoot, prefix, err := resolveStarterRepo(ctx, workspaceDir)
	if err != nil {
		return nil, err
	}
	headOut, err := runGitRead(ctx, repoRoot, "rev-parse", "--verify", pinnedCommit+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("project %s is missing pinned evaluator suite commit %s: %w", workspaceDir, pinnedCommit, err)
	}
	suiteCommit := strings.TrimSpace(string(headOut))
	return starterVerificationFromRepoCommit(ctx, workspaceDir, repoRoot, prefix, suiteCommit, runsDir, reportDir)
}

// resolveStarterRepo resolves dir to its physical path and queries git for its
// repository root and slash-separated prefix within that repository.
func resolveStarterRepo(ctx context.Context, dir string) (repoRoot, prefix string, err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", "", fmt.Errorf("read project %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("project %s is not a directory", dir)
	}
	// Resolve from the working directory without cleaning parent components
	// before following symlinks: alias/.. must name the linked directory's parent.
	projectRoot, err := abspath.Eval(dir)
	if err != nil {
		return "", "", fmt.Errorf("resolve project %s: %w", dir, err)
	}
	// Git reports the repository root and the project's path inside it, such
	// as "services/shop/" or "" at the root, one per line.
	topOut, err := runGitRead(ctx, projectRoot, "rev-parse", "--show-toplevel", "--show-prefix")
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return "", "", fmt.Errorf(
				"project %s is not inside a git repository (run git init && git add -A && git commit -m 'Initial scaffold' first, or initialize a starter project with init_project): %w",
				dir,
				err,
			)
		}
		return "", "", fmt.Errorf("project %s: find its git repository: %w", dir, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(topOut), "\n"), "\n")
	if len(lines) != 2 {
		return "", "", fmt.Errorf("project %s: git rev-parse printed %q, want the repository root and prefix", dir, topOut)
	}
	return lines[0], lines[1], nil
}

// starterVerificationFromRepoCommit reads both starter go.mod files and checks
// both command directories at suiteCommit in repoRoot, returning a
// VerificationConfig pinned to suiteCommit with RunsDir and ReportDir placed at
// runsDir and reportDir.
func starterVerificationFromRepoCommit(
	ctx context.Context,
	dir, repoRoot, prefix, suiteCommit, runsDir, reportDir string,
) (*controlplane.VerificationConfig, error) {
	// Module directories and committed paths are slash-separated and relative
	// to the repository root: "." and "eval" for a project at the root,
	// "services/shop" and "services/shop/eval" for one nested in it.
	candidateModule := path.Clean("./" + prefix)
	suiteModule := path.Join(candidateModule, scaffold.SuiteDir)
	rootGoMod, err := readGitBlob(ctx, repoRoot, suiteCommit, path.Join(candidateModule, "go.mod"), dir)
	if err != nil {
		return nil, err
	}
	evalGoMod, err := readGitBlob(ctx, repoRoot, suiteCommit, path.Join(suiteModule, "go.mod"), dir)
	if err != nil {
		return nil, err
	}
	layout, err := scaffold.StarterLayout(rootGoMod, evalGoMod)
	if err != nil {
		return nil, fmt.Errorf("project %s does not match the surla init starter layout at HEAD (%s): %w (commit the starter layout or pass verification settings)",
			dir, suiteCommit, err)
	}
	for _, tree := range []string{
		path.Join(candidateModule, layout.CandidatePackage),
		path.Join(suiteModule, layout.SuiteCommand),
	} {
		if err := requireGitTree(ctx, repoRoot, suiteCommit, tree, dir); err != nil {
			return nil, err
		}
	}
	self, err := selfCommand()
	if err != nil {
		return nil, err
	}
	return &controlplane.VerificationConfig{
		Timeout: defaultProjectVerifyTimeout,
		RunsDir: runsDir,
		Plan: verify.Plan{
			CandidateRepo:   repoRoot,
			CandidateModule: candidateModule,
			Binaries:        map[string]string{layout.Service: layout.CandidatePackage},
			SuiteRepo:       repoRoot,
			SuiteCommit:     suiteCommit,
			SuiteModule:     suiteModule,
			SuiteCommand:    layout.SuiteCommand,
			ReportDir:       reportDir,
		},
		Command: self,
	}, nil
}

// readGitBlob reads a committed regular-file blob at <commit>:<treePath> in
// repoRoot, rejecting missing paths, symlinks, and directories.
func readGitBlob(ctx context.Context, repoRoot, commit, treePath, project string) ([]byte, error) {
	mode, kind, err := gitEntry(ctx, repoRoot, commit, treePath)
	switch {
	case err != nil:
		return nil, fmt.Errorf("project %s: %w", project, err)
	case kind == "":
		return nil, fmt.Errorf("project %s is missing committed %s at HEAD (%s)"+
			" (commit the starter layout or pass verification settings)", project, treePath, commit)
	case kind != "blob" || (mode != "100644" && mode != "100755"):
		return nil, fmt.Errorf("project %s: committed %s at HEAD (%s) must be a regular file", project, treePath, commit)
	}
	content, err := runGitRead(ctx, repoRoot, "cat-file", "blob", commit+":"+treePath)
	if err != nil {
		return nil, fmt.Errorf("project %s: read committed %s at HEAD (%s): %w (the project server never downloads"+
			" objects; if this is a partial clone, check out HEAD there first so git fetches them)",
			project, treePath, commit, err)
	}
	return content, nil
}

// requireGitTree checks that <commit>:<treePath> exists as a committed
// directory in repoRoot.
func requireGitTree(ctx context.Context, repoRoot, commit, treePath, project string) error {
	_, kind, err := gitEntry(ctx, repoRoot, commit, treePath)
	if err != nil {
		return fmt.Errorf("project %s: %w", project, err)
	}
	if kind != "tree" {
		return fmt.Errorf("project %s is missing committed directory %s at HEAD (%s)"+
			" (commit the starter layout or pass verification settings)", project, treePath, commit)
	}
	return nil
}

// gitEntry returns the mode and object type, such as "100644" and "blob",
// of <commit>:<treePath> in repoRoot, or two empty strings when the commit
// has no such path. An error means git itself failed.
func gitEntry(ctx context.Context, repoRoot, commit, treePath string) (string, string, error) {
	out, err := runGitRead(ctx, repoRoot, "ls-tree", commit, "--", treePath)
	if err != nil {
		return "", "", fmt.Errorf("list committed %s at HEAD (%s): %w", treePath, commit, err)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return "", "", nil
	}
	return fields[0], fields[1], nil
}

// runGitRead runs one read-only git query in dir and returns its stdout. It
// removes every inherited GIT_* variable, so dir alone selects the repository;
// ignores replace refs, so a commit ID names its real content; disables a
// configured file-system monitor and hooks, and never fetches objects missing
// from a partial clone, so no repository program or network access runs;
// takes no optional locks; and stops when ctx ends. Output past
// gitReadMaxOutput on stdout or gitReadMaxStderr on stderr fails the query.
func runGitRead(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmdArgs := append([]string{"-C", dir, "--no-optional-locks", "--no-replace-objects", "--no-lazy-fetch", "--literal-pathspecs",
		"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = cleanGitEnv()
	cmd.WaitDelay = time.Second
	stdout := &cappedBuffer{limit: gitReadMaxOutput}
	stderr := &cappedBuffer{limit: gitReadMaxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if strings.Contains(stderr.buf.String(), "unknown option: --no-lazy-fetch") {
		return nil, errors.New("the project server needs Git 2.45 or newer (for --no-lazy-fetch, so startup" +
			" never downloads objects); upgrade Git or pass verification settings")
	}
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("git %s printed more output than a startup read allows", args[0])
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
// different repository, objects or configuration than the project
// directory's own, plus settings that disable terminal prompts and the
// system-wide configuration file and keep git's messages in English, which
// resolveStarterRepo matches.
func cleanGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
}

// verificationConfig validates one settings snapshot and derives all storage paths from its project owner.
func verificationConfig(f verificationFile, runsDir, reportDir string) (*controlplane.VerificationConfig, error) {
	var missing []string
	for _, field := range []struct{ name, value string }{
		{"candidate_repo", f.CandidateRepo},
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
		return nil, fmt.Errorf("verification settings are missing %s", strings.Join(missing, ", "))
	}
	timeout, err := time.ParseDuration(f.Timeout)
	if err != nil || timeout <= 0 {
		return nil, fmt.Errorf("verification timeout %q must be a positive duration such as 30m", f.Timeout)
	}
	self, err := selfCommand()
	if err != nil {
		return nil, err
	}
	return &controlplane.VerificationConfig{
		Timeout: timeout,
		RunsDir: runsDir,
		Plan: verify.Plan{
			CandidateRepo: f.CandidateRepo, CandidateModule: f.CandidateModule, Binaries: f.Binaries,
			SuiteRepo: f.SuiteRepo, SuiteCommit: f.SuiteCommit, SuiteModule: f.SuiteModule,
			SuiteCommand: f.SuiteCommand, ReportDir: reportDir,
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
