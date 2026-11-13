// This file checks that the newest run stays historical evidence while its
// input match follows the agreed revision and the tests selected today.
package controlplane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// TestListChangesCurrentInputsFollowAcceptedWrittenTests exercises real build
// starts and saved-state reads with simulated preparation and checker verdicts.
// Neither candidate code nor a model runs; saveListedTestState supplies the
// checker's durable result, rather than testing how that result is obtained.
func TestListChangesCurrentInputsFollowAcceptedWrittenTests(t *testing.T) {
	runsDir := t.TempDir()
	cp := newBuildControlPlane(t, Options{
		RunsDir:         runsDir,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Timeout: time.Minute},
	})
	user := asCaller("alice@example.com", CallerUser)
	card := confirmedChange(t, cp, "chg")
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, "alice@example.com", card, runID,
		time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	listed := func(want bool) *gensurla.ChangeList {
		t.Helper()
		list, err := cp.ListChanges(user)
		require.NoError(t, err)
		require.Len(t, list.Changes, 1)
		require.NotNil(t, list.Changes[0].LatestVerification)
		assert.Equal(t, runID, list.Changes[0].LatestVerification.RunID)
		assert.Equal(t, "passed", list.Changes[0].LatestVerification.State)
		assert.Equal(t, want, list.Changes[0].LatestVerification.InputsCurrent)
		return list
	}
	listed(false) // No build has selected accepted written tests.
	start := func() {
		t.Helper()
		_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{
			ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
		})
		require.NoError(t, err)
	}
	start()
	listed(false)
	commit := strings.Repeat("2", 40)
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, commit)
	accepted := listed(true)
	assert.Equal(t, accepted.Version, listed(true).Version, "unchanged inputs keep the version")

	// Starting again at the same revision clears the selected tests. Acceptance
	// of the same recorded commit can restore the match: runs have no build ID.
	start()
	reset := listed(false)
	assert.NotEqual(t, accepted.Version, reset.Version)
	assert.Equal(t, accepted.Changes[0].SavedAt, reset.Changes[0].SavedAt)
	assert.Equal(t, accepted.Changes[0].RequestRevision, reset.Changes[0].RequestRevision)
	for _, state := range []string{testsReported, testsNeedChanges, testsCouldNotRun} {
		t.Run(state, func(t *testing.T) {
			saveListedTestState(t, cp, card.ChangeID, state, "")
			listed(false)
		})
	}
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, commit)
	listed(true)
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, strings.Repeat("3", 40))
	listed(false) // Same revision, but accepted tests T2 replace recorded T1.
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, commit)
	setListedRunSource(t, runsDir, runID, verify.TestsEvaluator)
	listed(false) // A matching commit cannot erase the test-source distinction.
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	listed(true)

	// The list must neither consume assignments nor rewrite the saved build.
	buildPath := filepath.Join(cp.cards.dir, buildFileName(card.ChangeID))
	before, err := os.ReadFile(buildPath)
	require.NoError(t, err)
	listed(true)
	after, err := os.ReadFile(buildPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	build := mustStartedBuild(t, cp, user, card.ChangeID)
	assert.False(t, build.Builder.Read)
	assert.False(t, build.TestWriter.Read)

	// A formerly usable builder-only build supplies no written tests after a
	// switch away from a stored evaluator, even if its saved verdict matches.
	cp.mu.Lock()
	saved, found, err := cp.cards.loadBuild(card.ChangeID)
	require.NoError(t, err)
	require.True(t, found)
	saved.Roles, saved.TestWriter.WorkingFolder = buildRolesBuilderOnly, nil
	require.NoError(t, cp.cards.saveBuild(saved))
	cp.mu.Unlock()
	listed(false)

	// A usable build of an earlier agreed revision also cannot select tests
	// for the new revision, even when both the run and its commit match it.
	start()
	saveListedTestState(t, cp, card.ChangeID, testsAccepted, commit)
	listed(true)
	updated, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, Answers: map[string]string{},
		Examples: append(card.Examples, &gensurla.ExampleItem{
			ID: "check_02", Scenario: "Refund over the limit", InputSummary: "Order total $50.01",
			ExpectedOutcome: "Waits for a manager", VerificationKind: "deterministic",
		}),
	})
	require.NoError(t, err)
	list := listed(false)
	assert.Equal(t, updated.RequestRevision, list.Changes[0].RequestRevision)
	runID = strings.Repeat("4", 32)
	writeListedRun(t, runsDir, "alice@example.com", updated, runID,
		time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC), true)
	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	listed(false) // The run now names the new revision, but the build does not.
}

// TestListChangesKeepsEarlierRevisionRuns checks both the card-wide newest
// selection and history after a newly agreed revision replaces the run's inputs.
func TestListChangesKeepsEarlierRevisionRuns(t *testing.T) {
	runsDir := t.TempDir()
	plan := verify.Plan{SuiteCommit: strings.Repeat("2", 40)}
	cp := newControlPlane(t, Options{Verification: &VerificationConfig{RunsDir: runsDir, Plan: plan}})
	user := asCaller("alice@example.com", CallerUser)
	first := confirmedChange(t, cp, "chg")
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, "alice@example.com", first, runID,
		time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), true)
	initial, err := cp.ListChanges(user)
	require.NoError(t, err)
	require.Len(t, initial.Changes, 1)
	require.NotNil(t, initial.Changes[0].LatestVerification)
	assert.True(t, initial.Changes[0].LatestVerification.InputsCurrent)

	second, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
		ChangeID: first.ChangeID, RequestRevision: first.RequestRevision, Answers: map[string]string{},
		Examples: append(first.Examples, &gensurla.ExampleItem{
			ID: "check_02", Scenario: "Refund over the limit", InputSummary: "Order total $50.01",
			ExpectedOutcome: "Waits for a manager", VerificationKind: "deterministic",
		}),
	})
	require.NoError(t, err)
	require.NotEqual(t, first.RequestRevision, second.RequestRevision)
	list, err := cp.ListChanges(user)
	require.NoError(t, err)
	assert.Equal(t, second.RequestRevision, list.Changes[0].RequestRevision)
	assert.Equal(t, runID, list.Changes[0].LatestVerification.RunID)
	assert.Equal(t, "passed", list.Changes[0].LatestVerification.State)
	assert.False(t, list.Changes[0].LatestVerification.InputsCurrent)

	// An older acceptance for the current revision never replaces the newest
	// card-wide run just because its inputs match.
	olderID := strings.Repeat("2", 32)
	writeListedRun(t, runsDir, "alice@example.com", second, olderID,
		time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC), true)
	list, err = cp.ListChanges(user)
	require.NoError(t, err)
	assert.Equal(t, runID, list.Changes[0].LatestVerification.RunID)
	assert.False(t, list.Changes[0].LatestVerification.InputsCurrent)
	history, err := cp.ListVerifications(user, &gensurla.ListVerificationsPayload{ChangeID: first.ChangeID})
	require.NoError(t, err)
	require.Len(t, history.Runs, 2)
	assert.Equal(t, runID, history.Runs[0].RunID)
	assert.Equal(t, first.RequestRevision, history.Runs[0].RequestRevision)
	assert.Equal(t, olderID, history.Runs[1].RunID)
}

// TestListChangesCurrentInputsFollowStoredSuite reopens the same records under
// each immutable server configuration. No build is needed for stored tests;
// changing only the selected suite changes the fact and the list version.
func TestListChangesCurrentInputsFollowStoredSuite(t *testing.T) {
	cardsDir, runsDir := t.TempDir(), t.TempDir()
	plan := verify.Plan{SuiteCommit: strings.Repeat("2", 40)}
	options := func(selected verify.Plan) Options {
		return Options{CardsDir: cardsDir, Verification: &VerificationConfig{RunsDir: runsDir, Plan: selected}}
	}
	cp := newControlPlane(t, options(plan))
	card := confirmedChange(t, cp, "chg")
	runID := strings.Repeat("1", 32)
	writeListedRun(t, runsDir, "alice@example.com", card, runID,
		time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), true)
	user := asCaller("alice@example.com", CallerUser)
	baseline, err := cp.ListChanges(user)
	require.NoError(t, err)
	require.Len(t, baseline.Changes, 1)
	require.NotNil(t, baseline.Changes[0].LatestVerification)
	assert.True(t, baseline.Changes[0].LatestVerification.InputsCurrent)
	assertNoBuildSaved(t, cp, card.ChangeID)
	require.NoError(t, cp.Close(), "release the card store before reopening it")
	cases := []struct {
		name   string
		change func(*verify.Plan)
	}{
		{name: "repository", change: func(p *verify.Plan) { p.SuiteRepo = t.TempDir() }},
		{name: "commit", change: func(p *verify.Plan) { p.SuiteCommit = strings.Repeat("3", 40) }},
		{name: "module", change: func(p *verify.Plan) { p.SuiteModule = "eval" }},
		{name: "command", change: func(p *verify.Plan) { p.SuiteCommand = "./cmd/other-evals" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selected := plan
			tc.change(&selected)
			reopened := newControlPlane(t, options(selected))
			list, err := reopened.ListChanges(user)
			require.NoError(t, err)
			require.Len(t, list.Changes, 1)
			assert.False(t, list.Changes[0].LatestVerification.InputsCurrent)
			assert.Equal(t, runID, list.Changes[0].LatestVerification.RunID)
			assert.Equal(t, baseline.Changes[0].SavedAt, list.Changes[0].SavedAt)
			assert.Equal(t, baseline.Changes[0].RequestRevision, list.Changes[0].RequestRevision)
			assert.NotEqual(t, baseline.Version, list.Version, "the changed fact alone moves the version")
			again, err := reopened.ListChanges(user)
			require.NoError(t, err)
			assert.Equal(t, list.Version, again.Version)
			require.NoError(t, reopened.Close())
		})
	}
	unchanged := newControlPlane(t, options(plan))
	list, err := unchanged.ListChanges(user)
	require.NoError(t, err)
	assert.True(t, list.Changes[0].LatestVerification.InputsCurrent)
	assert.Equal(t, baseline.Version, list.Version)

	setListedRunSource(t, runsDir, runID, verify.TestsWritten)
	list, err = unchanged.ListChanges(user)
	require.NoError(t, err)
	assert.False(t, list.Changes[0].LatestVerification.InputsCurrent)
	setListedRunSource(t, runsDir, runID, verify.TestsEvaluator)
	require.NoError(t, unchanged.Close())
	withoutVerifier := newControlPlane(t, Options{CardsDir: cardsDir, RunsDir: runsDir})
	list, err = withoutVerifier.ListChanges(user)
	require.NoError(t, err)
	assert.False(t, list.Changes[0].LatestVerification.InputsCurrent)
	require.NoError(t, withoutVerifier.Close())

	written := newBuildControlPlane(t, Options{
		CardsDir: cardsDir, RunsDir: runsDir,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Timeout: time.Minute},
	})
	list, err = written.ListChanges(user)
	require.NoError(t, err)
	assert.False(t, list.Changes[0].LatestVerification.InputsCurrent, "switching source does not reuse the old evaluator verdict")
}

// TestListChangesUnconfirmedInputsAreNotCurrent checks that matching a
// proposed revision cannot establish a match before the user agrees it.
func TestListChangesUnconfirmedInputsAreNotCurrent(t *testing.T) {
	runsDir := t.TempDir()
	cp := newControlPlane(t, Options{Verification: &VerificationConfig{
		RunsDir: runsDir, Plan: verify.Plan{SuiteCommit: strings.Repeat("2", 40)},
	}})
	guide := asCaller("alice@example.com", CallerGuide)
	card := mustPropose(t, cp, guide, "chg", "Refunds up to $50 are instant.",
		refundAnalysis("Refunds up to $50 are instant."))
	writeListedRun(t, runsDir, "alice@example.com", card, strings.Repeat("1", 32),
		time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC), true)
	list, err := cp.ListChanges(guide)
	require.NoError(t, err)
	require.Len(t, list.Changes, 1)
	require.NotNil(t, list.Changes[0].LatestVerification)
	assert.False(t, list.Changes[0].LatestVerification.InputsCurrent)
}

// saveListedTestState simulates the report checker's saved result for a build
// started through StartBuild. It preserves the independently owned assignment
// and build fields; no candidate compilation or test evaluation runs.
func saveListedTestState(t *testing.T, cp *ControlPlane, changeID, state, commit string) {
	t.Helper()
	cp.mu.Lock()
	defer cp.mu.Unlock()
	saved, found, err := cp.cards.loadBuild(changeID)
	require.NoError(t, err)
	require.True(t, found)
	saved.ReportNumber = 1
	saved.TestWriter = testWriterAssignment{
		State: state, Commit: commit, WorkingFolder: saved.TestWriter.WorkingFolder,
	}
	switch state {
	case testsNeedChanges:
		saved.TestWriter.Findings = []string{"The required test is missing."}
	case testsCouldNotRun:
		saved.TestWriter.Reason, saved.TestWriter.Step = "The check could not start.", "Build again."
	}
	if state != testsReported {
		saved.TestWriter.CheckedAt = time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	}
	require.NoError(t, cp.cards.saveBuild(saved))
}

// setListedRunSource changes only the fixture request's test source. The
// supervisor and report writers are simulated by writeListedRun; the real
// verifyrun readers still validate these saved requests and reports.
func setListedRunSource(t *testing.T, runsDir, runID string, source verify.TestsSource) {
	t.Helper()
	file := filepath.Join(runsDir, runID, "request.json")
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	var request verifyrun.Request
	require.NoError(t, json.Unmarshal(raw, &request))
	request.Plan.Tests = source
	raw, err = json.Marshal(request)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, raw, 0o600))
}
