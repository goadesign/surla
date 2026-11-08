// This file tests Checkpoint 1 analysis_rejected error contracts, configurable
// analyzer error preservation, OpenTelemetry span recording, and design_source
// prompt specialization.

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
)

// stubCP1Analyzer implements CP1Analyzer directly so tests can exercise
// post-decode structural checks in validateCP1Analysis and concurrent
// analyzer turns.
type stubCP1Analyzer struct {
	// analyzeFn handles Analyze calls.
	analyzeFn func(ctx context.Context, promptText, designSource string) (CP1SemanticAnalysis, error)
	// reviseFn handles Revise calls.
	reviseFn func(ctx context.Context, prior CP1PriorCard, promptText, designSource string) (CP1RevisionSemanticAnalysis, error)
}

// TestCP1AnalysisRejectedContractAcrossModelBoundaries verifies that:
//  1. Both direct model.NewClient and gRPC gateway.NewGRPCClient boundaries
//     classify model output validation failures (structured_output,
//     response_shape, wrapped, and duplicate-joined) and OutputLimited stops
//     as HTTP 502 analysis_rejected (fault: false) on propose_change and
//     revise_change while retaining the underlying cause via Unwrap() and
//     never echoing provider StopReason or rejected model strings.
//  2. OutputValidationError joined with an independent provider/observer fault
//     or context cancellation remains an HTTP 500 fault with all causes
//     preserved.
//  3. Post-decode validateCP1Analysis failures (duplicate ambiguity option,
//     invalid verification_kind) return HTTP 502 analysis_rejected without
//     echoing rejected model strings.
//  4. Non-validation provider/transport failures remain HTTP 500 faults.
//  5. OpenTelemetry spans record the failure on both propose_change and
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
	validReviseBytes, err := json.Marshal(CP1RevisionSemanticAnalysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1RevisionModelExample{{
			PriorID:          "check_01",
			Scenario:         "Refund at limit",
			InputSummary:     "$50 subtotal",
			ExpectedOutcome:  "Instant refund",
			VerificationKind: "deterministic",
		}},
		RemovedExampleIDs:  []string{},
		PreservedBehaviors: []string{"Cross-tenant lookups return 404."},
	})
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
						Parts: []model.Part{model.TextPart{Text: string(validReviseBytes)}},
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
					Parts: []model.Part{model.TextPart{Text: string(validReviseBytes)}},
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
//  2. Mixed named-rejection joins (`errors.Join(sharedRejected, context.Canceled)`
//     and `errors.Join(sharedRejected, independentFault)`) return HTTP 500
//     (fault: true) on both POST /changes/propose and POST /changes/revise,
//     preserve all causes in process, and remain MCP isError tool results.
//  3. Declared non-rejection analyzer errors (`gensurla.MakeUnprocessable`)
//     remain HTTP 422 and explicit analyzer faults (`goa.Fault`, direct and
//     singly wrapped) remain HTTP 500 and MCP protocol errors on both
//     propose_change and revise_change.
//  4. R3: Storage failures reached after valid CP1 analysis (such as an
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
			reviseFn: func(context.Context, CP1PriorCard, string, string) (CP1RevisionSemanticAnalysis, error) {
				return CP1RevisionSemanticAnalysis{}, sharedRejected
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

	// Configurable analyzer joining a named analysis_rejected ServiceError with
	// context.Canceled or an independent fault must return HTTP 500 (fault: true)
	// on both POST /changes/propose and POST /changes/revise, preserve all
	// causes in process, and remain an MCP isError tool result.
	independentAnalyzerFault := errors.New("independent analyzer fault")
	classifyPromptError := func(promptText string) (error, bool) {
		switch promptText {
		case "unprocessable":
			return fmt.Errorf("wrap: %w", gensurla.MakeUnprocessable(errors.New("unsupported domain"))), true
		case "unprocessable_retained_direct":
			return gensurla.MakeUnprocessable(sharedRejected), true
		case "unprocessable_retained_wrapped":
			return fmt.Errorf("wrap: %w", gensurla.MakeUnprocessable(sharedRejected)), true
		case "explicit_fault":
			return fmt.Errorf("wrap: %w", goa.Fault("provider unavailable")), true
		case "explicit_fault_retained_direct":
			return goa.NewServiceError(errors.Join(sharedRejected, context.Canceled), "fault", false, false, true), true
		case "explicit_fault_retained_wrapped":
			return fmt.Errorf("wrap: %w", goa.NewServiceError(errors.Join(sharedRejected, context.Canceled), "fault", false, false, true)), true
		default:
			return nil, false
		}
	}
	mixedNamedTurn := 0
	mixedNamedCP := newControlPlane(t, Options{
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
				if err, ok := classifyPromptError(promptText); ok {
					return CP1SemanticAnalysis{}, err
				}
				mixedNamedTurn++
				if mixedNamedTurn == 1 {
					return CP1SemanticAnalysis{}, errors.Join(sharedRejected, context.Canceled)
				}
				return CP1SemanticAnalysis{}, errors.Join(sharedRejected, independentAnalyzerFault)
			},
			reviseFn: func(_ context.Context, _ CP1PriorCard, promptText, _ string) (CP1RevisionSemanticAnalysis, error) {
				if err, ok := classifyPromptError(promptText); ok {
					return CP1RevisionSemanticAnalysis{}, err
				}
				mixedNamedTurn++
				if mixedNamedTurn == 3 {
					return CP1RevisionSemanticAnalysis{}, errors.Join(sharedRejected, context.Canceled)
				}
				return CP1RevisionSemanticAnalysis{}, errors.Join(sharedRejected, independentAnalyzerFault)
			},
		},
	})
	mixedSeed, err := mixedNamedCP.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-mixed-seed",
		Prompt:   "seed",
	})
	require.NoError(t, err)
	mixedHandler := NewHTTPHandler(mixedNamedCP, NewTokenAuthenticator(map[string]Principal{
		"alice-token": {ID: "alice@example.com"},
	}))
	mixedMCP := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(mixedNamedCP), nil)

	// 1. Go in-process & HTTP POST /changes/propose with joined analysis_rejected + context.Canceled / independent fault.
	_, goMixedProposeErr := mixedNamedCP.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-mixed-propose-go",
		Prompt:   "fail",
	})
	require.Error(t, goMixedProposeErr)
	assert.ErrorIs(t, goMixedProposeErr, context.Canceled)
	assert.ErrorIs(t, goMixedProposeErr, sharedRejected)
	assert.ErrorIs(t, goMixedProposeErr, rootCause)

	proposeMixedBody, err := json.Marshal(map[string]any{"change_id": "chg-mixed-propose-http", "prompt": "fail"})
	require.NoError(t, err)
	proposeMixedReq := httptest.NewRequest(http.MethodPost, "/changes/propose", strings.NewReader(string(proposeMixedBody)))
	proposeMixedReq.Header.Set("Authorization", "Bearer alice-token")
	proposeMixedReq.Header.Set("Content-Type", "application/json")
	proposeMixedRec := httptest.NewRecorder()
	mixedHandler.ServeHTTP(proposeMixedRec, proposeMixedReq)
	assert.Equal(t, http.StatusInternalServerError, proposeMixedRec.Code)
	var mixedHTTPBody struct {
		Name    string `json:"name"`
		Message string `json:"message"`
		Fault   bool   `json:"fault"`
	}
	require.NoError(t, json.Unmarshal(proposeMixedRec.Body.Bytes(), &mixedHTTPBody))
	assert.True(t, mixedHTTPBody.Fault)
	assert.Equal(t, "fault", mixedHTTPBody.Name)

	// 2. HTTP POST /changes/revise & MCP revise_change with joined analysis_rejected + context.Canceled / independent fault.
	reviseMixedBody, err := json.Marshal(map[string]any{
		"change_id":        "chg-mixed-seed",
		"request_revision": mixedSeed.RequestRevision,
		"prompt":           "fail",
	})
	require.NoError(t, err)
	reviseMixedReq := httptest.NewRequest(http.MethodPost, "/changes/revise", strings.NewReader(string(reviseMixedBody)))
	reviseMixedReq.Header.Set("Authorization", "Bearer alice-token")
	reviseMixedReq.Header.Set("Content-Type", "application/json")
	reviseMixedRec := httptest.NewRecorder()
	mixedHandler.ServeHTTP(reviseMixedRec, reviseMixedReq)
	assert.Equal(t, http.StatusInternalServerError, reviseMixedRec.Code)
	require.NoError(t, json.Unmarshal(reviseMixedRec.Body.Bytes(), &mixedHTTPBody))
	assert.True(t, mixedHTTPBody.Fault)
	assert.Equal(t, "fault", mixedHTTPBody.Name)

	mcpMixedRes, err := mixedMCP.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{
		Name:      "revise_change",
		Arguments: reviseMixedBody,
	})
	require.NoError(t, err)
	mcpMixedComplete, ok := mcpMixedRes.Outcome.AsComplete()
	require.True(t, ok)
	require.NotNil(t, mcpMixedComplete.IsError)
	assert.True(t, *mcpMixedComplete.IsError)

	// 3. Preservation of declared non-rejection errors (MakeUnprocessable -> HTTP 422, MCP isError)
	// and explicit faults (goa.Fault -> HTTP 500, MCP protocol error) on propose_change and revise_change.
	for _, ep := range []struct {
		name     string
		httpPath string
		mcpTool  string
		makeBody func(promptText string) []byte
	}{
		{
			name:     "propose_change",
			httpPath: "/changes/propose",
			mcpTool:  "propose_change",
			makeBody: func(promptText string) []byte {
				b, err := json.Marshal(map[string]any{"change_id": "chg-preserve-" + promptText, "prompt": promptText})
				require.NoError(t, err)
				return b
			},
		},
		{
			name:     "revise_change",
			httpPath: "/changes/revise",
			mcpTool:  "revise_change",
			makeBody: func(promptText string) []byte {
				b, err := json.Marshal(map[string]any{
					"change_id":        "chg-mixed-seed",
					"request_revision": mixedSeed.RequestRevision,
					"prompt":           promptText,
				})
				require.NoError(t, err)
				return b
			},
		},
	} {
		t.Run("preserve_"+ep.name, func(t *testing.T) {
			for _, unprocPrompt := range []string{"unprocessable", "unprocessable_retained_direct", "unprocessable_retained_wrapped"} {
				unprocBody := ep.makeBody(unprocPrompt)
				unprocReq := httptest.NewRequest(http.MethodPost, ep.httpPath, strings.NewReader(string(unprocBody)))
				unprocReq.Header.Set("Authorization", "Bearer alice-token")
				unprocReq.Header.Set("Content-Type", "application/json")
				unprocRec := httptest.NewRecorder()
				mixedHandler.ServeHTTP(unprocRec, unprocReq)
				assert.Equal(t, http.StatusUnprocessableEntity, unprocRec.Code, unprocPrompt)
				require.NoError(t, json.Unmarshal(unprocRec.Body.Bytes(), &mixedHTTPBody))
				assert.False(t, mixedHTTPBody.Fault, unprocPrompt)
				assert.Equal(t, "unprocessable", mixedHTTPBody.Name, unprocPrompt)

				mcpUnproc, err := mixedMCP.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{
					Name:      ep.mcpTool,
					Arguments: unprocBody,
				})
				require.NoError(t, err, unprocPrompt)
				unprocComplete, ok := mcpUnproc.Outcome.AsComplete()
				require.True(t, ok, unprocPrompt)
				require.NotNil(t, unprocComplete.IsError, unprocPrompt)
				assert.True(t, *unprocComplete.IsError, unprocPrompt)
			}

			for _, faultPrompt := range []string{"explicit_fault", "explicit_fault_retained_direct", "explicit_fault_retained_wrapped"} {
				faultBody := ep.makeBody(faultPrompt)
				faultReq := httptest.NewRequest(http.MethodPost, ep.httpPath, strings.NewReader(string(faultBody)))
				faultReq.Header.Set("Authorization", "Bearer alice-token")
				faultReq.Header.Set("Content-Type", "application/json")
				faultRec := httptest.NewRecorder()
				mixedHandler.ServeHTTP(faultRec, faultReq)
				assert.Equal(t, http.StatusInternalServerError, faultRec.Code, faultPrompt)
				require.NoError(t, json.Unmarshal(faultRec.Body.Bytes(), &mixedHTTPBody))
				assert.True(t, mixedHTTPBody.Fault, faultPrompt)

				_, mcpFaultErr := mixedMCP.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{
					Name:      ep.mcpTool,
					Arguments: faultBody,
				})
				require.Error(t, mcpFaultErr, "explicit fault (%s) must remain an MCP protocol error", faultPrompt)
				var faultSE *goa.ServiceError
				require.ErrorAs(t, mcpFaultErr, &faultSE, faultPrompt)
				assert.True(t, faultSE.Fault, faultPrompt)
			}
		})
	}

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
			reviseFn: func(_ context.Context, prior CP1PriorCard, _, _ string) (CP1RevisionSemanticAnalysis, error) {
				var priorID string
				if len(prior.Examples) > 0 {
					priorID = prior.Examples[0].ID
				}
				return CP1RevisionSemanticAnalysis{
					Summary: "Revised valid summary.",
					Examples: []CP1RevisionModelExample{{
						PriorID:          priorID,
						Scenario:         "S",
						InputSummary:     "I",
						ExpectedOutcome:  "O",
						VerificationKind: "deterministic",
					}},
					RemovedExampleIDs: []string{},
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
		Summary:     "Single-service assistant.",
		Ambiguities: []CP1ModelAmbiguity{},
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
	validReviseAnalysis := CP1RevisionSemanticAnalysis{
		Summary:     "Single-service assistant.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1RevisionModelExample{{
			PriorID:          "check_01",
			Scenario:         "Ask assistant",
			InputSummary:     "Hello",
			ExpectedOutcome:  "Replies",
			VerificationKind: "deterministic",
		}},
		RemovedExampleIDs:  []string{},
		PreservedBehaviors: []string{},
	}
	q.enqueueJSON(t, validReviseAnalysis)
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
func (s *stubCP1Analyzer) Revise(ctx context.Context, prior CP1PriorCard, promptText, designSource string) (CP1RevisionSemanticAnalysis, error) {
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
