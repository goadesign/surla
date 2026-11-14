// Verification reads retain saved evidence while observing the inputs selected
// now. These tests simulate supervisor and checker files without executing code.
package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// TestVerificationCurrentInputsFollowWrittenSelection checks each historical
// run against one current selection, including an older matching run when the
// newest run is stale, and preserves test-writer access without accepted tests.
func TestVerificationCurrentInputsFollowWrittenSelection(t *testing.T) {
	runsDir := t.TempDir()
	cp := newBuildControlPlane(t, Options{
		RunsDir:         runsDir,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Timeout: time.Minute},
	})
	user := asCaller("alice@example.com", CallerUser)
	writer := asCaller("alice@example.com", CallerTestWriter)
	card := confirmedChange(t, cp, "chg")
	older, newer := strings.Repeat("1", 32), strings.Repeat("2", 32)
	for i, runID := range []string{older, newer} {
		writeListedRun(t, runsDir, "alice@example.com", card, runID,
			time.Date(2026, 10, 1+i, 9, 0, 0, 0, time.UTC), true)
		setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	}
	newCommit, oldCommit := strings.Repeat("3", 40), strings.Repeat("2", 40)
	editVerificationRequest(t, runsDir, newer, func(req *verifyrun.Request) {
		req.Plan.SuiteCommit = newCommit
	})
	// The second fixture has no report: its stored request can name another
	// test commit without breaking the immutable report binding.
	require.NoError(t, os.WriteFile(filepath.Join(runsDir, newer, "result.json"),
		[]byte(`{"kind":"no_report","failure":"interrupted","stage":"supervise","summary":"Stopped."}`), 0o600))
	check := func(wantOld, wantNew bool) {
		t.Helper()
		listed, err := cp.ListVerifications(user, &gensurla.ListVerificationsPayload{ChangeID: card.ChangeID})
		require.NoError(t, err)
		require.Len(t, listed.Runs, 2)
		assert.Equal(t, newer, listed.Runs[0].RunID)
		assert.Equal(t, older, listed.Runs[1].RunID)
		for i, want := range []bool{wantNew, wantOld} {
			assert.Equal(t, want, listed.Runs[i].InputsCurrent)
			got, err := cp.GetVerification(user, &gensurla.GetVerificationPayload{RunID: listed.Runs[i].RunID})
			require.NoError(t, err)
			assert.Equal(t, listed.Runs[i], got)
		}
		completed, ok := listed.Runs[1].State.AsCompleted()
		require.True(t, ok)
		assert.True(t, completed.Report.Passed, "historical verdict does not change")
		assert.Equal(t, gensurla.NewTestSourceWrittenTests(&gensurla.AgentWrittenTests{
			Commit: oldCommit, Notice: writtenTestsNotice,
		}), listed.Runs[1].TestSource)
	}
	check(false, false) // Written verification requires a usable build.
	start := func() {
		t.Helper()
		_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: card.ChangeID, RequestRevision: card.RequestRevision})
		require.NoError(t, err)
	}
	start()
	check(false, false)
	for _, state := range []string{testsReported, testsNeedChanges, testsCouldNotRun} {
		saveListedTestState(t, cp, card.ChangeID, state, "")
		check(false, false)
		got, err := cp.GetVerification(writer, &gensurla.GetVerificationPayload{RunID: older})
		require.NoError(t, err, "historical assignment, not current acceptance, grants access")
		assert.False(t, got.InputsCurrent)
		_, err = cp.ListVerifications(writer, &gensurla.ListVerificationsPayload{ChangeID: card.ChangeID})
		require.NoError(t, err)
	}
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, oldCommit)
	check(true, false)
	start() // A rebuild clears acceptance even at the same agreed revision.
	check(false, false)
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, oldCommit)
	check(true, false)
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, newCommit)
	check(false, true)
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, oldCommit)
	check(true, false)

	buildPath := filepath.Join(cp.cards.dir, buildFileName(card.ChangeID))
	before, err := os.ReadFile(buildPath)
	require.NoError(t, err)
	cardBefore, err := os.ReadFile(cp.cards.path(card.ChangeID))
	require.NoError(t, err)
	check(true, false)
	after, err := os.ReadFile(buildPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "reads do not save or consume assignments")
	cardAfter, err := os.ReadFile(cp.cards.path(card.ChangeID))
	require.NoError(t, err)
	assert.Equal(t, cardBefore, cardAfter, "reads do not rewrite the card")
	build := mustStartedBuild(t, cp, user, card.ChangeID)
	assert.False(t, build.Builder.Read)
	assert.False(t, build.TestWriter.Read)

	updated, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, Answers: map[string]string{},
		Examples: append(card.Examples, &gensurla.ExampleItem{
			ID: "check_02", Scenario: "Refund over the limit", InputSummary: "Order total $50.01",
			ExpectedOutcome: "Waits for a manager", VerificationKind: "deterministic",
		}),
	})
	require.NoError(t, err)
	check(false, false)
	editVerificationRequest(t, runsDir, newer, func(req *verifyrun.Request) {
		req.Plan.RequestRevision, req.Plan.SuiteCommit = updated.RequestRevision, oldCommit
	})
	check(false, false) // Matching run revision cannot make the old build in force.
}

// TestVerificationCurrentInputsUseFullStoredTuple verifies evaluator identity
// even without a build. Each reopened server has one immutable selection.
func TestVerificationCurrentInputsUseFullStoredTuple(t *testing.T) {
	cardsDir, runsDir := t.TempDir(), t.TempDir()
	plan := verify.Plan{SuiteCommit: strings.Repeat("2", 40)}
	opts := func(selected verify.Plan) Options {
		return Options{CardsDir: cardsDir, Verification: &VerificationConfig{RunsDir: runsDir, Plan: selected}}
	}
	cp := newControlPlane(t, opts(plan))
	card := confirmedChange(t, cp, "chg")
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, "alice@example.com", card, runID, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), true)
	assertNoBuildSaved(t, cp, card.ChangeID)
	require.NoError(t, cp.Close())
	for _, tc := range []struct {
		name   string
		change func(*verify.Plan)
		want   bool
	}{
		{name: "same selection", change: func(*verify.Plan) {}, want: true},
		{name: "repository", change: func(p *verify.Plan) { p.SuiteRepo = t.TempDir() }},
		{name: "commit", change: func(p *verify.Plan) { p.SuiteCommit = strings.Repeat("3", 40) }},
		{name: "module", change: func(p *verify.Plan) { p.SuiteModule = "eval" }},
		{name: "command", change: func(p *verify.Plan) { p.SuiteCommand = "./cmd/eval" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selected := plan
			tc.change(&selected)
			reopened := newControlPlane(t, opts(selected))
			user := asCaller("alice@example.com", CallerUser)
			got, err := reopened.GetVerification(user, &gensurla.GetVerificationPayload{RunID: runID})
			require.NoError(t, err)
			assert.Equal(t, tc.want, got.InputsCurrent)
			list, err := reopened.ListVerifications(user, &gensurla.ListVerificationsPayload{ChangeID: card.ChangeID})
			require.NoError(t, err)
			require.Len(t, list.Runs, 1)
			assert.Equal(t, got, list.Runs[0])
			assertNoBuildSaved(t, reopened, card.ChangeID)
			require.NoError(t, reopened.Close())
		})
	}
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	reopened := newControlPlane(t, opts(plan))
	got, err := reopened.GetVerification(asCaller("alice@example.com", CallerUser), &gensurla.GetVerificationPayload{RunID: runID})
	require.NoError(t, err)
	assert.False(t, got.InputsCurrent, "the same commit cannot erase a different test source")
	listed, err := reopened.ListVerifications(asCaller("alice@example.com", CallerUser), &gensurla.ListVerificationsPayload{ChangeID: card.ChangeID})
	require.NoError(t, err)
	require.Len(t, listed.Runs, 1)
	assert.False(t, listed.Runs[0].InputsCurrent)
}

// TestVerificationCurrentInputsSafeFailures keeps run authorization ahead of
// current-record reads and treats unreadable owned inputs as unknown errors.
func TestVerificationCurrentInputsSafeFailures(t *testing.T) {
	for _, damaged := range []string{"card", "build", "unreadable card", "unreadable build"} {
		t.Run(damaged, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
			cfg := verificationConfig(t)
			cp := newBuildControlPlane(t, Options{
				RunsDir: cfg.RunsDir, TracerProvider: provider,
				WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Timeout: time.Minute},
			})
			card := confirmedChange(t, cp, "chg")
			runID := strings.Repeat("1", 32)
			writeListedRun(t, cfg.RunsDir, "alice@example.com", card, runID, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), true)
			target := cp.cards.path(card.ChangeID)
			if strings.HasSuffix(damaged, "build") {
				target = filepath.Join(cp.cards.dir, buildFileName(card.ChangeID))
			}
			if strings.HasPrefix(damaged, "unreadable") {
				if strings.HasSuffix(damaged, "card") {
					require.NoError(t, os.Remove(target))
				}
				require.NoError(t, os.Mkdir(target, 0o700))
			} else {
				require.NoError(t, os.WriteFile(target, []byte(`{"private_path":"/private/secret"`), 0o600))
			}
			user := asCaller("alice@example.com", CallerUser)
			_, err := cp.GetVerification(user, &gensurla.GetVerificationPayload{RunID: runID})
			require.Error(t, err)
			assert.Equal(t, `read verification "`+runID+`": saved verification inputs could not be read`, err.Error())
			assert.NotContains(t, err.Error(), cp.cards.dir)
			_, err = cp.ListVerifications(user, &gensurla.ListVerificationsPayload{ChangeID: card.ChangeID})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), cp.cards.dir)
			assert.NotContains(t, err.Error(), "private_path")
			writer := asCaller("alice@example.com", CallerTestWriter)
			_, err = cp.GetVerification(writer, &gensurla.GetVerificationPayload{RunID: runID})
			require.Error(t, err)
			assert.Equal(t, `read verification "`+runID+`": saved verification inputs could not be read`, err.Error())
			_, err = cp.ListVerifications(writer, &gensurla.ListVerificationsPayload{ChangeID: card.ChangeID})
			require.Error(t, err)
			assert.NotContains(t, err.Error(), cp.cards.dir)
			assert.NotContains(t, err.Error(), "private_path")
			spans := exporter.GetSpans().Snapshots()
			for _, name := range []string{"surla.controlplane.get_verification", "surla.controlplane.list_verifications"} {
				found := false
				for _, span := range spans {
					if span.Name() == name {
						found = true
						assert.Equal(t, codes.Error, span.Status().Code)
						assert.NotEmpty(t, span.Events())
					}
				}
				assert.True(t, found)
			}
			// Foreign access must be refused before reading the damaged card or
			// a malformed result from an otherwise attributable run request.
			require.NoError(t, os.WriteFile(filepath.Join(cfg.RunsDir, runID, "result.json"), []byte("{"), 0o600))
			_, err = cp.GetVerification(asCaller("bob@example.com", CallerUser), &gensurla.GetVerificationPayload{RunID: runID})
			assert.ErrorContains(t, err, "is not the owner of run_id")
			assert.NotContains(t, err.Error(), "saved verification")
		})
	}
}

// TestVerificationCurrentInputsIgnoreForeignCard preserves authorized history
// when the current card belongs to another principal; its build is never read.
func TestVerificationCurrentInputsIgnoreForeignCard(t *testing.T) {
	runsDir := t.TempDir()
	cp := newBuildControlPlane(t, Options{
		RunsDir:         runsDir,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Timeout: time.Minute},
	})
	card := confirmedChange(t, cp, "chg")
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, "alice@example.com", card, runID, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), true)
	require.NoError(t, os.Remove(cp.cards.path(card.ChangeID)))
	mustPropose(t, cp, asCaller("bob@example.com", CallerGuide), card.ChangeID,
		"Bob's current change.", refundAnalysis("Bob's current change."))
	require.NoError(t, os.WriteFile(filepath.Join(cp.cards.dir, buildFileName(card.ChangeID)), []byte("{"), 0o600))
	got, err := cp.GetVerification(asCaller("alice@example.com", CallerUser), &gensurla.GetVerificationPayload{RunID: runID})
	require.NoError(t, err)
	assert.False(t, got.InputsCurrent)
	_, err = cp.GetVerification(asCaller("alice@example.com", CallerTestWriter), &gensurla.GetVerificationPayload{RunID: runID})
	assertRefusalSays(t, err, "forbidden", "is not the owner of change_id")
}

// TestVerifyChangeReadsCurrentInputsBeforeAcceptance damages only the build:
// stored-evaluator plan selection needs no build, but the observation must fail
// before the supervisor can accept and publish a run identifier.
func TestVerifyChangeReadsCurrentInputsBeforeAcceptance(t *testing.T) {
	cp, card, cfg := verificationSetup(t)
	require.NoError(t, os.WriteFile(filepath.Join(cp.cards.dir, buildFileName(card.ChangeID)), []byte("{"), 0o600))
	_, err := cp.VerifyChange(asCaller("alice@example.com", CallerUser), &gensurla.VerifyChangePayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, CandidateCommit: strings.Repeat("a", 40),
	})
	require.Error(t, err)
	assert.Equal(t, `start verification for "chg": saved verification inputs could not be read`, err.Error())
	assert.NotContains(t, err.Error(), cp.cards.dir)
	runs, err := verifyrun.List(cfg.RunsDir, "alice@example.com", card.ChangeID)
	require.NoError(t, err)
	assert.Empty(t, runs, "no accepted run is hidden behind an observation failure")
}

// editVerificationRequest modifies a fixture's stored request before its reads.
func editVerificationRequest(t *testing.T, runsDir, runID string, edit func(*verifyrun.Request)) {
	t.Helper()
	file := filepath.Join(runsDir, runID, "request.json")
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	var req verifyrun.Request
	require.NoError(t, json.Unmarshal(raw, &req))
	edit(&req)
	raw, err = json.Marshal(req)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, raw, 0o600))
}
