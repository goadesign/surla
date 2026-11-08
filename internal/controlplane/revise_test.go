// This file tests Checkpoint 1 (CP1) Change Card revision (`revise_change`)
// and `ModelCP1Analyzer.Revise` across Go, HTTP, and MCP.

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/goldenpath"
	"goa.design/goa-ai/runtime/agent/model"
)

// queuedModelClient is a model.Provider that records every model.Request it
// receives and returns queued responses or errors in order when wrapped by
// model.NewClient.
type queuedModelClient struct {
	mu        sync.Mutex
	requests  []*model.Request
	responses []queuedModelTurn
}

type queuedModelTurn struct {
	resp    *model.Response
	err     error
	started chan struct{}
	release chan struct{}
}

func newQueuedModelClient(t *testing.T, p *queuedModelClient) model.Client {
	t.Helper()
	client, err := model.NewClient(p)
	require.NoError(t, err)
	return client
}

// Complete records req and returns the next queued response or error.
func (c *queuedModelClient) Complete(_ context.Context, req *model.Request) (*model.Response, error) {
	c.mu.Lock()
	c.requests = append(c.requests, req)
	requireLen := len(c.responses)
	var turn queuedModelTurn
	if requireLen > 0 {
		turn = c.responses[0]
		c.responses = c.responses[1:]
	}
	c.mu.Unlock()
	if requireLen == 0 {
		return nil, errors.New("unexpected model.Client.Complete call with empty queue")
	}
	if turn.started != nil {
		close(turn.started)
		<-turn.release
	}
	if turn.err != nil {
		return nil, turn.err
	}
	return turn.resp, nil
}

// Stream is unused by ModelCP1Analyzer.
func (c *queuedModelClient) Stream(context.Context, *model.Request) (model.Streamer, error) {
	return nil, errors.New("stream not implemented")
}

func (c *queuedModelClient) enqueueJSON(t *testing.T, analysis any) {
	t.Helper()
	raw, err := json.Marshal(analysis)
	require.NoError(t, err)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.responses = append(c.responses, queuedModelTurn{
		resp: &model.Response{
			Content: []model.Message{{
				Role:  model.ConversationRoleAssistant,
				Parts: []model.Part{model.TextPart{Text: string(raw)}},
			}},
			StopReason: "stop",
		},
	})
}

func (c *queuedModelClient) lastRequest() *model.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.requests[len(c.requests)-1]
}

// extractPriorCardFromUserPrompt parses the `Prior Change Card:` JSON block out
// of the user message sent by ModelCP1Analyzer.Revise.
func extractPriorCardFromUserPrompt(t *testing.T, req *model.Request) (CP1PriorCard, string, string) {
	t.Helper()
	require.Len(t, req.Messages, 2)
	assert.Equal(t, model.ConversationRoleSystem, req.Messages[0].Role)
	sysPart, ok := req.Messages[0].Parts[0].(model.TextPart)
	require.True(t, ok)
	assert.Equal(t, goldenpath.ArchitectReviseSystemPrompt(), sysPart.Text)
	require.NotNil(t, req.StructuredOutput)
	assert.Equal(t, "cp1_revise_change_card_analysis", req.StructuredOutput.Name)
	assert.Equal(t, cp1ReviseAnalysisJSONSchema, req.StructuredOutput.Schema)
	assert.Equal(t, cp1AnalysisMaxOutputTokens, req.MaxTokens)

	assert.Equal(t, model.ConversationRoleUser, req.Messages[1].Role)
	userPart, ok := req.Messages[1].Parts[0].(model.TextPart)
	require.True(t, ok)
	text := userPart.Text
	require.True(t, strings.HasPrefix(text, "Prior Change Card:\n"))
	rest := strings.TrimPrefix(text, "Prior Change Card:\n")
	priorJSON, rest, ok := strings.Cut(rest, "\n\nRevised Request:\n")
	require.True(t, ok)
	revisedPrompt, designSrc, ok := strings.Cut(rest, "\n\nDesign:\n")
	require.True(t, ok)

	// Verify no positional ambiguity IDs appear in the prior JSON, and example IDs are preserved.
	assert.NotContains(t, priorJSON, "-ambiguity-")
	assert.Contains(t, priorJSON, `"id":"check_01"`)

	var prior CP1PriorCard
	dec := json.NewDecoder(strings.NewReader(priorJSON))
	dec.DisallowUnknownFields()
	require.NoError(t, dec.Decode(&prior))
	return prior, revisedPrompt, designSrc
}

// TestModelCP1AnalyzerReviseRequestContractAndSemanticCases verifies the model
// request contract of ModelCP1Analyzer.Revise and post-model CP1 revision
// invariants using scripted model responses:
//   - Case 1: unrelated addition on a confirmed card preserves prior confirmed
//     decisions and operator-edited examples in `CP1PriorCard`, folds them into
//     `summary`, `examples`, and `preserved_behaviors`, and enters
//     `needs_confirmation` even when the revised card has 0 open ambiguities.
//   - Case 2: explicit replacement supersedes a prior confirmed decision.
//   - Case 3: an unanswered prior question (`confirmed_answer == ""`) remains
//     in `ambiguities` with fresh 1..N code-assigned IDs.
//   - Case 4: successive revisions `R1 -> R2 -> R3` carry `R1`'s surviving
//     decisions and `R2`'s surviving examples forward into `R3` and subsequent
//     `CP1PriorCard` revision inputs through `R2`'s authoritative `summary`,
//     `examples`, and `preserved_behaviors`, while `R2`'s `ui_composition` is
//     re-bound to the new revision digest.
func TestModelCP1AnalyzerReviseRequestContractAndSemanticCases(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	const commit = "0123456789abcdef0123456789abcdef01234567"
	client := &queuedModelClient{}
	inputs := PromotionInputs{
		SuiteID: "orders_acceptance", SuiteCommit: "suite-1", Environment: "sim",
		Artifacts: map[string]string{"orders": "d1"},
	}
	target := &promotionTargetStub{inputs: inputs}
	cp := newControlPlane(t, Options{
		Analyzer:        NewModelCP1Analyzer(newQueuedModelClient(t, client)),
		PromotionTarget: target,
	})

	// Step 1: Propose R1 with 2 ambiguities.
	client.enqueueJSON(t, CP1SemanticAnalysis{
		Summary: "Refunds up to $50 are instant.",
		Ambiguities: []CP1ModelAmbiguity{
			{Question: "Does the $50 limit include tax?", Options: []string{"subtotal_only", "total_with_tax"}},
			{Question: "Who approves refunds over $50?", Options: []string{"store_manager", "finance_lead"}},
		},
		Examples: []CP1ModelExample{
			{Scenario: "Refund at limit", InputSummary: "$50 subtotal", ExpectedOutcome: "Instant refund", VerificationKind: "deterministic"},
		},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
	designV1 := "package design\n// v1"
	r1Draft, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID:     "chg-rev",
		Prompt:       "Refunds up to $50 are instant.",
		DesignSource: &designV1,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusNeedsConfirmation, r1Draft.Status)
	require.Len(t, r1Draft.Ambiguities, 2)

	// Case 3: Revise R1 while still in needs_confirmation (unanswered questions have ConfirmedAnswer == "").
	uiProgR1b := "root = DecisionWorkspace(\"Refund Policy\", [card1])\ncard1 = AmbiguityChoiceCard(1, \"Choose tax basis\")"
	client.enqueueJSON(t, CP1RevisionSemanticAnalysis{
		Summary: "Refunds up to $50 are instant and emit an audit event.",
		Ambiguities: []CP1ModelAmbiguity{
			{Question: "Does the $50 limit include tax?", Options: []string{"subtotal_only", "total_with_tax"}},
			{Question: "Who approves refunds over $50?", Options: []string{"store_manager", "finance_lead"}},
		},
		Examples: []CP1RevisionModelExample{
			{PriorID: "check_01", Scenario: "Refund at limit", InputSummary: "$50 subtotal", ExpectedOutcome: "Instant refund with audit event", VerificationKind: "deterministic"},
		},
		RemovedExampleIDs:  []string{},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
		UIComposition:      &uiProgR1b,
	})
	r1RevisedUnconfirmed, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-rev",
		RequestRevision: r1Draft.RequestRevision,
		Prompt:          "Refunds up to $50 are instant and emit an audit event.",
	})
	require.NoError(t, err)
	priorUnconfirmed, sentPrompt, sentDesign := extractPriorCardFromUserPrompt(t, client.lastRequest())
	assert.Equal(t, StatusNeedsConfirmation, priorUnconfirmed.Status)
	assert.Equal(t, "Refunds up to $50 are instant and emit an audit event.", sentPrompt)
	assert.Equal(t, designV1, sentDesign, "omitted design_source preserves the card's existing design_source")
	require.Len(t, priorUnconfirmed.Decisions, 2)
	assert.Equal(t, "", priorUnconfirmed.Decisions[0].ConfirmedAnswer)
	assert.Equal(t, "", priorUnconfirmed.Decisions[1].ConfirmedAnswer)
	require.NotNil(t, r1RevisedUnconfirmed.UIComposition)
	assert.Equal(t, r1RevisedUnconfirmed.RequestRevision, r1RevisedUnconfirmed.UIComposition.ComposedForRevision)

	// Confirm R1b with operator answers and an operator-edited example, then record a CP5 approval.
	editedExample := *r1RevisedUnconfirmed.Examples[0]
	editedExample.ExpectedOutcome = "Instant store credit with audit event"
	r1Confirmed, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID:        "chg-rev",
		RequestRevision: r1RevisedUnconfirmed.RequestRevision,
		Answers: map[string]string{
			"chg-rev-ambiguity-01": "subtotal_only",
			"chg-rev-ambiguity-02": "store_manager",
		},
		Examples: []*gensurla.ExampleItem{&editedExample},
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfirmed, r1Confirmed.Status)
	require.NotNil(t, r1Confirmed.ConfirmedBy)

	target.report = passingReport(t, r1Confirmed.RequestRevision, commit, inputs)
	r1Approved, err := cp.ApproveMerge(aliceCtx, &gensurla.ApproveMergePayload{
		ChangeID:        "chg-rev",
		RequestRevision: r1Confirmed.RequestRevision,
		CandidateCommit: commit,
		ReportDigest:    target.report.Digest,
	})
	require.NoError(t, err)
	require.NotNil(t, r1Approved.Approval)

	// Case 1: Unrelated addition on confirmed R1 -> R2 has 0 ambiguities, folds
	// confirmed decisions into summary/examples/preserved_behaviors, drops the
	// prior CP5 approval, and enters needs_confirmation.
	client.enqueueJSON(t, CP1RevisionSemanticAnalysis{
		Summary:     "Refunds up to $50 (subtotal only) are instant; refunds over $50 require store_manager approval; all refunds send a webhook.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1RevisionModelExample{
			{PriorID: "check_01", Scenario: "Refund at limit", InputSummary: "$50 subtotal", ExpectedOutcome: "Instant store credit with audit event and webhook", VerificationKind: "deterministic"},
		},
		RemovedExampleIDs: []string{},
		PreservedBehaviors: []string{
			"Cross-tenant lookups return 404.",
			"The $50 instant-refund limit applies to subtotal_only.",
			"Refunds over $50 require store_manager approval.",
		},
	})
	r2, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-rev",
		RequestRevision: r1Approved.RequestRevision,
		Prompt:          "Refunds up to $50 are instant, emit an audit event, and send a webhook.",
	})
	require.NoError(t, err)
	priorFromR1, _, _ := extractPriorCardFromUserPrompt(t, client.lastRequest())
	assert.Equal(t, StatusConfirmed, priorFromR1.Status)
	require.Len(t, priorFromR1.Decisions, 2)
	assert.Equal(t, "subtotal_only", priorFromR1.Decisions[0].ConfirmedAnswer)
	assert.Equal(t, "store_manager", priorFromR1.Decisions[1].ConfirmedAnswer)
	require.Len(t, priorFromR1.Examples, 1)
	assert.Equal(t, "Instant store credit with audit event", priorFromR1.Examples[0].ExpectedOutcome, "operator-edited example must be passed in Prior Change Card")

	assert.Equal(t, StatusNeedsConfirmation, r2.Status, "0-ambiguity ReviseChange must still enter needs_confirmation")
	assert.Nil(t, r2.ConfirmedBy, "revised card must clear ConfirmedBy until confirmed")
	assert.Nil(t, r2.Approval, "revised card must not carry the prior revision's CP5 approval")
	assert.Nil(t, r2.UnverifiedApproval)
	assert.Empty(t, r2.Ambiguities)
	assert.Empty(t, r2.Answers)

	// Confirm R2 (0 ambiguities -> StatusConfirmed).
	r2Confirmed, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID:        "chg-rev",
		RequestRevision: r2.RequestRevision,
		Answers:         map[string]string{},
		Examples:        r2.Examples,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfirmed, r2Confirmed.Status)
	require.NotNil(t, r2Confirmed.ConfirmedBy)

	// Case 2 & Case 4: Successive revision R2 -> R3 explicitly replaces the
	// store_manager decision with vp_finance while R2 had 0 Ambiguities; R3's
	// Prior Change Card carries R1's settled rules in R2's summary, examples,
	// and preserved_behaviors, and R3 preserves R2's surviving <= $50 example
	// alongside the updated > $50 example.
	designV2 := "package design\n// v2"
	client.enqueueJSON(t, CP1RevisionSemanticAnalysis{
		Summary: "Refunds up to $50 (subtotal only) are instant; refunds over $50 require vp_finance approval; all refunds send a webhook.",
		Ambiguities: []CP1ModelAmbiguity{
			{Question: "Should vp_finance approval expire after 24h?", Options: []string{"expire_24h", "no_expiry"}},
		},
		Examples: []CP1RevisionModelExample{
			{PriorID: "check_01", Scenario: "Refund at limit", InputSummary: "$50 subtotal", ExpectedOutcome: "Instant store credit with audit event and webhook", VerificationKind: "deterministic"},
			{Scenario: "Refund over limit", InputSummary: "$75 subtotal", ExpectedOutcome: "Pending vp_finance approval", VerificationKind: "deterministic"},
		},
		RemovedExampleIDs: []string{},
		PreservedBehaviors: []string{
			"Cross-tenant lookups return 404.",
			"The $50 instant-refund limit applies to subtotal_only.",
		},
	})
	r3, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-rev",
		RequestRevision: r2Confirmed.RequestRevision,
		Prompt:          "Route refunds over $50 to vp_finance instead of store_manager.",
		DesignSource:    &designV2,
	})
	require.NoError(t, err)
	priorFromR2, _, sentDesignV2 := extractPriorCardFromUserPrompt(t, client.lastRequest())
	assert.Equal(t, designV2, sentDesignV2)
	assert.Empty(t, priorFromR2.Decisions, "R2 had 0 open ambiguities")
	assert.Contains(t, priorFromR2.Summary, "subtotal only")
	assert.Contains(t, priorFromR2.PreservedBehaviors, "The $50 instant-refund limit applies to subtotal_only.")
	require.Len(t, r3.Ambiguities, 1)
	assert.Equal(t, "chg-rev-ambiguity-01", r3.Ambiguities[0].ID)
	require.Len(t, r3.Examples, 2)
	assert.Equal(t, "check_01", r3.Examples[0].ID)
	assert.Equal(t, "Instant store credit with audit event and webhook", r3.Examples[0].ExpectedOutcome)
	assert.Equal(t, "check_02", r3.Examples[1].ID)
	assert.Equal(t, "Pending vp_finance approval", r3.Examples[1].ExpectedOutcome)

	// Pure content digest invariant: revising R3 back to the exact R1Draft
	// content and design_source produces the exact same RequestRevision as R1Draft,
	// and passes both surviving R3 examples in the Prior Change Card input.
	client.enqueueJSON(t, CP1RevisionSemanticAnalysis{
		Summary: "Refunds up to $50 are instant.",
		Ambiguities: []CP1ModelAmbiguity{
			{Question: "Does the $50 limit include tax?", Options: []string{"subtotal_only", "total_with_tax"}},
			{Question: "Who approves refunds over $50?", Options: []string{"store_manager", "finance_lead"}},
		},
		Examples: []CP1RevisionModelExample{
			{PriorID: "check_01", Scenario: "Refund at limit", InputSummary: "$50 subtotal", ExpectedOutcome: "Instant refund", VerificationKind: "deterministic"},
		},
		RemovedExampleIDs:  []string{"check_02"},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
	rRestored, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-rev",
		RequestRevision: r3.RequestRevision,
		Prompt:          "Refunds up to $50 are instant.",
		DesignSource:    &designV1,
	})
	require.NoError(t, err)
	priorFromR3, _, _ := extractPriorCardFromUserPrompt(t, client.lastRequest())
	require.Len(t, priorFromR3.Examples, 2, "R3's surviving <= $50 example and updated > $50 example must both reach the subsequent revision input")
	assert.Equal(t, "Instant store credit with audit event and webhook", priorFromR3.Examples[0].ExpectedOutcome)
	assert.Equal(t, "Pending vp_finance approval", priorFromR3.Examples[1].ExpectedOutcome)
	assert.Equal(t, r1Draft.RequestRevision, rRestored.RequestRevision, "RequestRevision must be a pure content digest")
}

// TestReviseChangeTransportsConcurrencyAndFailureAtomicity verifies that
// `revise_change` works over HTTP (`POST /changes/revise`) and MCP (`revise_change`
// with `ui://surla/checkpoint` metadata), enforces ownership and optimistic
// concurrency (`request_revision` check both before and after the slow model
// call), and leaves the on-disk card file untouched when the model fails, hits
// `OutputLimited`, returns invalid analysis, or loses a race with a concurrent
// `ConfirmIntent` or `ReviseChange`.
func TestReviseChangeTransportsConcurrencyAndFailureAtomicity(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	bobCtx := WithPrincipal(context.Background(), Principal{ID: "bob@example.com"})

	cardsDir := t.TempDir()
	client := &queuedModelClient{}
	cp := newControlPlane(t, Options{
		CardsDir: cardsDir,
		Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, client)),
	})
	srv := httptest.NewServer(NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{
		"alice-token": {ID: "alice@example.com"},
		"bob-token":   {ID: "bob@example.com"},
	})))
	t.Cleanup(srv.Close)
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)

	// Verify MCP ToolsList advertises revise_change with ToolUI("ui://surla/checkpoint").
	toolsList, err := adapter.ToolsList(aliceCtx, &genmcpadapter.ToolsListPayload{})
	require.NoError(t, err)
	var foundReviseTool bool
	for _, tool := range toolsList.Tools {
		if tool.Name == "revise_change" {
			foundReviseTool = true
			rawMeta, err := json.Marshal(tool.Meta)
			require.NoError(t, err)
			assert.Contains(t, string(rawMeta), "ui://surla/checkpoint")
		}
	}
	require.True(t, foundReviseTool, "MCP ToolsList must include revise_change")

	// Propose initial card (0 ambiguities -> auto-confirmed).
	client.enqueueJSON(t, CP1SemanticAnalysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1ModelExample{
			{Scenario: "Refund at limit", InputSummary: "$50 subtotal", ExpectedOutcome: "Instant refund", VerificationKind: "deterministic"},
		},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
	r1, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-tx",
		Prompt:   "Refunds up to $50 are instant.",
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfirmed, r1.Status, "initial 0-ambiguity ProposeChange auto-confirms")
	require.NotNil(t, r1.ConfirmedBy)

	cardPath := cp.cards.path("chg-tx")
	r1DiskBytes, err := os.ReadFile(cardPath)
	require.NoError(t, err)

	// Entry rejections: unknown change_id, non-owner, stale request_revision.
	_, err = cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID: "missing", RequestRevision: r1.RequestRevision, Prompt: "New prompt",
	})
	assert.ErrorContains(t, err, `unknown change_id "missing"`)

	_, err = cp.ReviseChange(bobCtx, &gensurla.ReviseChangePayload{
		ChangeID: "chg-tx", RequestRevision: r1.RequestRevision, Prompt: "New prompt",
	})
	assert.ErrorContains(t, err, "is not the owner")

	_, err = cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID: "chg-tx", RequestRevision: "rev_stale", Prompt: "New prompt",
	})
	assert.ErrorContains(t, err, "request_revision mismatch")

	// Model failure, OutputLimited, and invalid analysis leave on-disk bytes untouched.
	validR2Bytes, err := json.Marshal(CP1RevisionSemanticAnalysis{
		Summary:     "Refunds up to $100 are instant.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1RevisionModelExample{
			{PriorID: "check_01", Scenario: "Refund at $100", InputSummary: "$100 subtotal", ExpectedOutcome: "Instant refund", VerificationKind: "deterministic"},
		},
		RemovedExampleIDs:  []string{},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
	require.NoError(t, err)
	client.mu.Lock()
	client.responses = append(client.responses,
		queuedModelTurn{err: errors.New("upstream model timeout")},
		queuedModelTurn{resp: &model.Response{
			OutputLimited: true,
			StopReason:    "max_tokens",
			Content: []model.Message{{
				Role:  model.ConversationRoleAssistant,
				Parts: []model.Part{model.TextPart{Text: string(validR2Bytes)}},
			}},
		}},
		queuedModelTurn{resp: &model.Response{
			Content: []model.Message{{
				Role:  model.ConversationRoleAssistant,
				Parts: []model.Part{model.TextPart{Text: `{"summary":"x","ambiguities":[{"question":"Q?","options":["dup","dup"]}],"examples":[{"prior_id":"check_01","scenario":"s","input_summary":"i","expected_outcome":"o","verification_kind":"deterministic"}],"removed_example_ids":[],"preserved_behaviors":[]}`}},
			}},
			StopReason: "stop",
		}},
	)
	client.mu.Unlock()

	for _, wantErr := range []string{"upstream model timeout", "output limit", `cp1 analysis rejected (ambiguity "chg-tx-ambiguity-01" has duplicate option at options[1])`} {
		_, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
			ChangeID: "chg-tx", RequestRevision: r1.RequestRevision, Prompt: "New prompt",
		})
		assert.ErrorContains(t, err, wantErr)
		afterBytes, readErr := os.ReadFile(cardPath)
		require.NoError(t, readErr)
		assert.Equal(t, string(r1DiskBytes), string(afterBytes), "failed ReviseChange must not modify saved card bytes")
	}

	// Concurrent ConfirmIntent landing while analyzer.Revise is in flight causes
	// ReviseChange to fail with request_revision mismatch and keep the confirmed card.
	started, release := make(chan struct{}), make(chan struct{})
	client.mu.Lock()
	client.responses = append(client.responses, queuedModelTurn{
		started: started,
		release: release,
		resp: &model.Response{
			Content: []model.Message{{
				Role:  model.ConversationRoleAssistant,
				Parts: []model.Part{model.TextPart{Text: string(validR2Bytes)}},
			}},
			StopReason: "stop",
		},
	})
	client.mu.Unlock()

	reviseErrCh := make(chan error, 1)
	go func() {
		_, rErr := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
			ChangeID: "chg-tx", RequestRevision: r1.RequestRevision, Prompt: "Refunds up to $100 are instant.",
		})
		reviseErrCh <- rErr
	}()
	<-started

	// While ReviseChange is waiting on the model, ConfirmIntent edits the example and advances the revision.
	editedEx := *r1.Examples[0]
	editedEx.ExpectedOutcome = "Instant store credit"
	r1Edited, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg-tx", RequestRevision: r1.RequestRevision, Answers: map[string]string{}, Examples: []*gensurla.ExampleItem{&editedEx},
	})
	require.NoError(t, err)
	require.NotEqual(t, r1.RequestRevision, r1Edited.RequestRevision)

	close(release)
	assert.ErrorContains(t, <-reviseErrCh, "request_revision mismatch")
	gotAfterRace, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-tx"})
	require.NoError(t, err)
	assert.Equal(t, r1Edited.RequestRevision, gotAfterRace.RequestRevision)

	// Concurrent ReviseChange-vs-ReviseChange race: Revise A starts against
	// r1Edited.RequestRevision and blocks in the model call while Revise B
	// completes against r1Edited.RequestRevision and advances the card; when A
	// finishes its model call, its post-model re-check under c.mu.Lock() fails
	// with request_revision mismatch and leaves B's saved card intact.
	startedA, releaseA := make(chan struct{}), make(chan struct{})
	validR2WinnerBytes, err := json.Marshal(CP1RevisionSemanticAnalysis{
		Summary:     "Refunds up to $80 are instant (winner B).",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1RevisionModelExample{
			{PriorID: "check_01", Scenario: "Refund at $80", InputSummary: "$80 subtotal", ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic"},
		},
		RemovedExampleIDs:  []string{},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
	require.NoError(t, err)
	client.mu.Lock()
	client.responses = append(client.responses,
		queuedModelTurn{
			started: startedA,
			release: releaseA,
			resp: &model.Response{
				Content: []model.Message{{
					Role:  model.ConversationRoleAssistant,
					Parts: []model.Part{model.TextPart{Text: string(validR2Bytes)}},
				}},
				StopReason: "stop",
			},
		},
		queuedModelTurn{
			resp: &model.Response{
				Content: []model.Message{{
					Role:  model.ConversationRoleAssistant,
					Parts: []model.Part{model.TextPart{Text: string(validR2WinnerBytes)}},
				}},
				StopReason: "stop",
			},
		},
	)
	client.mu.Unlock()

	reviseAErrCh := make(chan error, 1)
	go func() {
		_, rErr := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
			ChangeID: "chg-tx", RequestRevision: r1Edited.RequestRevision, Prompt: "Refunds up to $100 are instant (stale A).",
		})
		reviseAErrCh <- rErr
	}()
	<-startedA

	r2Winner, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID: "chg-tx", RequestRevision: r1Edited.RequestRevision, Prompt: "Refunds up to $80 are instant (winner B).",
	})
	require.NoError(t, err)
	require.NotEqual(t, r1Edited.RequestRevision, r2Winner.RequestRevision)

	close(releaseA)
	assert.ErrorContains(t, <-reviseAErrCh, "request_revision mismatch")
	gotAfterReviseRace, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-tx"})
	require.NoError(t, err)
	assert.Equal(t, r2Winner.RequestRevision, gotAfterReviseRace.RequestRevision)
	assert.Equal(t, "Refunds up to $80 are instant (winner B).", gotAfterReviseRace.Summary)

	// HTTP POST /changes/revise succeeds against r2Winner.RequestRevision.
	client.enqueueJSON(t, CP1RevisionSemanticAnalysis{
		Summary:     "Refunds up to $100 are instant via HTTP.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1RevisionModelExample{
			{PriorID: "check_01", Scenario: "Refund at $100", InputSummary: "$100 subtotal", ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic"},
		},
		RemovedExampleIDs:  []string{},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
	httpBody, err := json.Marshal(map[string]any{
		"change_id":        "chg-tx",
		"request_revision": r2Winner.RequestRevision,
		"prompt":           "Refunds up to $100 are instant via HTTP.",
	})
	require.NoError(t, err)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/changes/revise", bytes.NewReader(httpBody))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer alice-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var httpCard struct {
		Status          string `json:"status"`
		Summary         string `json:"summary"`
		RequestRevision string `json:"request_revision"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&httpCard))
	assert.Equal(t, StatusNeedsConfirmation, httpCard.Status)
	assert.Equal(t, "Refunds up to $100 are instant via HTTP.", httpCard.Summary)

	// MCP ToolsCall("revise_change") succeeds against httpCard.RequestRevision.
	client.enqueueJSON(t, CP1RevisionSemanticAnalysis{
		Summary:     "Refunds up to $150 are instant via MCP.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1RevisionModelExample{
			{PriorID: "check_01", Scenario: "Refund at $150", InputSummary: "$150 subtotal", ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic"},
		},
		RemovedExampleIDs:  []string{},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
	mcpArgs, err := json.Marshal(map[string]any{
		"change_id":        "chg-tx",
		"request_revision": httpCard.RequestRevision,
		"prompt":           "Refunds up to $150 are instant via MCP.",
	})
	require.NoError(t, err)
	mcpRes, err := adapter.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{Name: "revise_change", Arguments: mcpArgs})
	require.NoError(t, err)
	complete, ok := mcpRes.Outcome.AsComplete()
	require.True(t, ok)
	require.False(t, complete.IsError != nil && *complete.IsError)
	var mcpCard struct {
		Status          string `json:"status"`
		Summary         string `json:"summary"`
		RequestRevision string `json:"request_revision"`
	}
	require.NoError(t, json.Unmarshal(complete.StructuredContent, &mcpCard))
	assert.Equal(t, StatusNeedsConfirmation, mcpCard.Status)
	assert.Equal(t, "Refunds up to $150 are instant via MCP.", mcpCard.Summary)
}
