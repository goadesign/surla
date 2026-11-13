// This file tests builds: who may start one and at which revision, what the
// builder's and the test writer's assignments carry and record, the test
// writer's report, and the strict decoding of saved builds.
package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	goa "goa.design/goa/v3/pkg"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
)

// recordingTeam is a BuildTeam stand-in that records each build it prepares
// and returns folder, or err when set.
type recordingTeam struct {
	// folder is the working folder Prepare returns.
	folder string
	// err, when set, is returned by Prepare instead of folder.
	err error
	// during, when set, runs inside Prepare, as another caller's change to
	// the card could while the roles are being prepared.
	during func()
	// prepared lists the builds Prepare received, in call order.
	prepared []Build
}

// Prepare records build, runs during, and returns the configured folder or
// error.
func (r *recordingTeam) Prepare(_ context.Context, build Build) (string, error) {
	r.prepared = append(r.prepared, build)
	if r.during != nil {
		r.during()
	}
	if r.err != nil {
		return "", r.err
	}
	return r.folder, nil
}

// TestStartBuildOnlyAtTheConfirmedRevisionInForce refuses an unknown change,
// another owner's change, an unconfirmed change and a revision that is not in
// force, each without saving a build, then saves the build at the confirmed
// revision with unread assignments.
func TestStartBuildOnlyAtTheConfirmedRevisionInForce(t *testing.T) {
	cp := newControlPlane(t, Options{})
	guide := asCaller("alice@example.com", CallerGuide)
	user := asCaller("alice@example.com", CallerUser)
	proposed := mustPropose(t, cp, guide, "chg", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	cases := []struct {
		name     string
		ctx      context.Context
		changeID string
		revision string
		refusal  string
	}{
		{name: "unknown change", ctx: user, changeID: "missing", revision: proposed.RequestRevision, refusal: "not_found"},
		{name: "not confirmed", ctx: user, changeID: "chg", revision: proposed.RequestRevision, refusal: "conflict"},
		{name: "not confirmed, no revision", ctx: user, changeID: "chg", revision: "", refusal: "conflict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cp.StartBuild(tc.ctx, &gensurla.StartBuildPayload{ChangeID: tc.changeID, RequestRevision: tc.revision})
			assertRefused(t, err, tc.refusal)
			assertNoBuildSaved(t, cp, tc.changeID)
		})
	}

	confirmed, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	require.NotEqual(t, proposed.RequestRevision, confirmed.RequestRevision, "confirming mints the revision in force")
	refused := []struct {
		name     string
		ctx      context.Context
		revision string
		refusal  string
	}{
		{name: "earlier revision", ctx: user, revision: proposed.RequestRevision, refusal: "conflict"},
		{name: "made-up revision", ctx: user, revision: strings.Repeat("0", 64), refusal: "conflict"},
		{name: "another owner", ctx: asCaller("bob@example.com", CallerUser), revision: confirmed.RequestRevision, refusal: "forbidden"},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := cp.StartBuild(tc.ctx, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: tc.revision})
			assertRefused(t, err, tc.refusal)
			assertNoBuildSaved(t, cp, "chg")
		})
	}

	started, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: confirmed.RequestRevision})
	require.NoError(t, err)
	assert.Equal(t, confirmed.RequestRevision, started.RequestRevision)
	assert.True(t, started.InForce)
	assert.Equal(t, &gensurla.BuilderProgress{Read: false}, started.Builder)
	assert.Equal(t, gensurla.TestsKindAssigned, started.TestWriter.Tests.Kind())
	assert.False(t, started.TestWriter.Read)
	startedAt, err := time.Parse(time.RFC3339Nano, started.StartedAt)
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), startedAt, time.Minute)
	read, err := cp.GetBuild(user, &gensurla.GetBuildPayload{ChangeID: "chg"})
	require.NoError(t, err)
	saved, ok := read.Build.AsStarted()
	require.True(t, ok, "the build read reports the saved build")
	assert.Equal(t, started, saved)
}

// TestBuildingAgainReplacesTheBuildAndClearsReads reads both assignments and
// reports the tests, then checks that the build read says the build is for
// an earlier revision once the user confirms a new one, that the earlier
// revision can no longer be built, and that building the new revision
// replaces the build with unread, newly assigned work. Building again at the
// same revision also starts over.
func TestBuildingAgainReplacesTheBuildAndClearsReads(t *testing.T) {
	cp := newControlPlane(t, Options{})
	user := asCaller("alice@example.com", CallerUser)
	builder := asCaller("alice@example.com", CallerBuilder)
	tests := asCaller("alice@example.com", CallerTestWriter)
	first := confirmedChange(t, cp, "chg")
	_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: first.RequestRevision})
	require.NoError(t, err)

	listed, err := cp.ListBuilderAssignments(builder)
	require.NoError(t, err)
	require.Len(t, listed.Assignments, 1)
	assert.False(t, listed.Assignments[0].Read, "listing records no read")
	full, err := cp.GetBuilderAssignment(builder, &gensurla.GetBuilderAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, &gensurla.BuilderAssignment{
		ChangeID: "chg", RequestRevision: first.RequestRevision, InForce: true, Read: true,
		WrittenTests: gensurla.NewWrittenTestsNotYetAccepted(&gensurla.TestsNotYetAccepted{}),
	}, full)
	build := mustStartedBuild(t, cp, user, "chg")
	assert.True(t, build.Builder.Read, "reading in full records the builder's read")
	assert.False(t, build.TestWriter.Read, "the builder's read is not the test writer's")
	_, err = cp.GetTestWriterAssignment(tests, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	_, err = cp.ReportTestsFinished(tests, &gensurla.ReportTestsFinishedPayload{ChangeID: "chg", RequestRevision: first.RequestRevision})
	require.NoError(t, err)
	build = mustStartedBuild(t, cp, user, "chg")
	assert.True(t, build.TestWriter.Read)
	assert.Equal(t, gensurla.TestsKindReported, build.TestWriter.Tests.Kind())

	second, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: first.RequestRevision, Answers: map[string]string{},
		Examples: append(first.Examples, &gensurla.ExampleItem{
			ID: "check_02", Scenario: "Refund over the limit", InputSummary: "Order total $50.01",
			ExpectedOutcome: "Waits for a manager", VerificationKind: "deterministic",
		}),
	})
	require.NoError(t, err)
	require.NotEqual(t, first.RequestRevision, second.RequestRevision)
	build = mustStartedBuild(t, cp, user, "chg")
	assert.Equal(t, first.RequestRevision, build.RequestRevision, "nothing changes the build automatically")
	assert.False(t, build.InForce, "the build read says the build is for an earlier revision")
	_, err = cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: first.RequestRevision})
	assertRefused(t, err, "conflict")

	rebuilt, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: second.RequestRevision})
	require.NoError(t, err)
	assert.Equal(t, second.RequestRevision, rebuilt.RequestRevision)
	assert.True(t, rebuilt.InForce)
	assert.False(t, rebuilt.Builder.Read, "a new build clears the builder's read")
	assert.False(t, rebuilt.TestWriter.Read, "a new build clears the test writer's read")
	assert.Equal(t, gensurla.TestsKindAssigned, rebuilt.TestWriter.Tests.Kind(), "a new build assigns the tests again")
	listed, err = cp.ListBuilderAssignments(builder)
	require.NoError(t, err)
	require.Len(t, listed.Assignments, 1, "the build was replaced, not added")
	assert.Equal(t, second.RequestRevision, listed.Assignments[0].RequestRevision)

	_, err = cp.GetBuilderAssignment(builder, &gensurla.GetBuilderAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	again, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: second.RequestRevision})
	require.NoError(t, err)
	assert.False(t, again.Builder.Read, "building the same revision again also starts over")
}

// TestAssignmentsSayWhetherTheirBuildIsInForce reads each role's assignment,
// listed and in full, and the test writer's report result, while the build is
// for the revision in force and after the user confirms another revision, and
// checks that each reports the same in_force as the build read.
func TestAssignmentsSayWhetherTheirBuildIsInForce(t *testing.T) {
	cp := newControlPlane(t, Options{})
	user := asCaller("alice@example.com", CallerUser)
	builder := asCaller("alice@example.com", CallerBuilder)
	tests := asCaller("alice@example.com", CallerTestWriter)
	first := confirmedChange(t, cp, "chg")
	_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: first.RequestRevision})
	require.NoError(t, err)
	reads := []struct {
		name string
		read func() (bool, error)
	}{
		{name: "builder list", read: func() (bool, error) {
			list, err := cp.ListBuilderAssignments(builder)
			if err != nil || len(list.Assignments) != 1 {
				return false, errors.Join(err, errors.New("want one builder assignment"))
			}
			return list.Assignments[0].InForce, nil
		}},
		{name: "builder full read", read: func() (bool, error) {
			full, err := cp.GetBuilderAssignment(builder, &gensurla.GetBuilderAssignmentPayload{ChangeID: "chg"})
			if err != nil {
				return false, err
			}
			return full.InForce, nil
		}},
		{name: "test writer list", read: func() (bool, error) {
			list, err := cp.ListTestWriterAssignments(tests)
			if err != nil || len(list.Assignments) != 1 {
				return false, errors.Join(err, errors.New("want one test writer assignment"))
			}
			return list.Assignments[0].InForce, nil
		}},
		{name: "test writer full read", read: func() (bool, error) {
			full, err := cp.GetTestWriterAssignment(tests, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
			if err != nil {
				return false, err
			}
			return full.InForce, nil
		}},
		{name: "test writer report", read: func() (bool, error) {
			item, err := cp.ReportTestsFinished(tests, &gensurla.ReportTestsFinishedPayload{ChangeID: "chg", RequestRevision: first.RequestRevision})
			if err != nil {
				return false, err
			}
			return item.InForce, nil
		}},
	}
	check := func(want bool) {
		t.Helper()
		assert.Equal(t, want, mustStartedBuild(t, cp, user, "chg").InForce, "the build read")
		for _, r := range reads {
			got, err := r.read()
			if assert.NoError(t, err, r.name) {
				assert.Equal(t, want, got, r.name)
			}
		}
	}
	check(true)
	_, err = cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: first.RequestRevision, Answers: map[string]string{},
		Examples: append(first.Examples, &gensurla.ExampleItem{
			ID: "check_02", Scenario: "Refund over the limit", InputSummary: "Order total $50.01",
			ExpectedOutcome: "Waits for a manager", VerificationKind: "deterministic",
		}),
	})
	require.NoError(t, err)
	check(false)
}

// TestStartBuildChecksTheChangeAgainAfterPreparing changes the card while
// Prepare runs, as another caller could, and checks that start_build then
// refuses with conflict and saves no build, although Prepare ran.
func TestStartBuildChecksTheChangeAgainAfterPreparing(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{name: "revised while preparing", status: StatusNeedsConfirmation},
		{name: "confirmed again while preparing", status: StatusConfirmed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			team := &recordingTeam{folder: filepath.Join(t.TempDir(), ".surla-tests")}
			cp := newControlPlane(t, Options{BuildTeam: team})
			card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit"))
			team.during = func() { saveStoryCard(t, cp, "chg", tc.status, refundStory("Refund over the new limit")) }
			_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			assertRefused(t, err, "conflict")
			assert.Len(t, team.prepared, 1, "the change was buildable when the start began")
			assertNoBuildSaved(t, cp, "chg")
		})
	}
}

// TestTestAssignmentKeepsTheStoryAgreedAtTheBuiltRevision builds a change
// whose Story has one finished and one unfinished flow, revises the change
// to another Story, and checks that the test writer's assignment still
// carries the agreed Story and one required test per finished flow, named as
// the generator names them.
func TestTestAssignmentKeepsTheStoryAgreedAtTheBuiltRevision(t *testing.T) {
	cp := newControlPlane(t, Options{})
	user := asCaller("alice@example.com", CallerUser)
	tests := asCaller("alice@example.com", CallerTestWriter)
	agreed := refundStory("Refund over the limit")
	card := saveStoryCard(t, cp, "chg", StatusConfirmed, agreed)
	_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)

	revised := refundStory("Refund over the new limit")
	revised[0].Flows = append(revised[0].Flows, designeval.StoryFlow{Key: "store-credit", Title: "Store credit", Complete: true})
	saveStoryCard(t, cp, "chg", StatusNeedsConfirmation, revised)

	assignment, err := cp.GetTestWriterAssignment(tests, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, card.RequestRevision, assignment.RequestRevision)
	assert.Equal(t, storySummaryItems(agreed), assignment.Stories, "the Story as agreed at the built revision")
	assert.Equal(t, []*gensurla.RequiredTest{{
		Story: "Refunds", FlowKey: "refund-over-limit", Scenario: "refund_over_limit", Hook: "RefundOverLimit",
	}}, assignment.RequiredTests, "one test per finished flow, named by the generator's rule")
	assert.Empty(t, assignment.WorkingFolder, "no working copy is prepared without a bound BuildTeam")
	assert.True(t, assignment.Progress.Read)
	build := mustStartedBuild(t, cp, user, "chg")
	assert.False(t, build.InForce, "a revision awaiting confirmation leaves the build for an earlier revision")
}

// TestStartBuildRefusesAStoryWhoseTestsCannotBeNamed refuses a Story with two
// finished flows that the generator would give the same test name, and saves
// nothing.
func TestStartBuildRefusesAStoryWhoseTestsCannotBeNamed(t *testing.T) {
	cp := newControlPlane(t, Options{})
	stories := refundStory("Refund over the limit")
	stories[0].Flows = append(stories[0].Flows, designeval.StoryFlow{Key: "refund_over_limit", Title: "Same name", Complete: true})
	card := saveStoryCard(t, cp, "chg", StatusConfirmed, stories)
	_, err := cp.StartBuild(asCaller("alice@example.com", CallerUser), &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	assertRefused(t, err, "unprocessable")
	assert.ErrorContains(t, err, "refund_over_limit")
	assertNoBuildSaved(t, cp, "chg")
}

// TestReportForAnotherRevisionIsRefused records a report at the build's
// revision and refuses one for another revision, or for a change that has no
// build, leaving the saved build byte for byte as it was.
func TestReportForAnotherRevisionIsRefused(t *testing.T) {
	cp := newControlPlane(t, Options{})
	user := asCaller("alice@example.com", CallerUser)
	tests := asCaller("alice@example.com", CallerTestWriter)
	card := confirmedChange(t, cp, "chg")
	confirmedChange(t, cp, "unbuilt")
	_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	before := readBuildFile(t, cp, "chg")
	cases := []struct {
		name     string
		changeID string
		revision string
		refusal  string
	}{
		{name: "another revision", changeID: "chg", revision: strings.Repeat("0", 64), refusal: "conflict"},
		{name: "a change without a build", changeID: "unbuilt", revision: card.RequestRevision, refusal: "not_found"},
		{name: "another owner", changeID: "chg", revision: card.RequestRevision, refusal: "forbidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := tests
			if tc.refusal == "forbidden" {
				ctx = asCaller("bob@example.com", CallerTestWriter)
			}
			_, err := cp.ReportTestsFinished(ctx, &gensurla.ReportTestsFinishedPayload{ChangeID: tc.changeID, RequestRevision: tc.revision})
			assertRefused(t, err, tc.refusal)
			assert.Equal(t, before, readBuildFile(t, cp, "chg"), "a refused report changes nothing")
		})
	}
	reported, err := cp.ReportTestsFinished(tests, &gensurla.ReportTestsFinishedPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	assert.Equal(t, gensurla.TestsKindReported, reported.Progress.Tests.Kind())
	assert.Equal(t, gensurla.TestsKindReported, mustStartedBuild(t, cp, user, "chg").TestWriter.Tests.Kind(), "the report is saved")
}

// TestAssignmentsListOnlyTheCallersOwnChanges lists builds most recently
// started first, and gives another owner's agents none of them.
func TestAssignmentsListOnlyTheCallersOwnChanges(t *testing.T) {
	cp := newControlPlane(t, Options{})
	user := asCaller("alice@example.com", CallerUser)
	older := confirmedChange(t, cp, "older")
	newer := confirmedChange(t, cp, "newer")
	_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "older", RequestRevision: older.RequestRevision})
	require.NoError(t, err)
	time.Sleep(2 * time.Millisecond)
	_, err = cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "newer", RequestRevision: newer.RequestRevision})
	require.NoError(t, err)

	builds, err := cp.ListBuilderAssignments(asCaller("alice@example.com", CallerBuilder))
	require.NoError(t, err)
	require.Len(t, builds.Assignments, 2)
	assert.Equal(t, []string{"newer", "older"}, []string{builds.Assignments[0].ChangeID, builds.Assignments[1].ChangeID})
	tests, err := cp.ListTestWriterAssignments(asCaller("alice@example.com", CallerTestWriter))
	require.NoError(t, err)
	require.Len(t, tests.Assignments, 2)
	assert.Equal(t, []string{"newer", "older"}, []string{tests.Assignments[0].ChangeID, tests.Assignments[1].ChangeID})

	otherBuilder := asCaller("bob@example.com", CallerBuilder)
	otherTests := asCaller("bob@example.com", CallerTestWriter)
	none, err := cp.ListBuilderAssignments(otherBuilder)
	require.NoError(t, err)
	assert.Empty(t, none.Assignments)
	noTests, err := cp.ListTestWriterAssignments(otherTests)
	require.NoError(t, err)
	assert.Empty(t, noTests.Assignments)
	_, err = cp.GetBuilderAssignment(otherBuilder, &gensurla.GetBuilderAssignmentPayload{ChangeID: "older"})
	assertRefused(t, err, "forbidden")
	_, err = cp.GetTestWriterAssignment(otherTests, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "older"})
	assertRefused(t, err, "forbidden")
}

// TestBuildTeamPreparesBeforeTheBuildIsSaved checks the interface a later
// step binds: Prepare receives the change, revision and agreed Story and its
// working folder reaches the test writer; a failing or relative-path Prepare
// refuses the build with nothing saved and is recorded on the span; a refused
// start never calls Prepare.
func TestBuildTeamPreparesBeforeTheBuildIsSaved(t *testing.T) {
	folder := filepath.Join(t.TempDir(), ".surla-tests")
	cases := []struct {
		name   string
		team   *recordingTeam
		failed string
	}{
		{name: "prepared", team: &recordingTeam{folder: folder}},
		{name: "prepare fails", team: &recordingTeam{err: errors.New("the project folder is not a git repository")}, failed: "not a git repository"},
		{name: "relative folder", team: &recordingTeam{folder: ".surla-tests"}, failed: "not an absolute path"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			t.Cleanup(func() { assert.NoError(t, provider.Shutdown(context.Background())) })
			cp := newControlPlane(t, Options{BuildTeam: tc.team, TracerProvider: provider})
			user := asCaller("alice@example.com", CallerUser)
			stories := refundStory("Refund over the limit")
			card := saveStoryCard(t, cp, "chg", StatusConfirmed, stories)
			_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: strings.Repeat("0", 64)})
			assertRefused(t, err, "conflict")
			assert.Empty(t, tc.team.prepared, "a refused start prepares nothing")

			_, err = cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.Len(t, tc.team.prepared, 1)
			assert.Equal(t, Build{ChangeID: "chg", RequestRevision: card.RequestRevision, Stories: stories}, tc.team.prepared[0])
			var statuses []codes.Code
			for _, span := range exporter.GetSpans() {
				if span.Name == "surla.controlplane.start_build" {
					statuses = append(statuses, span.Status.Code)
				}
			}
			if tc.failed != "" {
				assert.ErrorContains(t, err, tc.failed)
				assertNoBuildSaved(t, cp, "chg")
				assert.Equal(t, []codes.Code{codes.Unset, codes.Error}, statuses, "a refusal is not a failure; a failed Prepare is")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, []codes.Code{codes.Unset, codes.Unset}, statuses)
			assignment, err := cp.GetTestWriterAssignment(asCaller("alice@example.com", CallerTestWriter), &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
			require.NoError(t, err)
			assert.Equal(t, folder, assignment.WorkingFolder)
		})
	}
}

// TestBuildFileIsDecodedStrictly damages a saved build in each way the
// decoder must refuse, and checks that states a later check writes, with
// their findings or commit, are read back as saved.
func TestBuildFileIsDecodedStrictly(t *testing.T) {
	commit := strings.Repeat("c", 40)
	at := "2026-11-13T09:30:00Z"
	cases := []struct {
		name    string
		edit    func(map[string]any) map[string]any
		trailer string
		refusal string
		tests   gensurla.TestsKind
	}{
		{name: "unknown field", edit: func(f map[string]any) map[string]any { f["extra"] = true; return f }, refusal: "unknown field"},
		{name: "unknown assignment field", edit: func(f map[string]any) map[string]any {
			f["builder"].(map[string]any)["extra"] = true
			return f
		}, refusal: "unknown field"},
		{name: "trailing data", edit: func(f map[string]any) map[string]any { return f }, trailer: "{}", refusal: "trailing data"},
		{name: "another change's build", edit: func(f map[string]any) map[string]any { f["change_id"] = "other"; return f }, refusal: "does not hold"},
		{name: "builder state", edit: func(f map[string]any) map[string]any {
			f["builder"].(map[string]any)["state"] = "assigned"
			return f
		}, refusal: "unknown field"},
		{name: "unknown test writer state", edit: func(f map[string]any) map[string]any {
			f["test_writer"].(map[string]any)["state"] = "approved"
			return f
		}, refusal: "unknown test writer state"},
		{name: "needs changes without findings", edit: testWriterState("needs_changes", nil, ""), refusal: "does not allow"},
		{name: "assigned with a commit", edit: testWriterState("assigned", nil, commit), refusal: "does not allow"},
		{name: "accepted without a commit", edit: testWriterState("accepted", nil, ""), refusal: "does not allow"},
		{name: "needs changes", edit: savedTestWriter(map[string]any{
			"state": "needs_changes", "findings": []string{"RefundOverLimit is not implemented"}, "checked_at": at,
		}), tests: gensurla.TestsKindNeedsChanges},
		{name: "accepted", edit: savedTestWriter(map[string]any{"state": "accepted", "commit": commit, "checked_at": at}), tests: gensurla.TestsKindAccepted},
		{name: "not used is never saved", edit: savedTestWriter(map[string]any{"state": "not_used"}), refusal: "unknown test writer state"},
		{name: "needs changes with a commit", edit: savedTestWriter(map[string]any{
			"state": "needs_changes", "findings": []string{"RefundOverLimit is not implemented"}, "commit": commit, "checked_at": at,
		}), refusal: "does not allow"},
		{name: "needs changes with a reason", edit: savedTestWriter(map[string]any{
			"state": "needs_changes", "findings": []string{"RefundOverLimit is not implemented"}, "reason": "git failed", "checked_at": at,
		}), refusal: "does not allow"},
		{name: "accepted with findings", edit: savedTestWriter(map[string]any{
			"state": "accepted", "commit": commit, "findings": []string{"RefundOverLimit is not implemented"}, "checked_at": at,
		}), refusal: "does not allow"},
		{name: "accepted with a step", edit: savedTestWriter(map[string]any{
			"state": "accepted", "commit": commit, "step": "Report again.", "checked_at": at,
		}), refusal: "does not allow"},
		{name: "could not run with findings", edit: savedTestWriter(map[string]any{
			"state": "could_not_run", "reason": "git failed", "step": "Report again.", "findings": []string{"RefundOverLimit is not implemented"}, "checked_at": at,
		}), refusal: "does not allow"},
		{name: "could not run with a commit", edit: savedTestWriter(map[string]any{
			"state": "could_not_run", "reason": "git failed", "step": "Report again.", "commit": commit, "checked_at": at,
		}), refusal: "does not allow"},
		{name: "could not run", edit: savedTestWriter(map[string]any{
			"state": "could_not_run", "reason": "git could not write the tests branch", "step": "Free disk space, then report again.", "checked_at": at,
		}), tests: gensurla.TestsKindCouldNotRun},
		{name: "could not run, reason at its limit", edit: savedTestWriter(map[string]any{
			"state": "could_not_run", "reason": strings.Repeat("é", maxCheckReason), "step": strings.Repeat("s", maxCheckStep), "checked_at": at,
		}), tests: gensurla.TestsKindCouldNotRun},
		{name: "could not run, reason over its limit", edit: savedTestWriter(map[string]any{
			"state": "could_not_run", "reason": strings.Repeat("é", maxCheckReason+1), "step": "Report again.", "checked_at": at,
		}), refusal: "does not allow"},
		{name: "could not run, step over its limit", edit: savedTestWriter(map[string]any{
			"state": "could_not_run", "reason": "git failed", "step": strings.Repeat("s", maxCheckStep+1), "checked_at": at,
		}), refusal: "does not allow"},
		{name: "could not run without a step", edit: savedTestWriter(map[string]any{
			"state": "could_not_run", "reason": "git failed", "checked_at": at,
		}), refusal: "does not allow"},
		{name: "needs changes without its time", edit: savedTestWriter(map[string]any{
			"state": "needs_changes", "findings": []string{"RefundOverLimit is not implemented"},
		}), refusal: "verdict time"},
		{name: "accepted without its time", edit: savedTestWriter(map[string]any{"state": "accepted", "commit": commit}), refusal: "verdict time"},
		{name: "could not run without its time", edit: savedTestWriter(map[string]any{
			"state": "could_not_run", "reason": "git failed", "step": "Report again.",
		}), refusal: "verdict time"},
		{name: "assigned with a verdict time", edit: savedTestWriter(map[string]any{"state": "assigned", "checked_at": at}), refusal: "verdict time"},
		{name: "reported with a reason", edit: savedTestWriter(map[string]any{"state": "reported", "reason": "git failed"}), refusal: "does not allow"},
		{name: "counted reports", edit: func(f map[string]any) map[string]any { f["report_number"] = 3; return f }, tests: gensurla.TestsKindAssigned},
		{name: "negative report number", edit: func(f map[string]any) map[string]any { f["report_number"] = -1; return f }, refusal: "negative report number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newControlPlane(t, Options{})
			user := asCaller("alice@example.com", CallerUser)
			card := confirmedChange(t, cp, "chg")
			_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
			require.NoError(t, err)
			var file map[string]any
			require.NoError(t, json.Unmarshal(readBuildFile(t, cp, "chg"), &file))
			raw, err := json.Marshal(tc.edit(file))
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(cp.cards.dir, buildFileName("chg")), append(raw, tc.trailer...), 0o600))

			read, err := cp.GetBuild(user, &gensurla.GetBuildPayload{ChangeID: "chg"})
			if tc.refusal != "" {
				assert.ErrorContains(t, err, tc.refusal)
				return
			}
			require.NoError(t, err)
			build, ok := read.Build.AsStarted()
			require.True(t, ok)
			assert.Equal(t, tc.tests, build.TestWriter.Tests.Kind())
			assignment, err := cp.GetTestWriterAssignment(asCaller("alice@example.com", CallerTestWriter), &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
			require.NoError(t, err)
			if findings, ok := assignment.Progress.Tests.AsNeedsChanges(); ok {
				assert.Equal(t, []string{"RefundOverLimit is not implemented"}, findings.Findings)
			}
			if accepted, ok := assignment.Progress.Tests.AsAccepted(); ok {
				assert.Equal(t, commit, accepted.Commit)
			}
		})
	}
}

// TestBuildOperationsFollowTheRoleTable calls every build operation as each
// caller over HTTP and over MCP, and lists each caller's tools: only the user
// starts builds, every caller with a role reads them, each agent role reaches
// only its own assignments, refused calls change nothing, and a tool is
// listed exactly for the callers the table allows.
func TestBuildOperationsFollowTheRoleTable(t *testing.T) {
	cp := newControlPlane(t, Options{})
	card := confirmedChange(t, cp, "chg")
	tokens := map[string]CallerKind{"user": CallerUser, "guide": CallerGuide, "builder": CallerBuilder, "tests": CallerTestWriter}
	credentials := map[string]Principal{}
	for token, kind := range tokens {
		credentials[token] = Principal{ID: "alice@example.com", CallerID: token, Kind: kind}
	}
	srv := httptest.NewServer(NewHTTPHandler(cp, fixtureCredentials(credentials)))
	t.Cleanup(srv.Close)
	revision := `{"request_revision":"` + card.RequestRevision + `"}`

	notStarted := `{"change_id":"chg","build":{"state":"not_started"}}`
	readStatus, readBody := httpCall(t, srv.URL, "user", http.MethodGet, "/changes/chg/build", "")
	require.Equal(t, http.StatusOK, readStatus, "a never-built change is a value, not an error: "+readBody)
	assert.JSONEq(t, notStarted, readBody)
	failure, result := mcpToolCall(t, srv.URL, "guide", "get_build", `{"change_id":"chg"}`)
	require.Empty(t, failure, "a never-built change is a value, not a tool error")
	assert.JSONEq(t, notStarted, string(result))

	for _, token := range []string{"guide", "builder", "tests"} {
		status, _ := httpCall(t, srv.URL, token, http.MethodPost, "/changes/chg/build", revision)
		assert.Equal(t, http.StatusForbidden, status, token)
	}
	assertNoBuildSaved(t, cp, "chg")
	status, body := httpCall(t, srv.URL, "user", http.MethodPost, "/changes/chg/build", revision)
	require.Equal(t, http.StatusOK, status, body)

	// callers lists the fixture tokens the contract allows, independently of
	// the policy table under test.
	operations := []struct {
		name, method, path, body string
		arguments                string
		callers                  []string
	}{
		{name: "get_build", method: http.MethodGet, path: "/changes/chg/build", arguments: `{"change_id":"chg"}`,
			callers: []string{"user", "guide", "builder", "tests"}},
		{name: "list_builder_assignments", method: http.MethodGet, path: "/assignments/builder", arguments: `{}`, callers: []string{"builder"}},
		{name: "get_builder_assignment", method: http.MethodGet, path: "/assignments/builder/chg", arguments: `{"change_id":"chg"}`, callers: []string{"builder"}},
		{name: "list_test_writer_assignments", method: http.MethodGet, path: "/assignments/test-writer", arguments: `{}`, callers: []string{"tests"}},
		{name: "get_test_writer_assignment", method: http.MethodGet, path: "/assignments/test-writer/chg", arguments: `{"change_id":"chg"}`, callers: []string{"tests"}},
		{name: "report_tests_finished", method: http.MethodPost, path: "/assignments/test-writer/chg/finished", body: revision,
			arguments: `{"change_id":"chg","request_revision":"` + card.RequestRevision + `"}`, callers: []string{"tests"}},
	}
	for _, token := range []string{"user", "guide", "builder", "tests"} {
		t.Run(token, func(t *testing.T) {
			listed := listToolNames(t, srv.URL, token)
			assert.False(t, listed["start_build"], "starting a build is the views page's HTTP operation, never a tool")
			for _, op := range operations {
				allowed := slices.Contains(op.callers, token)
				assert.Equal(t, allowed, listed[op.name], "%s is listed exactly for the callers allowed", op.name)
				before := readBuildFile(t, cp, "chg")
				status, body := httpCall(t, srv.URL, token, op.method, op.path, op.body)
				failure, result := mcpToolCall(t, srv.URL, token, op.name, op.arguments)
				if !allowed {
					assert.Equal(t, http.StatusForbidden, status, op.name)
					assert.Contains(t, body, "may not call "+op.name)
					assert.Contains(t, failure, "may not call "+op.name)
					assert.Equal(t, before, readBuildFile(t, cp, "chg"), "a refused %s changes nothing", op.name)
					continue
				}
				assert.Equal(t, http.StatusOK, status, op.name+": "+body)
				assert.Empty(t, failure, op.name)
				assert.Contains(t, string(result), `"chg"`, op.name)
			}
		})
	}
	assert.False(t, OperationAllowed(CallerManagement, "start_build"))
	build := mustStartedBuild(t, cp, asCaller("alice@example.com", CallerUser), "chg")
	assert.True(t, build.Builder.Read, "the builder read its assignment over both interfaces")
	assert.True(t, build.TestWriter.Read)
	assert.Equal(t, gensurla.TestsKindReported, build.TestWriter.Tests.Kind())
}

// asCaller returns a context for owner's caller of kind.
func asCaller(owner string, kind CallerKind) context.Context {
	return WithPrincipal(context.Background(), Principal{ID: owner, CallerID: string(kind), Kind: kind})
}

// confirmedChange proposes changeID as alice's guide, confirms it as alice's
// user session, and returns the confirmed card.
func confirmedChange(t *testing.T, cp *ControlPlane, changeID string) *gensurla.ChangeCardResult {
	t.Helper()
	proposed := mustPropose(t, cp, asCaller("alice@example.com", CallerGuide), changeID, "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	confirmed, err := cp.ConfirmIntent(asCaller("alice@example.com", CallerUser), &gensurla.ConfirmIntentPayload{
		ChangeID: changeID, RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	return confirmed
}

// saveStoryCard saves alice's card changeID with status and stories, as
// propose_change, revise_change or confirm_intent would leave it, and returns
// it.
func saveStoryCard(t *testing.T, cp *ControlPlane, changeID, status string, stories []designeval.StorySummary) *gensurla.ChangeCardResult {
	t.Helper()
	card := &gensurla.ChangeCardResult{
		ChangeID: changeID, Status: status, Prompt: "Refunds up to $50 are instant.",
		Summary: "Refunds up to $50 are instant.", Ambiguities: []*gensurla.AmbiguityQuestion{},
		Answers: map[string]string{}, PreservedBehaviors: []string{}, Stories: storySummaryItems(stories),
		Examples: []*gensurla.ExampleItem{{
			ID: "check_01", Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
			ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
		}},
	}
	if status == StatusConfirmed {
		confirmedBy := "alice@example.com"
		card.ConfirmedBy = &confirmedBy
	}
	rev, err := computeCanonicalRevision(card, "alice@example.com", "")
	require.NoError(t, err)
	card.RequestRevision = rev
	cp.mu.Lock()
	defer cp.mu.Unlock()
	require.NoError(t, cp.cards.save(storedChangeCard{card: card, owner: "alice@example.com", stories: stories}))
	return card
}

// refundStory returns a Story with one finished flow titled title and one
// unfinished flow with an open question.
func refundStory(title string) []designeval.StorySummary {
	return []designeval.StorySummary{{
		Name:   "Refunds",
		Actors: []designeval.StoryActor{{Key: "customer", Title: "Customer"}},
		Flows: []designeval.StoryFlow{
			{Key: "refund-over-limit", Title: title, Complete: true, Steps: []designeval.StoryStep{
				{Key: "ask", ActorKey: "customer", Summary: "Customer asks for a $60 refund", Kind: "start"},
				{Key: "wait", ActorKey: "customer", Summary: "The refund waits for a manager", Kind: "outcome"},
			}},
			{Key: "disputed-chargeback", Title: "Disputed chargeback", Questions: []designeval.StoryQuestion{
				{Key: "who-decides", Prompt: "Who decides a disputed chargeback?", Options: []string{"Manager", "Bank"}},
			}},
		},
	}}
}

// testWriterState returns an edit that sets the saved test writer's state,
// findings and commit.
func testWriterState(state string, findings []string, commit string) func(map[string]any) map[string]any {
	return func(f map[string]any) map[string]any {
		tw := f["test_writer"].(map[string]any)
		tw["state"] = state
		tw["findings"] = findings
		tw["commit"] = commit
		return f
	}
}

// savedTestWriter returns an edit that sets each of fields on the saved test
// writer, leaving its other fields as saved.
func savedTestWriter(fields map[string]any) func(map[string]any) map[string]any {
	return func(f map[string]any) map[string]any {
		tw := f["test_writer"].(map[string]any)
		for key, value := range fields {
			tw[key] = value
		}
		return f
	}
}

// mustStartedBuild reads changeID's build as ctx and fails the test unless
// one was started.
func mustStartedBuild(t *testing.T, cp *ControlPlane, ctx context.Context, changeID string) *gensurla.StartedBuild {
	t.Helper()
	read, err := cp.GetBuild(ctx, &gensurla.GetBuildPayload{ChangeID: changeID})
	require.NoError(t, err)
	build, ok := read.Build.AsStarted()
	require.True(t, ok, "change %q has a build", changeID)
	return build
}

// assertRefused checks that err is the design's refusal named name.
func assertRefused(t *testing.T, err error, name string) {
	t.Helper()
	var refusal *goa.ServiceError
	if assert.ErrorAs(t, err, &refusal) {
		assert.Equal(t, name, refusal.Name)
	}
}

// assertNoBuildSaved checks that changeID has no build file.
func assertNoBuildSaved(t *testing.T, cp *ControlPlane, changeID string) {
	t.Helper()
	_, err := os.Stat(filepath.Join(cp.cards.dir, buildFileName(changeID)))
	assert.ErrorIs(t, err, os.ErrNotExist, "no build of %q is saved", changeID)
}

// readBuildFile returns the saved bytes of changeID's build.
func readBuildFile(t *testing.T, cp *ControlPlane, changeID string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cp.cards.dir, buildFileName(changeID)))
	require.NoError(t, err)
	return raw
}

// httpCall sends method path with body as token's caller and returns the
// status and body.
func httpCall(t *testing.T, base, token, method, path, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp.StatusCode, string(raw)
}

// listToolNames returns the names of the MCP tools token's caller is shown.
func listToolNames(t *testing.T, base, token string) map[string]bool {
	t.Helper()
	raw := mcpRequest(t, base, token, "tools/list", "", `{}`)
	var result struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	names := map[string]bool{}
	for _, tool := range result.Tools {
		names[tool.Name] = true
	}
	return names
}

// mcpToolCall calls tool with arguments as token's caller and returns the
// tool error text, empty when the call succeeded, and its structured result.
func mcpToolCall(t *testing.T, base, token, tool, arguments string) (string, json.RawMessage) {
	t.Helper()
	raw := mcpRequest(t, base, token, "tools/call", tool, `{"name":"`+tool+`","arguments":`+arguments+`}`)
	var result struct {
		IsError           bool            `json:"isError"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		Content           []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal(raw, &result))
	if !result.IsError {
		return "", result.StructuredContent
	}
	var text []string
	for _, c := range result.Content {
		text = append(text, c.Text)
	}
	return strings.Join(text, "\n"), nil
}

// mcpRequest sends one JSON-RPC MCP request with params as token's caller and
// returns its result, reading either a JSON or an event-stream answer.
func mcpRequest(t *testing.T, base, token, method, name, params string) json.RawMessage {
	t.Helper()
	var withMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(params), &withMeta))
	withMeta["_meta"] = map[string]any{
		"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
		"io.modelcontextprotocol/clientCapabilities": map[string]any{},
	}
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": withMeta})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/mcp", strings.NewReader(string(body)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", method)
	if name != "" {
		req.Header.Set("Mcp-Name", name)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { require.NoError(t, resp.Body.Close()) }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	payload, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		scanner := bufio.NewScanner(strings.NewReader(string(payload)))
		scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
		for scanner.Scan() {
			if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
				payload = []byte(data)
			}
		}
	}
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	require.NoError(t, json.Unmarshal(payload, &envelope))
	require.Empty(t, envelope.Error, string(payload))
	return envelope.Result
}
