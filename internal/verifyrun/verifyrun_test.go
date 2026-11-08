package verifyrun

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
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/eval"

	"github.com/goadesign/surla/internal/verify"
)

const (
	// fakeModeEnv selects what the test executor's verification does: "pass"
	// and "failing" save a report that passed or failed, "fail" fails at the
	// build stage, and "hang" creates a store directory, starts a child
	// process in the run's group, records both PIDs, and on cancellation
	// removes the store, as a suite removes its pinned binaries.
	fakeModeEnv = "VERIFYRUN_TEST_FAKE"
	// fakeTimeoutEnv, when set, is the run timeout start uses.
	fakeTimeoutEnv = "VERIFYRUN_TEST_TIMEOUT"
)

// TestMain lets the test binary act as `surla verify-run supervise|execute`
// so the lifecycle runs in real processes.
func TestMain(m *testing.M) {
	if len(os.Args) == 4 && os.Args[1] == "verify-run" {
		os.Exit(helper(os.Args[2], os.Args[3]))
	}
	if len(os.Args) >= 2 && os.Args[1] == "--live" {
		os.Exit(fakeSuiteLive())
	}
	if len(os.Args) == 3 && os.Args[1] == "live-host" {
		os.Exit(liveHost(os.Args[2]))
	}
	os.Exit(m.Run())
}

// helper runs one role of the test binary and returns its exit status.
func helper(role, dir string) int {
	var err error
	switch role {
	case "supervise":
		err = Supervise(dir, []string{os.Args[0]})
	case "execute":
		err = Execute(dir, fakeRun, os.Stdout, os.Stderr)
	case "live":
		err = Live(dir, fakePrepare, os.Stdout, os.Stderr)
	default:
		err = fmt.Errorf("unknown role %q", role)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// fakeRun stands in for verify.Run according to fakeModeEnv.
func fakeRun(ctx context.Context, plan verify.Plan) (verify.Report, error) {
	switch os.Getenv(fakeModeEnv) {
	case "pass":
		if err := os.MkdirAll(filepath.Join(plan.WorkDir, "candidate-bin"), 0o700); err != nil {
			return verify.Report{}, err
		}
		return saveReport(plan, true)
	case "failing":
		return saveReport(plan, false)
	case "fail":
		return verify.Report{}, &verify.StageError{Stage: verify.StageBuild, Summary: "The candidate did not build.",
			Err: errors.New("compile error in /private/secret/path")}
	case "fail-exit71":
		return verify.Report{}, &verify.StageError{
			Stage:   verify.StageBuild,
			Summary: "The sandboxed command exited with status 71. If Surla is running inside a restricted coding-agent shell, its sandbox may have been blocked; run Surla from an unrestricted host shell.",
			Err:     errors.New("build candidate app: exit status 71 in /private/secret/path"),
		}
	case "hang":
		store := filepath.Join(plan.WorkDir, "store")
		if err := os.MkdirAll(store, 0o700); err != nil {
			return verify.Report{}, err
		}
		child := exec.Command("sleep", "300")
		if err := child.Start(); err != nil {
			return verify.Report{}, err
		}
		if err := writePIDs(plan, os.Getpid(), child.Process.Pid); err != nil {
			return verify.Report{}, err
		}
		<-ctx.Done()
		if err := os.RemoveAll(store); err != nil {
			return verify.Report{}, err
		}
		return verify.Report{}, &verify.StageError{Stage: verify.StageSuite, Summary: "The evaluator suite ended without a report.", Err: ctx.Err()}
	case "stop-with-report":
		// Like a suite that saves a partial report when it is stopped.
		if err := writePIDs(plan, os.Getpid(), os.Getpid()); err != nil {
			return verify.Report{}, err
		}
		<-ctx.Done()
		return saveReport(plan, false)
	}
	return verify.Report{}, errors.New("no fake mode")
}

// saveReport assembles a report for plan whose one scenario passed or failed
// and saves it where verify.Run would.
func saveReport(plan verify.Plan, passed bool) (verify.Report, error) {
	binding := verify.Binding{
		CandidateCommit: plan.CandidateCommit, RequestRevision: plan.RequestRevision,
		SuiteID: "starter", SuiteCommit: plan.SuiteCommit, Environment: "sim",
		Artifacts: map[string]string{"app": strings.Repeat("d", 64)},
	}
	suite := verify.Suite{ID: "starter", Environment: "sim", Scenarios: []verify.Scenario{{ID: "smoke"}}}
	check := eval.Check{Name: "ok", Passed: true}
	if !passed {
		check = eval.Check{Name: "ok", Diagnostic: "wrong answer"}
	}
	evidence := eval.Report{SuiteID: "starter", Scenarios: []eval.ScenarioReport{{
		ID: "smoke", StartedAt: time.Now(), Passed: passed,
		Result: &eval.Result{
			Checks:    []eval.Check{check},
			Artifacts: []eval.Artifact{{Name: "candidate:app", URI: "sha256:" + strings.Repeat("d", 64)}},
		},
	}}}
	report, err := verify.Assemble(binding, suite, evidence, 0)
	if err != nil {
		return verify.Report{}, err
	}
	b, err := json.Marshal(report)
	if err != nil {
		return verify.Report{}, err
	}
	return report, os.WriteFile(filepath.Join(plan.ReportDir, report.Digest+".json"), b, 0o600)
}

// start accepts a run in fake mode and returns its root, ID and supervisor
// PID.
func start(t *testing.T, mode string) (string, string, int) {
	t.Helper()
	t.Setenv(fakeModeEnv, mode)
	root, reports := t.TempDir(), t.TempDir()
	runID := NewRunID()
	timeout := time.Hour
	if v := os.Getenv(fakeTimeoutEnv); v != "" {
		d, err := time.ParseDuration(v)
		require.NoError(t, err)
		timeout = d
	}
	pid, err := Start(root, Request{
		RunID: runID, Owner: "alice", ChangeID: "chg", AcceptedAt: time.Now().UTC(), Timeout: timeout,
		Plan: verify.Plan{
			CandidateCommit: strings.Repeat("a", 40), RequestRevision: "rev-1",
			SuiteCommit: strings.Repeat("b", 40), ReportDir: reports,
			WorkDir: filepath.Join(root, runID, "work"),
		},
	}, []string{os.Args[0]})
	require.NoError(t, err)
	return root, runID, pid
}

// writePIDs records the executor and child PIDs of a fake run in the run's
// "pids" file. It writes a temporary file and renames it, so hangingPIDs never
// reads a file that exists but is not yet written.
func writePIDs(plan verify.Plan, executor, child int) error {
	path := filepath.Join(filepath.Dir(plan.WorkDir), "pids")
	if err := os.WriteFile(path+".tmp", []byte(fmt.Sprintf("%d %d", executor, child)), 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

// hangingPIDs waits up to 30s for a hanging run to record its executor and
// child PIDs.
func hangingPIDs(t *testing.T, root, runID string) (executor, child int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(root, runID, "pids"))
		if err == nil {
			fields := strings.Fields(string(raw))
			require.Len(t, fields, 2)
			executor, err = strconv.Atoi(fields[0])
			require.NoError(t, err)
			child, err = strconv.Atoi(fields[1])
			require.NoError(t, err)
			return executor, child
		}
		require.True(t, time.Now().Before(deadline), "the hanging run never recorded its PIDs")
		time.Sleep(20 * time.Millisecond)
	}
}

// assertGone waits up to 10s for pid to stop existing.
func assertGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		require.True(t, time.Now().Before(deadline), "process %d outlived the run", pid)
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRunThatProducesAReportReadsAsCompleted(t *testing.T) {
	root, runID, _ := start(t, "pass")
	require.NoError(t, Wait(root, runID))
	req, state, err := Read(root, runID)
	require.NoError(t, err)
	assert.Equal(t, "alice", req.Owner)
	require.Equal(t, StateCompleted, state.Kind)
	assert.True(t, state.Report.Passed)
	assert.DirExists(t, filepath.Join(root, runID, "work", "candidate-bin"),
		"the caller owns the work directory; its built binaries stay readable after the run")
}

func TestRunWhoseReportFailedReadsAsCompleted(t *testing.T) {
	root, runID, _ := start(t, "failing")
	require.NoError(t, Wait(root, runID))
	_, state, err := Read(root, runID)
	require.NoError(t, err)
	require.Equal(t, StateCompleted, state.Kind)
	assert.False(t, state.Report.Passed)
}

func TestFailedStepReadsAsExecutionFailedWithoutItsOutput(t *testing.T) {
	root, runID, _ := start(t, "fail")
	require.NoError(t, Wait(root, runID))
	_, state, err := Read(root, runID)
	require.NoError(t, err)
	assert.Equal(t, State{Kind: StateNotCompleted, Result: Result{
		Kind: ResultNoReport, Failure: FailureExecution, Stage: "build", Summary: "The candidate did not build.",
	}}, state)
	saved, err := os.ReadFile(filepath.Join(root, runID, "result.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(saved), "/private/secret/path")
	evidence, err := os.ReadFile(filepath.Join(root, runID, "executor.log"))
	require.NoError(t, err)
	assert.Contains(t, string(evidence), "/private/secret/path", "the full error stays with the evaluator")

	root71, runID71, _ := start(t, "fail-exit71")
	require.NoError(t, Wait(root71, runID71))
	_, state71, err := Read(root71, runID71)
	require.NoError(t, err)
	want71Summary := "The sandboxed command exited with status 71. If Surla is running inside a restricted coding-agent shell, its sandbox may have been blocked; run Surla from an unrestricted host shell."
	assert.Equal(t, State{Kind: StateNotCompleted, Result: Result{
		Kind: ResultNoReport, Failure: FailureExecution, Stage: "build", Summary: want71Summary,
	}}, state71)
	saved71, err := os.ReadFile(filepath.Join(root71, runID71, "result.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(saved71), "/private/secret/path")
	evidence71, err := os.ReadFile(filepath.Join(root71, runID71, "executor.log"))
	require.NoError(t, err)
	assert.Contains(t, string(evidence71), "/private/secret/path")
}

func TestKilledExecutorIsInterruptedAndLeavesNoProcesses(t *testing.T) {
	root, runID, _ := start(t, "hang")
	executor, child := hangingPIDs(t, root, runID)
	_, state, err := Read(root, runID)
	require.NoError(t, err)
	assert.Equal(t, StateRunning, state.Kind)

	require.NoError(t, syscall.Kill(executor, syscall.SIGKILL))
	require.NoError(t, Wait(root, runID))
	assertGone(t, child)
	_, state, err = Read(root, runID)
	require.NoError(t, err)
	assert.Equal(t, interrupted(summaryExecutorDied), state.Result)
	assert.DirExists(t, filepath.Join(root, runID, "work"), "an interrupted run keeps its work directory as evidence")
}

func TestKilledSupervisorStopsTheRunAndLeavesNoProcesses(t *testing.T) {
	root, runID, supervisor := start(t, "hang")
	executor, child := hangingPIDs(t, root, runID)

	require.NoError(t, syscall.Kill(supervisor, syscall.SIGKILL))
	require.NoError(t, Wait(root, runID))
	assertGone(t, executor)
	assertGone(t, child)
	_, state, err := Read(root, runID)
	require.NoError(t, err)
	assert.Equal(t, interrupted(summarySupervisorDied), state.Result)
}

func TestCanceledRunStopsGracefullyAndLeavesNoProcesses(t *testing.T) {
	root, runID, supervisor := start(t, "hang")
	executor, child := hangingPIDs(t, root, runID)

	require.NoError(t, syscall.Kill(supervisor, syscall.SIGTERM))
	require.NoError(t, Wait(root, runID))
	assertGone(t, executor)
	assertGone(t, child)
	assert.NoDirExists(t, filepath.Join(root, runID, "work", "store"), "the executor got to clean up before the group was killed")
	_, state, err := Read(root, runID)
	require.NoError(t, err)
	assert.Equal(t, interrupted(summaryCanceled), state.Result)
}

func TestRunStoppedWhileItsSuiteReportsIsInterruptedNotFailed(t *testing.T) {
	t.Setenv(fakeTimeoutEnv, "2s")
	root, runID, _ := start(t, "stop-with-report")
	hangingPIDs(t, root, runID)
	require.NoError(t, Wait(root, runID))
	_, state, err := Read(root, runID)
	require.NoError(t, err)
	assert.Equal(t, FailureInterrupted, state.Result.Failure,
		"a partial report from a stopped suite would read as failed scenarios")
	assert.Equal(t, summaryTimedOut, state.Result.Summary)
	req, _, err := Read(root, runID)
	require.NoError(t, err)
	partial, err := LoadReport(req.Plan.ReportDir, state.Result.PartialReportDigest)
	require.NoError(t, err, "the stopped outcome names the partial report")
	assert.False(t, partial.Passed)
}

func TestRunPastItsTimeoutIsStopped(t *testing.T) {
	t.Setenv(fakeTimeoutEnv, "2s")
	root, runID, _ := start(t, "hang")
	executor, child := hangingPIDs(t, root, runID)

	require.NoError(t, Wait(root, runID))
	assertGone(t, executor)
	assertGone(t, child)
	_, state, err := Read(root, runID)
	require.NoError(t, err)
	assert.Equal(t, interrupted(summaryTimedOut), state.Result)
}

func TestReadAndStartRejectInvalidRuns(t *testing.T) {
	root := t.TempDir()
	_, _, err := Read(root, "../escape")
	assert.ErrorContains(t, err, "is not 32 lowercase hex characters")
	_, _, err = Read(root, NewRunID())
	assert.ErrorContains(t, err, "read run request")

	runID := NewRunID()
	require.NoError(t, os.MkdirAll(filepath.Join(root, runID), 0o700))
	_, err = Start(root, Request{
		RunID:      runID,
		Owner:      "alice",
		AcceptedAt: time.Now().UTC(),
		Timeout:    time.Minute,
		Plan:       verify.Plan{WorkDir: "w"},
	}, []string{os.Args[0]})
	assert.ErrorContains(t, err, "create run directory", "a run directory is never reused")
	_, err = Start(root, Request{RunID: NewRunID(), Timeout: -time.Second}, []string{os.Args[0]})
	assert.ErrorContains(t, err, "must not be negative")
	_, err = Start(root, Request{RunID: NewRunID(), Timeout: time.Minute}, []string{os.Args[0]})
	assert.ErrorContains(t, err, "work directory is required")
	_, err = Start(root, Request{RunID: NewRunID(), Timeout: time.Minute, Plan: verify.Plan{WorkDir: "w"}}, []string{os.Args[0]})
	assert.ErrorContains(t, err, "run owner is required")
	_, err = Start(root, Request{RunID: NewRunID(), Owner: "alice", Timeout: time.Minute, Plan: verify.Plan{WorkDir: "w"}}, []string{os.Args[0]})
	assert.ErrorContains(t, err, "run accepted_at time is required")
}

func TestReadRequestValidationPreservesCLIAndRejectsCorruptRecords(t *testing.T) {
	root := t.TempDir()
	writeRawRun := func(t *testing.T, runID, requestBody, resultBody string) {
		t.Helper()
		dir := filepath.Join(root, runID)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, requestFile), []byte(requestBody), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, lockFile), nil, 0o600))
		if resultBody != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dir, resultFile), []byte(resultBody), 0o600))
		}
	}

	// Standalone CLI requests have Owner="cli" and ChangeID="" and must remain valid.
	cliRunID := NewRunID()
	cliReq, err := json.Marshal(Request{
		RunID:      cliRunID,
		Owner:      "cli",
		ChangeID:   "",
		AcceptedAt: time.Date(2026, 10, 31, 12, 0, 0, 100_000_000, time.UTC),
	})
	require.NoError(t, err)
	writeRawRun(t, cliRunID, string(cliReq), `{"kind":"no_report","failure":"execution_failed","stage":"build","summary":"Failed"}`)
	gotCLI, gotState, err := Read(root, cliRunID)
	require.NoError(t, err)
	assert.Equal(t, "cli", gotCLI.Owner)
	assert.Empty(t, gotCLI.ChangeID)
	assert.Equal(t, StateNotCompleted, gotState.Kind)

	for _, tc := range []struct {
		name      string
		buildBody func(runID string) string
		wantErr   string
		wantOwner string
	}{
		{
			name:      "empty JSON object",
			buildBody: func(_ string) string { return `{}` },
			wantErr:   "does not match directory",
		},
		{
			name:      "JSON null",
			buildBody: func(_ string) string { return `null` },
			wantErr:   "does not match directory",
		},
		{
			name: "mismatched run_id preserves decoded owner",
			buildBody: func(_ string) string {
				return `{"run_id":"` + NewRunID() + `","owner":"bob","change_id":"chg","accepted_at":"2026-10-31T12:00:00Z"}`
			},
			wantErr:   "does not match directory",
			wantOwner: "bob",
		},
		{
			name: "blank owner",
			buildBody: func(runID string) string {
				return `{"run_id":"` + runID + `","owner":"   ","change_id":"chg","accepted_at":"2026-10-31T12:00:00Z"}`
			},
			wantErr: "owner is required",
		},
		{
			name: "zero accepted_at preserves decoded owner",
			buildBody: func(runID string) string {
				return `{"run_id":"` + runID + `","owner":"bob","change_id":"chg"}`
			},
			wantErr:   "accepted_at is required",
			wantOwner: "bob",
		},
		{
			name: "trailing JSON data preserves decoded owner",
			buildBody: func(runID string) string {
				return `{"run_id":"` + runID + `","owner":"bob","change_id":"chg","accepted_at":"2026-10-31T12:00:00Z"}{}`
			},
			wantErr:   "unexpected trailing data",
			wantOwner: "bob",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runID := NewRunID()
			writeRawRun(t, runID, tc.buildBody(runID), "")
			req, _, err := Read(root, runID)
			require.ErrorContains(t, err, tc.wantErr)
			assert.Equal(t, tc.wantOwner, req.Owner)
		})
	}
}

func TestListFiltersByOwnerAndChangeBeforeReadingStateAndSortsDeterministically(t *testing.T) {
	missingRoot := filepath.Join(t.TempDir(), "not-created-yet")
	runs, err := List(missingRoot, "alice", "chg-1")
	require.NoError(t, err)
	assert.Empty(t, runs, "missing root returns nil, nil before any run is started")

	_, err = List(missingRoot, "", "chg-1")
	assert.ErrorContains(t, err, "run owner is required")
	_, err = List(missingRoot, "alice", "")
	assert.ErrorContains(t, err, "change_id is required")

	root := t.TempDir()
	writeRun := func(t *testing.T, runID, owner, changeID string, acceptedAt time.Time, resultBody string) {
		t.Helper()
		dir := filepath.Join(root, runID)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		rawReq, err := json.Marshal(Request{
			RunID:      runID,
			Owner:      owner,
			ChangeID:   changeID,
			AcceptedAt: acceptedAt,
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, requestFile), rawReq, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, lockFile), nil, 0o600))
		if resultBody != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dir, resultFile), []byte(resultBody), 0o600))
		}
	}

	// Unaccepted directory without request.json (e.g. Start publication window) is skipped cleanly.
	unacceptedID := NewRunID()
	require.NoError(t, os.MkdirAll(filepath.Join(root, unacceptedID), 0o700))
	// Non-run directory entry is ignored.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "not-a-run-id"), 0o700))

	// Foreign principal run and another card's run have corrupt result.json;
	// List for (alice, chg-1) must skip them before reading result.json.
	writeRun(t, NewRunID(), "bob", "chg-1", time.Date(2026, 10, 31, 12, 5, 0, 0, time.UTC), `{corrupt-foreign-result`)
	writeRun(t, NewRunID(), "alice", "chg-other", time.Date(2026, 10, 31, 12, 5, 0, 0, time.UTC), `{corrupt-other-card-result`)

	// Matching runs for (alice, chg-1), including sub-second precision and same-timestamp tie-break by RunID.
	idTieB := "22222222222222222222222222222222"
	idTieA := "11111111111111111111111111111111"
	idOlder := "33333333333333333333333333333333"
	idNewest := "00000000000000000000000000000000"
	sameTime := time.Date(2026, 10, 31, 12, 0, 0, 100_000_000, time.UTC)
	newerSubSec := time.Date(2026, 10, 31, 12, 0, 0, 900_000_000, time.UTC)
	olderTime := time.Date(2026, 10, 31, 11, 59, 0, 0, time.UTC)
	okResult := `{"kind":"no_report","failure":"execution_failed","stage":"build","summary":"Stopped at build."}`

	writeRun(t, idTieB, "alice", "chg-1", sameTime, okResult)
	writeRun(t, idOlder, "alice", "chg-1", olderTime, "")
	writeRun(t, idTieA, "alice", "chg-1", sameTime, okResult)
	writeRun(t, idNewest, "alice", "chg-1", newerSubSec, okResult)

	runs, err = List(root, "alice", "chg-1")
	require.NoError(t, err)
	require.Len(t, runs, 4)
	assert.Equal(t, []string{idNewest, idTieA, idTieB, idOlder}, []string{
		runs[0].Request.RunID,
		runs[1].Request.RunID,
		runs[2].Request.RunID,
		runs[3].Request.RunID,
	})
	assert.Equal(t, newerSubSec, runs[0].Request.AcceptedAt)

	// A published request.json that is {} or corrupt must fail List rather than being silently skipped.
	corruptID := NewRunID()
	corruptDir := filepath.Join(root, corruptID)
	require.NoError(t, os.MkdirAll(corruptDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(corruptDir, requestFile), []byte(`{}`), 0o600))
	_, err = List(root, "alice", "chg-1")
	require.ErrorContains(t, err, "does not match directory")
}
