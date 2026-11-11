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
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
)

// savedCardFromEarlierServer is a card file exactly as an earlier server
// (commit f1c1214 of this repository, which ran its own model to analyze
// requests) saved it: a workspace-backed card in needs_confirmation with one
// open question, one example, a preserved behavior and a decision view. Its
// request_revision was computed by that server.
const savedCardFromEarlierServer = `{"card":{"ChangeID":"chg-saved","Status":"needs_confirmation","Prompt":"Refunds up to $50 are instant.","Summary":"Refunds up to $50 are instant; larger refunds wait for a manager.","Ambiguities":[{"ID":"chg-saved-ambiguity-01","Question":"Is the limit the item subtotal or the order total?","Options":["item_subtotal","order_total"]}],"Answers":null,"Examples":[{"ID":"check_01","Scenario":"Refund at the limit","InputSummary":"Order total $50.00","ExpectedOutcome":"Instant store credit","VerificationKind":"deterministic"}],"PreservedBehaviors":["Cross-tenant order lookups return 404 Not Found."],"ConfirmedBy":null,"RequestRevision":"d90c6f9dea412aa2c9fd60c626bef576e6dfc3a023d36a7f03715db366cef4af","UIComposition":{"Program":"root = DecisionWorkspace(\"Refund limit\", [choice])\nchoice = AmbiguityChoiceCard(1)","ComposedForRevision":"d90c6f9dea412aa2c9fd60c626bef576e6dfc3a023d36a7f03715db366cef4af"},"Approval":null},"owner":"alice@example.com","design_source":"{\"design/design.go\":\"package design // refunds\\n\"}","source_origin":"workspace"}`

// TestASavedCardFromAnEarlierServerStillLoadsAndRevises verifies that a card
// file the earlier server saved loads unchanged under its saved
// request_revision, can be confirmed, and can be revised with a
// caller-supplied analysis that keeps its example; with no workspace bound,
// the workspace-backed card keeps its saved design source.
func TestASavedCardFromAnEarlierServerStillLoadsAndRevises(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	cp := newControlPlane(t, Options{})
	require.NoError(t, os.WriteFile(cp.cards.path("chg-saved"), []byte(savedCardFromEarlierServer), 0o600))

	loaded, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg-saved"})
	require.NoError(t, err)
	const savedRevision = "d90c6f9dea412aa2c9fd60c626bef576e6dfc3a023d36a7f03715db366cef4af"
	assert.Equal(t, savedRevision, loaded.RequestRevision)
	assert.Equal(t, StatusNeedsConfirmation, loaded.Status)
	require.NotNil(t, loaded.UIComposition)
	assert.Equal(t, savedRevision, loaded.UIComposition.ComposedForRevision)

	confirmed, err := cp.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg-saved", RequestRevision: savedRevision,
		Answers: map[string]string{"chg-saved-ambiguity-01": "item_subtotal"}, Examples: loaded.Examples,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfirmed, confirmed.Status)

	check01 := "check_01"
	revised, err := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
		ChangeID: "chg-saved", RequestRevision: confirmed.RequestRevision, Prompt: "Refunds up to $60 are instant.",
		Analysis: &gensurla.CP1RevisionAnalysis{
			Summary:     "Refunds up to $60 of item subtotal are instant; larger refunds wait for a manager.",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1RevisionAnalysisExample{
				{PriorID: &check01, Scenario: "Refund at the limit", InputSummary: "Item subtotal $60.00", ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic"},
				{Scenario: "Refund above the limit", InputSummary: "Item subtotal $60.01", ExpectedOutcome: "Waits for a manager", VerificationKind: "deterministic"},
			},
			RemovedExampleIds:  []string{},
			PreservedBehaviors: []string{"Cross-tenant order lookups return 404 Not Found.", "The limit applies to the item subtotal."},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, StatusNeedsConfirmation, revised.Status)
	assert.Equal(t, []string{"check_01", "check_02"}, []string{revised.Examples[0].ID, revised.Examples[1].ID})
	assert.Empty(t, revised.Answers)
	assert.Nil(t, revised.ConfirmedBy)
	stored, ok, err := cp.cards.load("chg-saved")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, `{"design/design.go":"package design // refunds\n"}`, stored.designSource)
	assert.Equal(t, sourceOriginWorkspace, stored.sourceOrigin)
}

// TestReviseChangeOverHTTPAndMCP verifies revise_change over the Go service,
// HTTP (`POST /changes/revise`) and MCP: the tool links the checkpoint viewer;
// unknown, foreign and stale cards are refused; an analysis that repeats an
// option or does not account for every current example exactly once is
// unprocessable and leaves the saved card file byte for byte unchanged; and a
// complete analysis saves a new revision in needs_confirmation.
func TestReviseChangeOverHTTPAndMCP(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	bobCtx := WithPrincipal(context.Background(), Principal{ID: "bob@example.com"})
	cp := newControlPlane(t, Options{})
	srv := httptest.NewServer(NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
	t.Cleanup(srv.Close)
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)

	toolsList, err := adapter.ToolsList(aliceCtx, &genmcpadapter.ToolsListPayload{})
	require.NoError(t, err)
	var foundReviseTool bool
	for _, tool := range toolsList.Tools {
		if tool.Name == "revise_change" {
			foundReviseTool = true
			assert.Contains(t, string(tool.Meta), "ui://surla/checkpoint")
		}
	}
	require.True(t, foundReviseTool, "MCP ToolsList must include revise_change")

	r1 := mustPropose(t, cp, aliceCtx, "chg-tx", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	require.Equal(t, StatusConfirmed, r1.Status, "a proposal without open questions is confirmed")
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
	var httpCard struct {
		Status          string `json:"status"`
		Summary         string `json:"summary"`
		RequestRevision string `json:"request_revision"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &httpCard))
	assert.Equal(t, StatusNeedsConfirmation, httpCard.Status)
	assert.Equal(t, "Refunds up to $100 are instant via HTTP.", httpCard.Summary)

	// MCP revise_change succeeds against the HTTP revision.
	res, err = adapter.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{Name: "revise_change", Arguments: reviseArgs(t, &gensurla.ReviseChangePayload{
		ChangeID: "chg-tx", RequestRevision: httpCard.RequestRevision, Prompt: "Refunds up to $150 are instant.",
		Analysis: revision("Refunds up to $150 are instant via MCP.", "check_01"),
	})})
	require.NoError(t, err)
	complete, ok = res.Outcome.AsComplete()
	require.True(t, ok)
	require.False(t, complete.IsError != nil && *complete.IsError)
	var mcpCard struct {
		Status  string `json:"status"`
		Summary string `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(complete.StructuredContent, &mcpCard))
	assert.Equal(t, StatusNeedsConfirmation, mcpCard.Status)
	assert.Equal(t, "Refunds up to $150 are instant via MCP.", mcpCard.Summary)
}

// TestReviseChangeConcurrentRevisionsSaveOnlyOne starts several revisions of
// the same card revision at once. Whatever their interleaving, exactly one is
// saved and every other one is refused as a conflict, because a revision is
// saved only while the card still has the caller's request_revision.
func TestReviseChangeConcurrentRevisionsSaveOnlyOne(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
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
