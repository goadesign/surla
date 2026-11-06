package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"goa.design/goa-ai/features/model/gateway"
	"goa.design/goa-ai/runtime/agent/model"
	goa "goa.design/goa/v3/pkg"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/swarm"
	"github.com/goadesign/surla/internal/testmodule"
)

// stubCP1Analyzer implements CP1Analyzer directly so tests can exercise
// post-decode structural checks in validateCP1Analysis and concurrent
// analyzer turns.
type stubCP1Analyzer struct {
	// analyzeFn handles Analyze calls.
	analyzeFn func(ctx context.Context, promptText, designSource string) (CP1SemanticAnalysis, error)
	// reviseFn handles Revise calls.
	reviseFn func(ctx context.Context, prior CP1PriorCard, promptText, designSource string) (CP1SemanticAnalysis, error)
}

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
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	ambiguousJSON, err := json.Marshal(CP1SemanticAnalysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []CP1ModelAmbiguity{{Question: "Who approves?", Options: []string{"instant", "manager"}}},
		Examples: []CP1ModelExample{{
			Scenario: "Refund at limit", InputSummary: "$50", ExpectedOutcome: "Instant", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{},
	})
	require.NoError(t, err)
	auth := NewTokenAuthenticator(map[string]Principal{
		"alice-token": {ID: "alice@example.com"},
		"bob-token":   {ID: "bob@example.com"},
	})
	cp := newControlPlane(t, Options{Analyzer: NewModelCP1Analyzer(newStubModelClient(t, string(ambiguousJSON)))})
	pending, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds."})
	require.NoError(t, err)
	confirmed, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg-ok", RequestRevision: func() string {
			c, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg-ok", Prompt: "Refunds."})
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
	noAnalyzerHandler := NewHTTPHandler(newControlPlane(t, Options{}), auth)
	badSwarmCP := newControlPlane(t, Options{
		Analyzer:      NewModelCP1Analyzer(newStubModelClient(t, string(ambiguousJSON))),
		SwarmProfiles: []swarm.ProviderProfile{{ID: "bad"}},
	})
	badCard, err := badSwarmCP.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "bad", Prompt: "Refunds."})
	require.NoError(t, err)
	badConfirmed, err := badSwarmCP.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID: "bad", RequestRevision: badCard.RequestRevision,
		Answers: map[string]string{"bad-ambiguity-01": "instant"}, Examples: badCard.Examples,
	})
	require.NoError(t, err)
	badSwarmHandler := NewHTTPHandler(badSwarmCP, auth)

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
		{"propose without analyzer", noAnalyzerHandler, http.MethodPost, "/changes/propose", "alice-token", map[string]any{"change_id": "x", "prompt": "p"}, http.StatusNotImplemented, false, "cp1 semantic analyzer is required"},
		{"propose duplicate change_id", handler, http.MethodPost, "/changes/propose", "alice-token", map[string]any{"change_id": "chg", "prompt": "p"}, http.StatusConflict, false, `change_id "chg" already exists`},
		{"get_change unknown", handler, http.MethodGet, "/changes/missing", "alice-token", nil, http.StatusNotFound, false, `unknown change_id "missing"`},
		{"get_change wrong owner", handler, http.MethodGet, "/changes/chg", "bob-token", nil, http.StatusForbidden, false, `principal "bob@example.com" is not the owner of change_id "chg"`},
		{"revise stale revision", handler, http.MethodPost, "/changes/revise", "alice-token", map[string]any{"change_id": "chg", "request_revision": strings.Repeat("0", 64), "prompt": "p"}, http.StatusConflict, false, "request_revision mismatch"},
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
		{"preview malformed server SwarmProfiles", badSwarmHandler, http.MethodPost, "/changes/preview", "alice-token", map[string]any{"change_id": "bad", "request_revision": badConfirmed.RequestRevision, "design": map[string]any{"files": designFiles}}, http.StatusInternalServerError, true, "plan swarm roster"},
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

// TestCP1AnalysisRejectedContractAcrossModelBoundaries verifies that:
//  1. Both direct model.NewClient and gRPC gateway.NewGRPCClient boundaries
//     classify model output validation failures (structured_output,
//     output_bounds) and OutputLimited stops as HTTP 502 analysis_rejected
//     (fault: false) on propose_change and revise_change while retaining the
//     underlying cause via Unwrap() and never echoing provider StopReason or
//     rejected model strings.
//  2. Post-decode validateCP1Analysis failures (duplicate ambiguity option,
//     invalid verification_kind) return HTTP 502 analysis_rejected without
//     echoing rejected model strings.
//  3. Non-validation provider/transport failures remain HTTP 500 faults.
//  4. OpenTelemetry spans record the failure on both propose_change and
//     revise_change.
func TestCP1AnalysisRejectedContractAcrossModelBoundaries(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	recorder := &recordingTracerProvider{}
	otel.SetTracerProvider(recorder)
	t.Cleanup(func() { otel.SetTracerProvider(prevProvider) })

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	auth := NewTokenAuthenticator(map[string]Principal{
		"alice-token": {ID: "alice@example.com"},
	})
	validAnalysis := CP1SemanticAnalysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1ModelExample{{
			Scenario:         "Refund at limit",
			InputSummary:     "$50 subtotal",
			ExpectedOutcome:  "Instant refund",
			VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	}
	validBytes, err := json.Marshal(validAnalysis)
	require.NoError(t, err)

	for _, mode := range []struct {
		name      string
		makeModel func(t *testing.T, p *queuedModelClient) model.Client
	}{
		{name: "direct_model_client", makeModel: newQueuedModelClient},
		{name: "grpc_model_gateway", makeModel: func(t *testing.T, p *queuedModelClient) model.Client {
			return startGatewayModelClient(t, p)
		}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			q := &queuedModelClient{}
			cp := newControlPlane(t, Options{
				Analyzer: NewModelCP1Analyzer(mode.makeModel(t, q)),
			})
			handler := NewHTTPHandler(cp, auth)
			adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)

			// Seed an initial card for revise_change checks.
			q.enqueueJSON(t, validAnalysis)
			seedCard, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
				ChangeID: "chg-seed",
				Prompt:   "Refunds up to $50 are instant.",
			})
			require.NoError(t, err)

			// 1. Malformed assistant text -> OutputValidationStructuredOutput -> 502 analysis_rejected.
			// 2. Empty message parts -> OutputValidationResponseShape -> 502 analysis_rejected.
			// 3. OutputLimited with private StopReason -> 502 analysis_rejected without StopReason leak.
			// 4. Upstream provider transport fault -> 500 fault.
			q.mu.Lock()
			q.responses = append(q.responses,
				queuedModelTurn{resp: &model.Response{
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: "not-json-private-marker"}},
					}},
					StopReason: "stop",
				}},
				queuedModelTurn{resp: &model.Response{
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: nil,
					}},
					StopReason: "stop",
				}},
				queuedModelTurn{resp: &model.Response{
					OutputLimited: true,
					StopReason:    "private-stop-marker",
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: string(validBytes)}},
					}},
				}},
				queuedModelTurn{err: errors.New("upstream transport reset")},
			)
			q.mu.Unlock()

			// Check 1: Direct Go call on ProposeChange preserves *model.OutputValidationError via errors.As.
			recorder.reset()
			_, err = cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
				ChangeID: "chg-reject-1",
				Prompt:   "Refunds.",
			})
			require.Error(t, err)
			var ovErr *model.OutputValidationError
			require.ErrorAs(t, err, &ovErr)
			assert.Equal(t, model.OutputValidationStructuredOutput, ovErr.Kind())
			assert.Contains(t, err.Error(), `cp1 analysis rejected (model output failed "structured_output" validation): this propose_change request did not save Change Card "chg-reject-1"`)
			assert.NotContains(t, err.Error(), "not-json-private-marker")
			spans := recorder.matching("surla.controlplane.propose_change")
			require.Len(t, spans, 1)
			assert.Equal(t, codes.Error, spans[0].statusCode)

			// Check 2: HTTP POST /changes/propose on response_shape returns 502 analysis_rejected (fault: false).
			reqBody, err := json.Marshal(map[string]any{"change_id": "chg-reject-2", "prompt": "Refunds."})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/changes/propose", strings.NewReader(string(reqBody)))
			req.Header.Set("Authorization", "Bearer alice-token")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, http.StatusBadGateway, rec.Code)
			var errBody struct {
				Name    string `json:"name"`
				Message string `json:"message"`
				Fault   bool   `json:"fault"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
			assert.Equal(t, "analysis_rejected", errBody.Name)
			assert.False(t, errBody.Fault)
			assert.Contains(t, errBody.Message, `model output failed "response_shape" validation`)

			// Check 3: HTTP POST /changes/revise & MCP on OutputLimited returns 502 without leaking StopReason.
			recorder.reset()
			revBody, err := json.Marshal(map[string]any{
				"change_id":        "chg-seed",
				"request_revision": seedCard.RequestRevision,
				"prompt":           "Revised refunds.",
			})
			require.NoError(t, err)
			revReq := httptest.NewRequest(http.MethodPost, "/changes/revise", strings.NewReader(string(revBody)))
			revReq.Header.Set("Authorization", "Bearer alice-token")
			revReq.Header.Set("Content-Type", "application/json")
			revRec := httptest.NewRecorder()
			handler.ServeHTTP(revRec, revReq)
			assert.Equal(t, http.StatusBadGateway, revRec.Code)
			require.NoError(t, json.Unmarshal(revRec.Body.Bytes(), &errBody))
			assert.Equal(t, "analysis_rejected", errBody.Name)
			assert.False(t, errBody.Fault)
			assert.Contains(t, errBody.Message, `cp1 analysis rejected (provider stopped at output limit): this revise_change request did not modify Change Card "chg-seed"; retry revise_change with request_revision `+seedCard.RequestRevision)
			assert.NotContains(t, errBody.Message, "private-stop-marker")
			revSpans := recorder.matching("surla.controlplane.revise_change")
			require.Len(t, revSpans, 1)
			assert.Equal(t, codes.Error, revSpans[0].statusCode)

			// Check 4: Non-validation transport error remains HTTP 500 fault.
			faultReq := httptest.NewRequest(http.MethodPost, "/changes/propose", strings.NewReader(string(reqBody)))
			faultReq.Header.Set("Authorization", "Bearer alice-token")
			faultReq.Header.Set("Content-Type", "application/json")
			faultRec := httptest.NewRecorder()
			handler.ServeHTTP(faultRec, faultReq)
			assert.Equal(t, http.StatusInternalServerError, faultRec.Code)
			require.NoError(t, json.Unmarshal(faultRec.Body.Bytes(), &errBody))
			assert.True(t, errBody.Fault)

			// Verify MCP tools/call returns isError: true with the sanitized message on OutputLimited.
			q.mu.Lock()
			q.responses = append(q.responses, queuedModelTurn{resp: &model.Response{
				OutputLimited: true,
				StopReason:    "private-stop-marker",
				Content: []model.Message{{
					Role:  model.ConversationRoleAssistant,
					Parts: []model.Part{model.TextPart{Text: string(validBytes)}},
				}},
			}})
			q.mu.Unlock()
			mcpRes, err := adapter.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{
				Name:      "revise_change",
				Arguments: revBody,
			})
			require.NoError(t, err)
			complete, ok := mcpRes.Outcome.AsComplete()
			require.True(t, ok)
			require.NotNil(t, complete.IsError)
			assert.True(t, *complete.IsError)
			require.NotEmpty(t, complete.Content)
			require.NotNil(t, complete.Content[0].Text)
			assert.Contains(t, *complete.Content[0].Text, "cp1 analysis rejected (provider stopped at output limit)")
			assert.NotContains(t, *complete.Content[0].Text, "private-stop-marker")

			// Check 5 (R1): Sole wrapped and duplicate-joined OutputValidationError remain 502 analysis_rejected,
			// whereas OutputValidationError joined with an independent provider/observer fault or context
			// cancellation remains an HTTP 500 fault with all causes preserved.
			soleValidationErr := ovErr
			independentFault := errors.New("independent provider fault")
			q.mu.Lock()
			q.responses = append(q.responses,
				queuedModelTurn{err: fmt.Errorf("translated: %w", soleValidationErr)},
				queuedModelTurn{err: fmt.Errorf(
					"translated: %w",
					errors.Join(soleValidationErr, errors.Join(soleValidationErr, soleValidationErr)),
				)},
				queuedModelTurn{err: errors.Join(soleValidationErr, independentFault)},
				queuedModelTurn{err: errors.Join(soleValidationErr, context.Canceled)},
			)
			q.mu.Unlock()

			for _, name := range []string{"wrapped_sole_validation", "duplicate_joined_sole_validation"} {
				t.Run(name, func(t *testing.T) {
					_, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
						ChangeID: "chg-" + name,
						Prompt:   "Refunds.",
					})
					require.Error(t, err)
					se, ok := asAnalysisRejected(err)
					require.True(t, ok, "sole wrapped/duplicate validation must classify as analysis_rejected: %v", err)
					assert.Contains(t, se.Message, `model output failed "structured_output" validation`)
					var gotOV *model.OutputValidationError
					require.ErrorAs(t, err, &gotOV)
					assert.Equal(t, model.OutputValidationStructuredOutput, gotOV.Kind())
				})
			}

			// Combined validation + independent fault -> HTTP 500 fault, retaining both causes in Go.
			_, err = cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
				ChangeID: "chg-mixed-fault",
				Prompt:   "Refunds.",
			})
			require.Error(t, err)
			_, isRejected := asAnalysisRejected(err)
			assert.False(t, isRejected, "validation joined with independent fault must not classify as analysis_rejected")
			var gotOV *model.OutputValidationError
			require.ErrorAs(t, err, &gotOV)
			assert.ErrorContains(t, err, "independent provider fault")

			// Combined validation + context.Canceled -> HTTP 500 fault over HTTP.
			mixedCancelReq := httptest.NewRequest(http.MethodPost, "/changes/propose", strings.NewReader(string(reqBody)))
			mixedCancelReq.Header.Set("Authorization", "Bearer alice-token")
			mixedCancelReq.Header.Set("Content-Type", "application/json")
			mixedCancelRec := httptest.NewRecorder()
			handler.ServeHTTP(mixedCancelRec, mixedCancelReq)
			assert.Equal(t, http.StatusInternalServerError, mixedCancelRec.Code)
			require.NoError(t, json.Unmarshal(mixedCancelRec.Body.Bytes(), &errBody))
			assert.True(t, errBody.Fault)
		})
	}

	// Post-decode validateCP1Analysis sanitization: duplicate option and invalid verification_kind
	// never echo rejected model-authored strings.
	t.Run("validateCP1Analysis_sanitizes_rejected_model_values", func(t *testing.T) {
		calls := 0
		cp := newControlPlane(t, Options{
			Analyzer: &stubCP1Analyzer{
				analyzeFn: func(context.Context, string, string) (CP1SemanticAnalysis, error) {
					calls++
					if calls == 1 {
						return CP1SemanticAnalysis{
							Summary: "Summary",
							Ambiguities: []CP1ModelAmbiguity{{
								Question: "Which option?",
								Options:  []string{"private-response-marker", "private-response-marker"},
							}},
							Examples: []CP1ModelExample{{
								Scenario: "S", InputSummary: "I", ExpectedOutcome: "O", VerificationKind: "deterministic",
							}},
						}, nil
					}
					return CP1SemanticAnalysis{
						Summary: "Summary",
						Examples: []CP1ModelExample{{
							Scenario: "S", InputSummary: "I", ExpectedOutcome: "O", VerificationKind: "private-kind-marker",
						}},
					}, nil
				},
			},
		})
		handler := NewHTTPHandler(cp, auth)

		for _, tc := range []struct {
			name        string
			wantSubstr  string
			forbidValue string
		}{
			{
				name:        "duplicate_option",
				wantSubstr:  `ambiguity "chg-val-ambiguity-01" has duplicate option at options[1]`,
				forbidValue: "private-response-marker",
			},
			{
				name:        "invalid_verification_kind",
				wantSubstr:  `examples[0] has invalid verification_kind (want "deterministic" or "model_judged")`,
				forbidValue: "private-kind-marker",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				reqBody, err := json.Marshal(map[string]any{"change_id": "chg-val", "prompt": "Refunds."})
				require.NoError(t, err)
				req := httptest.NewRequest(http.MethodPost, "/changes/propose", strings.NewReader(string(reqBody)))
				req.Header.Set("Authorization", "Bearer alice-token")
				req.Header.Set("Content-Type", "application/json")
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				assert.Equal(t, http.StatusBadGateway, rec.Code)
				var errBody struct {
					Name    string `json:"name"`
					Message string `json:"message"`
					Fault   bool   `json:"fault"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
				assert.Equal(t, "analysis_rejected", errBody.Name)
				assert.False(t, errBody.Fault)
				assert.Contains(t, errBody.Message, tc.wantSubstr)
				assert.NotContains(t, errBody.Message, tc.forbidValue)
				assert.NotContains(t, rec.Body.String(), tc.forbidValue)
			})
		}
	})
}

// TestCP1AnalysisRejectedConcurrentMutationCounterexample verifies that when
// Request A fails CP1 analysis validation while concurrent Request B creates or
// updates the same Change Card, Request A's error message claims only that
// Request A did not save/modify the card and leaves Request B's saved revision
// intact.
func TestCP1AnalysisRejectedConcurrentMutationCounterexample(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	startedA, releaseA := make(chan struct{}), make(chan struct{})
	callNum := 0
	cp := newControlPlane(t, Options{
		Analyzer: &stubCP1Analyzer{
			analyzeFn: func(context.Context, string, string) (CP1SemanticAnalysis, error) {
				callNum++
				if callNum == 1 {
					close(startedA)
					<-releaseA
					return CP1SemanticAnalysis{Summary: ""}, nil
				}
				return CP1SemanticAnalysis{
					Summary: "Saved by concurrent Request B.",
					Examples: []CP1ModelExample{{
						Scenario: "S", InputSummary: "I", ExpectedOutcome: "O", VerificationKind: "deterministic",
					}},
				}, nil
			},
		},
	})

	errCh := runAsyncProposeChange(aliceCtx, cp, &gensurla.ProposeChangePayload{
		ChangeID: "chg-race",
		Prompt:   "Request A",
	})
	<-startedA

	cardB, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-race",
		Prompt:   "Request B",
	})
	require.NoError(t, err)
	close(releaseA)

	errA := <-errCh
	require.Error(t, errA)
	assert.Contains(t, errA.Error(), `this propose_change request did not save Change Card "chg-race"`)
	assert.Contains(t, errA.Error(), "or call get_change if another request may have created the card")

	got, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-race"})
	require.NoError(t, err)
	assert.Equal(t, cardB.RequestRevision, got.RequestRevision)
	assert.Equal(t, "Saved by concurrent Request B.", got.Summary)
}

// TestCP1ReusedAnalyzerErrorImmutabilityAndSaveSpanRecording verifies:
//  1. R2: Reusing a single shared analysis_rejected ServiceError (and an outer
//     wrapped analysis_rejected error) across sequential and concurrent
//     ProposeChange and ReviseChange calls never mutates the analyzer's error
//     or earlier callers' returned error messages, and retains the entire
//     original cause chain via Unwrap.
//  2. R3: Storage failures reached after valid CP1 analysis (such as an
//     unusable staging path in cardStore.save) record both the error and
//     codes.Error on the surla.controlplane.propose_change and
//     surla.controlplane.revise_change spans, while expected conflict refusals
//     leave the span status Unset.
func TestCP1ReusedAnalyzerErrorImmutabilityAndSaveSpanRecording(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	recorder := &recordingTracerProvider{}
	otel.SetTracerProvider(recorder)
	t.Cleanup(func() { otel.SetTracerProvider(prevProvider) })

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	rootCause := errors.New("underlying analyzer cause")
	sharedRejected := newAnalysisRejected("shared analyzer reason", rootCause)
	wrappedShared := fmt.Errorf("outer analyzer wrapper: %w", sharedRejected)

	reusedCP := newControlPlane(t, Options{
		Analyzer: &stubCP1Analyzer{
			analyzeFn: func(_ context.Context, promptText, _ string) (CP1SemanticAnalysis, error) {
				if promptText == "seed" {
					return CP1SemanticAnalysis{
						Summary: "Seed summary.",
						Examples: []CP1ModelExample{{
							Scenario: "S", InputSummary: "I", ExpectedOutcome: "O", VerificationKind: "deterministic",
						}},
					}, nil
				}
				return CP1SemanticAnalysis{}, wrappedShared
			},
			reviseFn: func(context.Context, CP1PriorCard, string, string) (CP1SemanticAnalysis, error) {
				return CP1SemanticAnalysis{}, sharedRejected
			},
		},
	})
	seed, err := reusedCP.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-seed",
		Prompt:   "seed",
	})
	require.NoError(t, err)

	_, errA := reusedCP.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-seq-a",
		Prompt:   "fail",
	})
	require.Error(t, errA)
	msgABefore := errA.Error()
	assert.Contains(t, msgABefore, `Change Card "chg-seq-a"`)
	assert.Equal(t, "shared analyzer reason", sharedRejected.Message)
	assert.ErrorIs(t, errA, rootCause)
	assert.ErrorContains(t, errors.Unwrap(errA), "outer analyzer wrapper")

	// Concurrent ProposeChange and ReviseChange reusing the same sharedRejected instance.
	proposeCh := runAsyncProposeChange(aliceCtx, reusedCP, &gensurla.ProposeChangePayload{
		ChangeID: "chg-conc-b",
		Prompt:   "fail",
	})
	_, errRev := reusedCP.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-seed",
		RequestRevision: seed.RequestRevision,
		Prompt:          "revise fail",
	})
	errB := <-proposeCh
	require.Error(t, errB)
	require.Error(t, errRev)
	assert.Equal(t, "shared analyzer reason", sharedRejected.Message)
	assert.Equal(t, msgABefore, errA.Error(), "earlier caller's returned error message must not be mutated")
	assert.Contains(t, errB.Error(), `this propose_change request did not save Change Card "chg-conc-b"`)
	assert.NotContains(t, errB.Error(), "chg-seq-a")
	assert.Contains(t, errRev.Error(), `this revise_change request did not modify Change Card "chg-seed"; retry revise_change with request_revision `+seed.RequestRevision)

	// R3: Deterministic cardStore.save failure after valid analysis records codes.Error on both spans.
	cardsDir := t.TempDir()
	saveFailCP, err := New(Options{
		CardsDir: cardsDir,
		Analyzer: &stubCP1Analyzer{
			analyzeFn: func(context.Context, string, string) (CP1SemanticAnalysis, error) {
				return CP1SemanticAnalysis{
					Summary: "Valid summary.",
					Examples: []CP1ModelExample{{
						Scenario: "S", InputSummary: "I", ExpectedOutcome: "O", VerificationKind: "deterministic",
					}},
				}, nil
			},
			reviseFn: func(context.Context, CP1PriorCard, string, string) (CP1SemanticAnalysis, error) {
				return CP1SemanticAnalysis{
					Summary: "Revised valid summary.",
					Examples: []CP1ModelExample{{
						Scenario: "S", InputSummary: "I", ExpectedOutcome: "O", VerificationKind: "deterministic",
					}},
				}, nil
			},
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, saveFailCP.Close()) })

	initialCard, err := saveFailCP.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-existing",
		Prompt:   "Initial.",
	})
	require.NoError(t, err)

	// Replace the staging file path with a directory so cardStore.save fails deterministically after analysis.
	stagingPath := filepath.Join(cardsDir, stagingFile)
	require.NoError(t, os.Mkdir(stagingPath, 0o700))

	recorder.reset()
	_, err = saveFailCP.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-save-fail",
		Prompt:   "Will fail on save.",
	})
	require.Error(t, err)
	proposeSpans := recorder.matching("surla.controlplane.propose_change")
	require.Len(t, proposeSpans, 1)
	assert.Equal(t, codes.Error, proposeSpans[0].statusCode)
	assert.Equal(t, "save cp1 change card", proposeSpans[0].statusDesc)
	assert.NotEmpty(t, proposeSpans[0].errors)

	recorder.reset()
	_, err = saveFailCP.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-existing",
		RequestRevision: initialCard.RequestRevision,
		Prompt:          "Will fail on revise save.",
	})
	require.Error(t, err)
	reviseSpans := recorder.matching("surla.controlplane.revise_change")
	require.Len(t, reviseSpans, 1)
	assert.Equal(t, codes.Error, reviseSpans[0].statusCode)
	assert.Equal(t, "save revised cp1 change card", reviseSpans[0].statusDesc)
	assert.NotEmpty(t, reviseSpans[0].errors)

	// Expected post-analysis conflict refusal keeps span status Unset with no recorded error.
	startedConflict, releaseConflict := make(chan struct{}), make(chan struct{})
	conflictTurns := 0
	conflictCP := newControlPlane(t, Options{
		Analyzer: &stubCP1Analyzer{
			analyzeFn: func(context.Context, string, string) (CP1SemanticAnalysis, error) {
				conflictTurns++
				if conflictTurns == 1 {
					close(startedConflict)
					<-releaseConflict
				}
				return CP1SemanticAnalysis{
					Summary: "Valid summary.",
					Examples: []CP1ModelExample{{
						Scenario: "S", InputSummary: "I", ExpectedOutcome: "O", VerificationKind: "deterministic",
					}},
				}, nil
			},
		},
	})
	recorder.reset()
	conflictCh := runAsyncProposeChange(aliceCtx, conflictCP, &gensurla.ProposeChangePayload{
		ChangeID: "chg-conflict",
		Prompt:   "First.",
	})
	<-startedConflict
	_, err = conflictCP.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-conflict",
		Prompt:   "Second wins.",
	})
	require.NoError(t, err)
	close(releaseConflict)
	conflictErr := <-conflictCh
	require.Error(t, conflictErr)
	var conflictSE *goa.ServiceError
	require.ErrorAs(t, conflictErr, &conflictSE)
	assert.Equal(t, "conflict", conflictSE.GoaErrorName())
	for _, s := range recorder.matching("surla.controlplane.propose_change") {
		assert.Equal(t, codes.Unset, s.statusCode)
		assert.Empty(t, s.errors)
	}
}

// TestFormatCP1DesignSectionPreservesDesignAndSpecializesEmpty verifies that
// Analyze and Revise substitute the explicit no-design notice when
// design_source is empty or whitespace-only, preserve exact design_source
// bytes when provided or inherited on ReviseChange, and keep canonical
// RequestRevision hashing unchanged.
func TestFormatCP1DesignSectionPreservesDesignAndSpecializesEmpty(t *testing.T) {
	const noDesignNotice = "(no Goa design source was supplied — do not assume or invent service splits, endpoints, or fixture IDs that are not stated in the request or prior card)"
	assert.Equal(t, noDesignNotice, formatCP1DesignSection(""))
	assert.Equal(t, noDesignNotice, formatCP1DesignSection("   \n\t "))
	assert.Equal(t, "  package design\n", formatCP1DesignSection("  package design\n"))

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	q := &queuedModelClient{}
	cp := newControlPlane(t, Options{
		Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, q)),
	})
	validAnalysis := CP1SemanticAnalysis{
		Summary: "Single-service assistant.",
		Examples: []CP1ModelExample{{
			Scenario: "Ask assistant", InputSummary: "Hello", ExpectedOutcome: "Replies", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{},
	}

	// 1. ProposeChange with omitted design_source sends noDesignNotice in user prompt,
	// while canonical RequestRevision still hashes raw "".
	q.enqueueJSON(t, validAnalysis)
	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-nodesign",
		Prompt:   "Build a single-service assistant.",
	})
	require.NoError(t, err)
	userText := q.lastRequest().Messages[1].Parts[0].(model.TextPart).Text
	assert.True(t, strings.HasSuffix(userText, "\n\nDesign:\n"+noDesignNotice), userText)
	wantRev, err := computeCanonicalRevision(card, "alice@example.com", "")
	require.NoError(t, err)
	assert.Equal(t, wantRev, card.RequestRevision)

	// 2. ReviseChange with omitted design_source on a card with empty designSource also sends noDesignNotice.
	q.enqueueJSON(t, validAnalysis)
	_, err = cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-nodesign",
		RequestRevision: card.RequestRevision,
		Prompt:          "Refine the single-service assistant.",
	})
	require.NoError(t, err)
	_, _, sentDesign := extractPriorCardFromUserPrompt(t, q.lastRequest())
	assert.Equal(t, noDesignNotice, sentDesign)
}

// Analyze delegates to analyzeFn.
func (s *stubCP1Analyzer) Analyze(ctx context.Context, promptText, designSource string) (CP1SemanticAnalysis, error) {
	return s.analyzeFn(ctx, promptText, designSource)
}

// Revise delegates to reviseFn.
func (s *stubCP1Analyzer) Revise(ctx context.Context, prior CP1PriorCard, promptText, designSource string) (CP1SemanticAnalysis, error) {
	return s.reviseFn(ctx, prior, promptText, designSource)
}

// runAsyncProposeChange runs cp.ProposeChange in a background goroutine and
// sends the returned error to the returned buffered channel.
func runAsyncProposeChange(
	ctx context.Context,
	cp *ControlPlane,
	p *gensurla.ProposeChangePayload,
) <-chan error {
	errCh := make(chan error, 1)
	go executeProposeChange(ctx, cp, p, errCh)
	return errCh
}

// executeProposeChange invokes cp.ProposeChange and writes the error to errCh.
func executeProposeChange(
	ctx context.Context,
	cp *ControlPlane,
	p *gensurla.ProposeChangePayload,
	errCh chan<- error,
) {
	_, err := cp.ProposeChange(ctx, p)
	errCh <- err
}

// startGatewayModelClient serves provider through an in-process goa-ai gRPC
// model gateway and returns a connected gateway.NewGRPCClient.
func startGatewayModelClient(t *testing.T, provider model.Provider) model.Client {
	t.Helper()
	gw, err := gateway.NewServer(gateway.WithProvider(provider))
	require.NoError(t, err)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer()
	gateway.RegisterGRPCServer(srv, gw)
	go serveGateway(t, srv, lis)
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, conn.Close())
	})
	client, err := gateway.NewGRPCClient(conn)
	require.NoError(t, err)
	return client
}

// serveGateway runs srv.Serve(lis) and fails t if Serve returns an unexpected error.
func serveGateway(t *testing.T, srv *grpc.Server, lis net.Listener) {
	if serveErr := srv.Serve(lis); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
		t.Errorf("gateway server: %v", serveErr)
	}
}
