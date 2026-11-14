// This file checks that a build carries the agreed revision's confirmed
// answers, examples and preserved behaviors to the test writer, who may not
// read the change itself.
package controlplane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestTestWriterAssignmentHoldsTheAgreedTerms confirms a change with two
// answered questions, an edited and an added example and two preserved
// behaviors, builds it, and reads the test writer's assignment as each later
// step happens: a revision the guide proposes beside the agreed one, then the
// user adopting it. The assignment keeps returning exactly what the user
// confirmed for the built revision.
func TestTestWriterAssignmentHoldsTheAgreedTerms(t *testing.T) {
	cp := newBuildControlPlane(t, Options{})
	agreed := confirmAgreedTerms(t, cp, "chg")
	_, err := cp.StartBuild(agreedUser, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: agreed.RequestRevision})
	require.NoError(t, err)
	wantAnswers := []*gensurla.ConfirmedAnswer{
		{ID: "chg-ambiguity-01", Question: "Does the limit include tax?", Answer: "without_tax"},
		{ID: "chg-ambiguity-02", Question: "Where does an instant refund go?", Answer: "original_payment"},
	}
	wantExamples := []*gensurla.ExampleItem{
		{
			ID: agreed.Examples[0].ID, Scenario: "Refund at the limit", InputSummary: "Order total $50.00 before tax",
			ExpectedOutcome: "Instant refund to the original card", VerificationKind: "deterministic",
		},
		{
			ID: "refund_over_limit", Scenario: "Refund just over the limit", InputSummary: "Order total $50.01 before tax",
			ExpectedOutcome: "The refund waits for a manager", VerificationKind: "deterministic",
		},
	}
	wantPreserved := []string{"Cross-tenant order lookups return 404 Not Found.", "Refunds of gift cards stay manual."}
	require.Equal(t, wantExamples, agreed.Examples, "the user confirmed the edited and the added example")
	require.Equal(t, wantPreserved, agreed.PreservedBehaviors)

	var proposal *gensurla.ProposedRevisionItem
	steps := []struct {
		name    string
		step    func(t *testing.T)
		inForce bool
	}{
		{name: "build started", step: func(*testing.T) {}, inForce: true},
		{name: "later revision proposed beside the agreed one", step: func(t *testing.T) {
			payload := revisionPayload("chg", agreed.RequestRevision, "Refunds up to $60 are instant.", agreed.Examples[0].ID)
			payload.Analysis.RemovedExampleIds = []string{"refund_over_limit"}
			revised, err := cp.ReviseChange(agreedGuide, payload)
			require.NoError(t, err)
			proposal = revised.ProposedRevision
			require.NotNil(t, proposal)
			require.NotEqual(t, agreed.Examples, proposal.Examples, "the proposal's examples differ from the agreed ones")
			require.NotEqual(t, agreed.PreservedBehaviors, proposal.PreservedBehaviors, "so do its preserved behaviors")
		}, inForce: true},
		{name: "later revision adopted", step: func(t *testing.T) {
			adoptProposal(t, cp, "chg", proposal)
		}, inForce: false},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			s.step(t)
			assignment, err := cp.GetTestWriterAssignment(agreedTestWriter, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
			require.NoError(t, err)
			assert.Equal(t, agreed.RequestRevision, assignment.RequestRevision)
			assert.Equal(t, s.inForce, assignment.InForce)
			assert.Equal(t, wantAnswers, assignment.Answers)
			assert.Equal(t, wantExamples, assignment.Examples)
			assert.Equal(t, wantPreserved, assignment.PreservedBehaviors)
		})
	}
}

// TestBuildFileKeepsTheAgreedTerms saves and loads builds with and without
// questions and preserved behaviors: the loaded build equals the saved one,
// and empty lists stay empty rather than absent.
func TestBuildFileKeepsTheAgreedTerms(t *testing.T) {
	folder := t.TempDir()
	example := &gensurla.ExampleItem{
		ID: "check_01", Scenario: "Refund at the limit", InputSummary: "Order total $50.00 before tax",
		ExpectedOutcome: "Instant refund to the original card", VerificationKind: "deterministic",
	}
	cases := []struct {
		name      string
		answers   []*gensurla.ConfirmedAnswer
		preserved []string
	}{
		{
			name:      "answered questions and preserved behaviors",
			answers:   []*gensurla.ConfirmedAnswer{{ID: "chg-ambiguity-01", Question: "Does the limit include tax?", Answer: "without_tax"}},
			preserved: []string{"Cross-tenant order lookups return 404 Not Found."},
		},
		{name: "no questions and no preserved behaviors", answers: []*gensurla.ConfirmedAnswer{}, preserved: []string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newControlPlane(t, Options{})
			saved := buildFile{
				ChangeID: "chg", RequestRevision: "revision", StartedAt: time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC),
				Roles: buildRolesBuilderAndTestWriter, Stories: refundStory("Refund over the limit"),
				DesignSource: `{"design/design.go":"package design\n"}`,
				Answers:      tc.answers, Examples: []*gensurla.ExampleItem{example}, PreservedBehaviors: tc.preserved,
				TestWriter: testWriterAssignment{State: testsAssigned, WorkingFolder: &folder, Findings: []string{}},
			}
			require.NoError(t, cp.cards.saveBuild(saved))
			loaded, found, err := cp.cards.loadBuild("chg")
			require.NoError(t, err)
			require.True(t, found)
			assert.Equal(t, saved, loaded)
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(readBuildFile(t, cp, "chg"), &fields))
			for _, key := range []string{"answers", "examples", "preserved_behaviors"} {
				assert.NotEqual(t, "null", string(fields[key]), key)
			}
		})
	}
}

// TestOnlyTheTestWriterReadsTheAgreedTerms calls get_test_writer_assignment
// through the operation policy as every caller kind after a build of a change
// with confirmed terms: only the test writer reads them, and every refused
// read leaves the saved build as it was.
func TestOnlyTheTestWriterReadsTheAgreedTerms(t *testing.T) {
	cp := newBuildControlPlane(t, Options{})
	agreed := confirmAgreedTerms(t, cp, "chg")
	_, err := cp.StartBuild(agreedUser, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: agreed.RequestRevision})
	require.NoError(t, err)
	endpoints := gensurla.NewEndpoints(cp)
	applyOperationPolicy(endpoints, cp.tracer("test"))
	for _, kind := range []CallerKind{CallerUser, CallerGuide, CallerBuilder, CallerManagement, CallerTestWriter} {
		t.Run(string(kind), func(t *testing.T) {
			before := readBuildFile(t, cp, "chg")
			result, err := endpoints.GetTestWriterAssignment(asCaller(agreedOwner, kind), &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
			if kind != CallerTestWriter {
				assertRefused(t, err, "forbidden")
				assert.Nil(t, result)
				assert.Equal(t, before, readBuildFile(t, cp, "chg"), "a refused read changes nothing")
				return
			}
			require.NoError(t, err)
			assignment, ok := result.(*gensurla.TestWriterAssignment)
			require.True(t, ok)
			assert.Equal(t, agreed.Examples, assignment.Examples)
		})
	}
}

// TestBuildWithoutAgreedTermsAsksToBuildAgain writes a test-writer build file
// without the agreed terms, as Surla saved builds before it kept them. The
// file still decodes, so the build reads and lists, but the test writer's assignment is refused with the step to
// build again, recording no read; building again gives the terms.
func TestBuildWithoutAgreedTermsAsksToBuildAgain(t *testing.T) {
	cp := newBuildControlPlane(t, Options{})
	agreed := confirmAgreedTerms(t, cp, "chg")
	folder := t.TempDir()
	raw, err := json.Marshal(buildFile{
		ChangeID: "chg", RequestRevision: agreed.RequestRevision, StartedAt: time.Date(2026, 11, 13, 9, 30, 0, 0, time.UTC),
		Roles: buildRolesBuilderAndTestWriter, Stories: refundStory("Refund over the limit"),
		TestWriter: testWriterAssignment{State: testsAssigned, WorkingFolder: &folder, Findings: []string{}},
	})
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	delete(fields, "answers")
	delete(fields, "examples")
	delete(fields, "preserved_behaviors")
	raw, err = json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cp.cards.dir, buildFileName("chg")), raw, 0o600))

	build := mustStartedBuild(t, cp, agreedUser, "chg")
	assert.True(t, build.InForce)
	listed, err := cp.ListTestWriterAssignments(agreedTestWriter)
	require.NoError(t, err)
	require.Len(t, listed.Assignments, 1)
	before := readBuildFile(t, cp, "chg")
	_, err = cp.GetTestWriterAssignment(agreedTestWriter, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the user builds this change again on the views page")
	assert.Equal(t, before, readBuildFile(t, cp, "chg"), "a refused read records nothing")

	_, err = cp.StartBuild(agreedUser, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: agreed.RequestRevision})
	require.NoError(t, err)
	assignment, err := cp.GetTestWriterAssignment(agreedTestWriter, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, agreed.Examples, assignment.Examples)
}

// confirmAgreedTerms proposes changeID as the guide with two questions, one
// example and two preserved behaviors, then confirms it as the user with an
// answer to each question, the example edited and one example added. It
// returns the confirmed card.
func confirmAgreedTerms(t *testing.T, cp *ControlPlane, changeID string) *gensurla.ChangeCardResult {
	t.Helper()
	analysis := refundAnalysis("Refunds up to $50 are instant.", refundTaxQuestion(), &gensurla.CP1AnalysisAmbiguity{
		Question: "Where does an instant refund go?", Options: []string{"original_payment", "store_credit"},
	})
	analysis.PreservedBehaviors = append(analysis.PreservedBehaviors, "Refunds of gift cards stay manual.")
	proposed := mustPropose(t, cp, agreedGuide, changeID, "Refunds up to $50 are instant.", analysis)
	require.Len(t, proposed.Ambiguities, 2)
	require.Len(t, proposed.Examples, 1)
	edited := *proposed.Examples[0]
	edited.InputSummary = "Order total $50.00 before tax"
	edited.ExpectedOutcome = "Instant refund to the original card"
	confirmed, err := cp.ConfirmIntent(agreedUser, &gensurla.ConfirmIntentPayload{
		ChangeID: changeID, RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{
			proposed.Ambiguities[0].ID: "without_tax",
			proposed.Ambiguities[1].ID: "original_payment",
		},
		Examples: []*gensurla.ExampleItem{&edited, {
			ID: "refund_over_limit", Scenario: "Refund just over the limit", InputSummary: "Order total $50.01 before tax",
			ExpectedOutcome: "The refund waits for a manager", VerificationKind: "deterministic",
		}},
	})
	require.NoError(t, err)
	require.Equal(t, StatusConfirmed, confirmed.Status)
	return confirmed
}
