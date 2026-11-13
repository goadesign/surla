// This file tests that an agreed change stays in force until the user adopts
// a revision: a revision of a confirmed change waits beside it as a proposal,
// the agreed revision keeps working for every operation that names an exact
// revision, the user adopts by confirming the proposal or declines it, and
// the card file is checked for both revisions on load.
package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/testmodule"
)

// agreedOwner owns every change these tests save.
const agreedOwner = "alice@example.com"

var (
	// agreedUser is the user's session acting for agreedOwner.
	agreedUser = WithPrincipal(context.Background(), Principal{ID: agreedOwner, CallerID: "user-1", Kind: CallerUser})
	// agreedGuide is the guide acting for agreedOwner.
	agreedGuide = WithPrincipal(context.Background(), Principal{ID: agreedOwner, CallerID: "guide", Kind: CallerGuide})
	// agreedBuilder is the builder acting for agreedOwner.
	agreedBuilder = WithPrincipal(context.Background(), Principal{ID: agreedOwner, CallerID: "builder", Kind: CallerBuilder})
)

// TestARevisionOfAnAgreedChangeWaitsAndTheAgreedRevisionStaysInForce
// revises a confirmed and approved change and checks that the agreed
// revision is still read, previewed (its revision check passes and preview
// goes on to evaluate the design), verified and approved at its revision
// while the proposal waits, with its answers, confirmation, approval, Story
// and source untouched on disk; after the user adopts the proposal, each of
// those operations refuses the earlier revision.
func TestARevisionOfAnAgreedChangeWaitsAndTheAgreedRevisionStaysInForce(t *testing.T) {
	target := &pageTarget{inputs: agreedInputs()}
	cp := newControlPlane(t, Options{Verification: verificationConfig(t), ResolvePromotionTarget: target.resolve})
	agreed := agreeChange(t, cp, "chg")
	commit := strings.Repeat("1", 40)
	report := passingReport(t, agreed.RequestRevision, commit, agreedInputs())
	target.update(func(p *pageTarget) { p.report = report })
	approved, err := cp.ApproveMerge(agreedUser, &gensurla.ApproveMergePayload{
		ChangeID: "chg", RequestRevision: agreed.RequestRevision, CandidateCommit: commit, ReportDigest: report.Digest,
	})
	require.NoError(t, err)
	require.NotNil(t, approved.Approval)
	before := savedSections(t, cp, "chg")

	revised := proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.")
	assert.Equal(t, StatusConfirmed, revised.Status, "the agreed revision stays in force")
	assert.Equal(t, agreed.RequestRevision, revised.RequestRevision)
	assert.Equal(t, agreed.Answers, revised.Answers)
	require.NotNil(t, revised.ConfirmedBy)
	require.NotNil(t, revised.Approval, "the agreed revision keeps its approval")
	require.NotNil(t, revised.ProposedRevision)
	proposal := revised.ProposedRevision
	assert.NotEqual(t, agreed.RequestRevision, proposal.RequestRevision)
	assert.Equal(t, "Refunds up to $60 are instant.", proposal.Summary)
	after := savedSections(t, cp, "chg")
	for _, section := range []string{"card", "owner", "design_source", "stories"} {
		assert.JSONEq(t, string(before[section]), string(after[section]), "the agreed revision's %s changed on disk", section)
	}

	read, err := cp.GetChange(agreedBuilder, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, agreed.RequestRevision, read.RequestRevision, "the agreed revision is read")
	require.NotNil(t, read.ProposedRevision)
	assert.Equal(t, proposal.RequestRevision, read.ProposedRevision.RequestRevision, "reading returns the proposal beside it")

	_, err = cp.PreviewArchitecture(agreedGuide, &gensurla.PreviewArchitecturePayload{ChangeID: "chg", RequestRevision: agreed.RequestRevision})
	assertServiceError(t, err, "unprocessable")
	assert.ErrorContains(t, err, "workspace directory is not bound", "preview accepted the agreed revision and went on to read the design")

	run, err := cp.VerifyChange(agreedBuilder, &gensurla.VerifyChangePayload{ChangeID: "chg", RequestRevision: agreed.RequestRevision, CandidateCommit: commit})
	require.NoError(t, err, "verification of the agreed revision is accepted")
	assert.Equal(t, agreed.RequestRevision, run.RequestRevision)

	reapproved, err := cp.ApproveMerge(agreedUser, &gensurla.ApproveMergePayload{
		ChangeID: "chg", RequestRevision: agreed.RequestRevision, CandidateCommit: commit, ReportDigest: report.Digest,
	})
	require.NoError(t, err, "the agreed revision is still approvable")
	require.NotNil(t, reapproved.Approval)
	require.NotNil(t, reapproved.ProposedRevision, "approving keeps the proposal waiting")

	adoptProposal(t, cp, "chg", proposal)
	_, err = cp.PreviewArchitecture(agreedGuide, &gensurla.PreviewArchitecturePayload{ChangeID: "chg", RequestRevision: agreed.RequestRevision})
	assertServiceError(t, err, "conflict")
	_, err = cp.VerifyChange(agreedBuilder, &gensurla.VerifyChangePayload{ChangeID: "chg", RequestRevision: agreed.RequestRevision, CandidateCommit: commit})
	assertServiceError(t, err, "conflict")
	_, err = cp.ApproveMerge(agreedUser, &gensurla.ApproveMergePayload{
		ChangeID: "chg", RequestRevision: agreed.RequestRevision, CandidateCommit: commit, ReportDigest: report.Digest,
	})
	assertServiceError(t, err, "conflict")
}

// TestTheAgreedRevisionStaysPreviewableWhileAProposalWaits previews a real
// design with the Go toolchain at the agreed revision while a proposal waits.
func TestTheAgreedRevisionStaysPreviewableWhileAProposalWaits(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	workspace := t.TempDir()
	writeWorkspaceFiles(t, workspace, testmodule.Snapshot(t, "", `package design

import . "goa.design/goa/v3/dsl"

var _ = API("orders", func() {})

var _ = Service("orders", func() {
	Method("refund", func() {
		Payload(String)
		Result(String)
		HTTP(func() { POST("/refunds") })
	})
})
`))
	cp := newControlPlane(t, Options{WorkspaceDir: workspace})
	agreed := agreeChange(t, cp, "chg")
	proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.")
	preview, err := cp.PreviewArchitecture(agreedGuide, &gensurla.PreviewArchitecturePayload{ChangeID: "chg", RequestRevision: agreed.RequestRevision})
	require.NoError(t, err)
	assert.Equal(t, agreed.RequestRevision, preview.RequestRevision)
	assert.NotEmpty(t, preview.Containers)
}

// TestAProposalIsReplacedByTheNextRevisionAndAStaleOneIsRefused revises a
// waiting proposal, which replaces it, and refuses a revision naming the
// agreed revision or a replaced proposal without changing the file.
func TestAProposalIsReplacedByTheNextRevisionAndAStaleOneIsRefused(t *testing.T) {
	cp := newControlPlane(t, Options{})
	agreed := agreeChange(t, cp, "chg")
	first := proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.").ProposedRevision
	second := proposeRevision(t, cp, "chg", first.RequestRevision, "Refunds up to $70 are instant.")
	assert.Equal(t, agreed.RequestRevision, second.RequestRevision)
	require.NotNil(t, second.ProposedRevision)
	assert.NotEqual(t, first.RequestRevision, second.ProposedRevision.RequestRevision, "the second revision replaced the first proposal")
	assert.Equal(t, "Refunds up to $70 are instant.", second.ProposedRevision.Summary)

	saved := savedFile(t, cp, "chg")
	for _, stale := range []string{agreed.RequestRevision, first.RequestRevision} {
		_, err := cp.ReviseChange(agreedGuide, revisionPayload("chg", stale, "Refunds up to $80 are instant.", "check_01"))
		assertServiceError(t, err, "conflict")
		assert.Equal(t, saved, savedFile(t, cp, "chg"), "a stale revision changed the file")
	}
	_, err := cp.ReviseChange(agreedGuide, revisionPayload("chg", agreed.RequestRevision, "Refunds up to $80 are instant.", "check_01"))
	assert.ErrorContains(t, err, "revise it by naming its request_revision", "naming the agreed revision explains that the proposal waits")
}

// TestAdoptingMakesTheProposalTheRevisionInForce adopts a proposal of an
// approved change: the proposal, answered and confirmed by the user, is the
// revision in force under a new request_revision, the slot is empty, and no
// approval carries over. A missing answer refuses the adoption.
func TestAdoptingMakesTheProposalTheRevisionInForce(t *testing.T) {
	cp := newControlPlane(t, Options{})
	saveCard(t, cp, &gensurla.ApprovalItem{
		ApprovedBy: agreedOwner, ReportDigest: strings.Repeat("d", 64), CandidateCommit: strings.Repeat("c", 40),
		SuiteID: "orders", SuiteCommit: strings.Repeat("b", 40), Environment: "sim",
		Artifacts: map[string]string{"orders": strings.Repeat("e", 64)}, ScenarioCount: 1,
	})
	agreed, err := cp.GetChange(agreedUser, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	proposal := proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.").ProposedRevision

	saved := savedFile(t, cp, "chg")
	_, err = cp.ConfirmIntent(agreedUser, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposal.RequestRevision, Answers: map[string]string{}, Examples: proposal.Examples,
	})
	assertServiceError(t, err, "unprocessable")
	assert.Equal(t, saved, savedFile(t, cp, "chg"), "an adoption missing an answer changed the file")

	adopted := adoptProposal(t, cp, "chg", proposal)
	assert.Equal(t, StatusConfirmed, adopted.Status)
	assert.Equal(t, "Refunds up to $60 are instant.", adopted.Summary)
	require.NotNil(t, adopted.ConfirmedBy)
	assert.Equal(t, agreedOwner, *adopted.ConfirmedBy, "the user's session confirmed it")
	assert.Equal(t, map[string]string{"chg-ambiguity-01": "with_tax"}, adopted.Answers)
	assert.NotEqual(t, agreed.RequestRevision, adopted.RequestRevision)
	assert.NotEqual(t, proposal.RequestRevision, adopted.RequestRevision, "confirming mints the confirmed revision")
	assert.Nil(t, adopted.Approval)
	assert.Nil(t, adopted.UnverifiedApproval)
	assert.Nil(t, adopted.ProposedRevision)
	stored, ok, err := cp.cards.load("chg")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, stored.proposed, "adopting empties the proposal slot")
	assert.Nil(t, stored.card.Approval, "no approval carries over")
}

// TestDecliningRestoresTheAgreedChangeExactly declines a proposal: the saved
// file equals the one before the revision except for its saved time, and a
// second decline or one naming the agreed revision is refused.
func TestDecliningRestoresTheAgreedChangeExactly(t *testing.T) {
	cp := newControlPlane(t, Options{})
	saveCard(t, cp, &gensurla.ApprovalItem{
		ApprovedBy: agreedOwner, ReportDigest: strings.Repeat("d", 64), CandidateCommit: strings.Repeat("c", 40),
		SuiteID: "orders", SuiteCommit: strings.Repeat("b", 40), Environment: "sim",
		Artifacts: map[string]string{"orders": strings.Repeat("e", 64)}, ScenarioCount: 1,
	})
	before := savedSections(t, cp, "chg")
	agreed, err := cp.GetChange(agreedUser, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	proposal := proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.").ProposedRevision

	declined, err := cp.DeclineRevision(agreedUser, &gensurla.DeclineRevisionPayload{ChangeID: "chg", RequestRevision: proposal.RequestRevision})
	require.NoError(t, err)
	assert.Equal(t, agreed.RequestRevision, declined.RequestRevision)
	assert.Nil(t, declined.ProposedRevision)
	assert.NotNil(t, declined.UnverifiedApproval, "the agreed revision keeps its approval")
	after := savedSections(t, cp, "chg")
	delete(before, "saved_at")
	delete(after, "saved_at")
	assert.Equal(t, before, after, "declining did not restore the earlier saved change")

	saved := savedFile(t, cp, "chg")
	for _, rev := range []string{proposal.RequestRevision, agreed.RequestRevision} {
		_, err = cp.DeclineRevision(agreedUser, &gensurla.DeclineRevisionPayload{ChangeID: "chg", RequestRevision: rev})
		assertServiceError(t, err, "conflict")
	}
	assert.Equal(t, saved, savedFile(t, cp, "chg"), "a refused decline changed the file")
}

// TestRefiningTheAgreedRevisionWhileAProposalWaitsIsRefused confirms the
// agreed revision with a changed example while a proposal waits: the request
// is refused with a message naming Adopt and Decline, and the file is
// unchanged.
func TestRefiningTheAgreedRevisionWhileAProposalWaitsIsRefused(t *testing.T) {
	cp := newControlPlane(t, Options{})
	agreed := agreeChange(t, cp, "chg")
	proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.")
	saved := savedFile(t, cp, "chg")
	refined := *agreed.Examples[0]
	refined.ExpectedOutcome = "Instant store credit within a minute"
	_, err := cp.ConfirmIntent(agreedUser, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: agreed.RequestRevision, Answers: agreed.Answers, Examples: []*gensurla.ExampleItem{&refined},
	})
	assertServiceError(t, err, "conflict")
	assert.ErrorContains(t, err, "Adopt")
	assert.ErrorContains(t, err, "Decline")
	assert.Equal(t, saved, savedFile(t, cp, "chg"), "a refused refinement changed the file")
}

// TestOnlyTheUserMayDeclineAProposal sends decline_revision over HTTP as the
// guide and the builder, which are refused without changing the file, and
// then as the user, which declines.
func TestOnlyTheUserMayDeclineAProposal(t *testing.T) {
	cp := newControlPlane(t, Options{})
	agreed := agreeChange(t, cp, "chg")
	proposal := proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.").ProposedRevision
	srv := httptest.NewServer(NewHTTPHandler(cp, fixtureCredentials(map[string]Principal{
		"user":    {ID: agreedOwner, CallerID: "user-1", Kind: CallerUser},
		"guide":   {ID: agreedOwner, CallerID: "guide", Kind: CallerGuide},
		"builder": {ID: agreedOwner, CallerID: "builder", Kind: CallerBuilder},
	})))
	t.Cleanup(srv.Close)
	body := `{"change_id":"chg","request_revision":"` + proposal.RequestRevision + `"}`
	saved := savedFile(t, cp, "chg")
	for _, token := range []string{"guide", "builder"} {
		status, _ := postJSON(t, srv.URL+"/changes/decline", token, body)
		assert.Equal(t, http.StatusForbidden, status, "%s declined a proposal", token)
		assert.Equal(t, saved, savedFile(t, cp, "chg"), "a refused decline by %s changed the file", token)
	}
	status, answer := postJSON(t, srv.URL+"/changes/decline", "user", body)
	require.Equal(t, http.StatusOK, status, answer)
	stored, ok, err := cp.cards.load("chg")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, stored.proposed)
}

// TestRevisingAChangeNeverConfirmedStillReplacesIt revises an unconfirmed
// change: the new revision replaces it and no proposal is kept.
func TestRevisingAChangeNeverConfirmedStillReplacesIt(t *testing.T) {
	cp := newControlPlane(t, Options{})
	proposed := mustPropose(t, cp, agreedGuide, "chg", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant.", refundTaxQuestion()))
	revised := proposeRevision(t, cp, "chg", proposed.RequestRevision, "Refunds up to $60 are instant.")
	assert.Equal(t, StatusNeedsConfirmation, revised.Status)
	assert.NotEqual(t, proposed.RequestRevision, revised.RequestRevision)
	assert.Equal(t, "Refunds up to $60 are instant.", revised.Summary)
	assert.Nil(t, revised.ProposedRevision)
	stored, ok, err := cp.cards.load("chg")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, stored.proposed)
	assert.Equal(t, revised.RequestRevision, stored.card.RequestRevision)
}

// TestTheChangeListNamesAWaitingProposal lists a change before, while and
// after a proposal waits: the item names the proposal's revision only while
// it waits, and the version moves at each step.
func TestTheChangeListNamesAWaitingProposal(t *testing.T) {
	cp := newControlPlane(t, Options{})
	agreed := agreeChange(t, cp, "chg")
	list := func() *gensurla.ChangeList {
		out, err := cp.ListChanges(agreedUser)
		require.NoError(t, err)
		require.Len(t, out.Changes, 1)
		return out
	}
	before := list()
	assert.Nil(t, before.Changes[0].ProposedRevision)
	proposal := proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.").ProposedRevision
	waiting := list()
	require.NotNil(t, waiting.Changes[0].ProposedRevision)
	assert.Equal(t, proposal.RequestRevision, *waiting.Changes[0].ProposedRevision)
	assert.Equal(t, agreed.RequestRevision, waiting.Changes[0].RequestRevision, "the item names the revision in force")
	assert.Equal(t, StatusConfirmed, waiting.Changes[0].Status)
	assert.NotEqual(t, before.Version, waiting.Version)
	_, err := cp.DeclineRevision(agreedUser, &gensurla.DeclineRevisionPayload{ChangeID: "chg", RequestRevision: proposal.RequestRevision})
	require.NoError(t, err)
	declined := list()
	assert.Nil(t, declined.Changes[0].ProposedRevision)
	assert.NotEqual(t, waiting.Version, declined.Version)
}

// TestACardFileWithAProposalIsCheckedOnLoad saves a change with a proposal
// and alters its file: a proposal beside an unconfirmed revision, a proposal
// whose content no longer matches its request_revision, a proposal carrying
// an approval, and a proposal of another change are each refused.
func TestACardFileWithAProposalIsCheckedOnLoad(t *testing.T) {
	cp := newControlPlane(t, Options{})
	agreed := agreeChange(t, cp, "chg")
	proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.")
	valid := savedFile(t, cp, "chg")
	unconfirmed := mustPropose(t, newControlPlane(t, Options{}), agreedGuide, "chg", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant.", refundTaxQuestion()))
	cases := []struct {
		name  string
		alter func(file map[string]any)
		want  string
	}{
		{
			name: "proposal beside an unconfirmed revision",
			alter: func(file map[string]any) {
				card := file["card"].(map[string]any)
				card["Status"] = StatusNeedsConfirmation
				card["Answers"] = map[string]any{}
				card["ConfirmedBy"] = nil
				card["RequestRevision"] = unconfirmed.RequestRevision
			},
			want: "beside an unconfirmed one",
		},
		{
			name:  "proposal that does not match its revision",
			alter: func(file map[string]any) { proposedCard(file)["Summary"] = "Refunds up to $90 are instant." },
			want:  "does not match its request_revision",
		},
		{
			name: "proposal with an approval",
			alter: func(file map[string]any) {
				proposedCard(file)["Approval"] = map[string]any{
					"ApprovedBy": agreedOwner, "ReportDigest": strings.Repeat("d", 64), "CandidateCommit": strings.Repeat("c", 40),
					"SuiteID": "orders", "SuiteCommit": strings.Repeat("b", 40), "Environment": "sim",
					"Artifacts": map[string]any{"orders": strings.Repeat("e", 64)}, "ScenarioCount": 1,
				}
			},
			want: "not awaiting confirmation",
		},
		{
			name:  "proposal of another change",
			alter: func(file map[string]any) { proposedCard(file)["ChangeID"] = "other" },
			want:  "proposed revision of another change",
		},
	}
	_, err := cp.GetChange(agreedUser, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err, "the unaltered file loads")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var file map[string]any
			require.NoError(t, json.Unmarshal([]byte(valid), &file))
			tc.alter(file)
			raw, err := json.Marshal(file)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(cp.cards.path("chg"), raw, 0o600))
			_, err = cp.GetChange(agreedUser, &gensurla.GetChangePayload{ChangeID: "chg"})
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// TestRevisionDescriptionsSayTheAgreedChangeStaysInForce checks the
// model-visible descriptions of revise_change and confirm_intent: revising
// does not replace or clear an agreed change, and the agreed revision stays
// in force until the user adopts or declines.
func TestRevisionDescriptionsSayTheAgreedChangeStaysInForce(t *testing.T) {
	cp := newControlPlane(t, Options{})
	tools, err := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil).ToolsList(t.Context(), &genmcpadapter.ToolsListPayload{})
	require.NoError(t, err)
	descriptions := map[string]string{}
	for _, tool := range tools.Tools {
		require.NotNil(t, tool.Description)
		descriptions[tool.Name] = *tool.Description
	}
	assert.Contains(t, descriptions["revise_change"], "stays in force")
	assert.Contains(t, descriptions["decline_revision"], "stays in force")
	assert.Contains(t, descriptions["confirm_intent"], "adopt")
	openAPI, err := os.ReadFile("../../gen/http/openapi3.json")
	require.NoError(t, err)
	for _, stale := range []string{"answers, confirmed_by and any approval are cleared", "dropped when confirm_intent or revise_change mints"} {
		assert.NotContains(t, string(openAPI), stale, "the contract still says a revision clears an agreed change")
		for name, text := range descriptions {
			assert.NotContains(t, text, stale, "%s still says a revision clears an agreed change", name)
		}
	}
}

// agreeChange proposes changeID with one question and confirms it as the
// user, returning the confirmed card.
func agreeChange(t *testing.T, cp *ControlPlane, changeID string) *gensurla.ChangeCardResult {
	t.Helper()
	proposed := mustPropose(t, cp, agreedGuide, changeID, "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant.", refundTaxQuestion()))
	confirmed, err := cp.ConfirmIntent(agreedUser, &gensurla.ConfirmIntentPayload{
		ChangeID: changeID, RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{changeID + "-ambiguity-01": "with_tax"}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	require.Equal(t, StatusConfirmed, confirmed.Status)
	return confirmed
}

// proposeRevision revises changeID as the guide, naming rev, with summary and
// one open question, keeping the first example of the revision rev names; it
// returns the change.
func proposeRevision(t *testing.T, cp *ControlPlane, changeID, rev, summary string) *gensurla.ChangeCardResult {
	t.Helper()
	change, err := cp.GetChange(agreedGuide, &gensurla.GetChangePayload{ChangeID: changeID})
	require.NoError(t, err)
	examples := change.Examples
	if change.ProposedRevision != nil && change.ProposedRevision.RequestRevision == rev {
		examples = change.ProposedRevision.Examples
	}
	revised, err := cp.ReviseChange(agreedGuide, revisionPayload(changeID, rev, summary, examples[0].ID))
	require.NoError(t, err)
	return revised
}

// revisionPayload is a revision of changeID naming rev with summary, one open
// question, and the example kept kept.
func revisionPayload(changeID, rev, summary, kept string) *gensurla.ReviseChangePayload {
	return &gensurla.ReviseChangePayload{
		ChangeID: changeID, RequestRevision: rev, Prompt: summary,
		Analysis: &gensurla.CP1RevisionAnalysis{
			Summary:     summary,
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{refundTaxQuestion()},
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID: &kept, Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
				ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
			}},
			RemovedExampleIds:  []string{},
			PreservedBehaviors: []string{},
		},
	}
}

// adoptProposal confirms proposal as the user, answering its one question.
func adoptProposal(t *testing.T, cp *ControlPlane, changeID string, proposal *gensurla.ProposedRevisionItem) *gensurla.ChangeCardResult {
	t.Helper()
	adopted, err := cp.ConfirmIntent(agreedUser, &gensurla.ConfirmIntentPayload{
		ChangeID: changeID, RequestRevision: proposal.RequestRevision,
		Answers: map[string]string{changeID + "-ambiguity-01": "with_tax"}, Examples: proposal.Examples,
	})
	require.NoError(t, err)
	return adopted
}

// refundTaxQuestion is the open question these tests' changes ask.
func refundTaxQuestion() *gensurla.CP1AnalysisAmbiguity {
	return &gensurla.CP1AnalysisAmbiguity{Question: "Does the limit include tax?", Options: []string{"with_tax", "without_tax"}}
}

// agreedInputs are the promotion inputs these tests' target reports.
func agreedInputs() PromotionInputs {
	return PromotionInputs{
		SuiteID: "orders_acceptance", SuiteCommit: strings.Repeat("2", 40),
		Environment: "sim", Artifacts: map[string]string{"orders": strings.Repeat("a", 64)},
	}
}

// savedFile returns changeID's saved card file.
func savedFile(t *testing.T, cp *ControlPlane, changeID string) string {
	t.Helper()
	raw, err := os.ReadFile(cp.cards.path(changeID))
	require.NoError(t, err)
	return string(raw)
}

// savedSections returns the top-level sections of changeID's saved file.
func savedSections(t *testing.T, cp *ControlPlane, changeID string) map[string]json.RawMessage {
	t.Helper()
	var sections map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(savedFile(t, cp, changeID)), &sections))
	return sections
}

// proposedCard returns the proposed revision's card inside a decoded file.
func proposedCard(file map[string]any) map[string]any {
	return file["proposed"].(map[string]any)["card"].(map[string]any)
}
