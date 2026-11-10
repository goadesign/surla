// Package controlplane provides the core business logic and state management
// for the Surla control plane.
//
// This file tests the Checkpoint 1 two-phase `read_design` inspection loop and
// typed CP1 OpenTelemetry span attributes across both direct model clients and
// gRPC model gateway boundaries.
package controlplane

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"goa.design/goa-ai/runtime/agent/model"
	"goa.design/goa-ai/runtime/agent/rawjson"
)

type (
	// modelClientFactory builds a model.Client backed either directly by
	// queuedModelClient or through an in-process gRPC model gateway.
	modelClientFactory func(t *testing.T, q *queuedModelClient) model.Client
)

const (
	// validAnalysisJSON is a valid CP1ChangeCardAnalysis response JSON string.
	validAnalysisJSON = `{"summary":"Order refund processing.","ambiguities":[],"examples":[{"scenario":"Standard refund","input_summary":"Refund $10","expected_outcome":"Success","verification_kind":"deterministic"}],"preserved_behaviors":["Existing orders unchanged"]}`

	// validRevisionAnalysisJSON is a valid CP1ReviseChangeCardAnalysis response JSON string.
	validRevisionAnalysisJSON = `{"summary":"Revised order refund processing.","ambiguities":[],"examples":[{"prior_id":"check_01","scenario":"Standard refund","input_summary":"Refund $10","expected_outcome":"Success","verification_kind":"deterministic"}],"removed_example_ids":[],"preserved_behaviors":["Existing orders unchanged"]}`
)

// clientFactories provides the test matrix for both direct and gRPC gateway model clients.
var clientFactories = []struct {
	name    string
	buildFn modelClientFactory
}{
	{
		name: "direct_client",
		buildFn: func(t *testing.T, q *queuedModelClient) model.Client {
			return newQueuedModelClient(t, q)
		},
	},
	{
		name: "grpc_gateway",
		buildFn: func(t *testing.T, q *queuedModelClient) model.Client {
			return startGatewayModelClient(t, q)
		},
	},
}

// TestCP1TwoPhaseReadDesign_SelectiveAndOmitPath verifies that multi-file
// workspaces run Phase 1 read_design tool turns and transition to Phase 2
// either upon reading each file selectively or via omit-path read-all.
func TestCP1TwoPhaseReadDesign_SelectiveAndOmitPath(t *testing.T) {
	for _, cf := range clientFactories {
		t.Run(cf.name, func(t *testing.T) {
			t.Run("selective_single_file_inspection", func(t *testing.T) {
				wsDir := setupWorkspaceWithFiles(t, map[string]string{
					"design/design.go": "package design\n// main design\n",
					"design/orders.go": "package design\n// orders design\n",
				})
				q := &queuedModelClient{
					responses: []queuedModelTurn{
						// Turn 0: Model inspects design/design.go
						{
							resp: &model.Response{
								Content: []model.Message{{
									Role: model.ConversationRoleAssistant,
									Parts: []model.Part{
										model.ToolUsePart{
											ID:    "call_1",
											Name:  "read_design",
											Input: rawjson.Message(`{"path":"design/design.go"}`),
										},
									},
								}},
								StopReason: "tool_use",
							},
						},
						// Turn 1: Model inspects design/orders.go (completing manifest)
						{
							resp: &model.Response{
								Content: []model.Message{{
									Role: model.ConversationRoleAssistant,
									Parts: []model.Part{
										model.ToolUsePart{
											ID:    "call_2",
											Name:  "read_design",
											Input: rawjson.Message(`{"path":"design/orders.go"}`),
										},
									},
								}},
								StopReason: "tool_use",
							},
						},
						// Turn 2: Phase 2 structured completion
						{
							resp: &model.Response{
								Content: []model.Message{{
									Role:  model.ConversationRoleAssistant,
									Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
								}},
								StopReason: "stop",
							},
						},
					},
				}

				cardsDir := t.TempDir()
				client := cf.buildFn(t, q)
				cp := newControlPlane(t, Options{
					CardsDir:     cardsDir,
					WorkspaceDir: wsDir,
					Analyzer:     NewModelCP1Analyzer(client),
				})

				aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
				card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
					ChangeID: "chg-selective",
					Prompt:   "Add order refunds",
				})
				require.NoError(t, err)
				assert.Equal(t, "Order refund processing.", card.Summary)
				assert.Len(t, q.requests, 3)

				// Verify Phase 1 turns
				assert.Equal(t, cp1InspectionMaxOutputTokens, q.requests[0].MaxTokens)
				assert.Len(t, q.requests[0].Tools, 1)
				assert.Equal(t, "read_design", q.requests[0].Tools[0].Name)

				// Verify Phase 2 turn
				assert.Equal(t, cp1AnalysisMaxOutputTokens, q.requests[2].MaxTokens)
				assert.Empty(t, q.requests[2].Tools)
				assertAlternatingRoles(t, q.requests[2].Messages)
				lastMsg := q.requests[2].Messages[len(q.requests[2].Messages)-1]
				assert.Equal(t, model.ConversationRoleUser, lastMsg.Role)
				require.Len(t, lastMsg.Parts, 2)
				assert.Contains(t, lastMsg.Parts[1].(model.TextPart).Text, "Produce the Change Card analysis JSON matching the schema.")
			})

			t.Run("omit_path_read_all_transitions_immediately", func(t *testing.T) {
				wsDir := setupWorkspaceWithFiles(t, map[string]string{
					"design/design.go": "package design\n// main design\n",
					"design/orders.go": "package design\n// orders design\n",
				})
				q := &queuedModelClient{
					responses: []queuedModelTurn{
						// Turn 0: Model calls read_design with omit-path ({})
						{
							resp: &model.Response{
								Content: []model.Message{{
									Role: model.ConversationRoleAssistant,
									Parts: []model.Part{
										model.ToolUsePart{
											ID:    "call_all",
											Name:  "read_design",
											Input: rawjson.Message(`{}`),
										},
									},
								}},
								StopReason: "tool_use",
							},
						},
						// Turn 1: Phase 2 structured completion (triggered by Stop Outcome 3)
						{
							resp: &model.Response{
								Content: []model.Message{{
									Role:  model.ConversationRoleAssistant,
									Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
								}},
								StopReason: "stop",
							},
						},
					},
				}

				cardsDir := t.TempDir()
				client := cf.buildFn(t, q)
				cp := newControlPlane(t, Options{
					CardsDir:     cardsDir,
					WorkspaceDir: wsDir,
					Analyzer:     NewModelCP1Analyzer(client),
				})

				aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
				card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
					ChangeID: "chg-omit-path",
					Prompt:   "Add order refunds",
				})
				require.NoError(t, err)
				assert.Equal(t, "Order refund processing.", card.Summary)
				assert.Len(t, q.requests, 2)
			})
		})
	}
}

// TestCP1TwoPhaseReadDesign_ModelChosenZeroTool verifies Stop Outcome 2:
// when the model returns 0 tool calls in Phase 1, it immediately transitions
// to Phase 2 with its assistant message preserved.
func TestCP1TwoPhaseReadDesign_ModelChosenZeroTool(t *testing.T) {
	wsDir := setupWorkspaceWithFiles(t, map[string]string{
		"design/design.go": "package design\n",
	})
	q := &queuedModelClient{
		responses: []queuedModelTurn{
			// Turn 0: Model returns 0 tool calls, only assistant text
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: "I have enough context from the manifest."}},
					}},
					StopReason: "stop",
				},
			},
			// Turn 1: Phase 2 structured completion
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
					}},
					StopReason: "stop",
				},
			},
		},
	}

	cardsDir := t.TempDir()
	cp := newControlPlane(t, Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
	})

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-zero-tool",
		Prompt:   "Add feature",
	})
	require.NoError(t, err)
	assert.Equal(t, "Order refund processing.", card.Summary)
	assert.Len(t, q.requests, 2)

	// Check that Phase 2 transcript includes the assistant message before the Phase 2 prompt
	phase2Req := q.requests[1]
	require.GreaterOrEqual(t, len(phase2Req.Messages), 4)
	asstMsg := phase2Req.Messages[len(phase2Req.Messages)-2]
	assert.Equal(t, model.ConversationRoleAssistant, asstMsg.Role)
	assert.Contains(t, asstMsg.Parts[0].(model.TextPart).Text, "I have enough context")
	finalUserMsg := phase2Req.Messages[len(phase2Req.Messages)-1]
	assert.Equal(t, model.ConversationRoleUser, finalUserMsg.Role)
}

// TestCP1TwoPhaseReadDesign_NonASCIIAndSpaces verifies that paths with non-ASCII
// characters (e.g. design/注文.go) and spaces (design/order rules.go) pass
// payload decoding and can be inspected without error.
func TestCP1TwoPhaseReadDesign_NonASCIIAndSpaces(t *testing.T) {
	wsDir := setupWorkspaceWithFiles(t, map[string]string{
		"design/注文.go":          "package design\n// Japanese order design\n",
		"design/order rules.go": "package design\n// order rules\n",
	})
	q := &queuedModelClient{
		responses: []queuedModelTurn{
			// Turn 0: Model calls read_design on non-ASCII path
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{
							model.ToolUsePart{
								ID:    "call_unicode",
								Name:  "read_design",
								Input: rawjson.Message(`{"path":"design/注文.go"}`),
							},
						},
					}},
					StopReason: "tool_use",
				},
			},
			// Turn 1: Model calls read_design on path with space
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{
							model.ToolUsePart{
								ID:    "call_space",
								Name:  "read_design",
								Input: rawjson.Message(`{"path":"design/order rules.go"}`),
							},
						},
					}},
					StopReason: "tool_use",
				},
			},
			// Turn 2: Phase 2 completion
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
					}},
					StopReason: "stop",
				},
			},
		},
	}

	cardsDir := t.TempDir()
	cp := newControlPlane(t, Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
	})

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-unicode",
		Prompt:   "Support Japanese orders",
	})
	require.NoError(t, err)
	assert.Equal(t, "Order refund processing.", card.Summary)
	assert.Len(t, q.requests, 3)
}

// TestCP1TwoPhaseReadDesign_UnknownPathAndRecovery verifies that unknown paths
// return IsError: true tool results listing available manifest files, and the
// model can recover on the subsequent turn.
func TestCP1TwoPhaseReadDesign_UnknownPathAndRecovery(t *testing.T) {
	wsDir := setupWorkspaceWithFiles(t, map[string]string{
		"design/design.go": "package design\n",
	})
	q := &queuedModelClient{
		responses: []queuedModelTurn{
			// Turn 0: Model calls read_design with unknown path
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{
							model.ToolUsePart{
								ID:    "call_unknown",
								Name:  "read_design",
								Input: rawjson.Message(`{"path":"design/missing.go"}`),
							},
						},
					}},
					StopReason: "tool_use",
				},
			},
			// Turn 1: Model recovers by calling valid path
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{
							model.ToolUsePart{
								ID:    "call_valid",
								Name:  "read_design",
								Input: rawjson.Message(`{"path":"design/design.go"}`),
							},
						},
					}},
					StopReason: "tool_use",
				},
			},
			// Turn 2: Phase 2 completion
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
					}},
					StopReason: "stop",
				},
			},
		},
	}

	cardsDir := t.TempDir()
	cp := newControlPlane(t, Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
	})

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-recovery",
		Prompt:   "Add feature",
	})
	require.NoError(t, err)
	assert.Equal(t, "Order refund processing.", card.Summary)
	assert.Len(t, q.requests, 3)

	// Check that Turn 1 received the tool error result
	turn1Msg := q.requests[1].Messages[len(q.requests[1].Messages)-1]
	require.Equal(t, model.ConversationRoleUser, turn1Msg.Role)
	toolRes, ok := turn1Msg.Parts[0].(model.ToolResultPart)
	require.True(t, ok)
	assert.True(t, toolRes.IsError)
	assert.Contains(t, toolRes.Content.(string), `unknown design file "design/missing.go"`)
	assert.Contains(t, toolRes.Content.(string), "design/design.go")
}

// TestCP1TwoPhaseReadDesign_PreservesMetadataAndThoughtSignature verifies that
// Meta, ThinkingPart, and ToolUsePart.ThoughtSignature from Phase 1 are
// preserved into Phase 2 requests.
func TestCP1TwoPhaseReadDesign_PreservesMetadataAndThoughtSignature(t *testing.T) {
	wsDir := setupWorkspaceWithFiles(t, map[string]string{
		"design/design.go": "package design\n",
	})
	q := &queuedModelClient{
		responses: []queuedModelTurn{
			// Turn 0: Model returns tool use with Meta, ThinkingPart, and ThoughtSignature
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Meta: map[string]any{"provider_request_id": "req-12345"},
						Parts: []model.Part{
							model.ThinkingPart{
								Text:      "Checking design requirements...",
								Signature: "sig-thinking-token",
								Final:     true,
							},
							model.ToolUsePart{
								ID:               "call_with_sig",
								Name:             "read_design",
								Input:            rawjson.Message(`{"path":"design/design.go"}`),
								ThoughtSignature: "thought-sig-opaque",
							},
						},
					}},
					StopReason: "tool_use",
				},
			},
			// Turn 1: Phase 2 completion
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
					}},
					StopReason: "stop",
				},
			},
		},
	}

	cardsDir := t.TempDir()
	cp := newControlPlane(t, Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
	})

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-metadata",
		Prompt:   "Add feature",
	})
	require.NoError(t, err)
	assert.Equal(t, "Order refund processing.", card.Summary)
	assert.Len(t, q.requests, 2)

	// Verify Phase 2 transcript preserves all fields
	phase2Messages := q.requests[1].Messages
	var foundAsst *model.Message
	for _, m := range phase2Messages {
		if m.Role == model.ConversationRoleAssistant {
			foundAsst = m
			break
		}
	}
	require.NotNil(t, foundAsst)
	assert.Equal(t, "req-12345", foundAsst.Meta["provider_request_id"])
	require.Len(t, foundAsst.Parts, 2)
	thinking, ok := foundAsst.Parts[0].(model.ThinkingPart)
	require.True(t, ok)
	assert.Equal(t, "sig-thinking-token", thinking.Signature)
	toolUse, ok := foundAsst.Parts[1].(model.ToolUsePart)
	require.True(t, ok)
	assert.Equal(t, "thought-sig-opaque", toolUse.ThoughtSignature)
}

// TestCP1TwoPhaseReadDesign_OutputLimitedCutoff verifies that Phase 1 fails
// immediately with OutputValidationOutputLimited rejection when the model
// response exceeds token allowances.
func TestCP1TwoPhaseReadDesign_OutputLimitedCutoff(t *testing.T) {
	wsDir := setupWorkspaceWithFiles(t, map[string]string{
		"design/design.go": "package design\n",
	})

	t.Run("resp_OutputLimited_true_before_tool_calls", func(t *testing.T) {
		q := &queuedModelClient{
			responses: []queuedModelTurn{
				{
					resp: &model.Response{
						OutputLimited: true,
						StopReason:    "max_tokens",
						Content: []model.Message{{
							Role: model.ConversationRoleAssistant,
							Parts: []model.Part{
								model.ToolUsePart{
									ID:    "truncated_tool_call",
									Name:  "read_design",
									Input: rawjson.Message(`{"path":"design/design.go"}`),
								},
							},
						}},
					},
				},
			},
		}

		cardsDir := t.TempDir()
		cp := newControlPlane(t, Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})

		aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
		_, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-limited-phase1",
			Prompt:   "Add feature",
		})
		require.Error(t, err)
		se, ok := asAnalysisRejected(err)
		require.True(t, ok)
		assert.Contains(t, se.Message, "provider stopped at output limit; shorten the request before retrying")
		assert.Equal(t, "output_limited", cp1RejectionKind(err))
	})

	t.Run("output_validation_error_output_limited", func(t *testing.T) {
		ovErr, err := model.RestoreOutputValidationError(
			model.OutputValidationOutputLimited,
			errors.New("hit token limit"),
			model.ResponseEvidence{},
			nil,
		)
		require.NoError(t, err)

		q := &queuedModelClient{
			responses: []queuedModelTurn{{err: ovErr}},
		}

		cardsDir := t.TempDir()
		cp := newControlPlane(t, Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})

		aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
		_, err = cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-limited-ov-err",
			Prompt:   "Add feature",
		})
		require.Error(t, err)
		se, ok := asAnalysisRejected(err)
		require.True(t, ok)
		assert.Contains(t, se.Message, "provider stopped at output limit; shorten the request before retrying")
		assert.Equal(t, "output_limited", cp1RejectionKind(err))
	})
}

// TestCP1TwoPhaseReadDesign_RecoveryCorrection verifies that correctable model
// output validation errors append the recovery correction as a user message and
// continue to the next turn, but reject if the budget is exhausted.
func TestCP1TwoPhaseReadDesign_RecoveryCorrection(t *testing.T) {
	wsDir := setupWorkspaceWithFiles(t, map[string]string{
		"design/design.go": "package design\n",
	})

	baseErr, err := model.RestoreOutputValidationError(
		model.OutputValidationStructuredOutput,
		errors.New("invalid tool input"),
		model.ResponseEvidence{},
		nil,
	)
	require.NoError(t, err)
	correctable, err := model.RestoreCorrectableOutputValidationError(
		baseErr,
		"pass a valid JSON object with path property",
	)
	require.NoError(t, err)

	t.Run("recovers_on_next_turn", func(t *testing.T) {
		q := &queuedModelClient{
			responses: []queuedModelTurn{
				// Turn 0: fails with correctable error
				{err: correctable},
				// Turn 1: model recovers and calls read_design
				{
					resp: &model.Response{
						Content: []model.Message{{
							Role: model.ConversationRoleAssistant,
							Parts: []model.Part{
								model.ToolUsePart{
									ID:    "call_recovered",
									Name:  "read_design",
									Input: rawjson.Message(`{}`),
								},
							},
						}},
						StopReason: "tool_use",
					},
				},
				// Turn 2: Phase 2 completion
				{
					resp: &model.Response{
						Content: []model.Message{{
							Role:  model.ConversationRoleAssistant,
							Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
						}},
						StopReason: "stop",
					},
				},
			},
		}

		cardsDir := t.TempDir()
		cp := newControlPlane(t, Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})

		aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-recover-turn",
			Prompt:   "Add feature",
		})
		require.NoError(t, err)
		assert.Equal(t, "Order refund processing.", card.Summary)
		assert.Len(t, q.requests, 3)

		// Turn 1 request must merge the recovery correction into the trailing user turn
		assertAlternatingRoles(t, q.requests[1].Messages)
		recoveryMsg := q.requests[1].Messages[len(q.requests[1].Messages)-1]
		require.Equal(t, model.ConversationRoleUser, recoveryMsg.Role)
		require.Len(t, recoveryMsg.Parts, 2)
		assert.Equal(t, "pass a valid JSON object with path property", recoveryMsg.Parts[1].(model.TextPart).Text)
	})

	t.Run("last_turn_exhaustion_fails_without_retry", func(t *testing.T) {
		// 1 file -> maxInspectionTurns = min(1+2, 8) = 3
		q := &queuedModelClient{
			responses: []queuedModelTurn{
				{err: correctable},
				{err: correctable},
				{err: correctable},
			},
		}

		cardsDir := t.TempDir()
		cp := newControlPlane(t, Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})

		aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
		_, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-recover-exhaust",
			Prompt:   "Add feature",
		})
		require.Error(t, err)
		se, ok := asAnalysisRejected(err)
		require.True(t, ok)
		assert.Contains(t, se.Message, `model output failed "structured_output" validation`)
		assert.Equal(t, "structured_output", cp1RejectionKind(err))
	})
}

// TestCP1TwoPhaseReadDesign_TurnBudgetExhaustion verifies Stop Outcome 4:
// when the model exceeds maxInspectionTurns without completing inspection,
// it rejects with "inspection_budget_exhausted".
func TestCP1TwoPhaseReadDesign_TurnBudgetExhaustion(t *testing.T) {
	wsDir := setupWorkspaceWithFiles(t, map[string]string{
		"design/design.go": "package design\n",
	})
	// 1 file -> maxInspectionTurns = min(1+2, 8) = 3 turns
	q := &queuedModelClient{
		responses: []queuedModelTurn{
			// Turn 0: Unknown path
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{
							model.ToolUsePart{ID: "call_1", Name: "read_design", Input: rawjson.Message(`{"path":"design/bad1.go"}`)},
						},
					}},
					StopReason: "tool_use",
				},
			},
			// Turn 1: Unknown path again
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{
							model.ToolUsePart{ID: "call_2", Name: "read_design", Input: rawjson.Message(`{"path":"design/bad2.go"}`)},
						},
					}},
					StopReason: "tool_use",
				},
			},
			// Turn 2: Unknown path again (exhausting budget)
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{
							model.ToolUsePart{ID: "call_3", Name: "read_design", Input: rawjson.Message(`{"path":"design/bad3.go"}`)},
						},
					}},
					StopReason: "tool_use",
				},
			},
		},
	}

	cardsDir := t.TempDir()
	cp := newControlPlane(t, Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
	})

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	_, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-budget",
		Prompt:   "Add feature",
	})
	require.Error(t, err)
	se, ok := asAnalysisRejected(err)
	require.True(t, ok)
	assert.Contains(t, se.Message, "exhausted turn budget before completing file inspection")
	assert.Equal(t, "inspection_budget_exhausted", cp1RejectionKind(err))
}

// TestCP1TwoPhaseReadDesign_EmptyWorkspaceAndSupplied verifies that empty
// workspaces (0 design files) and caller-supplied designs make exactly 1 model
// call skipping Phase 1.
func TestCP1TwoPhaseReadDesign_EmptyWorkspaceAndSupplied(t *testing.T) {
	t.Run("empty_workspace_skips_phase1", func(t *testing.T) {
		wsDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(wsDir, "go.mod"), []byte("module example.com/test\n\ngo 1.23\n"), 0o644))

		q := &queuedModelClient{
			responses: []queuedModelTurn{
				{
					resp: &model.Response{
						Content: []model.Message{{
							Role:  model.ConversationRoleAssistant,
							Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
						}},
						StopReason: "stop",
					},
				},
			},
		}

		cardsDir := t.TempDir()
		cp := newControlPlane(t, Options{
			CardsDir:     cardsDir,
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})

		aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-empty-ws",
			Prompt:   "Add feature",
		})
		require.NoError(t, err)
		assert.Equal(t, "Order refund processing.", card.Summary)
		assert.Len(t, q.requests, 1)
		assert.Contains(t, q.requests[0].Messages[1].Parts[0].(model.TextPart).Text, "(no Goa design source was supplied")
	})

	t.Run("supplied_design_skips_phase1", func(t *testing.T) {
		q := &queuedModelClient{
			responses: []queuedModelTurn{
				{
					resp: &model.Response{
						Content: []model.Message{{
							Role:  model.ConversationRoleAssistant,
							Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
						}},
						StopReason: "stop",
					},
				},
			},
		}

		cardsDir := t.TempDir()
		cp := newControlPlane(t, Options{
			CardsDir: cardsDir,
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})

		supplied := "package design\n// explicit design\n"
		aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID:     "chg-supplied",
			Prompt:       "Add feature",
			DesignSource: &supplied,
		})
		require.NoError(t, err)
		assert.Equal(t, "Order refund processing.", card.Summary)
		assert.Len(t, q.requests, 1)
		assert.Contains(t, q.requests[0].Messages[1].Parts[0].(model.TextPart).Text, "explicit design")
	})
}

// TestCP1TwoPhaseReadDesign_ReviseChangeWorkspaceReReadAndSavedDecode verifies
// Case R2 (re-reading modified workspace files on ReviseChange) and Case R3
// (decoding saved workspace JSON on an unbound control plane).
func TestCP1TwoPhaseReadDesign_ReviseChangeWorkspaceReReadAndSavedDecode(t *testing.T) {
	wsDir := setupWorkspaceWithFiles(t, map[string]string{
		"design/design.go": "package design\n// v1 design\n",
	})
	cardsDir := t.TempDir()

	q := &queuedModelClient{
		responses: []queuedModelTurn{
			// Propose Turn 0: read_design
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{
							model.ToolUsePart{ID: "call_1", Name: "read_design", Input: rawjson.Message(`{}`)},
						},
					}},
					StopReason: "tool_use",
				},
			},
			// Propose Turn 1: structured completion
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
					}},
					StopReason: "stop",
				},
			},
		},
	}

	cp := newControlPlane(t, Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
	})

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-revise-cases",
		Prompt:   "Add refunds",
	})
	require.NoError(t, err)

	t.Run("case_R2_live_workspace_reread", func(t *testing.T) {
		// Update design.go on disk to v2
		require.NoError(t, os.WriteFile(filepath.Join(wsDir, "design/design.go"), []byte("package design\n// v2 design\n"), 0o644))

		q.mu.Lock()
		q.responses = append(q.responses,
			// Revise Turn 0: read_design
			queuedModelTurn{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{
							model.ToolUsePart{ID: "call_r2", Name: "read_design", Input: rawjson.Message(`{}`)},
						},
					}},
					StopReason: "tool_use",
				},
			},
			// Revise Turn 1: structured revision completion
			queuedModelTurn{
				resp: &model.Response{
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: validRevisionAnalysisJSON}},
					}},
					StopReason: "stop",
				},
			},
		)
		q.mu.Unlock()

		revised, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
			ChangeID:        "chg-revise-cases",
			RequestRevision: card.RequestRevision,
			Prompt:          "Updated prompt",
		})
		require.NoError(t, err)
		assert.Equal(t, "Revised order refund processing.", revised.Summary)

		// Check tool result content had v2 in the combined final user message
		assertAlternatingRoles(t, q.requests[3].Messages)
		toolResultMsg := q.requests[3].Messages[len(q.requests[3].Messages)-1]
		toolResult := toolResultMsg.Parts[0].(model.ToolResultPart)
		filesList := toolResult.Content.([]any)
		assert.Contains(t, filesList[0].(map[string]any)["content"], "v2 design")
	})

	t.Run("case_R3_unbound_workspace_saved_decode", func(t *testing.T) {
		require.NoError(t, cp.Close())

		// Create unbound control plane with same CardsDir
		qUnbound := &queuedModelClient{
			responses: []queuedModelTurn{
				// Revise Turn 0: read_design
				{
					resp: &model.Response{
						Content: []model.Message{{
							Role: model.ConversationRoleAssistant,
							Parts: []model.Part{
								model.ToolUsePart{ID: "call_r3", Name: "read_design", Input: rawjson.Message(`{}`)},
							},
						}},
						StopReason: "tool_use",
					},
				},
				// Revise Turn 1: structured revision completion
				{
					resp: &model.Response{
						Content: []model.Message{{
							Role:  model.ConversationRoleAssistant,
							Parts: []model.Part{model.TextPart{Text: validRevisionAnalysisJSON}},
						}},
						StopReason: "stop",
					},
				},
			},
		}

		cpUnbound := newControlPlane(t, Options{
			CardsDir: cardsDir,
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, qUnbound)),
		})

		currentCard, err := cpUnbound.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-revise-cases"})
		require.NoError(t, err)

		revised, err := cpUnbound.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
			ChangeID:        "chg-revise-cases",
			RequestRevision: currentCard.RequestRevision,
			Prompt:          "Unbound revise prompt",
		})
		require.NoError(t, err)
		assert.Equal(t, "Revised order refund processing.", revised.Summary)
		assert.Len(t, qUnbound.requests, 2)
	})
}

// TestCP1SpanAttributes_SuccessAndRejection verifies that surla.public_summary
// and surla.cp1.rejection_kind attributes are recorded on propose_change and
// revise_change OpenTelemetry spans.
func TestCP1SpanAttributes_SuccessAndRejection(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		require.NoError(t, tp.Shutdown(context.Background()))
	})

	wsDir := setupWorkspaceWithFiles(t, map[string]string{
		"design/design.go": "package design\n",
	})
	cardsDir := t.TempDir()

	q := &queuedModelClient{
		responses: []queuedModelTurn{
			// Propose 1: success
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{
							model.ToolUsePart{ID: "call_1", Name: "read_design", Input: rawjson.Message(`{}`)},
						},
					}},
					StopReason: "tool_use",
				},
			},
			{
				resp: &model.Response{
					Content: []model.Message{{
						Role:  model.ConversationRoleAssistant,
						Parts: []model.Part{model.TextPart{Text: validAnalysisJSON}},
					}},
					StopReason: "stop",
				},
			},
		},
	}

	cp := newControlPlane(t, Options{
		CardsDir:     cardsDir,
		WorkspaceDir: wsDir,
		Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
	})
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	// 1. ProposeChange success
	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-span-success",
		Prompt:   "Add refunds",
	})
	require.NoError(t, err)

	spans := exp.GetSpans()
	require.NotEmpty(t, spans)
	proposeSpan := findSpanByName(spans, "surla.controlplane.propose_change")
	require.NotNil(t, proposeSpan)
	val, ok := findSpanAttr(proposeSpan.Attributes, "surla.public_summary")
	assert.True(t, ok)
	assert.Equal(t, card.Summary, val.AsString())
	_, hasRejectionKind := findSpanAttr(proposeSpan.Attributes, "surla.cp1.rejection_kind")
	assert.False(t, hasRejectionKind, "successful span must not have rejection_kind")

	// 2. ReviseChange success
	q.mu.Lock()
	q.responses = append(q.responses,
		queuedModelTurn{
			resp: &model.Response{
				Content: []model.Message{{
					Role: model.ConversationRoleAssistant,
					Parts: []model.Part{
						model.ToolUsePart{ID: "call_rev", Name: "read_design", Input: rawjson.Message(`{}`)},
					},
				}},
				StopReason: "tool_use",
			},
		},
		queuedModelTurn{
			resp: &model.Response{
				Content: []model.Message{{
					Role:  model.ConversationRoleAssistant,
					Parts: []model.Part{model.TextPart{Text: validRevisionAnalysisJSON}},
				}},
				StopReason: "stop",
			},
		},
	)
	q.mu.Unlock()

	revised, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-span-success",
		RequestRevision: card.RequestRevision,
		Prompt:          "Updated prompt",
	})
	require.NoError(t, err)

	spans = exp.GetSpans()
	reviseSpan := findSpanByName(spans, "surla.controlplane.revise_change")
	require.NotNil(t, reviseSpan)
	val, ok = findSpanAttr(reviseSpan.Attributes, "surla.public_summary")
	assert.True(t, ok)
	assert.Equal(t, revised.Summary, val.AsString())

	// 3. ProposeChange analysis_rejected (OutputLimited)
	q.mu.Lock()
	q.responses = append(q.responses, queuedModelTurn{
		resp: &model.Response{
			OutputLimited: true,
			StopReason:    "max_tokens",
			Content: []model.Message{{
				Role: model.ConversationRoleAssistant,
				Parts: []model.Part{
					model.ToolUsePart{
						ID:    "call_span_lim",
						Name:  "read_design",
						Input: rawjson.Message(`{}`),
					},
				},
			}},
		},
	})
	q.mu.Unlock()

	_, err = cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-span-rejected",
		Prompt:   "Fail with output limit",
	})
	require.Error(t, err)
	se, ok := asAnalysisRejected(err)
	require.True(t, ok)

	spans = exp.GetSpans()
	var lastProposeSpan *tracetest.SpanStub
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name == "surla.controlplane.propose_change" {
			lastProposeSpan = &spans[i]
			break
		}
	}
	require.NotNil(t, lastProposeSpan)
	rejKindVal, ok := findSpanAttr(lastProposeSpan.Attributes, "surla.cp1.rejection_kind")
	assert.True(t, ok)
	assert.Equal(t, "output_limited", rejKindVal.AsString())
	summaryVal, ok := findSpanAttr(lastProposeSpan.Attributes, "surla.public_summary")
	assert.True(t, ok)
	assert.Equal(t, se.Message, summaryVal.AsString())

	// 4. ProposeChange semantic_validation rejection (duplicate ambiguity options)
	invalidSemanticJSON := `{"summary":"Valid summary","ambiguities":[{"question":"Which option?","options":["option-a","option-a"]}],"examples":[{"scenario":"S","input_summary":"I","expected_outcome":"O","verification_kind":"deterministic"}],"preserved_behaviors":[]}`
	q.mu.Lock()
	q.responses = append(q.responses,
		queuedModelTurn{
			resp: &model.Response{
				Content: []model.Message{{
					Role: model.ConversationRoleAssistant,
					Parts: []model.Part{
						model.ToolUsePart{ID: "call_sem", Name: "read_design", Input: rawjson.Message(`{}`)},
					},
				}},
				StopReason: "tool_use",
			},
		},
		queuedModelTurn{
			resp: &model.Response{
				Content: []model.Message{{
					Role:  model.ConversationRoleAssistant,
					Parts: []model.Part{model.TextPart{Text: invalidSemanticJSON}},
				}},
				StopReason: "stop",
			},
		},
	)
	q.mu.Unlock()

	_, err = cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-span-semantic-fail",
		Prompt:   "Fail with semantic validation",
	})
	require.Error(t, err)
	se, ok = asAnalysisRejected(err)
	require.True(t, ok)

	spans = exp.GetSpans()
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name == "surla.controlplane.propose_change" {
			lastProposeSpan = &spans[i]
			break
		}
	}
	require.NotNil(t, lastProposeSpan)
	rejKindVal, ok = findSpanAttr(lastProposeSpan.Attributes, "surla.cp1.rejection_kind")
	assert.True(t, ok)
	assert.Equal(t, "semantic_validation", rejKindVal.AsString())
	summaryVal, ok = findSpanAttr(lastProposeSpan.Attributes, "surla.public_summary")
	assert.True(t, ok)
	assert.Equal(t, se.Message, summaryVal.AsString())
}

// setupWorkspaceWithFiles creates a valid Go module workspace containing the
// specified relative design file paths and contents.
func setupWorkspaceWithFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	wsDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "go.mod"), []byte("module example.com/test\n\ngo 1.23\n"), 0o644))
	for relPath, content := range files {
		fullPath := filepath.Join(wsDir, relPath)
		require.NoError(t, os.MkdirAll(filepath.Dir(fullPath), 0o755))
		require.NoError(t, os.WriteFile(fullPath, []byte(content), 0o644))
	}
	return wsDir
}

// findSpanByName locates the first span with matching name in spans.
func findSpanByName(spans []tracetest.SpanStub, name string) *tracetest.SpanStub {
	for i := range spans {
		if spans[i].Name == name {
			return &spans[i]
		}
	}
	return nil
}

// findSpanAttr finds the attribute value for key in attrs.
func findSpanAttr(attrs []attribute.KeyValue, key attribute.Key) (attribute.Value, bool) {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value, true
		}
	}
	return attribute.Value{}, false
}

// assertAlternatingRoles verifies that after the leading system message, no two
// adjacent messages share the same conversation role.
func assertAlternatingRoles(t *testing.T, msgs []*model.Message) {
	t.Helper()
	var prev model.ConversationRole
	for i, m := range msgs {
		require.NotNil(t, m, "message %d", i)
		if m.Role == model.ConversationRoleSystem {
			assert.Zero(t, i, "system message must lead the transcript")
			continue
		}
		if prev != "" {
			assert.NotEqual(t, prev, m.Role, "adjacent messages at %d must alternate user and assistant roles", i)
		}
		prev = m.Role
	}
}
