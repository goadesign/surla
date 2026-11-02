// Package controlplane tests Checkpoints 1, 2, 3 and 5 of the Surla MCP
// software factory control plane.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/features/model/anthropic"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/goldenpath"
	"github.com/goadesign/surla/internal/swarm"
	"github.com/goadesign/surla/internal/testmodule"
	"goa.design/goa-ai/runtime/agent/model"
	"goa.design/goa-ai/runtime/agent/prompt"
)

type (
	// stubProvider implements model.Provider for testing ModelCP1Analyzer through
	// the goa-ai model.NewClient StructuredOutput validation wrapper.
	stubProvider struct {
		// responseJSON is the structured JSON payload returned by Complete.
		responseJSON string
	}

	// boundTargetPromptUpdater binds a target platform's prompt.Store to the
	// TargetPromptUpdater contract for testing CP3 Fast Draft Mode.
	boundTargetPromptUpdater struct {
		// store is the target platform's prompt.Store.
		store prompt.Store
	}

	// recordingAnthropicMessages is an anthropic.MessagesClient that records the
	// encoded request and returns a fixed message.
	recordingAnthropicMessages struct {
		// resp is the message returned by New.
		resp *anthropicsdk.Message
		// last is the most recent request body encoded by the adapter.
		last anthropicsdk.MessageNewParams
		// calls counts New invocations.
		calls int
	}
)

// TestCP1AmbiguityResolutionStrictValidationOwnershipAndRevisionLock verifies
// CP1 ambiguity detection, code-owned question/check IDs, authenticated owner
// binding, and canonical revision locking.
func TestCP1AmbiguityResolutionStrictValidationOwnershipAndRevisionLock(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	bobCtx := WithPrincipal(context.Background(), Principal{ID: "bob@example.com"})

	ambiguousAnalysis := CP1SemanticAnalysis{
		Summary: "  Auto-approve refunds under $50 and route larger refunds to manager approval.\n",
		Ambiguities: []CP1ModelAmbiguity{
			{
				Question: "  Does the $50 threshold apply to item subtotal or total order amount including tax? ",
				Options:  []string{"item_subtotal_cents", "total_order_cents"},
			},
			{
				Question: "Should an exact $50.00 refund auto-approve or require manager approval?",
				Options:  []string{"auto_approve_lte_5000", "require_manager_at_5000"},
			},
		},
		Examples: []CP1ModelExample{
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
	rawBytes, err := json.Marshal(ambiguousAnalysis)
	if err != nil {
		t.Fatalf("marshal analysis: %v", err)
	}

	cp := newControlPlane(t, Options{
		Analyzer: NewModelCP1Analyzer(newStubModelClient(t, string(rawBytes))),
	})

	designSrc := `package design // orders service`
	if _, err := cp.ProposeChange(context.Background(), &gensurla.ProposeChangePayload{
		ChangeID:     "chg-refund-50",
		Prompt:       "Refunds under $50 should be automatic, otherwise ask a manager.",
		DesignSource: &designSrc,
	}); err == nil {
		t.Fatal("expected unauthenticated ProposeChange to fail")
	}

	proposed, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID:     "chg-refund-50",
		Prompt:       "Refunds under $50 should be automatic, otherwise ask a manager.",
		DesignSource: &designSrc,
	})
	if err != nil {
		t.Fatalf("ProposeChange failed: %v", err)
	}
	if proposed.Status != StatusNeedsConfirmation {
		t.Fatalf("expected status %q, got %q", StatusNeedsConfirmation, proposed.Status)
	}
	if len(proposed.Ambiguities) != 2 ||
		proposed.Ambiguities[0].ID != "chg-refund-50-ambiguity-01" ||
		proposed.Ambiguities[1].ID != "chg-refund-50-ambiguity-02" {
		t.Fatalf("expected 2 ambiguities with code-owned IDs, got %+v", proposed.Ambiguities)
	}
	if len(proposed.Examples) != 3 || proposed.Examples[0].ID != "chg-refund-50-check-01" {
		t.Fatalf("unexpected examples: %+v", proposed.Examples)
	}
	if proposed.Summary != ambiguousAnalysis.Summary ||
		proposed.Ambiguities[0].Question != ambiguousAnalysis.Ambiguities[0].Question ||
		proposed.Examples[0].Scenario != ambiguousAnalysis.Examples[0].Scenario ||
		proposed.PreservedBehaviors[0] != ambiguousAnalysis.PreservedBehaviors[0] {
		t.Fatalf("expected accepted authored strings to be preserved verbatim without TrimSpace rewriting")
	}

	// Attempting to overwrite an existing change_id must be rejected.
	if _, err := cp.ProposeChange(bobCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-refund-50",
		Prompt:   "Attempt to clobber Alice's change_id",
	}); err == nil {
		t.Fatal("expected duplicate change_id in ProposeChange to be rejected")
	}

	// Non-owner principal cannot confirm Alice's Change Card.
	if _, err := cp.ConfirmIntent(bobCtx, &gensurla.ConfirmIntentPayload{
		ChangeID:        "chg-refund-50",
		RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{
			"chg-refund-50-ambiguity-01": "item_subtotal_cents",
			"chg-refund-50-ambiguity-02": "require_manager_at_5000",
		},
		Examples: proposed.Examples,
	}); err == nil {
		t.Fatal("expected ConfirmIntent by non-owner principal to be rejected")
	}

	// Stale request_revision must be rejected.
	if _, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID:        "chg-refund-50",
		RequestRevision: "stale-revision",
		Answers: map[string]string{
			"chg-refund-50-ambiguity-01": "item_subtotal_cents",
			"chg-refund-50-ambiguity-02": "require_manager_at_5000",
		},
		Examples: proposed.Examples,
	}); err == nil {
		t.Fatal("expected ConfirmIntent with mismatched request_revision to be rejected")
	}

	confirmed, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID:        "chg-refund-50",
		RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{
			"chg-refund-50-ambiguity-01": "item_subtotal_cents",
			"chg-refund-50-ambiguity-02": "require_manager_at_5000",
		},
		Examples: proposed.Examples,
	})
	if err != nil {
		t.Fatalf("ConfirmIntent failed: %v", err)
	}
	if confirmed.Status != StatusConfirmed {
		t.Fatalf("expected status %q, got %q", StatusConfirmed, confirmed.Status)
	}
	if len(confirmed.Ambiguities) != 2 || len(confirmed.Answers) != 2 {
		t.Fatalf("expected ambiguities and answers to be retained for audit, got %d ambiguities and %d answers", len(confirmed.Ambiguities), len(confirmed.Answers))
	}
	if confirmed.ConfirmedBy == nil || *confirmed.ConfirmedBy != "alice@example.com" {
		t.Fatalf("expected ConfirmedBy alice@example.com, got %v", confirmed.ConfirmedBy)
	}
	if confirmed.RequestRevision == "" || confirmed.RequestRevision == proposed.RequestRevision {
		t.Fatalf("expected new confirmed RequestRevision, got %q (proposed %q)", confirmed.RequestRevision, proposed.RequestRevision)
	}
}

// TestCP1RejectsMalformedOrUnknownFieldsViaStructuredOutput verifies that
// invalid enum values, unknown fields, and model-supplied IDs are rejected at
// the CP1 model boundary.
func TestCP1RejectsMalformedOrUnknownFieldsViaStructuredOutput(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "invalid_verification_kind",
			raw:  `{"summary":"Valid summary","ambiguities":[],"examples":[{"scenario":"s","input_summary":"i","expected_outcome":"o","verification_kind":"bogus_kind"}],"preserved_behaviors":[]}`,
		},
		{
			name: "unknown_top_level_field",
			raw:  `{"summary":"Valid summary","unexpected":true,"ambiguities":[],"examples":[{"scenario":"s","input_summary":"i","expected_outcome":"o","verification_kind":"deterministic"}],"preserved_behaviors":[]}`,
		},
		{
			name: "model_supplied_ambiguity_id",
			raw:  `{"summary":"Valid summary","ambiguities":[{"id":"m1","question":"q","options":["a","b"]}],"examples":[{"scenario":"s","input_summary":"i","expected_outcome":"o","verification_kind":"deterministic"}],"preserved_behaviors":[]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newControlPlane(t, Options{
				Analyzer: NewModelCP1Analyzer(newStubModelClient(t, tc.raw)),
			})
			if _, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{
				ChangeID: "chg-" + tc.name,
				Prompt:   "Test structured output validation rejection",
			}); err == nil {
				t.Fatalf("expected ProposeChange to reject %s", tc.name)
			}
		})
	}
}

// TestCP2EvaluatedArchitecturePreviewCP3FastDraftAndTokenAuthenticator verifies
// CP2 design evaluation, Golden Path MCP resource serving, proposed swarm
// planning, CP3 Fast Draft Mode prompt overrides, and bearer-token authentication.
func TestCP2EvaluatedArchitecturePreviewCP3FastDraftAndTokenAuthenticator(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	clearAnalysis := CP1SemanticAnalysis{
		Summary:     "Refunds with item_subtotal_cents < 5000 auto-approve; >= 5000 require manager approval.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1ModelExample{
			{
				Scenario:         "Subtotal 4999 cents auto-approves",
				InputSummary:     "item_subtotal_cents=4999",
				ExpectedOutcome:  "Auto-approved",
				VerificationKind: "deterministic",
			},
		},
		PreservedBehaviors: []string{"Idempotency key refund/<tenant>/<order>"},
	}
	rawBytes, err := json.Marshal(clearAnalysis)
	if err != nil {
		t.Fatalf("marshal clearAnalysis: %v", err)
	}

	targetStore := prompt.NewInMemoryStore()
	cp := newControlPlane(t, Options{
		Analyzer:      NewModelCP1Analyzer(newStubModelClient(t, string(rawBytes))),
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
	if preview.Swarm == nil || preview.Swarm.Status != string(swarm.StatusUnconfigured) || len(preview.Swarm.MissingRoles) != 4 || preview.Swarm.Manifest != nil {
		t.Fatalf("expected unconfigured swarm plan when no SwarmProfiles are configured, got %+v", preview.Swarm)
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
		"propose_change", "revise_change", "confirm_intent", "get_change", "preview_architecture", "draft_override", "approve_merge", "diff",
		"start_live_target", "stop_live_target",
		"verify_change", "get_verification",
	}, toolNames)
	// Every tool, diff included, links the checkpoint viewer so an MCP Apps
	// host can render its result.
	for _, tool := range toolsRes.Tools {
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
	if !ok || len(goldenComplete.Contents) != 1 || goldenComplete.Contents[0].Text == nil || *goldenComplete.Contents[0].Text != goldenpath.PlaybookMarkdown() {
		t.Fatalf("unexpected surla://golden-path content: %+v", goldenRes)
	}

	// CP2 with server-configured multi-family provider profiles proposes cross-family swarm roster.
	cpWithSwarm := newControlPlane(t, Options{
		Analyzer: NewModelCP1Analyzer(newStubModelClient(t, string(rawBytes))),
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
	})
	if err != nil {
		t.Fatalf("ProposeChange(chg-swarm) failed: %v", err)
	}
	previewSwarm, err := cpWithSwarm.PreviewArchitecture(ctx, &gensurla.PreviewArchitecturePayload{
		ChangeID:        cardSwarm.ChangeID,
		RequestRevision: cardSwarm.RequestRevision,
		Design:          &gensurla.DesignSnapshot{Files: testmodule.Snapshot(t, "", candidateDesign)},
		Builders: []*gensurla.BuilderScopeItem{
			{Name: "orders", Paths: []string{"internal/orders/**"}},
			{Name: "api", Paths: []string{"cmd/orders-api/**"}},
		},
	})
	if err != nil {
		t.Fatalf("PreviewArchitecture(chg-swarm) failed: %v", err)
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

// TestCP1AnalyzerSendsOutputAllowanceThroughAnthropicAdapter runs the CP1
// analyzer through the real Anthropic adapter configured as NewFromAPIKey
// configures it (default model only, no client-wide MaxTokens). The request
// must carry cp1AnalysisMaxOutputTokens, and a response stopped at that
// allowance must fail without storing a Change Card, whether or not its JSON
// happens to parse.
func TestCP1AnalyzerSendsOutputAllowanceThroughAnthropicAdapter(t *testing.T) {
	analysis, err := json.Marshal(CP1SemanticAnalysis{
		Summary:            "Give VIP customers instant store credit up to $150.",
		Ambiguities:        []CP1ModelAmbiguity{},
		Examples:           []CP1ModelExample{{Scenario: "VIP refund", InputSummary: "$120.00 total", ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic"}},
		PreservedBehaviors: []string{},
	})
	if err != nil {
		t.Fatalf("marshal analysis: %v", err)
	}
	ctx := WithPrincipal(context.Background(), Principal{ID: "operator@example.com"})

	cases := []struct {
		name       string
		stopReason string
		text       string
		wantErr    bool
	}{
		{name: "complete response", stopReason: "end_turn", text: string(analysis)},
		{name: "malformed response cut off at the allowance", stopReason: "max_tokens", text: string(analysis[:len(analysis)/2]), wantErr: true},
		{name: "valid JSON stopped at the allowance", stopReason: "max_tokens", text: string(analysis), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msgs := &recordingAnthropicMessages{resp: anthropicTextMessage(t, tc.text, tc.stopReason)}
			client, err := anthropic.New(msgs, anthropic.Options{DefaultModel: "claude-sonnet-4-5"})
			if err != nil {
				t.Fatalf("anthropic.New: %v", err)
			}
			cp := newControlPlane(t, Options{Analyzer: NewModelCP1Analyzer(client)})

			card, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: "vip", Prompt: "VIP refunds up to $150."})
			if msgs.calls != 1 {
				t.Fatalf("Anthropic Messages.New calls = %d, want 1 (err: %v)", msgs.calls, err)
			}
			if got := msgs.last.MaxTokens; got != cp1AnalysisMaxOutputTokens {
				t.Fatalf("encoded max_tokens = %d, want %d", got, cp1AnalysisMaxOutputTokens)
			}
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ProposeChange: %v", err)
				}
				if card.Status != StatusConfirmed {
					t.Fatalf("status = %q, want %q", card.Status, StatusConfirmed)
				}
				return
			}
			if err == nil {
				t.Fatal("ProposeChange succeeded on a cut-off response")
			}
			_, stored, err := cp.cards.load("vip")
			require.NoError(t, err)
			if stored {
				t.Fatal("cut-off response stored a Change Card")
			}
		})
	}
}

// Complete returns the preconfigured JSON payload as an assistant TextPart.
func (s *stubProvider) Complete(_ context.Context, _ *model.Request) (*model.Response, error) {
	return &model.Response{
		Content: []model.Message{
			{
				Role:  model.ConversationRoleAssistant,
				Parts: []model.Part{model.TextPart{Text: s.responseJSON}},
			},
		},
		StopReason: "stop",
	}, nil
}

// Stream returns an error because ModelCP1Analyzer uses synchronous Complete calls.
func (s *stubProvider) Stream(_ context.Context, _ *model.Request) (model.Streamer, error) {
	return nil, errors.New("streaming not used by CP1 analyzer")
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

// New records body and returns the fixed message.
func (m *recordingAnthropicMessages) New(_ context.Context, body anthropicsdk.MessageNewParams, _ ...option.RequestOption) (*anthropicsdk.Message, error) {
	m.last = body
	m.calls++
	return m.resp, nil
}

// NewStreaming is unused because the CP1 analyzer only calls Complete.
func (m *recordingAnthropicMessages) NewStreaming(context.Context, anthropicsdk.MessageNewParams, ...option.RequestOption) *ssestream.Stream[anthropicsdk.MessageStreamEventUnion] {
	panic("streaming not used by CP1 analyzer")
}

// CountTokens is unused because the CP1 analyzer does not count tokens.
func (m *recordingAnthropicMessages) CountTokens(context.Context, anthropicsdk.MessageCountTokensParams, ...option.RequestOption) (*anthropicsdk.MessageTokensCount, error) {
	return nil, errors.New("token counting not used by CP1 analyzer")
}

// newStubModelClient wraps a stubProvider in a goa-ai model.Client.
func newStubModelClient(t *testing.T, responseJSON string) model.Client {
	t.Helper()
	client, err := model.NewClient(&stubProvider{responseJSON: responseJSON})
	if err != nil {
		t.Fatalf("model.NewClient: %v", err)
	}
	return client
}

// anthropicTextMessage decodes a provider-shaped assistant message holding
// text with the given stop reason, as the Anthropic API would return it.
func anthropicTextMessage(t *testing.T, text, stopReason string) *anthropicsdk.Message {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id":          "msg_test",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-sonnet-4-5",
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": stopReason,
		"usage":       map[string]any{"input_tokens": 10, "output_tokens": 20},
	})
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	var msg anthropicsdk.Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	return &msg
}

// TestConfirmIntentRefinesNeedsConfirmationAndConfirmedCards verifies that the
// owner can confirm a card and later refine its answers or examples against
// the current request_revision, that unchanged submissions return the card as
// it is, and that stale revisions, other principals and previews bound to an
// earlier revision are refused.
func TestConfirmIntentRefinesNeedsConfirmationAndConfirmedCards(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	bobCtx := WithPrincipal(context.Background(), Principal{ID: "bob@example.com"})
	example := CP1ModelExample{
		Scenario:         "Refund at the limit",
		InputSummary:     "Order total $50.00",
		ExpectedOutcome:  "Instant store credit",
		VerificationKind: "deterministic",
	}
	ambiguous, err := json.Marshal(CP1SemanticAnalysis{
		Summary:            "Refunds up to $50 are instant.",
		Ambiguities:        []CP1ModelAmbiguity{{Question: "Is $50.00 itself instant?", Options: []string{"instant", "manager"}}},
		Examples:           []CP1ModelExample{example},
		PreservedBehaviors: []string{},
	})
	if err != nil {
		t.Fatalf("marshal analysis: %v", err)
	}
	unambiguous, err := json.Marshal(CP1SemanticAnalysis{
		Summary:            "Refunds up to $50 are instant.",
		Ambiguities:        []CP1ModelAmbiguity{},
		Examples:           []CP1ModelExample{example},
		PreservedBehaviors: []string{},
	})
	if err != nil {
		t.Fatalf("marshal analysis: %v", err)
	}
	propose := func(t *testing.T, analysis []byte) (*ControlPlane, *gensurla.ChangeCardResult) {
		t.Helper()
		cp := newControlPlane(t, Options{Analyzer: NewModelCP1Analyzer(newStubModelClient(t, string(analysis)))})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant."})
		if err != nil {
			t.Fatalf("ProposeChange: %v", err)
		}
		return cp, card
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
	analysis, err := json.Marshal(CP1SemanticAnalysis{
		Summary:            "Refunds up to $50 are instant.",
		Ambiguities:        []CP1ModelAmbiguity{},
		Examples:           []CP1ModelExample{{Scenario: "Refund at the limit", InputSummary: "Order total $50.00", ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic"}},
		PreservedBehaviors: []string{},
	})
	if err != nil {
		t.Fatalf("marshal analysis: %v", err)
	}
	cp := newControlPlane(t, Options{Analyzer: NewModelCP1Analyzer(newStubModelClient(t, string(analysis)))})
	start, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant."})
	if err != nil {
		t.Fatalf("ProposeChange: %v", err)
	}
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
