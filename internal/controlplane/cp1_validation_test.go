package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/swarm"
	"github.com/goadesign/surla/internal/testmodule"
)

// TestValidateAmbiguityAnswersReportsEveryProblemOnce verifies that
// confirm_intent's answer check accepts exact option text and otherwise
// returns one error naming every missing, invalid and unknown answer, with
// each question's options quoted. The revise_change pointer ends the error
// only when an answer is missing or is not one of its question's options.
func TestValidateAmbiguityAnswersReportsEveryProblemOnce(t *testing.T) {
	questions := []*gensurla.AmbiguityQuestion{
		{ID: "chg-ambiguity-01", Question: "Who signs in?", Options: []string{"Bearer token auth", "Demo persona header"}},
		{ID: "chg-ambiguity-02", Question: "Who accepts offers?", Options: []string{"Listing seller", "Any seller"}},
		{ID: "chg-ambiguity-03", Question: "Keep history?", Options: []string{"Yes", "No"}},
	}
	const pointer = "; each answer must be exactly one of its question's options, and a decision that no option expresses goes in revise_change's prompt"
	complete := map[string]string{
		"chg-ambiguity-01": "Demo persona header",
		"chg-ambiguity-02": "Listing seller",
		"chg-ambiguity-03": "No",
	}
	cases := []struct {
		name    string
		answers map[string]string
		want    string
	}{
		{
			name:    "exact options are accepted",
			answers: complete,
		},
		{
			name: "every problem is reported in question order, then unknown IDs sorted",
			answers: map[string]string{
				"chg-ambiguity-01": "Demo persona selector at the edge",
				"chg-ambiguity-03": "  ",
				"chg-ambiguity-99": "Yes",
				"chg-ambiguity-42": "No",
			},
			want: `answer "Demo persona selector at the edge" for ambiguity "chg-ambiguity-01" is not one of its options: ["Bearer token auth" "Demo persona header"]; ` +
				`missing confirmation answer for ambiguity "chg-ambiguity-02" (options: ["Listing seller" "Any seller"]); ` +
				`missing confirmation answer for ambiguity "chg-ambiguity-03" (options: ["Yes" "No"]); ` +
				`unexpected confirmation answer for unknown ambiguity "chg-ambiguity-42"; ` +
				`unexpected confirmation answer for unknown ambiguity "chg-ambiguity-99"` + pointer,
		},
		{
			name: "an option that differs only in spacing is not an option",
			answers: map[string]string{
				"chg-ambiguity-01": "Bearer token auth ",
				"chg-ambiguity-02": "Listing seller",
				"chg-ambiguity-03": "Yes",
			},
			want: `answer "Bearer token auth " for ambiguity "chg-ambiguity-01" is not one of its options: ["Bearer token auth" "Demo persona header"]` + pointer,
		},
		{
			name: "only an unknown question ID gets no revise_change pointer",
			answers: map[string]string{
				"chg-ambiguity-01": "Demo persona header",
				"chg-ambiguity-02": "Listing seller",
				"chg-ambiguity-03": "No",
				"chg-ambiguity-04": "Yes",
			},
			want: `unexpected confirmation answer for unknown ambiguity "chg-ambiguity-04"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAmbiguityAnswers(questions, tc.answers)
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tc.want, err.Error())
		})
	}
}

// TestChangeCardHTTPErrorsAndMCPToolPreservation verifies that established
// caller, state, and unconfigured-capability refusals across the Change Card
// HTTP routes return their declared non-fault HTTP status codes (403, 404, 409,
// 422, 501), that server-owned SwarmProfiles misconfiguration on an embedded
// host remains an HTTP 500 fault, and that MCP tools/call continues to return
// non-fault tool error results with the same domain error message.
func TestChangeCardHTTPErrorsAndMCPToolPreservation(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	ambiguous := &gensurla.CP1Analysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{{Question: "Who approves?", Options: []string{"instant", "manager"}}},
		Examples: []*gensurla.CP1AnalysisExample{{
			Scenario: "Refund at limit", InputSummary: "$50", ExpectedOutcome: "Instant", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{},
	}
	auth := NewTokenAuthenticator(map[string]Principal{
		"alice-token": {ID: "alice@example.com"},
		"bob-token":   {ID: "bob@example.com"},
	})
	// Both preview refusals use one server so its evaluator reuses its own
	// compiler workspace while the handler preserves each distinct error.
	cp := newControlPlane(t, Options{
		SwarmProfiles: []swarm.ProviderProfile{{ID: "bad"}},
	})
	pending, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds.", Analysis: ambiguous})
	require.NoError(t, err)
	confirmed, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg-ok", RequestRevision: func() string {
			c, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg-ok", Prompt: "Refunds.", Analysis: ambiguous})
			require.NoError(t, err)
			return c.RequestRevision
		}(),
		Answers:  map[string]string{"chg-ok-ambiguity-01": "instant"},
		Examples: pending.Examples,
	})
	require.NoError(t, err)
	designFiles := testmodule.Snapshot(t, "", `package design
import . "goa.design/goa/v3/dsl"
var _ = API("orders", func() {})
var _ = Service("orders", func() {})
`)
	handler := NewHTTPHandler(cp, auth)
	fixedHostHandler := NewHTTPHandler(newControlPlane(t, Options{}), auth)
	// proposalAnalysis and revisionAnalysis are the wire forms of complete
	// analyses, so these requests reach the service's own checks.
	proposalAnalysis := map[string]any{
		"summary":     "Refunds up to $50 are instant.",
		"ambiguities": []any{},
		"examples": []map[string]any{{
			"scenario": "Refund at limit", "input_summary": "$50", "expected_outcome": "Instant", "verification_kind": "deterministic",
		}},
		"preserved_behaviors": []string{},
	}
	revisionAnalysis := func(priorID string) map[string]any {
		return map[string]any{
			"summary":     "Refunds up to $60 are instant.",
			"ambiguities": []any{},
			"examples": []map[string]any{{
				"prior_id": priorID, "scenario": "Refund at limit", "input_summary": "$60", "expected_outcome": "Instant", "verification_kind": "deterministic",
			}},
			"removed_example_ids": []string{},
			"preserved_behaviors": []string{},
		}
	}

	for _, tc := range []struct {
		name       string
		h          http.Handler
		method     string
		path       string
		token      string
		body       map[string]any
		wantStatus int
		wantFault  bool
		wantMsg    string
	}{
		{"configure_model without LiveModelEnv on fixed host", fixedHostHandler, http.MethodPost, "/cp1/model", "alice-token", map[string]any{"gateway_addr": "127.0.0.1:9090"}, http.StatusNotImplemented, false, msgModelReconfigUnavailable},
		{"configure_model without LiveModelEnv on configured fixed host", handler, http.MethodPost, "/cp1/model", "alice-token", map[string]any{"gateway_addr": "127.0.0.1:9090"}, http.StatusNotImplemented, false, msgModelReconfigUnavailable},
		{"propose duplicate change_id", handler, http.MethodPost, "/changes/propose", "alice-token", map[string]any{"change_id": "chg", "prompt": "p", "analysis": proposalAnalysis}, http.StatusConflict, false, `change_id "chg" already exists`},
		{"propose blank prompt", handler, http.MethodPost, "/changes/propose", "alice-token", map[string]any{"change_id": "blank", "prompt": "  ", "analysis": proposalAnalysis}, http.StatusUnprocessableEntity, false, "change_id and prompt are required"},
		{"revise unknown change_id", handler, http.MethodPost, "/changes/revise", "alice-token", map[string]any{"change_id": "missing", "request_revision": strings.Repeat("0", 64), "prompt": "p", "analysis": revisionAnalysis("check_01")}, http.StatusNotFound, false, `unknown change_id "missing"`},
		{"revise another owner's card", handler, http.MethodPost, "/changes/revise", "bob-token", map[string]any{"change_id": "chg", "request_revision": pending.RequestRevision, "prompt": "p", "analysis": revisionAnalysis("check_01")}, http.StatusForbidden, false, `principal "bob@example.com" is not the owner of change_id "chg"`},
		{"revise with an unknown prior example", handler, http.MethodPost, "/changes/revise", "alice-token", map[string]any{"change_id": "chg", "request_revision": pending.RequestRevision, "prompt": "p", "analysis": revisionAnalysis("check_09")}, http.StatusUnprocessableEntity, false, `invalid analysis: examples[0].prior_id references unknown prior example ID "check_09"`},
		{"get_change unknown", handler, http.MethodGet, "/changes/missing", "alice-token", nil, http.StatusNotFound, false, `unknown change_id "missing"`},
		{"get_change wrong owner", handler, http.MethodGet, "/changes/chg", "bob-token", nil, http.StatusForbidden, false, `principal "bob@example.com" is not the owner of change_id "chg"`},
		{"revise stale revision", handler, http.MethodPost, "/changes/revise", "alice-token", map[string]any{"change_id": "chg", "request_revision": strings.Repeat("0", 64), "prompt": "p", "analysis": revisionAnalysis("check_01")}, http.StatusConflict, false, "request_revision mismatch"},
		{"confirm invalid answer", handler, http.MethodPost, "/changes/confirm", "alice-token", map[string]any{
			"change_id":        "chg",
			"request_revision": pending.RequestRevision,
			"answers":          map[string]string{"chg-ambiguity-01": "wrong"},
			"examples": []map[string]any{{
				"id":                pending.Examples[0].ID,
				"scenario":          pending.Examples[0].Scenario,
				"input_summary":     pending.Examples[0].InputSummary,
				"expected_outcome":  pending.Examples[0].ExpectedOutcome,
				"verification_kind": pending.Examples[0].VerificationKind,
			}},
		}, http.StatusUnprocessableEntity, false, "is not one of its options"},
		{"preview unconfirmed card", handler, http.MethodPost, "/changes/preview", "alice-token", map[string]any{"change_id": "chg", "request_revision": pending.RequestRevision, "design": map[string]any{"files": designFiles}}, http.StatusConflict, false, `change_id "chg" must be confirmed before architecture preview (status "needs_confirmation")`},
		{"preview nil builder element", handler, http.MethodPost, "/changes/preview", "alice-token", map[string]any{"change_id": "chg-ok", "request_revision": confirmed.RequestRevision, "design": map[string]any{"files": designFiles}, "builders": []any{nil}}, http.StatusUnprocessableEntity, false, "builders[0] is nil"},
		{"preview malformed server SwarmProfiles", handler, http.MethodPost, "/changes/preview", "alice-token", map[string]any{"change_id": "chg-ok", "request_revision": confirmed.RequestRevision, "design": map[string]any{"files": designFiles}}, http.StatusInternalServerError, true, "plan swarm roster"},
		{"verify_change without config", handler, http.MethodPost, "/changes/chg-ok/verifications", "alice-token", map[string]any{"request_revision": confirmed.RequestRevision, "candidate_commit": strings.Repeat("a", 40)}, http.StatusNotImplemented, false, "verification is not configured"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			if tc.body != nil {
				var err error
				raw, err = json.Marshal(tc.body)
				require.NoError(t, err)
			}
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer "+tc.token)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			tc.h.ServeHTTP(rec, req)
			assert.Equal(t, tc.wantStatus, rec.Code)
			var errBody struct {
				Message string `json:"message"`
				Fault   bool   `json:"fault"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody), rec.Body.String())
			assert.Equal(t, tc.wantFault, errBody.Fault)
			assert.Contains(t, errBody.Message, tc.wantMsg)
		})
	}

	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
	argsJSON, err := json.Marshal(map[string]any{
		"change_id": "chg", "request_revision": pending.RequestRevision, "design": map[string]any{"files": designFiles},
	})
	require.NoError(t, err)
	mcpRes, err := adapter.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{Name: "preview_architecture", Arguments: argsJSON})
	require.NoError(t, err)
	complete, ok := mcpRes.Outcome.AsComplete()
	require.True(t, ok)
	require.NotNil(t, complete.IsError)
	assert.True(t, *complete.IsError)
	require.NotEmpty(t, complete.Content)
	require.NotNil(t, complete.Content[0].Text)
	assert.Contains(t, *complete.Content[0].Text, `change_id "chg" must be confirmed before architecture preview (status "needs_confirmation")`)
}
