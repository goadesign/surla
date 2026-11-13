package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
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

// defaultProjectVerifyTimeout is how long the project server lets a run of the
// tests accepted for a change, or a live copy's start, take, matching the
// starter README.
const defaultProjectVerifyTimeout = 30 * time.Minute

// gitReadTimeout bounds the read-only git query the project server makes at
// startup to find the workspace's enclosing working tree, so an unresponsive
// repository stops startup with an error.
const gitReadTimeout = 30 * time.Second

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
