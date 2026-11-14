// This file tests Checkpoint 1 Change Card revision (`revise_change`) with a
// caller-supplied analysis: transports, ownership and revision checks,
// example accounting, concurrent revisions, and cards saved by an earlier
// server.

package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	gensurlatools "github.com/goadesign/surla/gen/surla/toolsets/surla"
	"github.com/goadesign/surla/internal/designeval"
)

// TestReviseChangeOverHTTPAndMCP verifies revise_change over the Go service,
// HTTP (`POST /changes/revise`) and MCP: the tool is listed;
// unknown, foreign and stale cards are refused; an analysis that repeats an
// option or does not account for every current example exactly once is
// unprocessable and leaves the saved card file byte for byte unchanged; and a
// complete analysis of the confirmed card saves a proposed revision in
// needs_confirmation beside it, which a later revision over MCP replaces.
func TestReviseChangeOverHTTPAndMCP(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	bobCtx := WithPrincipal(context.Background(), Principal{ID: "bob@example.com", CallerID: "bob@example.com", Kind: CallerUser})
	cp := newControlPlane(t, Options{})
	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
	t.Cleanup(srv.Close)
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)

	toolsList, err := adapter.ToolsList(aliceCtx, &genmcpadapter.ToolsListPayload{})
	require.NoError(t, err)
	var foundReviseTool bool
	for _, tool := range toolsList.Tools {
		foundReviseTool = foundReviseTool || tool.Name == "revise_change"
	}
	require.True(t, foundReviseTool, "MCP ToolsList must include revise_change")

	r1 := mustPropose(t, cp, aliceCtx, "chg-tx", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	r1 = mustConfirmNoop(t, cp, aliceCtx, r1)
	require.Equal(t, StatusConfirmed, r1.Status, "the user explicitly confirmed this fixture")
	cardPath := cp.cards.path("chg-tx")
	r1DiskBytes, err := os.ReadFile(cardPath)
	require.NoError(t, err)

	revision := func(summary string, priorIDs ...string) *gensurla.CP1RevisionAnalysis {
		examples := make([]*gensurla.CP1RevisionAnalysisExample, 0, len(priorIDs))
		for _, id := range priorIDs {
			examples = append(examples, &gensurla.CP1RevisionAnalysisExample{
				PriorID: &id, Scenario: "Refund at the limit", InputSummary: "Order total $100.00",
				ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
			})
		}
		return &gensurla.CP1RevisionAnalysis{
			Summary: summary, Ambiguities: []*gensurla.CP1AnalysisAmbiguity{}, Examples: examples,
			RemovedExampleIds: []string{}, PreservedBehaviors: []string{"Cross-tenant order lookups return 404 Not Found."},
		}
	}
	revise := func(ctx context.Context, changeID, rev string, analysis *gensurla.CP1RevisionAnalysis) (*gensurla.ChangeCardResult, error) {
		return cp.ReviseChange(ctx, &gensurla.ReviseChangePayload{
			ChangeID: changeID, RequestRevision: rev, Prompt: "Refunds up to $100 are instant.", Analysis: analysis,
		})
	}

	_, err = revise(aliceCtx, "missing", r1.RequestRevision, revision("Refunds up to $100 are instant.", "check_01"))
	assertServiceError(t, err, "not_found")
	_, err = revise(bobCtx, "chg-tx", r1.RequestRevision, revision("Refunds up to $100 are instant.", "check_01"))
	assertServiceError(t, err, "forbidden")
	_, err = revise(aliceCtx, "chg-tx", "rev_stale", revision("Refunds up to $100 are instant.", "check_01"))
	assertServiceError(t, err, "conflict")

	duplicate := revision("Refunds up to $100 are instant.", "check_01")
	duplicate.Ambiguities = []*gensurla.CP1AnalysisAmbiguity{{Question: "Does the limit include tax?", Options: []string{"with_tax", "with_tax"}}}
	dropped := revision("Refunds up to $100 are instant.")
	dropped.Examples = []*gensurla.CP1RevisionAnalysisExample{{
		Scenario: "Refund at the limit", InputSummary: "Order total $100.00", ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
	}}
	for _, tc := range []struct {
		name     string
		analysis *gensurla.CP1RevisionAnalysis
		want     string
	}{
		{"repeated option", duplicate, `invalid analysis: ambiguity "chg-tx-ambiguity-01" (ambiguities[0]) has duplicate option at options[1]`},
		{"current example neither kept nor dropped", dropped, `invalid analysis: prior example ID "check_01" is omitted from both examples and removed_example_ids`},
		{"unknown prior example", revision("Refunds up to $100 are instant.", "check_01", "check_07"), `invalid analysis: examples[1].prior_id references unknown prior example ID "check_07"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := revise(aliceCtx, "chg-tx", r1.RequestRevision, tc.analysis)
			assertServiceError(t, err, "unprocessable")
			assert.ErrorContains(t, err, tc.want)
			afterBytes, readErr := os.ReadFile(cardPath)
			require.NoError(t, readErr)
			assert.Equal(t, string(r1DiskBytes), string(afterBytes), "a rejected revision must not modify the saved card")
		})
	}

	// The same accounting rule is reported to an MCP caller as a tool error.
	res, err := adapter.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{Name: "revise_change", Arguments: reviseArgs(t, &gensurla.ReviseChangePayload{
		ChangeID: "chg-tx", RequestRevision: r1.RequestRevision, Prompt: "Refunds up to $100 are instant.", Analysis: dropped,
	})})
	require.NoError(t, err)
	complete, ok := res.Outcome.AsComplete()
	require.True(t, ok)
	require.NotNil(t, complete.IsError)
	assert.True(t, *complete.IsError)
	require.NotNil(t, complete.Content[0].Text)
	assert.Contains(t, *complete.Content[0].Text, `prior example ID "check_01" is omitted from both examples and removed_example_ids`)

	// HTTP POST /changes/revise succeeds against r1.
	httpBody := reviseArgs(t, &gensurla.ReviseChangePayload{
		ChangeID: "chg-tx", RequestRevision: r1.RequestRevision, Prompt: "Refunds up to $100 are instant.",
		Analysis: revision("Refunds up to $100 are instant via HTTP.", "check_01"),
	})
	status, body := postJSON(t, srv.URL+"/changes/revise", "alice-token", string(httpBody))
	require.Equal(t, http.StatusOK, status, body)
	type changeBody struct {
		Status           string `json:"status"`
		RequestRevision  string `json:"request_revision"`
		ProposedRevision *struct {
			Summary         string `json:"summary"`
			RequestRevision string `json:"request_revision"`
		} `json:"proposed_revision"`
	}
	var httpCard changeBody
	require.NoError(t, json.Unmarshal([]byte(body), &httpCard))
	assert.Equal(t, StatusConfirmed, httpCard.Status, "the agreed revision stays in force")
	assert.Equal(t, r1.RequestRevision, httpCard.RequestRevision)
	require.NotNil(t, httpCard.ProposedRevision)
	assert.Equal(t, "Refunds up to $100 are instant via HTTP.", httpCard.ProposedRevision.Summary)

	// MCP revise_change succeeds against the HTTP proposal and replaces it.
	res, err = adapter.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{Name: "revise_change", Arguments: reviseArgs(t, &gensurla.ReviseChangePayload{
		ChangeID: "chg-tx", RequestRevision: httpCard.ProposedRevision.RequestRevision, Prompt: "Refunds up to $150 are instant.",
		Analysis: revision("Refunds up to $150 are instant via MCP.", "check_01"),
	})})
	require.NoError(t, err)
	complete, ok = res.Outcome.AsComplete()
	require.True(t, ok)
	require.False(t, complete.IsError != nil && *complete.IsError)
	var mcpCard changeBody
	require.NoError(t, json.Unmarshal(complete.StructuredContent, &mcpCard))
	assert.Equal(t, StatusConfirmed, mcpCard.Status)
	require.NotNil(t, mcpCard.ProposedRevision)
	assert.Equal(t, "Refunds up to $150 are instant via MCP.", mcpCard.ProposedRevision.Summary)
	assert.NotEqual(t, httpCard.ProposedRevision.RequestRevision, mcpCard.ProposedRevision.RequestRevision, "the second revision replaced the first proposal")
}

// TestReviseChangeSavesARevisionThatOnlyItsContentDetermines verifies what a
// revision keeps and drops: it enters needs_confirmation with no answers,
// confirmed_by or approval while the confirmed revision beside it keeps its
// own, numbers its questions afresh from 01,
// binds its ui_composition to its own request_revision, and gets the same
// request_revision as an earlier revision whose saved content it repeats
// exactly, because request_revision is a digest of that content alone.
func TestReviseChangeSavesARevisionThatOnlyItsContentDetermines(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cp := newControlPlane(t, Options{Verification: verificationConfig(t)})

	// Revising a confirmed card with a recorded approval proposes a revision
	// without one, and the approval stays on the confirmed revision.
	approved := saveCard(t, cp, &gensurla.ApprovalItem{
		ApprovedBy: "alice@example.com", ReportDigest: strings.Repeat("d", 64), CandidateCommit: strings.Repeat("c", 40),
		SuiteID: "orders", SuiteCommit: strings.Repeat("b", 40), Environment: "sim",
		Artifacts: map[string]string{"orders": strings.Repeat("e", 64)}, ScenarioCount: 1,
	})
	kept := approved.Examples[0].ID
	revised, err := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
		ChangeID: "chg", RequestRevision: approved.RequestRevision, Prompt: "Refunds up to $60 are instant.",
		Analysis: &gensurla.CP1RevisionAnalysis{
			Summary:     "Refunds up to $60 are instant.",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID: &kept, Scenario: "Refund at the limit", InputSummary: "Order total $60.00",
				ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
			}},
			RemovedExampleIds:  []string{},
			PreservedBehaviors: []string{},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfirmed, revised.Status, "the confirmed revision stays in force")
	assert.Equal(t, approved.RequestRevision, revised.RequestRevision)
	require.NotNil(t, revised.Approval, "the confirmed revision keeps its approval")
	stored, ok, err := cp.cards.load("chg")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, stored.card.Approval, "the saved confirmed revision keeps its approval")
	require.NotNil(t, stored.proposed)
	assert.Equal(t, StatusNeedsConfirmation, stored.proposed.card.Status, "a revision without open questions still needs confirmation")
	assert.Nil(t, stored.proposed.card.ConfirmedBy)
	assert.Empty(t, stored.proposed.card.Answers)
	assert.Nil(t, stored.proposed.card.Approval, "the proposed revision carries no approval")

	// R1 -> R2 -> R3, where R3 repeats R1's content exactly.
	question := func(q string) []*gensurla.CP1AnalysisAmbiguity {
		return []*gensurla.CP1AnalysisAmbiguity{{Question: q, Options: []string{"item_subtotal", "order_total"}}}
	}
	r1 := mustPropose(t, cp, alice, "rev", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant.", question("Is the limit the item subtotal or the order total?")[0]))
	require.Equal(t, StatusNeedsConfirmation, r1.Status)
	check01 := "check_01"
	program := `root = DecisionWorkspace("Refund limit", [choice])
choice = AmbiguityChoiceCard(1, "Shipping changes who waits")`
	r2, err := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
		ChangeID: "rev", RequestRevision: r1.RequestRevision, Prompt: "Refunds up to $50 are instant; shipping is open.",
		Analysis: &gensurla.CP1RevisionAnalysis{
			Summary:     "Refunds up to $50 are instant; whether shipping counts is open.",
			Ambiguities: question("Does shipping count toward the limit?"),
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID: &check01, Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
				ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
			}},
			RemovedExampleIds:  []string{},
			PreservedBehaviors: []string{"Cross-tenant order lookups return 404 Not Found."},
			UIComposition:      &program,
		},
	})
	require.NoError(t, err)
	require.Len(t, r2.Ambiguities, 1)
	assert.Equal(t, "rev-ambiguity-01", r2.Ambiguities[0].ID, "a revision numbers its questions afresh")
	assert.Equal(t, "Does shipping count toward the limit?", r2.Ambiguities[0].Question)
	require.NotNil(t, r2.UIComposition)
	assert.Equal(t, program, r2.UIComposition.Program)
	assert.Equal(t, r2.RequestRevision, r2.UIComposition.ComposedForRevision, "the composition is bound to the revision it was sent with")
	assert.NotEqual(t, r1.RequestRevision, r2.RequestRevision)

	r3, err := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
		ChangeID: "rev", RequestRevision: r2.RequestRevision, Prompt: r1.Prompt,
		Analysis: &gensurla.CP1RevisionAnalysis{
			Summary:     r1.Summary,
			Ambiguities: question(r1.Ambiguities[0].Question),
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID: &check01, Scenario: r1.Examples[0].Scenario, InputSummary: r1.Examples[0].InputSummary,
				ExpectedOutcome: r1.Examples[0].ExpectedOutcome, VerificationKind: r1.Examples[0].VerificationKind,
			}},
			RemovedExampleIds:  []string{},
			PreservedBehaviors: r1.PreservedBehaviors,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, r1.RequestRevision, r3.RequestRevision, "request_revision is a digest of the card's content alone")
	assert.Nil(t, r3.UIComposition, "a revision sent without a composition carries none")
}

// TestReviseChangeConcurrentRevisionsSaveOnlyOne starts several revisions of
// the same card revision at once. Whatever their interleaving, exactly one is
// saved and every other one is refused as a conflict, because a revision is
// saved only while the card still has the caller's request_revision.
func TestReviseChangeConcurrentRevisionsSaveOnlyOne(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cp := newControlPlane(t, Options{})
	r1 := mustPropose(t, cp, alice, "chg", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))

	const revisions = 8
	start := make(chan struct{})
	results := make([]error, revisions)
	cards := make([]*gensurla.ChangeCardResult, revisions)
	var wg sync.WaitGroup
	for i := range revisions {
		wg.Go(func() {
			<-start
			check01 := "check_01"
			cards[i], results[i] = cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
				ChangeID: "chg", RequestRevision: r1.RequestRevision, Prompt: "Refunds up to $100 are instant.",
				Analysis: &gensurla.CP1RevisionAnalysis{
					Summary:     "Refunds up to $100 are instant.",
					Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
					Examples: []*gensurla.CP1RevisionAnalysisExample{{
						PriorID: &check01, Scenario: "Refund at the limit", InputSummary: "Order total $100.00",
						ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
					}},
					RemovedExampleIds:  []string{},
					PreservedBehaviors: []string{},
				},
			})
		})
	}
	close(start)
	wg.Wait()

	var saved *gensurla.ChangeCardResult
	for i, err := range results {
		if err == nil {
			require.Nil(t, saved, "only one revision of the same request_revision may be saved")
			saved = cards[i]
			continue
		}
		assertServiceError(t, err, "conflict")
	}
	require.NotNil(t, saved)
	current, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, saved.RequestRevision, current.RequestRevision)
}

// TestSavedStoriesStayPairedWithTheirSourceAcrossConfirmAndRevise
// verifies that a saved card file containing adopted Stories loads under its
// pinned request_revision, returns Stories on GetChange, keeps Stories on
// ConfirmIntent without re-evaluating, keeps Stories and saved design_source
// together on ReviseChange when story_refresh_op_id is omitted even after
// workspace design/*.go changes on disk, fails if a saved Story field is
// modified on disk.
func TestSavedStoriesStayPairedWithTheirSourceAcrossConfirmAndRevise(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	wsDir := t.TempDir()
	writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design // refunds\n"))

	seedCP := newControlPlane(t, Options{WorkspaceDir: wsDir})
	opID := seedStoryRefreshForTest(t, seedCP, "alice@example.com", storyRefreshStateSucceeded, sampleAdoptedStories(), "")
	program := "root = DecisionWorkspace(\"Refund limit\", [choice])\nchoice = AmbiguityChoiceCard(1)"
	proposed, err := seedCP.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID:         "chg-story-saved",
		Prompt:           "Refunds up to $50 are instant.",
		StoryRefreshOpID: &opID,
		Analysis: &gensurla.CP1Analysis{
			Summary: "Refunds up to $50 are instant; larger refunds wait for a manager.",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{{
				Question: "Is the limit the item subtotal or the order total?",
				Options:  []string{"item_subtotal", "order_total"},
			}},
			Examples: []*gensurla.CP1AnalysisExample{{
				Scenario:         "Refund at the limit",
				InputSummary:     "Order total $50.00",
				ExpectedOutcome:  "Instant store credit",
				VerificationKind: "deterministic",
			}},
			PreservedBehaviors: []string{"Cross-tenant order lookups return 404 Not Found."},
			UIComposition:      &program,
		},
	})
	require.NoError(t, err)
	savedBytes, err := os.ReadFile(seedCP.cards.path("chg-story-saved"))
	require.NoError(t, err)

	// Modify workspace design/design.go on disk after the card was saved so we
	// prove confirm_intent and revise_change (without story_refresh_op_id)
	// carry over the saved Stories and saved design_source together without
	// re-reading or re-evaluating the workspace.
	writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design // workspace moved on without refreshing\n"))
	cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
	require.NoError(t, os.WriteFile(cp.cards.path("chg-story-saved"), savedBytes, 0o600))

	loaded, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg-story-saved"})
	require.NoError(t, err)
	assert.Equal(t, proposed.RequestRevision, loaded.RequestRevision)
	require.Len(t, loaded.Stories, 1)
	assert.Equal(t, "Order Refunds", loaded.Stories[0].Name)
	require.Len(t, loaded.Stories[0].Flows, 2)
	assert.True(t, loaded.Stories[0].Flows[0].Complete)
	assert.False(t, loaded.Stories[0].Flows[1].Complete)

	confirmed, err := cp.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{
		ChangeID:        "chg-story-saved",
		RequestRevision: proposed.RequestRevision,
		Answers:         map[string]string{"chg-story-saved-ambiguity-01": "item_subtotal"},
		Examples:        loaded.Examples,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfirmed, confirmed.Status)
	assert.Equal(t, loaded.Stories, confirmed.Stories)
	assert.NotEqual(t, proposed.RequestRevision, confirmed.RequestRevision)

	check01 := "check_01"
	revised, err := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-story-saved",
		RequestRevision: confirmed.RequestRevision,
		Prompt:          "Refunds up to $60 are instant.",
		Analysis: &gensurla.CP1RevisionAnalysis{
			Summary:     "Refunds up to $60 of item subtotal are instant; larger refunds wait for a manager.",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID:          &check01,
				Scenario:         "Refund at the limit",
				InputSummary:     "Item subtotal $60.00",
				ExpectedOutcome:  "Instant store credit",
				VerificationKind: "deterministic",
			}},
			RemovedExampleIds:  []string{},
			PreservedBehaviors: []string{"Cross-tenant order lookups return 404 Not Found."},
		},
	})
	require.NoError(t, err)
	proposal := proposalCard(t, revised)
	assert.Equal(t, loaded.Stories, proposal.Stories, "omitting story_refresh_op_id keeps the agreed Story")
	stored, ok, err := cp.cards.load("chg-story-saved")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, stored.proposed)
	assert.Equal(t, `{"design/design.go":"package design // refunds\n"}`, stored.proposed.designSource, "saved design_source carries over with the agreed Story")
	assert.Equal(t, sampleAdoptedStories(), stored.proposed.stories)
	assert.Equal(t, stored.designSource, stored.proposed.designSource, "the confirmed revision keeps its own design source")

	// Modifying any Story field on disk invalidates the saved request_revision.
	tampered := strings.Replace(string(savedBytes), "Store credit is issued immediately", "Cash refund is issued immediately", 1)
	require.NoError(t, os.WriteFile(cp.cards.path("chg-story-saved"), []byte(tampered), 0o600))
	_, err = cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg-story-saved"})
	assert.ErrorContains(t, err, "does not match its request_revision")

}

// TestStoryWithActorsAndNoFlowsSurvivesReloadAndIdenticalRevision proves that
// an unfinished Story stays paired with its source and has a stable revision.
func TestStoryWithActorsAndNoFlowsSurvivesReloadAndIdenticalRevision(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	workspace := t.TempDir()
	writeWorkspaceFiles(t, workspace, validWorkspaceStub("package design // actor draft\n"))
	cardsDir := t.TempDir()
	stories := sampleAdoptedStories()
	stories[0].Flows = nil
	analysis := refundAnalysis("Define the refund participants.", &gensurla.CP1AnalysisAmbiguity{
		Question: "Who reviews a refund?", Options: []string{"manager", "support"},
	})
	first := newControlPlane(t, Options{CardsDir: cardsDir, WorkspaceDir: workspace})
	opID := seedStoryRefreshForTest(t, first, "alice@example.com", storyRefreshStateSucceeded, stories, "")
	card, err := first.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID: "chg-actor-draft", Prompt: "Define the refund participants.",
		StoryRefreshOpID: &opID, Analysis: analysis,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusNeedsConfirmation, card.Status)
	require.Len(t, card.Stories, 1)
	assert.Len(t, card.Stories[0].Actors, 2)
	assert.Empty(t, card.Stories[0].Flows)
	require.NoError(t, first.Close())

	// A later workspace edit cannot change the adopted actor-only Story/source pair.
	writeWorkspaceFiles(t, workspace, validWorkspaceStub("package design // later edit\n"))
	reopened := newControlPlane(t, Options{CardsDir: cardsDir, WorkspaceDir: workspace})
	loaded, err := reopened.GetChange(alice, &gensurla.GetChangePayload{ChangeID: card.ChangeID})
	require.NoError(t, err)
	assert.Equal(t, card, loaded)
	priorID := loaded.Examples[0].ID
	example := analysis.Examples[0]
	revised, err := reopened.ReviseChange(alice, &gensurla.ReviseChangePayload{
		ChangeID: card.ChangeID, RequestRevision: loaded.RequestRevision, Prompt: card.Prompt,
		Analysis: &gensurla.CP1RevisionAnalysis{
			Summary: analysis.Summary, Ambiguities: analysis.Ambiguities,
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID: &priorID, Scenario: example.Scenario, InputSummary: example.InputSummary,
				ExpectedOutcome: example.ExpectedOutcome, VerificationKind: example.VerificationKind,
			}},
			RemovedExampleIds: []string{}, PreservedBehaviors: analysis.PreservedBehaviors,
		},
	})
	require.NoError(t, err)
	assert.Equal(t, card.RequestRevision, revised.RequestRevision)
	assert.Equal(t, loaded.Stories, revised.Stories)
	record, found, err := reopened.cards.load(card.ChangeID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, stories, record.stories)
	assert.Contains(t, record.designSource, "actor draft")
}

// TestAdoptStoryRefreshOnProposeAndRevise verifies Story adoption across Go
// service, HTTP, and MCP calls:
//   - propose_change and revise_change adopt a succeeded story_refresh_op_id by
//     reading one workspace module snapshot, verifying its SnapshotDigest, and
//     encoding design_source from its design/ files;
//   - ChangeCardResult.Stories is returned by propose_change, revise_change,
//     confirm_intent, and get_change;
//   - adding or changing a Story flow, step, actor, or question changes
//     request_revision;
//   - adopting a succeeded refresh with zero Stories equals adopting none
//     (Stories is nil, omitted from the saved card file and revision digest).
func TestAdoptStoryRefreshOnProposeAndRevise(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	wsDir := t.TempDir()
	snapshot := map[string]string{
		"go.mod":                    "module example.com/orders\n\ngo 1.24\n",
		"go.sum":                    "",
		"design/design.go":          "package design // v1\n",
		"internal/policy/policy.go": "package policy\n",
	}
	writeWorkspaceFiles(t, wsDir, snapshot)

	cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{
		"alice-token": {ID: "alice@example.com"},
	})))
	t.Cleanup(srv.Close)

	storiesV1 := sampleAdoptedStories()
	opV1 := seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateSucceeded, storiesV1, "")

	// HTTP POST /changes/propose adopts opV1.
	proposeBody := proposeArgs(t, &gensurla.ProposeChangePayload{
		ChangeID:         "chg-http",
		Prompt:           "Refunds up to $50 are instant.",
		StoryRefreshOpID: &opV1,
		Analysis:         refundAnalysis("Refunds up to $50 are instant."),
	})
	status, body := postJSON(t, srv.URL+"/changes/propose", "alice-token", string(proposeBody))
	require.Equal(t, http.StatusOK, status, body)
	httpProposed, err := gensurlatools.UnmarshalProposeChangeResult([]byte(body))
	require.NoError(t, err)
	require.Len(t, httpProposed.Stories, 1)
	assert.Equal(t, "Order Refunds", httpProposed.Stories[0].Name)

	// Saved design_source encodes only the design/ entries from the verified snapshot.
	storedHTTP, ok, err := cp.cards.load("chg-http")
	require.NoError(t, err)
	require.True(t, ok)
	wantDesignSrc, err := encodeDesignSource(map[string]string{"design/design.go": "package design // v1\n"})
	require.NoError(t, err)
	assert.Equal(t, wantDesignSrc, storedHTTP.designSource)

	// MCP propose_change adopts the same succeeded opV1 on a second control
	// plane with the same change_id and produces the exact same request_revision.
	cpMCP := newControlPlane(t, Options{WorkspaceDir: wsDir})
	opMCPV1 := seedStoryRefreshForTest(t, cpMCP, "alice@example.com", storyRefreshStateSucceeded, storiesV1, "")
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cpMCP), nil)
	res, err := adapter.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{
		Name: "propose_change",
		Arguments: proposeArgs(t, &gensurla.ProposeChangePayload{
			ChangeID:         "chg-http",
			Prompt:           "Refunds up to $50 are instant.",
			StoryRefreshOpID: &opMCPV1,
			Analysis:         refundAnalysis("Refunds up to $50 are instant."),
		}),
	})
	require.NoError(t, err)
	complete, ok := res.Outcome.AsComplete()
	require.True(t, ok)
	require.False(t, complete.IsError != nil && *complete.IsError, "unexpected MCP error: %+v", complete.Content)
	mcpProposed, err := gensurlatools.UnmarshalProposeChangeResult(complete.StructuredContent)
	require.NoError(t, err)
	assert.Equal(t, httpProposed.RequestRevision, mcpProposed.RequestRevision)
	assert.Equal(t, httpProposed.Stories, mcpProposed.Stories)

	// Changing a flow (making high-value-refund complete with an outcome step)
	// changes request_revision even when prompt, analysis, and design_source
	// are identical.
	storiesV2 := sampleAdoptedStories()
	storiesV2[0].Flows[1].Complete = true
	storiesV2[0].Flows[1].Steps = append(storiesV2[0].Flows[1].Steps, designeval.StoryStep{
		Key:      "refund-approved",
		ActorKey: "manager",
		Summary:  "Manager approves and issues the refund",
		Kind:     "outcome",
	})
	opV2 := seedStoryRefreshForTest(t, cpMCP, "alice@example.com", storyRefreshStateSucceeded, storiesV2, "")
	check01 := "check_01"
	revAnalysis := &gensurla.CP1RevisionAnalysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
		Examples: []*gensurla.CP1RevisionAnalysisExample{{
			PriorID:          &check01,
			Scenario:         "Refund at the limit",
			InputSummary:     "Order total $50.00",
			ExpectedOutcome:  "Instant store credit",
			VerificationKind: "deterministic",
		}},
		RemovedExampleIds:  []string{},
		PreservedBehaviors: []string{"Cross-tenant order lookups return 404 Not Found."},
	}

	// MCP revise_change adopts opV2 on chg-http in cpMCP.
	res, err = adapter.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{
		Name: "revise_change",
		Arguments: reviseArgs(t, &gensurla.ReviseChangePayload{
			ChangeID:         "chg-http",
			RequestRevision:  mcpProposed.RequestRevision,
			Prompt:           "Refunds up to $50 are instant.",
			StoryRefreshOpID: &opV2,
			Analysis:         revAnalysis,
		}),
	})
	require.NoError(t, err)
	complete, ok = res.Outcome.AsComplete()
	require.True(t, ok)
	require.False(t, complete.IsError != nil && *complete.IsError)
	mcpRevised, err := gensurlatools.UnmarshalReviseChangeResult(complete.StructuredContent)
	require.NoError(t, err)
	require.Len(t, mcpRevised.Stories, 1)
	assert.True(t, mcpRevised.Stories[0].Flows[1].Complete)
	assert.Len(t, mcpRevised.Stories[0].Flows[1].Steps, 3)

	// Revising chg-http in cp without story_refresh_op_id keeps storiesV1, so its
	// request_revision differs from mcpRevised (which adopted storiesV2).
	keptV1, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-http",
		RequestRevision: httpProposed.RequestRevision,
		Prompt:          "Refunds up to $50 are instant.",
		Analysis:        revAnalysis,
	})
	require.NoError(t, err)
	assert.False(t, keptV1.Stories[0].Flows[1].Complete)
	assert.NotEqual(t, keptV1.RequestRevision, mcpRevised.RequestRevision, "adding a flow step changes request_revision")

	// Adopting a succeeded refresh with zero Stories equals adopting none on
	// both propose_change and revise_change for the same change_id.
	opZero := seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateSucceeded, nil, "")
	proposedZero, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID:         "chg-zero",
		Prompt:           "Refunds up to $50 are instant.",
		StoryRefreshOpID: &opZero,
		Analysis:         refundAnalysis("Refunds up to $50 are instant."),
	})
	require.NoError(t, err)
	assert.Nil(t, proposedZero.Stories)
	proposedPlain := mustPropose(t, cpMCP, aliceCtx, "chg-zero", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	assert.Equal(t, proposedPlain.RequestRevision, proposedZero.RequestRevision)
	rawZeroBytes, err := os.ReadFile(cp.cards.path("chg-zero"))
	require.NoError(t, err)
	assert.Contains(t, string(rawZeroBytes), `"stories":null`)

	revisedToZero, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:         "chg-http",
		RequestRevision:  keptV1.RequestRevision,
		Prompt:           "Refunds up to $50 are instant.",
		StoryRefreshOpID: &opZero,
		Analysis:         revAnalysis,
	})
	require.NoError(t, err)
	assert.Nil(t, revisedToZero.Stories)
	cpPlain := newControlPlane(t, Options{WorkspaceDir: wsDir})
	plainHTTP := mustPropose(t, cpPlain, aliceCtx, "chg-http", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	revisedPlain, err := cpPlain.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-http",
		RequestRevision: plainHTTP.RequestRevision,
		Prompt:          "Refunds up to $50 are instant.",
		Analysis:        revAnalysis,
	})
	require.NoError(t, err)
	assert.Equal(t, revisedPlain.RequestRevision, revisedToZero.RequestRevision)
}

// TestAdoptStoryRefreshRejectionsLeaveCardStorageUnchanged verifies every
// invalid story_refresh_op_id across
// propose_change and revise_change: each returns an actionable unprocessable
// error naming refresh_story and leaves card storage unchanged.
func TestAdoptStoryRefreshRejectionsLeaveCardStorageUnchanged(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	check01 := "check_01"
	revAnalysis := &gensurla.CP1RevisionAnalysis{
		Summary:     "Refunds up to $60 are instant.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
		Examples: []*gensurla.CP1RevisionAnalysisExample{{
			PriorID:          &check01,
			Scenario:         "Refund at the limit",
			InputSummary:     "Order total $60.00",
			ExpectedOutcome:  "Instant store credit",
			VerificationKind: "deterministic",
		}},
		RemovedExampleIds:  []string{},
		PreservedBehaviors: []string{"Cross-tenant order lookups return 404 Not Found."},
	}
	newWS := func(t *testing.T) (string, *ControlPlane, *gensurla.ChangeCardResult) {
		t.Helper()
		wsDir := t.TempDir()
		writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design\n"))
		cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
		card := mustPropose(t, cp, aliceCtx, "chg-existing", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
		return wsDir, cp, card
	}

	cases := []struct {
		name  string
		setup func(t *testing.T) (cp *ControlPlane, opID string, designSrc *string, existingCard *gensurla.ChangeCardResult)
		want  string
	}{
		{
			name: "no prior refresh_story",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				_, cp, card := newWS(t)
				return cp, "story-refresh-1", nil, card
			},
			want: "is not the current Story refresh operation for this project and workspace; call refresh_story",
		},
		{
			name: "blank story_refresh_op_id",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				_, cp, card := newWS(t)
				return cp, "   ", nil, card
			},
			want: "story_refresh_op_id must name a succeeded refresh_story operation; call refresh_story",
		},
		{
			name: "replaced story_refresh_op_id",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				_, cp, card := newWS(t)
				firstOp := seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateSucceeded, sampleAdoptedStories(), "")
				_ = seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateSucceeded, sampleAdoptedStories(), "")
				return cp, firstOp, nil, card
			},
			want: "is not the current Story refresh operation for this project and workspace; call refresh_story",
		},
		{
			name: "another principal's story_refresh_op_id",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				_, cp, card := newWS(t)
				bobOp := seedStoryRefreshForTest(t, cp, "bob@example.com", storyRefreshStateSucceeded, sampleAdoptedStories(), "")
				return cp, bobOp, nil, card
			},
			want: "is not the current Story refresh operation for this project and workspace; call refresh_story",
		},
		{
			name: "running refresh_story operation",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				_, cp, card := newWS(t)
				opID := seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateRunning, nil, "")
				return cp, opID, nil, card
			},
			want: `has state "running", not succeeded; wait for get_story_refresh to report succeeded or call refresh_story again`,
		},
		{
			name: "failed refresh_story operation",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				_, cp, card := newWS(t)
				opID := seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateFailed, nil, "syntax error")
				return cp, opID, nil, card
			},
			want: `has state "failed", not succeeded; wait for get_story_refresh to report succeeded or call refresh_story again`,
		},
		{
			name: "no workspace bound",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				_, cp, card := newWS(t)
				opID := seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateSucceeded, sampleAdoptedStories(), "")
				cp.workspaceMu.Lock()
				cp.activeWorkspaceDir, cp.boundWorkspaceDev, cp.boundWorkspaceIno = "", 0, 0
				cp.workspaceMu.Unlock()
				return cp, opID, nil, card
			},
			want: "no workspace directory is bound to this control plane; start Surla from a project directory, then call refresh_story",
		},
		{
			name: "different workspace bound after refresh_story",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				_, cp, card := newWS(t)
				opID := seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateSucceeded, sampleAdoptedStories(), "")
				ws2 := t.TempDir()
				writeWorkspaceFiles(t, ws2, validWorkspaceStub("package design // ws2\n"))
				cleanWS2, dev, ino, err := openWorkspaceRootAndStat(ws2)
				require.NoError(t, err)
				cp.workspaceMu.Lock()
				cp.activeWorkspaceDir, cp.boundWorkspaceDev, cp.boundWorkspaceIno = cleanWS2, dev, ino
				cp.workspaceMu.Unlock()
				return cp, opID, nil, card
			},
			want: "is not the current Story refresh operation for this project and workspace; call refresh_story",
		},
		{
			name: "rebound workspace inode at the same path",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				wsDir := filepath.Join(t.TempDir(), "ws")
				writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design\n"))
				cp := newControlPlane(t, Options{WorkspaceDir: wsDir})
				card := mustPropose(t, cp, aliceCtx, "chg-existing", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
				opID := seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateSucceeded, sampleAdoptedStories(), "")
				require.NoError(t, os.RemoveAll(wsDir))
				writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design\n"))
				cleanWS, dev, ino, err := openWorkspaceRootAndStat(wsDir)
				require.NoError(t, err)
				cp.workspaceMu.Lock()
				cp.activeWorkspaceDir, cp.boundWorkspaceDev, cp.boundWorkspaceIno = cleanWS, dev, ino
				cp.workspaceMu.Unlock()
				return cp, opID, nil, card
			},
			want: "is not the current Story refresh operation for this project and workspace; call refresh_story",
		},
		{
			name: "workspace design changed after refresh_story",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				wsDir, cp, card := newWS(t)
				opID := seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateSucceeded, sampleAdoptedStories(), "")
				writeWorkspaceFiles(t, wsDir, validWorkspaceStub("package design // after\n"))
				return cp, opID, nil, card
			},
			want: "no longer matches story_refresh_op_id",
		},
		{
			name: "workspace non-design Go file changed after refresh_story",
			setup: func(t *testing.T) (*ControlPlane, string, *string, *gensurla.ChangeCardResult) {
				wsDir, cp, card := newWS(t)
				files := validWorkspaceStub("package design\n")
				files["internal/policy/policy.go"] = "package policy // v1\n"
				writeWorkspaceFiles(t, wsDir, files)
				opID := seedStoryRefreshForTest(t, cp, "alice@example.com", storyRefreshStateSucceeded, sampleAdoptedStories(), "")
				files["internal/policy/policy.go"] = "package policy // v2\n"
				writeWorkspaceFiles(t, wsDir, files)
				return cp, opID, nil, card
			},
			want: "no longer matches story_refresh_op_id",
		},
	}

	for _, tc := range cases {
		t.Run("propose_change/"+tc.name, func(t *testing.T) {
			cp, opID, _, _ := tc.setup(t)
			_, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
				ChangeID: "chg-new", Prompt: "Refunds up to $50 are instant.",
				StoryRefreshOpID: &opID,
				Analysis:         refundAnalysis("Refunds up to $50 are instant."),
			})
			assertServiceError(t, err, "unprocessable")
			assert.ErrorContains(t, err, "refresh_story")
			assert.ErrorContains(t, err, tc.want)
			_, saved, loadErr := cp.cards.load("chg-new")
			require.NoError(t, loadErr)
			assert.False(t, saved, "rejected propose_change must not save a card")
		})

		t.Run("revise_change/"+tc.name, func(t *testing.T) {
			cp, opID, _, existing := tc.setup(t)
			beforeBytes, err := os.ReadFile(cp.cards.path(existing.ChangeID))
			require.NoError(t, err)
			_, err = cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
				ChangeID: existing.ChangeID, RequestRevision: existing.RequestRevision,
				Prompt:           "Refunds up to $60 are instant.",
				StoryRefreshOpID: &opID, Analysis: revAnalysis,
			})
			assertServiceError(t, err, "unprocessable")
			assert.ErrorContains(t, err, "refresh_story")
			assert.ErrorContains(t, err, tc.want)
			afterBytes, readErr := os.ReadFile(cp.cards.path(existing.ChangeID))
			require.NoError(t, readErr)
			assert.Equal(t, string(beforeBytes), string(afterBytes), "rejected revise_change must leave the saved card unchanged")
		})
	}

}

// seedStoryRefreshForTest records a Story refresh operation for owner
// against cp's currently bound workspace snapshot and returns its op_id.
func seedStoryRefreshForTest(
	t *testing.T,
	cp *ControlPlane,
	owner, state string,
	stories []designeval.StorySummary,
	diagnostic string,
) string {
	t.Helper()
	wsDir, wsDev, wsIno := cp.currentWorkspaceTarget()
	require.NotEmpty(t, wsDir)
	files, err := readWorkspaceSnapshotForStoryRefresh(wsDir, wsDev, wsIno)
	require.NoError(t, err)
	digest := designeval.SnapshotDigest(files)
	opID, err := newStoryRefreshOpID()
	require.NoError(t, err)

	key := storyRefreshKey{
		owner:        owner,
		workspaceDir: wsDir,
		workspaceDev: wsDev,
		workspaceIno: wsIno,
	}
	cp.storyRefreshMu.Lock()
	defer cp.storyRefreshMu.Unlock()
	if cp.storyRefreshes == nil {
		cp.storyRefreshes = make(map[storyRefreshKey]*storyRefreshOp)
	}
	cp.storyRefreshes[key] = &storyRefreshOp{
		id:             opID,
		state:          state,
		snapshotDigest: digest,
		stories:        cloneStorySummaries(stories),
		diagnostic:     diagnostic,
	}
	return opID
}

// sampleAdoptedStories returns a two-flow domain Story summary fixture with
// actors, complete and incomplete flows, and an open step question.
func sampleAdoptedStories() []designeval.StorySummary {
	return []designeval.StorySummary{{
		Name:        "Order Refunds",
		Description: "Domain timeline for customer refund requests.",
		Actors: []designeval.StoryActor{
			{Key: "customer", Title: "Customer", Description: "Places orders and requests refunds."},
			{Key: "manager", Title: "Support Manager"},
		},
		Flows: []designeval.StoryFlow{
			{
				Key:         "instant-refund",
				Title:       "Instant refund under the limit",
				Description: "Refunds under $50 complete automatically.",
				Complete:    true,
				Steps: []designeval.StoryStep{
					{Key: "request-refund", ActorKey: "customer", Summary: "Customer requests a $25 refund", Kind: "start"},
					{Key: "refund-paid", ActorKey: "customer", Summary: "Store credit is issued immediately", Kind: "outcome"},
				},
			},
			{
				Key:      "high-value-refund",
				Title:    "Refund over the limit",
				Complete: false,
				Steps: []designeval.StoryStep{
					{
						Key:      "request-high-refund",
						ActorKey: "customer",
						Summary:  "Customer requests a $120 refund",
						Kind:     "start",
						Questions: []designeval.StoryQuestion{{
							Key:         "limit-boundary",
							Prompt:      "Is $50.00 itself instant or manager-reviewed?",
							Description: "Boundary rule at $50.00.",
							Options:     []string{"Instant at $50.00", "Manager review at $50.00"},
						}},
					},
					{Key: "review-refund", ActorKey: "manager", Summary: "Manager reviews the refund request", Kind: "step"},
				},
			},
		},
	}}
}

// proposalCard returns the revision proposed beside change as a
// needs_confirmation card, failing the test when none waits. Revising a
// confirmed card returns the change with the new revision in
// ProposedRevision.
func proposalCard(t *testing.T, change *gensurla.ChangeCardResult) *gensurla.ChangeCardResult {
	t.Helper()
	require.NotNil(t, change.ProposedRevision, "revising a confirmed change proposes a revision beside it")
	p := change.ProposedRevision
	return &gensurla.ChangeCardResult{
		ChangeID:           change.ChangeID,
		Status:             StatusNeedsConfirmation,
		Prompt:             p.Prompt,
		Summary:            p.Summary,
		Ambiguities:        p.Ambiguities,
		Examples:           p.Examples,
		PreservedBehaviors: p.PreservedBehaviors,
		Stories:            p.Stories,
		RequestRevision:    p.RequestRevision,
		UIComposition:      p.UIComposition,
	}
}
