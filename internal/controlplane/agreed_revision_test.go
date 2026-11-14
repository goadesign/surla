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
	// agreedTestWriter is the test writer acting for agreedOwner.
	agreedTestWriter = WithPrincipal(context.Background(), Principal{ID: agreedOwner, CallerID: "tests", Kind: CallerTestWriter})
)

// TestARevisionOfAnAgreedChangeWaitsAndTheAgreedRevisionStaysInForce
// revises a confirmed and approved change and checks that the agreed
// revision is still read, previewed (its revision check passes and preview
// goes on to evaluate the design), verified and approved at its revision
// while the proposal waits, with its answers, confirmation, approval, Story
// and source untouched on disk; after the user adopts the proposal, each of
// those operations refuses the earlier revision.
func TestARevisionOfAnAgreedChangeWaitsAndTheAgreedRevisionStaysInForce(t *testing.T) {
	spec := agreedReportSpec()
	target := &pageTarget{artifacts: spec.Artifacts}
	cp := newControlPlane(t, Options{Verification: verificationConfig(t), ResolvePromotionTarget: target.resolve})
	agreed := agreeChange(t, cp, "chg")
	commit := strings.Repeat("1", 40)
	report := passingReport(t, agreed.RequestRevision, commit, spec)
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
	cp := newControlPlane(t, Options{Verification: verificationConfig(t)})
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
	cp := newControlPlane(t, Options{Verification: verificationConfig(t)})
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
	assert.NotNil(t, declined.Approval, "the agreed revision keeps its approval")
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
	for _, malformed := range []string{
		`{"change_id":"","request_revision":"` + proposal.RequestRevision + `"}`,
		`{"change_id":"chg","request_revision":"not-a-revision"}`,
		`{"change_id":"chg","request_revision":""}`,
	} {
		status, answer := postJSON(t, srv.URL+"/changes/decline", "user", malformed)
		assert.Equal(t, http.StatusBadRequest, status, "a malformed decline was not refused by the generated validation: %s", answer)
		assert.Equal(t, saved, savedFile(t, cp, "chg"), "a malformed decline changed the file")
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

// TestAProposalKeepsItsOwnStoryAndDesignSource proposes, beside an agreed
// change with one Story and design source, a revision evaluated from other
// design files with another Story. The proposal carries its own pair and
// names the changed design file; a second revision over it without a new
// evaluation keeps that pair; a proposal whose saved design source was
// altered is refused on load; and adopting it makes its Story and design
// source the change's.
func TestAProposalKeepsItsOwnStoryAndDesignSource(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFiles(t, ws, validWorkspaceStub("package design\n"))
	cp := newControlPlane(t, Options{WorkspaceDir: ws})
	agreedOp := seedStoryRefreshForTest(t, cp, agreedOwner, storyRefreshStateSucceeded, sampleAdoptedStories(), "")
	proposeWithStory(t, cp, agreedGuide, "chg", agreedOp)
	proposed, err := cp.GetChange(agreedUser, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	agreed, err := cp.ConfirmIntent(agreedUser, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)

	writeWorkspaceFiles(t, ws, map[string]string{"design/design.go": "package design // exchanges\n"})
	draftOp := seedStoryRefreshForTest(t, cp, agreedOwner, storyRefreshStateSucceeded, pageStories(), "")
	first := revisionPayload("chg", agreed.RequestRevision, "Refunds up to $60 are instant.", agreed.Examples[0].ID)
	first.StoryRefreshOpID = &draftOp
	revised, err := cp.ReviseChange(agreedGuide, first)
	require.NoError(t, err)
	require.NotNil(t, revised.ProposedRevision)
	assert.Equal(t, flowKeys(storySummaryItems(sampleAdoptedStories())), flowKeys(revised.Stories), "the agreed revision keeps its Story")
	assert.Equal(t, flowKeys(storySummaryItems(pageStories())), flowKeys(revised.ProposedRevision.Stories), "the proposal carries its own Story")
	assert.Equal(t, []string{"design/design.go"}, revised.ProposedRevision.ChangedDesignFiles)
	stored := loadChange(t, cp, "chg")
	assert.NotContains(t, stored.designSource, "exchanges", "the agreed revision keeps its design source")
	assert.Contains(t, stored.proposed.designSource, "exchanges", "the proposal records its own design source")

	second, err := cp.ReviseChange(agreedGuide, revisionPayload("chg", revised.ProposedRevision.RequestRevision, "Refunds up to $70 are instant.", agreed.Examples[0].ID))
	require.NoError(t, err)
	require.NotNil(t, second.ProposedRevision)
	assert.Equal(t, flowKeys(storySummaryItems(pageStories())), flowKeys(second.ProposedRevision.Stories), "revising the proposal keeps its Story")
	assert.Equal(t, []string{"design/design.go"}, second.ProposedRevision.ChangedDesignFiles)
	stored = loadChange(t, cp, "chg")
	assert.Contains(t, stored.proposed.designSource, "exchanges", "revising the proposal keeps its design source")
	assert.Equal(t, pageStories(), stored.proposed.stories)

	valid := savedFile(t, cp, "chg")
	var file map[string]any
	require.NoError(t, json.Unmarshal([]byte(valid), &file))
	file["proposed"].(map[string]any)["design_source"] = `{"design/design.go":"package design // altered\n"}`
	altered, err := json.Marshal(file)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cp.cards.path("chg"), altered, 0o600))
	_, err = cp.GetChange(agreedUser, &gensurla.GetChangePayload{ChangeID: "chg"})
	assert.ErrorContains(t, err, "does not match its request_revision", "a proposal whose design source was altered is refused")
	require.NoError(t, os.WriteFile(cp.cards.path("chg"), []byte(valid), 0o600))

	adopted := adoptProposal(t, cp, "chg", second.ProposedRevision)
	assert.Equal(t, flowKeys(storySummaryItems(pageStories())), flowKeys(adopted.Stories), "the adopted revision has the proposal's Story")
	stored = loadChange(t, cp, "chg")
	assert.Contains(t, stored.designSource, "exchanges", "the adopted revision has the proposal's design source")
	assert.Equal(t, pageStories(), stored.stories)
}

// TestChangedDesignFilesNamesAddedRemovedAndChangedFiles compares the agreed
// revision's saved design source with a proposal's: a file only the proposal
// has, a file only the agreed revision has, and a file whose content differs
// are each named once, in path order; identical sources name none, and a
// source that does not decode is an error.
func TestChangedDesignFilesNamesAddedRemovedAndChangedFiles(t *testing.T) {
	cases := []struct {
		name             string
		agreed, proposed string
		want             []string
		wantErr          string
	}{
		{name: "no design files", want: []string{}},
		{
			name:     "same files",
			agreed:   `{"design/design.go":"package design\n"}`,
			proposed: `{"design/design.go":"package design\n"}`,
			want:     []string{},
		},
		{
			name:     "changed file",
			agreed:   `{"design/design.go":"package design\n"}`,
			proposed: `{"design/design.go":"package design // exchanges\n"}`,
			want:     []string{"design/design.go"},
		},
		{
			name:     "added file",
			agreed:   `{"design/design.go":"package design\n"}`,
			proposed: `{"design/design.go":"package design\n","design/exchanges.go":"package design\n"}`,
			want:     []string{"design/exchanges.go"},
		},
		{
			name:     "removed file",
			agreed:   `{"design/design.go":"package design\n","design/refunds.go":"package design\n"}`,
			proposed: `{"design/design.go":"package design\n"}`,
			want:     []string{"design/refunds.go"},
		},
		{
			name:   "every file removed",
			agreed: `{"design/design.go":"package design\n"}`,
			want:   []string{"design/design.go"},
		},
		{
			name:     "removed, added and changed files",
			agreed:   `{"design/a.go":"package design\n","design/c.go":"package design\n"}`,
			proposed: `{"design/b.go":"package design\n","design/c.go":"package design // exchanges\n"}`,
			want:     []string{"design/a.go", "design/b.go", "design/c.go"},
		},
		{name: "agreed source that does not decode", agreed: "{", wantErr: "decode saved design source"},
		{name: "proposed source that does not decode", proposed: "[]", wantErr: "decode saved design source"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := changedDesignFiles(tc.agreed, tc.proposed)
			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDecliningNamesTheProposalTheUserSaw declines a proposal that the
// guide has since replaced: the request is refused and changes nothing, and
// declining the waiting proposal works.
func TestDecliningNamesTheProposalTheUserSaw(t *testing.T) {
	cp := newControlPlane(t, Options{})
	agreed := agreeChange(t, cp, "chg")
	seen := proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.").ProposedRevision
	waiting := proposeRevision(t, cp, "chg", seen.RequestRevision, "Refunds up to $70 are instant.").ProposedRevision
	saved := savedFile(t, cp, "chg")
	_, err := cp.DeclineRevision(agreedUser, &gensurla.DeclineRevisionPayload{ChangeID: "chg", RequestRevision: seen.RequestRevision})
	assertServiceError(t, err, "conflict")
	assert.Equal(t, saved, savedFile(t, cp, "chg"), "declining a replaced proposal changed the file")
	declined, err := cp.DeclineRevision(agreedUser, &gensurla.DeclineRevisionPayload{ChangeID: "chg", RequestRevision: waiting.RequestRevision})
	require.NoError(t, err)
	assert.Nil(t, declined.ProposedRevision)
}

// TestAProposalThatIsNotAwaitingConfirmationIsRefusedOnLoad saves a change
// whose proposal matches its own request_revision but carries answers, a
// confirmation, the confirmed status alone, or all three, and checks each is
// refused on load.
func TestAProposalThatIsNotAwaitingConfirmationIsRefusedOnLoad(t *testing.T) {
	confirmedBy := agreedOwner
	cases := []struct {
		name  string
		alter func(card *gensurla.ChangeCardResult)
	}{
		{name: "answered", alter: func(card *gensurla.ChangeCardResult) {
			card.Answers = map[string]string{"chg-ambiguity-01": "with_tax"}
		}},
		{name: "confirmed by someone", alter: func(card *gensurla.ChangeCardResult) { card.ConfirmedBy = &confirmedBy }},
		{name: "confirmed status alone", alter: func(card *gensurla.ChangeCardResult) { card.Status = StatusConfirmed }},
		{name: "confirmed with answers and a confirmation", alter: func(card *gensurla.ChangeCardResult) {
			card.Status = StatusConfirmed
			card.Answers = map[string]string{"chg-ambiguity-01": "with_tax"}
			card.ConfirmedBy = &confirmedBy
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newControlPlane(t, Options{})
			agreed := agreeChange(t, cp, "chg")
			proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.")
			record := loadChange(t, cp, "chg")
			proposal := *record.proposed.card
			tc.alter(&proposal)
			rev, err := computeCanonicalRevision(&proposal, record.owner, record.proposed.designSource)
			require.NoError(t, err)
			proposal.RequestRevision = rev
			record.proposed = &storedRevision{card: &proposal, designSource: record.proposed.designSource, stories: record.proposed.stories}
			require.NoError(t, cp.cards.save(record))
			_, err = cp.GetChange(agreedUser, &gensurla.GetChangePayload{ChangeID: "chg"})
			assert.ErrorContains(t, err, "not awaiting confirmation")
		})
	}
}

// TestAWaitingProposalsDesignFilesBlockInitialization proposes a change in a
// workspace without design files, confirms it, adds design files, and
// proposes a revision that records them: init_project's check then counts
// the proposal's files although the revision in force has none.
func TestAWaitingProposalsDesignFilesBlockInitialization(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFiles(t, ws, map[string]string{"go.mod": "module example.com/orders\n\ngo 1.24\n"})
	cp := newControlPlane(t, Options{WorkspaceDir: ws})
	agreed := agreeChange(t, cp, "chg")
	blocked, err := cp.cards.hasWorkspaceCardsWithFiles()
	require.NoError(t, err)
	require.False(t, blocked, "a change made before any design file does not block initialization")
	writeWorkspaceFiles(t, ws, map[string]string{"design/design.go": "package design\n"})
	proposeRevision(t, cp, "chg", agreed.RequestRevision, "Refunds up to $60 are instant.")
	stored := loadChange(t, cp, "chg")
	require.Empty(t, stored.designSource)
	require.NotEmpty(t, stored.proposed.designSource)
	blocked, err = cp.cards.hasWorkspaceCardsWithFiles()
	require.NoError(t, err)
	assert.True(t, blocked, "a waiting proposal's design files block initialization")
}

// TestBuildsFollowTheAgreedRevision builds an agreed change while the guide's
// proposal with another Story waits beside it: the proposal's revision cannot
// be built, the agreed revision can, and the test writer's assignment holds
// the agreed Story. Declining the proposal leaves the build in force on the
// build read and on both roles' assignments; adopting a later proposal makes
// it no longer in force on all three.
func TestBuildsFollowTheAgreedRevision(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFiles(t, ws, validWorkspaceStub("package design\n"))
	cp := newBuildControlPlane(t, Options{WorkspaceDir: ws})
	agreedOp := seedStoryRefreshForTest(t, cp, agreedOwner, storyRefreshStateSucceeded, sampleAdoptedStories(), "")
	proposeWithStory(t, cp, agreedGuide, "chg", agreedOp)
	proposed, err := cp.GetChange(agreedUser, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	agreed, err := cp.ConfirmIntent(agreedUser, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	propose := func(rev string) *gensurla.ProposedRevisionItem {
		t.Helper()
		draftOp := seedStoryRefreshForTest(t, cp, agreedOwner, storyRefreshStateSucceeded, pageStories(), "")
		payload := revisionPayload("chg", rev, "Refunds up to $60 are instant.", agreed.Examples[0].ID)
		payload.StoryRefreshOpID = &draftOp
		revised, err := cp.ReviseChange(agreedGuide, payload)
		require.NoError(t, err)
		require.NotNil(t, revised.ProposedRevision)
		return revised.ProposedRevision
	}
	proposal := propose(agreed.RequestRevision)

	_, err = cp.StartBuild(agreedUser, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: proposal.RequestRevision})
	assertServiceError(t, err, "conflict")
	started, err := cp.StartBuild(agreedUser, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: agreed.RequestRevision})
	require.NoError(t, err, "the agreed revision is built while a proposal waits")
	assert.Equal(t, agreed.RequestRevision, started.Build.RequestRevision)
	assert.True(t, started.Build.InForce)
	tests, err := cp.GetTestWriterAssignment(agreedTestWriter, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, flowKeys(storySummaryItems(sampleAdoptedStories())), flowKeys(tests.Stories), "the build holds the agreed Story, not the proposal's")

	inForce := func() map[string]bool {
		t.Helper()
		build := mustStartedBuild(t, cp, agreedUser, "chg")
		builder, err := cp.GetBuilderAssignment(agreedBuilder, &gensurla.GetBuilderAssignmentPayload{ChangeID: "chg"})
		require.NoError(t, err)
		tests, err := cp.GetTestWriterAssignment(agreedTestWriter, &gensurla.GetTestWriterAssignmentPayload{ChangeID: "chg"})
		require.NoError(t, err)
		return map[string]bool{"build": build.InForce, "builder": builder.InForce, "test writer": tests.InForce}
	}
	all := func(want bool) map[string]bool {
		return map[string]bool{"build": want, "builder": want, "test writer": want}
	}
	assert.Equal(t, all(true), inForce(), "a waiting proposal leaves the build in force")

	_, err = cp.DeclineRevision(agreedUser, &gensurla.DeclineRevisionPayload{ChangeID: "chg", RequestRevision: proposal.RequestRevision})
	require.NoError(t, err)
	assert.Equal(t, all(true), inForce(), "declining leaves the build in force")

	adoptProposal(t, cp, "chg", propose(agreed.RequestRevision))
	assert.Equal(t, all(false), inForce(), "adopting makes the earlier build no longer in force")
}

// TestRevisionDescriptionsSayTheAgreedChangeStaysInForce checks the
// model-visible descriptions of revise_change and confirm_intent: revising
// does not replace or clear an agreed change, the agreed revision stays in
// force until the user adopts or declines, each operation names its one
// caller (the guide revises, the user confirms), and list_changes names a
// waiting proposal's revision.
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
	assert.NotContains(t, descriptions["confirm_intent"], "call revise_change", "the user's tool tells its caller to revise, which only the guide does")
	assert.Contains(t, descriptions["confirm_intent"], "goes to the guide")
	assert.Contains(t, descriptions["list_changes"], "proposed_revision (the request_revision of a revision proposed beside a confirmed card", "the change list's tool does not name a waiting proposal")
	openAPI, err := os.ReadFile("../../gen/http/openapi3.json")
	require.NoError(t, err)
	assert.Contains(t, string(openAPI), "Only the guide revises a change; only the user confirms or adopts a revision")
	assert.Contains(t, string(openAPI), "Only the user's browser session calls it, from the views page; the guide revises with revise_change and never confirms.")
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

// agreedReportSpec is the report specification these tests assemble.
func agreedReportSpec() reportSpec {
	return reportSpec{
		SuiteID: "orders_acceptance", SuiteCommit: strings.Repeat("b", 40),
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

// loadChange returns changeID's saved change, failing when it is not saved.
func loadChange(t *testing.T, cp *ControlPlane, changeID string) storedChangeCard {
	t.Helper()
	record, ok, err := cp.cards.load(changeID)
	require.NoError(t, err)
	require.True(t, ok)
	return record
}

// flowKeys lists the flow keys of stories in order.
func flowKeys(stories []*gensurla.StorySummaryItem) []string {
	keys := []string{}
	for _, story := range stories {
		for _, flow := range story.Flows {
			keys = append(keys, flow.Key)
		}
	}
	return keys
}
