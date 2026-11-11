// Package controlplane tests Checkpoints 1, 2, 3 and 5 of the Surla MCP
// software factory control plane.
package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/goldenpath"
	"github.com/goadesign/surla/internal/swarm"
	"github.com/goadesign/surla/internal/testmodule"
	"goa.design/goa-ai/runtime/agent/prompt"
)

type (
	// boundTargetPromptUpdater binds a target platform's prompt.Store to the
	// TargetPromptUpdater contract for testing CP3 Fast Draft Mode.
	boundTargetPromptUpdater struct {
		// store is the target platform's prompt.Store.
		store prompt.Store
	}
)

// TestCP1AmbiguityResolutionStrictValidationOwnershipAndRevisionLock verifies
// that a control plane with no model configured saves the caller's analysis
// as a Change Card with code-owned question and check IDs, keeps the caller's
// text verbatim, binds the card to the authenticated owner, and locks
// confirmation to the current request_revision.
func TestCP1AmbiguityResolutionStrictValidationOwnershipAndRevisionLock(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	bobCtx := WithPrincipal(context.Background(), Principal{ID: "bob@example.com"})

	ambiguousAnalysis := &gensurla.CP1Analysis{
		Summary: "  Auto-approve refunds under $50 and route larger refunds to manager approval.\n",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{
			{
				Question: "  Does the $50 threshold apply to item subtotal or total order amount including tax? ",
				Options:  []string{"item_subtotal_cents", "total_order_cents"},
			},
			{
				Question: "Should an exact $50.00 refund auto-approve or require manager approval?",
				Options:  []string{"auto_approve_lte_5000", "require_manager_at_5000"},
			},
		},
		Examples: []*gensurla.CP1AnalysisExample{
			{
				Scenario:         "  Refund under $50 auto-approves  ",
				InputSummary:     "Order subtotal $48.00, tax $4.32 ($52.32 total)",
				ExpectedOutcome:  "Auto-approved immediately via payments provider",
				VerificationKind: "deterministic",
			},
			{
				Scenario:         "Exact $50.00 boundary",
				InputSummary:     "Order subtotal $50.00",
				ExpectedOutcome:  "Requires manager approval before payment execution",
				VerificationKind: "deterministic",
			},
			{
				Scenario:         "Polite customer explanation",
				InputSummary:     "Customer asks why $50.00 refund is pending",
				ExpectedOutcome:  "Agent explains manager review policy clearly",
				VerificationKind: "model_judged",
			},
		},
		PreservedBehaviors: []string{
			"  Cross-tenant order lookups return 404 Not Found.  ",
			"Customer principals cannot call approve_refund.",
		},
	}

	// No Live, LiveModelEnv or model configuration: proposing needs none.
	cp := newControlPlane(t, Options{})

	designSrc := `package design // orders service`
	if _, err := cp.ProposeChange(context.Background(), &gensurla.ProposeChangePayload{
		ChangeID:     "chg-refund-50",
		Prompt:       "Refunds under $50 should be automatic, otherwise ask a manager.",
		DesignSource: &designSrc,
		Analysis:     ambiguousAnalysis,
	}); err == nil {
		t.Fatal("expected unauthenticated ProposeChange to fail")
	}

	proposed, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID:     "chg-refund-50",
		Prompt:       "Refunds under $50 should be automatic, otherwise ask a manager.",
		DesignSource: &designSrc,
		Analysis:     ambiguousAnalysis,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusNeedsConfirmation, proposed.Status)
	require.Len(t, proposed.Ambiguities, 2)
	assert.Equal(t, "chg-refund-50-ambiguity-01", proposed.Ambiguities[0].ID)
	assert.Equal(t, "chg-refund-50-ambiguity-02", proposed.Ambiguities[1].ID)
	ids := make([]string, 0, len(proposed.Examples))
	for _, ex := range proposed.Examples {
		ids = append(ids, ex.ID)
	}
	assert.Equal(t, []string{"check_01", "check_02", "check_03"}, ids)
	assert.Nil(t, proposed.ConfirmedBy, "a card with open questions is not confirmed")
	assert.Equal(t, ambiguousAnalysis.Summary, proposed.Summary, "accepted text is kept verbatim, without trimming")
	assert.Equal(t, ambiguousAnalysis.Ambiguities[0].Question, proposed.Ambiguities[0].Question)
	assert.Equal(t, ambiguousAnalysis.Examples[0].Scenario, proposed.Examples[0].Scenario)
	assert.Equal(t, ambiguousAnalysis.PreservedBehaviors, proposed.PreservedBehaviors)
	stored, ok, err := cp.cards.load("chg-refund-50")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, designSrc, stored.designSource)
	assert.Equal(t, sourceOriginSupplied, stored.sourceOrigin)

	// Attempting to overwrite an existing change_id must be rejected.
	_, err = cp.ProposeChange(bobCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-refund-50",
		Prompt:   "Attempt to clobber Alice's change_id",
		Analysis: refundAnalysis("Clobber"),
	})
	assertServiceError(t, err, "conflict")

	answers := map[string]string{
		"chg-refund-50-ambiguity-01": "item_subtotal_cents",
		"chg-refund-50-ambiguity-02": "require_manager_at_5000",
	}
	// Non-owner principal cannot confirm Alice's Change Card.
	_, err = cp.ConfirmIntent(bobCtx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg-refund-50", RequestRevision: proposed.RequestRevision, Answers: answers, Examples: proposed.Examples,
	})
	assertServiceError(t, err, "forbidden")

	// Stale request_revision must be rejected.
	_, err = cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg-refund-50", RequestRevision: "stale-revision", Answers: answers, Examples: proposed.Examples,
	})
	assertServiceError(t, err, "conflict")

	confirmed, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg-refund-50", RequestRevision: proposed.RequestRevision, Answers: answers, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfirmed, confirmed.Status)
	assert.Len(t, confirmed.Ambiguities, 2, "ambiguities are retained for audit")
	assert.Equal(t, answers, confirmed.Answers)
	require.NotNil(t, confirmed.ConfirmedBy)
	assert.Equal(t, "alice@example.com", *confirmed.ConfirmedBy)
	assert.NotEqual(t, proposed.RequestRevision, confirmed.RequestRevision)
}

// TestProposeChangeAutoConfirmsACardWithoutQuestions verifies that an
// analysis with no open questions is saved confirmed by its owner, and that a
// proposal on a server bound to a workspace records that workspace's
// design/*.go as the card's design source.
func TestProposeChangeAutoConfirmsACardWithoutQuestions(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	ws := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(ws, "design"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(ws, "design", "design.go"), []byte("package design // refunds\n"), 0o644))
	cp := newControlPlane(t, Options{CardsDir: t.TempDir(), WorkspaceDir: ws})

	card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID: "chg-instant",
		Prompt:   "Refunds up to $50 are instant.",
		Analysis: refundAnalysis("Refunds up to $50 are instant."),
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfirmed, card.Status)
	require.NotNil(t, card.ConfirmedBy)
	assert.Equal(t, "alice@example.com", *card.ConfirmedBy)
	assert.Empty(t, card.Ambiguities)
	assert.Empty(t, card.Answers)
	assert.Nil(t, card.UIComposition)
	stored, ok, err := cp.cards.load("chg-instant")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, sourceOriginWorkspace, stored.sourceOrigin)
	assert.Equal(t, `{"design/design.go":"package design // refunds\n"}`, stored.designSource)
}

// TestProposeChangeRecordsTheDesignSourceItWasMadeAgainst verifies the design
// source a card records and hashes into request_revision: none when the
// caller sends none and no workspace is bound, design_source byte for byte
// when sent (whitespace included), none for a bound workspace without
// design/*.go (so the card does not block init_project), and the bound
// workspace's design/*.go as a JSON object with HTML characters unescaped.
func TestProposeChangeRecordsTheDesignSourceItWasMadeAgainst(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	whitespace := "  \n\t"
	emptyWorkspace := t.TempDir()
	designWorkspace := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(designWorkspace, "design"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(designWorkspace, "design", "design.go"), []byte("package design // a < b && c > d\n"), 0o644))
	for _, tc := range []struct {
		name       string
		workspace  string
		design     *string
		wantSource string
		wantOrigin string
	}{
		{"no design and no workspace", "", nil, "", sourceOriginSupplied},
		{"whitespace design_source kept as sent", "", &whitespace, whitespace, sourceOriginSupplied},
		{"workspace without design files", emptyWorkspace, nil, "", sourceOriginWorkspace},
		{"workspace design files", designWorkspace, nil, `{"design/design.go":"package design // a < b && c > d\n"}`, sourceOriginWorkspace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cp := newControlPlane(t, Options{CardsDir: t.TempDir(), WorkspaceDir: tc.workspace})
			card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
				ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", DesignSource: tc.design,
				Analysis: refundAnalysis("Refunds up to $50 are instant."),
			})
			require.NoError(t, err)
			stored, ok, err := cp.cards.load("chg")
			require.NoError(t, err)
			require.True(t, ok)
			assert.Equal(t, tc.wantSource, stored.designSource)
			assert.Equal(t, tc.wantOrigin, stored.sourceOrigin)
			rev, err := computeCanonicalRevision(stored.card, "alice@example.com", tc.wantSource)
			require.NoError(t, err)
			assert.Equal(t, rev, card.RequestRevision, "request_revision covers the recorded design source")
			blocked, err := cp.cards.hasWorkspaceCardsWithFiles()
			require.NoError(t, err)
			assert.Equal(t, tc.workspace == designWorkspace, blocked, "only a card that recorded workspace design files blocks init_project")
		})
	}
}

// TestChangeAnalysisIsRejectedAtTheBoundary verifies that the MCP and HTTP
// entries reject an analysis that breaks a structural rule (a missing
// analysis, a blank or missing required field, a null list element, too few
// options or examples, or an unknown verification_kind) or that carries a
// field the contract does not declare (such as a caller-chosen ambiguity ID)
// before the service runs, and that the service rejects the rules that span
// fields (repeated options) as unprocessable. No rejected case saves a card.
func TestChangeAnalysisIsRejectedAtTheBoundary(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	cp := newControlPlane(t, Options{})
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
	srv := httptest.NewServer(NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
	t.Cleanup(srv.Close)

	const example = `{"scenario":"Refund at the limit","input_summary":"Order total $50.00","expected_outcome":"Instant store credit","verification_kind":"deterministic"}`
	analysis := func(fields string) string {
		return `{"change_id":"chg","prompt":"Refunds up to $50 are instant.","analysis":{` + fields + `}}`
	}
	valid := `"summary":"Refunds up to $50 are instant.","ambiguities":[],"examples":[` + example + `],"preserved_behaviors":[]`
	structural := []struct {
		name, payload, want string
	}{
		{"missing analysis", `{"change_id":"chg","prompt":"Refunds up to $50 are instant."}`, "analysis"},
		{"blank summary", analysis(strings.Replace(valid, `"summary":"Refunds up to $50 are instant."`, `"summary":" \n\t"`, 1)), "summary"},
		{"missing summary", analysis(strings.Replace(valid, `"summary":"Refunds up to $50 are instant.",`, ``, 1)), "summary"},
		{"missing ambiguities", analysis(strings.Replace(valid, `"ambiguities":[],`, ``, 1)), "ambiguities"},
		{"missing preserved_behaviors", analysis(strings.Replace(valid, `,"preserved_behaviors":[]`, ``, 1)), "preserved_behaviors"},
		{"no examples", analysis(strings.Replace(valid, `[`+example+`]`, `[]`, 1)), "examples"},
		{"blank expected_outcome", analysis(strings.Replace(valid, `"expected_outcome":"Instant store credit"`, `"expected_outcome":"  "`, 1)), "expected_outcome"},
		{"invalid verification_kind", analysis(strings.Replace(valid, `"deterministic"`, `"bogus_kind"`, 1)), "verification_kind"},
		{"null example", analysis(strings.Replace(valid, `[`+example+`]`, `[null]`, 1)), "examples"},
		{"blank preserved behavior", analysis(strings.Replace(valid, `"preserved_behaviors":[]`, `"preserved_behaviors":[" "]`, 1)), "preserved_behaviors"},
		{"single option", analysis(strings.Replace(valid, `"ambiguities":[]`, `"ambiguities":[{"question":"Is $50.00 itself instant?","options":["instant"]}]`, 1)), "options"},
		{"blank question", analysis(strings.Replace(valid, `"ambiguities":[]`, `"ambiguities":[{"question":"","options":["instant","manager"]}]`, 1)), "question"},
	}
	undeclared := []struct {
		name, payload, want string
	}{
		{"caller-chosen ambiguity id", analysis(strings.Replace(valid, `"ambiguities":[]`, `"ambiguities":[{"id":"m1","question":"Is $50.00 itself instant?","options":["instant","manager"]}]`, 1)), "id"},
		{"unknown analysis field", analysis(valid + `,"unexpected":true`), "unexpected"},
	}
	for _, tc := range append(structural, undeclared...) {
		t.Run("mcp "+tc.name, func(t *testing.T) {
			res, err := adapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{Name: "propose_change", Arguments: json.RawMessage(tc.payload)})
			require.NoError(t, err)
			complete, ok := res.Outcome.AsComplete()
			require.True(t, ok)
			require.NotNil(t, complete.IsError)
			assert.True(t, *complete.IsError)
			require.NotEmpty(t, complete.Content)
			require.NotNil(t, complete.Content[0].Text)
			assert.Contains(t, *complete.Content[0].Text, "invalid arguments")
			assert.Contains(t, *complete.Content[0].Text, tc.want)
		})
	}
	for _, tc := range structural {
		t.Run("http "+tc.name, func(t *testing.T) {
			status, body := postJSON(t, srv.URL+"/changes/propose", "alice-token", tc.payload)
			assert.Equal(t, http.StatusBadRequest, status, body)
			assert.Contains(t, body, tc.want)
		})
	}
	for _, tc := range undeclared {
		t.Run("http refuses "+tc.name, func(t *testing.T) {
			status, body := postJSON(t, srv.URL+"/changes/propose", "alice-token", tc.payload)
			assert.Equal(t, http.StatusBadRequest, status, body)
			var refusal struct {
				Name    string `json:"name"`
				Message string `json:"message"`
				Fault   bool   `json:"fault"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
			assert.Equal(t, "decode_payload", refusal.Name)
			assert.Equal(t, `json: unknown field "`+tc.want+`"`, refusal.Message)
			assert.False(t, refusal.Fault)
		})
	}

	// A repeated option is a rule across list elements: the service rejects
	// it as unprocessable over both transports.
	duplicate := analysis(strings.Replace(valid, `"ambiguities":[]`, `"ambiguities":[{"question":"Is $50.00 itself instant?","options":["instant"," instant "]}]`, 1))
	res, err := adapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{Name: "propose_change", Arguments: json.RawMessage(duplicate)})
	require.NoError(t, err)
	complete, ok := res.Outcome.AsComplete()
	require.True(t, ok)
	require.NotNil(t, complete.IsError)
	assert.True(t, *complete.IsError)
	require.NotNil(t, complete.Content[0].Text)
	assert.Contains(t, *complete.Content[0].Text, `invalid analysis: ambiguity "chg-ambiguity-01" (ambiguities[0]) has duplicate option at options[1]`)
	status, body := postJSON(t, srv.URL+"/changes/propose", "alice-token", duplicate)
	assert.Equal(t, http.StatusUnprocessableEntity, status, body)
	assert.Contains(t, body, "duplicate option")

	_, saved, err := cp.cards.load("chg")
	require.NoError(t, err)
	assert.False(t, saved, "no rejected analysis saves a card")

	// The same complete analysis is accepted over both transports.
	res, err = adapter.ToolsCall(alice, &genmcpadapter.ToolsCallPayload{Name: "propose_change", Arguments: json.RawMessage(analysis(valid))})
	require.NoError(t, err)
	complete, ok = res.Outcome.AsComplete()
	require.True(t, ok)
	require.False(t, complete.IsError != nil && *complete.IsError)
	status, body = postJSON(t, srv.URL+"/changes/propose", "alice-token", strings.Replace(analysis(valid), `"change_id":"chg"`, `"change_id":"chg-http"`, 1))
	assert.Equal(t, http.StatusOK, status, body)
}

// TestHTTPRoutesRefuseUndeclaredJSONFields verifies that strict JSON decoding
// covers every HTTP route that reads a JSON body, not only the analysis
// routes: a configure_model body with a field the design does not declare is
// refused with 400 decode_payload before the service runs, while the same
// body without it reaches the service, which on this host answers 501.
func TestHTTPRoutesRefuseUndeclaredJSONFields(t *testing.T) {
	cp := newControlPlane(t, Options{})
	srv := httptest.NewServer(NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
	t.Cleanup(srv.Close)

	status, body := postJSON(t, srv.URL+"/cp1/model", "alice-token", `{"gateway_addr":"127.0.0.1:9090","timeout":"5s"}`)
	assert.Equal(t, http.StatusBadRequest, status, body)
	var refusal struct {
		Name    string `json:"name"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
	assert.Equal(t, "decode_payload", refusal.Name)
	assert.Equal(t, `json: unknown field "timeout"`, refusal.Message)

	status, body = postJSON(t, srv.URL+"/cp1/model", "alice-token", `{"gateway_addr":"127.0.0.1:9090"}`)
	assert.Equal(t, http.StatusNotImplemented, status, body)
}

// TestGoCallersGetTheDesignRules verifies that a Go caller of ProposeChange
// or ReviseChange, which bypasses the HTTP and MCP decoders, is held to the
// same structural rules: a blank summary, no examples, a null question, an
// unknown verification_kind, a missing analysis or a change_id that is not
// valid UTF-8 is refused as unprocessable and saves nothing.
func TestGoCallersGetTheDesignRules(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	cp := newControlPlane(t, Options{})
	saved := mustPropose(t, cp, alice, "chg", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))

	blankSummary := refundAnalysis(" \n\t")
	noExamples := refundAnalysis("Refunds up to $50 are instant.")
	noExamples.Examples = []*gensurla.CP1AnalysisExample{}
	nullQuestion := refundAnalysis("Refunds up to $50 are instant.", nil)
	unknownKind := refundAnalysis("Refunds up to $50 are instant.")
	unknownKind.Examples[0].VerificationKind = "eyeballed"
	for _, tc := range []struct {
		name     string
		changeID string
		analysis *gensurla.CP1Analysis
		want     string
	}{
		{"blank summary", "chg-blank", blankSummary, "summary"},
		{"no examples", "chg-none", noExamples, "examples"},
		{"null question", "chg-null", nullQuestion, "ambiguities"},
		{"unknown verification_kind", "chg-kind", unknownKind, "verification_kind"},
		{"missing analysis", "chg-missing", nil, "analysis"},
		{"change_id not valid UTF-8", "chg\xff", refundAnalysis("Refunds up to $50 are instant."), "invalid UTF-8"},
	} {
		t.Run("propose "+tc.name, func(t *testing.T) {
			_, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: tc.changeID, Prompt: "Refunds up to $50 are instant.", Analysis: tc.analysis})
			assertServiceError(t, err, "unprocessable")
			assert.ErrorContains(t, err, tc.want)
			_, ok, err := cp.cards.load(tc.changeID)
			require.NoError(t, err)
			assert.False(t, ok, "a refused proposal saves nothing")
		})
	}

	check01 := "check_01"
	_, err := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
		ChangeID: "chg", RequestRevision: saved.RequestRevision, Prompt: "Refunds up to $60 are instant.",
		Analysis: &gensurla.CP1RevisionAnalysis{
			Summary:     "",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID: &check01, Scenario: "Refund at the limit", InputSummary: "Order total $60.00",
				ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
			}},
			RemovedExampleIds:  []string{},
			PreservedBehaviors: []string{},
		},
	})
	assertServiceError(t, err, "unprocessable")
	assert.ErrorContains(t, err, "summary")
	current, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, saved.RequestRevision, current.RequestRevision, "a refused revision leaves the card unchanged")
}

// TestCP2EvaluatedArchitecturePreviewCP3FastDraftAndTokenAuthenticator verifies
// CP2 design evaluation, Golden Path MCP resource serving, proposed swarm
// planning, CP3 Fast Draft Mode prompt overrides, and bearer-token authentication.
func TestCP2EvaluatedArchitecturePreviewCP3FastDraftAndTokenAuthenticator(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	clearAnalysis := &gensurla.CP1Analysis{
		Summary:     "Refunds with item_subtotal_cents < 5000 auto-approve; >= 5000 require manager approval.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
		Examples: []*gensurla.CP1AnalysisExample{
			{
				Scenario:         "Subtotal 4999 cents auto-approves",
				InputSummary:     "item_subtotal_cents=4999",
				ExpectedOutcome:  "Auto-approved",
				VerificationKind: "deterministic",
			},
		},
		PreservedBehaviors: []string{"Idempotency key refund/<tenant>/<order>"},
	}

	targetStore := prompt.NewInMemoryStore()
	cp := newControlPlane(t, Options{
		TargetUpdater: &boundTargetPromptUpdater{store: targetStore},
	})

	candidateDesign := `package design

import (
	. "goa.design/goa/v3/dsl"
	. "goa.design/goa-ai/dsl"
)

var _ = API("orders", func() {})

var RefundTools = Toolset("refund_tools", func() {
	Description("Order lookup and payment refund tools")
	Tool("issue_refund", "Issue a payment refund", func() {
		Args(func() {
			Attribute("tenant_id", String)
			Attribute("order_id", String)
			Required("tenant_id", "order_id")
		})
		Return(func() {
			Attribute("status", String)
			Required("status")
		})
		Inject("tenant_id")
		Confirmation(func() {
			Title("Approve refund")
			PromptTemplate("Approve {{ .order_id }}?")
			DeniedResultTemplate(` + "`{\"status\":\"denied\"}`" + `)
		})
	})
})

var _ = Service("orders", func() {
	Description("Public Orders HTTP and MCP service")
	Agent("refund_agent", "Executes durable order refund workflows", func() {
		Use(RefundTools)
		RunPolicy(func() {
			DefaultCaps(MaxToolCalls(6), MaxRecoveryTurns(2))
			TimeBudget("2m")
			History(func() {
				CompressAtTurns(20)
				KeepMaxTurns(6)
			})
			Cache(func() {
				AfterSystem()
				AfterTools()
			})
		})
	})
})
`
	card, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{
		ChangeID:     "chg-clear",
		Prompt:       "Auto-approve when item_subtotal_cents < 5000; require manager when item_subtotal_cents >= 5000.",
		DesignSource: &candidateDesign,
		Analysis:     clearAnalysis,
	})
	if err != nil {
		t.Fatalf("ProposeChange failed: %v", err)
	}
	if card.Status != StatusConfirmed {
		t.Fatalf("expected clear request to be immediately %q, got %q", StatusConfirmed, card.Status)
	}

	preview, err := cp.PreviewArchitecture(ctx, &gensurla.PreviewArchitecturePayload{
		ChangeID:        card.ChangeID,
		RequestRevision: card.RequestRevision,
		Design:          &gensurla.DesignSnapshot{Files: testmodule.Snapshot(t, "", candidateDesign)},
	})
	if err != nil {
		t.Fatalf("PreviewArchitecture failed: %v", err)
	}
	if preview.SystemName != "orders" || len(preview.Containers) != 3 || len(preview.Edges) != 2 {
		t.Fatalf("unexpected CP2 topology: system=%q containers=%d edges=%d", preview.SystemName, len(preview.Containers), len(preview.Edges))
	}
	if len(preview.AgentPolicies) != 1 {
		t.Fatalf("expected 1 agent policy, got %d", len(preview.AgentPolicies))
	}
	pol := preview.AgentPolicies[0]
	if pol.MaxExecutionSeconds != 120 || pol.MaxToolCalls != 6 || pol.MaxRecoveryTurns != 2 || pol.CompressAtTurns != 20 || pol.KeepRecentTurns != 6 || !pol.CacheAfterSystem || !pol.CacheAfterTools {
		t.Fatalf("unexpected evaluated AgentPolicyItem: %+v", pol)
	}
	if len(preview.Tools) != 1 || !preview.Tools[0].RequiresConfirmation || len(preview.Tools[0].InjectedFields) != 1 || preview.Tools[0].InjectedFields[0] != "tenant_id" {
		t.Fatalf("unexpected evaluated ToolContractItem: %+v", preview.Tools)
	}
	if preview.Swarm == nil || preview.Swarm.Status != string(swarm.StatusUnconfigured) || len(preview.Swarm.MissingRoles) != 0 || preview.Swarm.Manifest != nil {
		t.Fatalf("expected solo unconfigured swarm plan with empty MissingRoles when no builders are supplied, got %+v", preview.Swarm)
	}

	// CP3: Fast Draft Mode updates the bound TargetPromptUpdater and rejects compiled_rule hacks.
	promptID := "orders.refund.system"
	draftPrompt, err := cp.DraftOverride(ctx, &gensurla.DraftOverridePayload{
		TargetKind: TargetKindPromptTemplate,
		PromptID:   &promptID,
		NewContent: "You are a concise, empathetic refund specialist.",
	})
	if err != nil {
		t.Fatalf("DraftOverride(prompt_template) failed: %v", err)
	}
	if !draftPrompt.AppliedInStore || draftPrompt.RequiresRebuild || draftPrompt.PromptVersion == nil || *draftPrompt.PromptVersion == "" {
		t.Fatalf("unexpected prompt_template DraftOverrideResult: %+v", draftPrompt)
	}

	// Verified bearer-token authenticator rejects unauthenticated HTTP requests and accepts valid bearer token.
	auth := NewTokenAuthenticator(map[string]Principal{
		"secret-token-alice": {ID: "alice@example.com"},
	})
	srv := httptest.NewServer(NewHTTPHandler(cp, auth))
	defer srv.Close()

	unauthResp, err := http.Get(srv.URL + "/ui/checkpoint")
	if err != nil {
		t.Fatalf("GET /ui/checkpoint failed: %v", err)
	}
	if closeErr := unauthResp.Body.Close(); closeErr != nil {
		t.Fatalf("close unauthResp: %v", closeErr)
	}
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized without bearer token, got %d", unauthResp.StatusCode)
	}

	authReq, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/ui/checkpoint", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	authReq.Header.Set("Authorization", "Bearer secret-token-alice")
	authResp, err := http.DefaultClient.Do(authReq)
	if err != nil {
		t.Fatalf("authenticated GET /ui/checkpoint failed: %v", err)
	}
	if closeErr := authResp.Body.Close(); closeErr != nil {
		t.Fatalf("close authResp: %v", closeErr)
	}
	if authResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK with valid bearer token, got %d", authResp.StatusCode)
	}

	// MCP Adapter ToolsList and ResourcesRead.
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
	toolsRes, err := adapter.ToolsList(ctx, &genmcpadapter.ToolsListPayload{})
	if err != nil {
		t.Fatalf("ToolsList: %v", err)
	}
	toolNames := make([]string, 0, len(toolsRes.Tools))
	for _, tool := range toolsRes.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	assert.ElementsMatch(t, []string{
		"propose_change", "revise_change", "confirm_intent", "get_change", "configure_model", "init_project", "generate_code", "preview_architecture", "draft_override", "approve_merge", "diff",
		"start_live_target", "stop_live_target",
		"verify_change", "get_verification", "list_verifications",
		"check_project", "update_dependencies", "read_check_output",
		"list_traces",
	}, toolNames)
	// Every checkpoint tool, diff included, links the checkpoint viewer so an
	// MCP Apps host can render its result; configure_model, init_project,
	// generate_code, check_project, update_dependencies and read_check_output
	// are server/workspace tools without a checkpoint UI view. list_traces is
	// also a data tool; the viewer reads it through its separate Traces control.
	workspaceTools := map[string]bool{
		"configure_model": true, "init_project": true, "generate_code": true,
		"check_project": true, "update_dependencies": true, "read_check_output": true,
		"list_traces": true,
	}
	for _, tool := range toolsRes.Tools {
		if workspaceTools[tool.Name] {
			if strings.Contains(string(tool.Meta), "resourceUri") {
				t.Fatalf("tool %q unexpectedly links a UI resource: _meta=%s", tool.Name, tool.Meta)
			}
			continue
		}
		if !strings.Contains(string(tool.Meta), `"resourceUri":"ui://surla/checkpoint"`) {
			t.Fatalf("tool %q does not link ui://surla/checkpoint: _meta=%s", tool.Name, tool.Meta)
		}
	}
	resRead, err := adapter.ResourcesRead(ctx, &genmcpadapter.ResourcesReadPayload{URI: "ui://surla/checkpoint"})
	if err != nil {
		t.Fatalf("ResourcesRead(ui://surla/checkpoint) failed: %v", err)
	}
	completeRes, ok := resRead.Outcome.AsComplete()
	if !ok || len(completeRes.Contents) != 1 || completeRes.Contents[0].Text == nil || !strings.Contains(*completeRes.Contents[0].Text, "surla-checkpoint-root") {
		t.Fatalf("unexpected ResourcesRead content: %+v", resRead)
	}

	goldenRes, err := adapter.ResourcesRead(ctx, &genmcpadapter.ResourcesReadPayload{URI: goldenpath.ResourceURI})
	if err != nil {
		t.Fatalf("ResourcesRead(%s) failed: %v", goldenpath.ResourceURI, err)
	}
	goldenComplete, ok := goldenRes.Outcome.AsComplete()
	if !ok || len(goldenComplete.Contents) != 1 || goldenComplete.Contents[0].Text == nil || *goldenComplete.Contents[0].Text != goldenpath.ResourceMarkdown() {
		t.Fatalf("unexpected surla://golden-path content: %+v", goldenRes)
	}

	// CP2 with server-configured multi-family provider profiles proposes cross-family swarm roster.
	cpWithSwarm := newControlPlane(t, Options{
		SwarmProfiles: []swarm.ProviderProfile{
			{
				ID:            "claude-opus",
				Provider:      "claude",
				Model:         "claude-opus-4-6",
				Family:        "anthropic",
				Authenticated: true,
				Capabilities: []swarm.RoleKind{
					swarm.RoleArchitect,
					swarm.RoleBuilder,
					swarm.RoleEvalAuthor,
					swarm.RoleReviewer,
				},
			},
			{
				ID:            "codex-gpt5",
				Provider:      "codex",
				Model:         "gpt-5.4",
				Family:        "openai",
				Authenticated: true,
				Capabilities: []swarm.RoleKind{
					swarm.RoleBuilder,
					swarm.RoleEvalAuthor,
					swarm.RoleReviewer,
				},
			},
			{
				ID:            "gemini-pro",
				Provider:      "gemini",
				Model:         "gemini-3.1-pro",
				Family:        "google",
				Authenticated: true,
				Capabilities: []swarm.RoleKind{
					swarm.RoleArchitect,
					swarm.RoleEvalAuthor,
					swarm.RoleReviewer,
				},
			},
		},
	})
	cardSwarm, err := cpWithSwarm.ProposeChange(ctx, &gensurla.ProposeChangePayload{
		ChangeID:     "chg-swarm",
		Prompt:       "Auto-approve when item_subtotal_cents < 5000; require manager when item_subtotal_cents >= 5000.",
		DesignSource: &candidateDesign,
		Analysis:     clearAnalysis,
	})
	if err != nil {
		t.Fatalf("ProposeChange(chg-swarm) failed: %v", err)
	}
	// 1. Builders only on full-capability profiles -> 2-role roster (@builder-* + @reviewer).
	previewSwarm2Role, err := cpWithSwarm.PreviewArchitecture(ctx, &gensurla.PreviewArchitecturePayload{
		ChangeID:        cardSwarm.ChangeID,
		RequestRevision: cardSwarm.RequestRevision,
		Design:          &gensurla.DesignSnapshot{Files: testmodule.Snapshot(t, "", candidateDesign)},
		Builders: []*gensurla.BuilderScopeItem{
			{Name: "orders", Paths: []string{"internal/orders/**"}},
			{Name: "api", Paths: []string{"cmd/orders-api/**"}},
		},
	})
	if err != nil {
		t.Fatalf("PreviewArchitecture(chg-swarm 2-role) failed: %v", err)
	}
	if previewSwarm2Role.Swarm == nil ||
		previewSwarm2Role.Swarm.Status != string(swarm.StatusConfigured) ||
		!previewSwarm2Role.Swarm.CrossFamilyReview ||
		previewSwarm2Role.Swarm.CrossFamilyEval ||
		previewSwarm2Role.Swarm.SingleFamily ||
		len(previewSwarm2Role.Swarm.Assignments) != 3 ||
		previewSwarm2Role.Swarm.Manifest == nil ||
		!strings.Contains(*previewSwarm2Role.Swarm.Manifest, "eval-author not assigned") {
		t.Fatalf("unexpected 2-role previewSwarm2Role.Swarm: %+v", previewSwarm2Role.Swarm)
	}

	// 2. Builders + ArchitectPaths + EvalPaths on the same server -> full 4-role roster.
	previewSwarm, err := cpWithSwarm.PreviewArchitecture(ctx, &gensurla.PreviewArchitecturePayload{
		ChangeID:        cardSwarm.ChangeID,
		RequestRevision: cardSwarm.RequestRevision,
		Design:          &gensurla.DesignSnapshot{Files: testmodule.Snapshot(t, "", candidateDesign)},
		Builders: []*gensurla.BuilderScopeItem{
			{Name: "orders", Paths: []string{"internal/orders/**"}},
			{Name: "api", Paths: []string{"cmd/orders-api/**"}},
		},
		ArchitectPaths: []string{"design/**"},
		EvalPaths:      []string{"*-eval/**"},
	})
	if err != nil {
		t.Fatalf("PreviewArchitecture(chg-swarm 4-role) failed: %v", err)
	}
	if previewSwarm.Swarm == nil ||
		previewSwarm.Swarm.Status != string(swarm.StatusConfigured) ||
		!previewSwarm.Swarm.CrossFamilyReview ||
		!previewSwarm.Swarm.CrossFamilyEval ||
		previewSwarm.Swarm.SingleFamily ||
		len(previewSwarm.Swarm.Assignments) != 5 ||
		previewSwarm.Swarm.Manifest == nil ||
		!strings.Contains(*previewSwarm.Swarm.Manifest, previewSwarm.DesignDigest) {
		t.Fatalf("unexpected configured previewSwarm.Swarm: %+v", previewSwarm.Swarm)
	}
}

// ApplyPromptOverride writes newContent into the target prompt.Store and returns the resolved version.
func (u *boundTargetPromptUpdater) ApplyPromptOverride(ctx context.Context, promptID, newContent string) (string, error) {
	id := prompt.Ident(promptID)
	if err := u.store.Set(ctx, id, prompt.Scope{}, newContent, map[string]string{"source": "cp3_fast_draft"}); err != nil {
		return "", err
	}
	override, err := u.store.Resolve(ctx, id, prompt.Scope{})
	if err != nil || override == nil {
		return "", err
	}
	return override.Version, nil
}

// TestConfirmIntentRefinesNeedsConfirmationAndConfirmedCards verifies that the
// owner can confirm a card and later refine its answers or examples against
// the current request_revision, that unchanged submissions return the card as
// it is, and that stale revisions, other principals and previews bound to an
// earlier revision are refused.
func TestConfirmIntentRefinesNeedsConfirmationAndConfirmedCards(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	bobCtx := WithPrincipal(context.Background(), Principal{ID: "bob@example.com"})
	ambiguous := refundAnalysis("Refunds up to $50 are instant.", &gensurla.CP1AnalysisAmbiguity{
		Question: "Is $50.00 itself instant?", Options: []string{"instant", "manager"},
	})
	unambiguous := refundAnalysis("Refunds up to $50 are instant.")
	propose := func(t *testing.T, analysis *gensurla.CP1Analysis) (*ControlPlane, *gensurla.ChangeCardResult) {
		t.Helper()
		cp := newControlPlane(t, Options{})
		return cp, mustPropose(t, cp, aliceCtx, "chg", "Refunds up to $50 are instant.", analysis)
	}
	confirm := func(ctx context.Context, cp *ControlPlane, rev string, answers map[string]string, examples []*gensurla.ExampleItem) (*gensurla.ChangeCardResult, error) {
		return cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{ChangeID: "chg", RequestRevision: rev, Answers: answers, Examples: examples})
	}
	edited := func(card *gensurla.ChangeCardResult, outcome string) []*gensurla.ExampleItem {
		ex := *card.Examples[0]
		ex.ExpectedOutcome = outcome
		return []*gensurla.ExampleItem{&ex}
	}

	t.Run("needs_confirmation card is confirmed, then refined", func(t *testing.T) {
		cp, proposed := propose(t, ambiguous)
		if proposed.Status != StatusNeedsConfirmation {
			t.Fatalf("status = %q, want %q", proposed.Status, StatusNeedsConfirmation)
		}
		instant := map[string]string{"chg-ambiguity-01": "instant"}
		first, err := confirm(aliceCtx, cp, proposed.RequestRevision, instant, proposed.Examples)
		if err != nil {
			t.Fatalf("first confirm: %v", err)
		}
		if first.Status != StatusConfirmed || first.RequestRevision == proposed.RequestRevision {
			t.Fatalf("first confirm = %q at %q, want confirmed at a new revision", first.Status, first.RequestRevision)
		}

		same, err := confirm(aliceCtx, cp, first.RequestRevision, map[string]string{"chg-ambiguity-01": "instant"}, edited(first, first.Examples[0].ExpectedOutcome))
		if err != nil {
			t.Fatalf("unchanged confirm: %v", err)
		}
		if same.RequestRevision != first.RequestRevision {
			t.Fatalf("unchanged answers and examples moved the revision from %q to %q", first.RequestRevision, same.RequestRevision)
		}

		manager, err := confirm(aliceCtx, cp, first.RequestRevision, map[string]string{"chg-ambiguity-01": "manager"}, first.Examples)
		if err != nil {
			t.Fatalf("changed answer: %v", err)
		}
		if manager.RequestRevision == first.RequestRevision || manager.Answers["chg-ambiguity-01"] != "manager" {
			t.Fatalf("changed answer kept revision %q or answer %v", manager.RequestRevision, manager.Answers)
		}
		if manager.Summary != first.Summary || manager.Prompt != first.Prompt {
			t.Fatal("refinement changed the prompt or summary")
		}

		if _, err := confirm(aliceCtx, cp, first.RequestRevision, instant, first.Examples); err == nil || !strings.Contains(err.Error(), "request_revision mismatch") {
			t.Fatalf("stale revision err = %v, want request_revision mismatch", err)
		}
		if _, err := confirm(bobCtx, cp, manager.RequestRevision, instant, manager.Examples); err == nil || !strings.Contains(err.Error(), "is not the owner") {
			t.Fatalf("other principal err = %v, want owner refusal", err)
		}
		if _, err := confirm(aliceCtx, cp, manager.RequestRevision, map[string]string{}, manager.Examples); err == nil || !strings.Contains(err.Error(), "missing confirmation answer") {
			t.Fatalf("missing answer err = %v, want missing confirmation answer", err)
		}

		_, err = cp.PreviewArchitecture(aliceCtx, &gensurla.PreviewArchitecturePayload{
			ChangeID: "chg", RequestRevision: first.RequestRevision, Design: &gensurla.DesignSnapshot{},
		})
		if err == nil || !strings.Contains(err.Error(), "request_revision mismatch") {
			t.Fatalf("preview bound to the earlier revision err = %v, want request_revision mismatch", err)
		}
	})

	t.Run("auto-confirmed card is refined later", func(t *testing.T) {
		cp, proposed := propose(t, unambiguous)
		if proposed.Status != StatusConfirmed {
			t.Fatalf("status = %q, want %q", proposed.Status, StatusConfirmed)
		}
		same, err := confirm(aliceCtx, cp, proposed.RequestRevision, map[string]string{}, proposed.Examples)
		if err != nil {
			t.Fatalf("unchanged confirm: %v", err)
		}
		if same.RequestRevision != proposed.RequestRevision {
			t.Fatalf("unchanged submission moved the revision from %q to %q", proposed.RequestRevision, same.RequestRevision)
		}

		refined, err := confirm(aliceCtx, cp, proposed.RequestRevision, map[string]string{}, edited(proposed, "Waits for a manager"))
		if err != nil {
			t.Fatalf("changed example: %v", err)
		}
		if refined.RequestRevision == proposed.RequestRevision || refined.Examples[0].ExpectedOutcome != "Waits for a manager" {
			t.Fatalf("changed example kept revision %q or outcome %q", refined.RequestRevision, refined.Examples[0].ExpectedOutcome)
		}
		if refined.ConfirmedBy == nil || *refined.ConfirmedBy != "alice@example.com" {
			t.Fatalf("confirmed_by = %v, want alice@example.com", refined.ConfirmedBy)
		}
	})
}

// TestGetChangeLetsAStaleEditorRecover verifies the stale-edit recovery path:
// two editors start from the same card, the second saves first, the first is
// refused for its stale revision, reads the current card with get_change, and
// succeeds only when it resubmits against that card's revision. It also checks
// that get_change is owner-only, fails for an unknown change_id, never changes
// the card, and is served at GET /changes/{change_id}.
func TestGetChangeLetsAStaleEditorRecover(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	bobCtx := WithPrincipal(context.Background(), Principal{ID: "bob@example.com"})
	cp := newControlPlane(t, Options{})
	start := mustPropose(t, cp, aliceCtx, "chg", "Refunds up to $50 are instant.", refundAnalysis("Refunds up to $50 are instant."))
	withOutcome := func(card *gensurla.ChangeCardResult, outcome string) []*gensurla.ExampleItem {
		ex := *card.Examples[0]
		ex.ExpectedOutcome = outcome
		return []*gensurla.ExampleItem{&ex}
	}

	// Editor B saves first; editor A then submits against the revision both started from.
	saved, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{ChangeID: "chg", RequestRevision: start.RequestRevision, Answers: map[string]string{}, Examples: withOutcome(start, "Waits for a manager")})
	if err != nil {
		t.Fatalf("editor B confirm: %v", err)
	}
	_, err = cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{ChangeID: "chg", RequestRevision: start.RequestRevision, Answers: map[string]string{}, Examples: withOutcome(start, "Store credit within a day")})
	if err == nil || !strings.Contains(err.Error(), "request_revision mismatch") {
		t.Fatalf("editor A stale submit err = %v, want request_revision mismatch", err)
	}

	current, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
	if err != nil {
		t.Fatalf("GetChange: %v", err)
	}
	if current.RequestRevision != saved.RequestRevision || current.Examples[0].ExpectedOutcome != "Waits for a manager" {
		t.Fatalf("GetChange = revision %q outcome %q, want editor B's saved card", current.RequestRevision, current.Examples[0].ExpectedOutcome)
	}
	again, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
	if err != nil || again.RequestRevision != current.RequestRevision {
		t.Fatalf("second GetChange = %v, %v; reading must not change the card", again, err)
	}

	resubmitted, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{ChangeID: "chg", RequestRevision: current.RequestRevision, Answers: map[string]string{}, Examples: withOutcome(current, "Store credit within a day")})
	if err != nil {
		t.Fatalf("editor A resubmit after reading: %v", err)
	}
	if resubmitted.RequestRevision == current.RequestRevision {
		t.Fatal("editor A's reviewed change kept the revision")
	}

	if _, err := cp.GetChange(bobCtx, &gensurla.GetChangePayload{ChangeID: "chg"}); err == nil || !strings.Contains(err.Error(), "is not the owner") {
		t.Fatalf("other principal GetChange err = %v, want owner refusal", err)
	}
	if _, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "missing"}); err == nil || !strings.Contains(err.Error(), `unknown change_id "missing"`) {
		t.Fatalf("unknown GetChange err = %v, want unknown change_id", err)
	}

	srv := httptest.NewServer(NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
	defer srv.Close()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/changes/chg", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer alice-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /changes/chg: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		RequestRevision string `json:"request_revision"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode GET /changes/chg: %v", err)
	}
	if resp.StatusCode != http.StatusOK || body.RequestRevision != resubmitted.RequestRevision {
		t.Fatalf("GET /changes/chg = %d revision %q, want 200 and %q", resp.StatusCode, body.RequestRevision, resubmitted.RequestRevision)
	}
}

// newControlPlane returns a control plane that saves its cards in a fresh
// temporary directory, or in opts.CardsDir when set, and closes it when the
// test ends.
func newControlPlane(t *testing.T, opts Options) *ControlPlane {
	t.Helper()
	if opts.CardsDir == "" {
		opts.CardsDir = t.TempDir()
	}
	cp, err := New(opts)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	return cp
}

// refundAnalysis returns the analysis these tests send for the instant-refund
// feature request: summary, the given open questions (none when omitted), one
// deterministic example at the $50.00 limit, and one tenant-isolation rule the
// change keeps.
func refundAnalysis(summary string, questions ...*gensurla.CP1AnalysisAmbiguity) *gensurla.CP1Analysis {
	return &gensurla.CP1Analysis{
		Summary:     summary,
		Ambiguities: append([]*gensurla.CP1AnalysisAmbiguity{}, questions...),
		Examples: []*gensurla.CP1AnalysisExample{{
			Scenario:         "Refund at the limit",
			InputSummary:     "Order total $50.00",
			ExpectedOutcome:  "Instant store credit",
			VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{"Cross-tenant order lookups return 404 Not Found."},
	}
}

// mustPropose proposes changeID with prompt and analysis as the principal in
// ctx and fails the test when the control plane refuses it.
func mustPropose(
	t *testing.T,
	cp *ControlPlane,
	ctx context.Context,
	changeID, prompt string,
	analysis *gensurla.CP1Analysis,
) *gensurla.ChangeCardResult {
	t.Helper()
	card, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: changeID, Prompt: prompt, Analysis: analysis})
	require.NoError(t, err)
	return card
}

// postJSON sends body to url with the bearer token and returns the HTTP
// status and response body.
func postJSON(t *testing.T, url, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader([]byte(body)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	var out bytes.Buffer
	_, err = out.ReadFrom(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, out.String()
}

// proposeArgs encodes p as the MCP arguments of a propose_change call.
func proposeArgs(t *testing.T, p *gensurla.ProposeChangePayload) json.RawMessage {
	t.Helper()
	raw, err := gensurla.EncodeProposeChangePayload(p)
	require.NoError(t, err)
	return raw
}

// reviseArgs encodes p as the MCP arguments of a revise_change call.
func reviseArgs(t *testing.T, p *gensurla.ReviseChangePayload) json.RawMessage {
	t.Helper()
	raw, err := gensurla.EncodeReviseChangePayload(p)
	require.NoError(t, err)
	return raw
}
