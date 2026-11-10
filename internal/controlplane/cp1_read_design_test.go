// This file tests the Checkpoint 1 two-phase `read_design` inspection loop,
// strict payload and saved-workspace decoders, and offline Anthropic and OpenAI
// request encoding across direct and gRPC gateway model clients.
package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	anthropicstream "github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	gensurla "github.com/goadesign/surla/gen/surla"
	openaioption "github.com/openai/openai-go/v3/option"
	openaistream "github.com/openai/openai-go/v3/packages/ssestream"
	"github.com/openai/openai-go/v3/responses"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/features/model/anthropic"
	"goa.design/goa-ai/features/model/openai"
	"goa.design/goa-ai/runtime/agent/model"
	"goa.design/goa-ai/runtime/agent/rawjson"
)

type (
	// modelClientFactory builds a model.Client backed either directly by
	// queuedModelClient or through an in-process gRPC model gateway.
	modelClientFactory func(t *testing.T, q *queuedModelClient) model.Client

	// scriptedAnthropicMessages implements anthropic.MessagesClient offline by
	// recording encoded requests and returning scripted provider messages.
	scriptedAnthropicMessages struct {
		// responses holds the queued Anthropic messages returned per New call.
		responses []*anthropicsdk.Message
		// requests records each encoded MessageNewParams sent by the adapter.
		requests []anthropicsdk.MessageNewParams
	}

	// scriptedOpenAIResponses implements openai.ResponsesClient offline by
	// recording encoded requests and returning scripted Responses API payloads.
	scriptedOpenAIResponses struct {
		// responses holds the queued OpenAI responses returned per New call.
		responses []*responses.Response
		// requests records each encoded ResponseNewParams sent by the adapter.
		requests []responses.ResponseNewParams
	}
)

const (
	// validAnalysisJSON is a valid CP1ChangeCardAnalysis response JSON string.
	validAnalysisJSON = `{"summary":"Order refund processing.","ambiguities":[],"examples":[{"scenario":"Standard refund","input_summary":"Refund $10","expected_outcome":"Success","verification_kind":"deterministic"}],"preserved_behaviors":["Existing orders unchanged"]}`

	// validOpenAIStrictAnalysisJSON is a valid CP1ChangeCardAnalysis wire JSON
	// string for OpenAI strict structured output, where optional ui_composition
	// is projected as nullable and required on the wire.
	validOpenAIStrictAnalysisJSON = `{"summary":"Order refund processing.","ambiguities":[],"examples":[{"scenario":"Standard refund","input_summary":"Refund $10","expected_outcome":"Success","verification_kind":"deterministic"}],"preserved_behaviors":["Existing orders unchanged"],"ui_composition":null}`

	// validRevisionAnalysisJSON is a valid CP1ReviseChangeCardAnalysis response JSON string.
	validRevisionAnalysisJSON = `{"summary":"Revised order refund processing.","ambiguities":[],"examples":[{"prior_id":"check_01","scenario":"Standard refund","input_summary":"Refund $10","expected_outcome":"Success","verification_kind":"deterministic"}],"removed_example_ids":[],"preserved_behaviors":["Existing orders unchanged"]}`
)

// clientFactories exercises both direct and gRPC gateway model client boundaries.
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

// TestCP1TwoPhaseReadDesign_SelectiveOmitAndLargeWorkspaces verifies selective
// single-file inspection, omit-path read-all, and multi-file workspaces larger
// than maxReadDesignTurnsCap (10 files across 8 valid inspection turns).
func TestCP1TwoPhaseReadDesign_SelectiveOmitAndLargeWorkspaces(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	for _, cf := range clientFactories {
		t.Run(cf.name, func(t *testing.T) {
			t.Run("selective_inspection_and_remaining_turns", func(t *testing.T) {
				wsDir := setupWorkspaceWithFiles(t, map[string]string{
					"design/design.go": "package design\n// main design\n",
					"design/orders.go": "package design\n// orders design\n",
					"design/unused.go": "package design\n// unread design body\n",
				})
				// 3 files -> maxInspectionTurns = min(3+2, 8) = 5.
				q := &queuedModelClient{
					responses: []queuedModelTurn{
						toolUseTurn("call_1", "read_design", `{"path":"design/orders.go"}`),
						toolUseTurn("call_2", "read_design", `{"path":"design/design.go"}`),
						jsonAnswerTurn("Inspection complete."),
						jsonAnswerTurn(validAnalysisJSON),
					},
				}
				cp := newControlPlane(t, Options{
					CardsDir:     t.TempDir(),
					WorkspaceDir: wsDir,
					Analyzer:     NewModelCP1Analyzer(cf.buildFn(t, q)),
				})

				card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
					ChangeID: "chg-selective",
					Prompt:   "Add order refunds",
				})
				require.NoError(t, err)
				assert.Equal(t, "Order refund processing.", card.Summary)
				require.Len(t, q.requests, 4)

				for i, req := range q.requests {
					assertAlternatingRoles(t, req.Messages)
					if i < 3 {
						assert.Equal(t, cp1InspectionMaxOutputTokens, req.MaxTokens)
						require.Len(t, req.Tools, 1)
						assert.Equal(t, "read_design", req.Tools[0].Name)
					}
				}
				// Turn 1 receives Turn 0's tool result with remaining_turns = 4.
				turn1Last := q.requests[1].Messages[len(q.requests[1].Messages)-1]
				resMap := turn1Last.Parts[0].(model.ToolResultPart).Content.(map[string]any)
				assert.Equal(t, "design/orders.go", resMap["path"])
				assert.Contains(t, resMap["content"], "orders design")
				assert.Equal(t, "4", fmt.Sprint(resMap["remaining_turns"]))

				// Phase 2 is a clean 2-message [System, User] structured request with
				// Tools empty, sorted read files, and unread file bodies omitted.
				phase2 := q.requests[3]
				assert.Equal(t, cp1AnalysisMaxOutputTokens, phase2.MaxTokens)
				assert.Empty(t, phase2.Tools)
				require.NotNil(t, phase2.StructuredOutput)
				require.Len(t, phase2.Messages, 2)
				assert.Equal(t, model.ConversationRoleSystem, phase2.Messages[0].Role)
				assert.Equal(t, model.ConversationRoleUser, phase2.Messages[1].Role)
				require.Len(t, phase2.Messages[1].Parts, 1)
				phase2Text := phase2.Messages[1].Parts[0].(model.TextPart).Text
				assert.Contains(t, phase2Text, "Workspace design manifest:")
				assert.Contains(t, phase2Text, "- design/unused.go (")
				assert.NotContains(t, phase2Text, "unread design body")
				assert.Contains(t, phase2Text, "--- design/design.go ---\npackage design\n// main design")
				assert.Contains(t, phase2Text, "--- design/orders.go ---\npackage design\n// orders design")
				assert.Less(
					t,
					strings.Index(phase2Text, "--- design/design.go ---"),
					strings.Index(phase2Text, "--- design/orders.go ---"),
					"Phase 2 read files must appear in deterministic sorted path order",
				)
			})

			t.Run("omit_path_read_all_transitions_immediately", func(t *testing.T) {
				wsDir := setupWorkspaceWithFiles(t, map[string]string{
					"design/design.go": "package design\n// main design\n",
					"design/orders.go": "package design\n// orders design\n",
				})
				q := &queuedModelClient{
					responses: []queuedModelTurn{
						toolUseTurn("call_all", "read_design", `{ }`),
						jsonAnswerTurn(validAnalysisJSON),
					},
				}
				cp := newControlPlane(t, Options{
					CardsDir:     t.TempDir(),
					WorkspaceDir: wsDir,
					Analyzer:     NewModelCP1Analyzer(cf.buildFn(t, q)),
				})

				card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
					ChangeID: "chg-omit-path",
					Prompt:   "Add order refunds",
				})
				require.NoError(t, err)
				assert.Equal(t, "Order refund processing.", card.Summary)
				require.Len(t, q.requests, 2)
				phase2 := q.requests[1]
				assert.Empty(t, phase2.Tools)
				require.Len(t, phase2.Messages, 2)
				phase2Text := phase2.Messages[1].Parts[0].(model.TextPart).Text
				assert.Contains(t, phase2Text, "--- design/design.go ---")
				assert.Contains(t, phase2Text, "--- design/orders.go ---")
			})

			t.Run("ten_file_workspace_succeeds_after_eight_selective_turns", func(t *testing.T) {
				files := make(map[string]string, 10)
				turns := make([]queuedModelTurn, 0, 9)
				for i := range 10 {
					rel := fmt.Sprintf("design/svc_%02d.go", i)
					files[rel] = fmt.Sprintf("package design\n// svc %02d body\n", i)
					if i < maxReadDesignTurnsCap {
						turns = append(turns, toolUseTurn(
							fmt.Sprintf("call_%d", i),
							"read_design",
							fmt.Sprintf(`{"path":%q}`, rel),
						))
					}
				}
				turns = append(turns, jsonAnswerTurn(validAnalysisJSON))
				wsDir := setupWorkspaceWithFiles(t, files)
				q := &queuedModelClient{responses: turns}
				cp := newControlPlane(t, Options{
					CardsDir:     t.TempDir(),
					WorkspaceDir: wsDir,
					Analyzer:     NewModelCP1Analyzer(cf.buildFn(t, q)),
				})

				card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
					ChangeID: "chg-ten-files",
					Prompt:   "Update services",
				})
				require.NoError(t, err)
				assert.Equal(t, "Order refund processing.", card.Summary)
				require.Len(t, q.requests, maxReadDesignTurnsCap+1)

				// Final Phase 1 request (turn index 7) receives turn 6's result with remaining_turns == 1.
				lastPhase1 := q.requests[maxReadDesignTurnsCap-1]
				lastPhase1Msg := lastPhase1.Messages[len(lastPhase1.Messages)-1]
				resMap := lastPhase1Msg.Parts[0].(model.ToolResultPart).Content.(map[string]any)
				assert.Equal(t, "design/svc_06.go", resMap["path"])
				assert.Equal(t, "1", fmt.Sprint(resMap["remaining_turns"]))

				// Phase 2 carries all 8 read files (including turn 7's svc_07.go) and omits unread svc_08/svc_09 bodies.
				phase2 := q.requests[maxReadDesignTurnsCap]
				assert.Empty(t, phase2.Tools)
				require.Len(t, phase2.Messages, 2)
				phase2Text := phase2.Messages[1].Parts[0].(model.TextPart).Text
				assert.Contains(t, phase2Text, "--- design/svc_07.go ---\npackage design\n// svc 07 body")
				assert.Contains(t, phase2Text, "- design/svc_08.go (")
				assert.NotContains(t, phase2Text, "svc 08 body")
				assert.NotContains(t, phase2Text, "svc 09 body")
			})
		})
	}
}

// TestCP1TwoPhaseReadDesign_StopAndPathCases verifies zero-tool completion,
// Unicode and space-containing paths, unknown-path recovery, metadata and
// ThoughtSignature preservation without slice aliasing, output-limit cutoffs,
// RecoveryCorrection, and unresolved-error budget exhaustion.
func TestCP1TwoPhaseReadDesign_StopAndPathCases(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	t.Run("zero_tool_call_completion", func(t *testing.T) {
		wsDir := setupWorkspaceWithFiles(t, map[string]string{"design/design.go": "package design\n// body\n"})
		q := &queuedModelClient{
			responses: []queuedModelTurn{
				jsonAnswerTurn("I have enough context from the manifest."),
				jsonAnswerTurn(validAnalysisJSON),
			},
		}
		cp := newControlPlane(t, Options{
			CardsDir:     t.TempDir(),
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-zero-tool",
			Prompt:   "Add feature",
		})
		require.NoError(t, err)
		assert.Equal(t, "Order refund processing.", card.Summary)
		require.Len(t, q.requests, 2)
		phase2 := q.requests[1]
		assert.Empty(t, phase2.Tools)
		require.Len(t, phase2.Messages, 2)
		phase2Text := phase2.Messages[1].Parts[0].(model.TextPart).Text
		assert.Contains(t, phase2Text, "Workspace design manifest:\n- design/design.go (")
		assert.Contains(t, phase2Text, "(no design files were read during inspection)")
		assert.NotContains(t, phase2Text, "// body")
	})

	t.Run("non_ascii_and_space_paths_plus_unknown_recovery", func(t *testing.T) {
		wsDir := setupWorkspaceWithFiles(t, map[string]string{
			"design/注文.go":          "package design\n// Japanese order design\n",
			"design/order rules.go": "package design\n// order rules\n",
		})
		q := &queuedModelClient{
			responses: []queuedModelTurn{
				toolUseTurn("call_unknown", "read_design", `{"path":"design/missing.go"}`),
				toolUseTurn("call_unicode", "read_design", `{"path":"design/注文.go"}`),
				toolUseTurn("call_space", "read_design", `{"path":"design/order rules.go"}`),
				jsonAnswerTurn(validAnalysisJSON),
			},
		}
		cp := newControlPlane(t, Options{
			CardsDir:     t.TempDir(),
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-unicode",
			Prompt:   "Support Japanese orders",
		})
		require.NoError(t, err)
		assert.Equal(t, "Order refund processing.", card.Summary)
		require.Len(t, q.requests, 4)

		errPart := q.requests[1].Messages[len(q.requests[1].Messages)-1].Parts[0].(model.ToolResultPart)
		assert.True(t, errPart.IsError)
		assert.Contains(t, errPart.Content.(string), `unknown design file "design/missing.go"`)
		assert.Contains(t, errPart.Content.(string), "design/注文.go")
	})

	t.Run("preserves_metadata_thinking_and_isolates_request_slices", func(t *testing.T) {
		wsDir := setupWorkspaceWithFiles(t, map[string]string{
			"design/a.go": "package design\n",
			"design/b.go": "package design\n",
			"design/c.go": "package design\n",
		})
		q := &queuedModelClient{
			responses: []queuedModelTurn{
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
									ID:               "call_sig",
									Name:             "read_design",
									Input:            rawjson.Message(`{"path":"design/a.go"}`),
									ThoughtSignature: "thought-sig-opaque",
								},
							},
						}},
						StopReason: "tool_use",
					},
				},
				toolUseTurn("call_b", "read_design", `{"path":"design/b.go"}`),
				jsonAnswerTurn("Done inspecting."),
				jsonAnswerTurn(validAnalysisJSON),
			},
		}
		cp := newControlPlane(t, Options{
			CardsDir:     t.TempDir(),
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})
		_, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-meta",
			Prompt:   "Add feature",
		})
		require.NoError(t, err)
		require.Len(t, q.requests, 4)

		// Both Phase 1 follow-up turns (requests[1] and requests[2]) replay Turn 0's
		// assistant message without sharing mutable maps or slices.
		var asst1, asst2 *model.Message
		for _, m := range q.requests[1].Messages {
			if m.Role == model.ConversationRoleAssistant {
				asst1 = m
				break
			}
		}
		for _, m := range q.requests[2].Messages {
			if m.Role == model.ConversationRoleAssistant {
				asst2 = m
				break
			}
		}
		require.NotNil(t, asst1)
		require.NotNil(t, asst2)
		asst1.Meta["provider_request_id"] = "mutated"
		assert.Equal(t, "req-12345", asst2.Meta["provider_request_id"])
		assert.Equal(t, "sig-thinking-token", asst2.Parts[0].(model.ThinkingPart).Signature)
		assert.Equal(t, "thought-sig-opaque", asst2.Parts[1].(model.ToolUsePart).ThoughtSignature)
	})

	t.Run("output_limited_and_recovery_and_unresolved_error_budget", func(t *testing.T) {
		wsDir := setupWorkspaceWithFiles(t, map[string]string{"design/design.go": "package design\n"})

		// 1. OutputLimited response flag checked before ToolCalls().
		qLim := &queuedModelClient{
			responses: []queuedModelTurn{{
				resp: &model.Response{
					OutputLimited: true,
					StopReason:    "max_tokens",
					Content: []model.Message{{
						Role: model.ConversationRoleAssistant,
						Parts: []model.Part{model.ToolUsePart{
							ID: "call_lim", Name: "read_design", Input: rawjson.Message(`{}`),
						}},
					}},
				},
			}},
		}
		cpLim := newControlPlane(t, Options{
			CardsDir: t.TempDir(), WorkspaceDir: wsDir,
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, qLim)),
		})
		_, err := cpLim.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg-lim", Prompt: "P"})
		require.Error(t, err)
		kind, ok := cp1RejectionKind(err)
		assert.True(t, ok)
		assert.Equal(t, "output_limited", kind)

		// 2. RecoveryCorrection retries within budget and fails on final turn.
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

		qRec := &queuedModelClient{
			responses: []queuedModelTurn{
				{err: correctable},
				toolUseTurn("call_ok", "read_design", `{}`),
				jsonAnswerTurn(validAnalysisJSON),
			},
		}
		cpRec := newControlPlane(t, Options{
			CardsDir: t.TempDir(), WorkspaceDir: wsDir,
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, qRec)),
		})
		_, err = cpRec.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg-rec", Prompt: "P"})
		require.NoError(t, err)
		assertAlternatingRoles(t, qRec.requests[1].Messages)
		recMsg := qRec.requests[1].Messages[len(qRec.requests[1].Messages)-1]
		assert.Equal(t, "pass a valid JSON object with path property", recMsg.Parts[1].(model.TextPart).Text)

		// 3. Unresolved tool error on final turn (3 turns for 1 file) -> inspection_budget_exhausted.
		qExh := &queuedModelClient{
			responses: []queuedModelTurn{
				toolUseTurn("c1", "read_design", `{"path":"design/bad1.go"}`),
				toolUseTurn("c2", "read_design", `{"path":"design/bad2.go"}`),
				toolUseTurn("c3", "read_design", `{"path":"design/bad3.go"}`),
			},
		}
		cpExh := newControlPlane(t, Options{
			CardsDir: t.TempDir(), WorkspaceDir: wsDir,
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, qExh)),
		})
		_, err = cpExh.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg-exh", Prompt: "P"})
		require.Error(t, err)
		kind, ok = cp1RejectionKind(err)
		assert.True(t, ok)
		assert.Equal(t, "inspection_budget_exhausted", kind)
	})
}

// TestCP1TwoPhaseReadDesign_SingleTurnAndWorkspaceRevise verifies empty
// workspaces, caller-supplied designs, Case R2 live workspace re-reads, and
// Case R3 unbound saved workspace JSON decoding.
func TestCP1TwoPhaseReadDesign_SingleTurnAndWorkspaceRevise(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	t.Run("empty_workspace_and_supplied_skip_phase1", func(t *testing.T) {
		emptyWS := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(emptyWS, "go.mod"), []byte("module example.com/test\n\ngo 1.23\n"), 0o644))
		qEmpty := &queuedModelClient{responses: []queuedModelTurn{jsonAnswerTurn(validAnalysisJSON)}}
		cpEmpty := newControlPlane(t, Options{
			CardsDir: t.TempDir(), WorkspaceDir: emptyWS,
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, qEmpty)),
		})
		_, err := cpEmpty.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg-empty", Prompt: "P"})
		require.NoError(t, err)
		require.Len(t, qEmpty.requests, 1)
		assert.Contains(t, qEmpty.requests[0].Messages[1].Parts[0].(model.TextPart).Text, "(no Goa design source was supplied")

		supplied := "package design\n// explicit design\n"
		qSup := &queuedModelClient{responses: []queuedModelTurn{jsonAnswerTurn(validAnalysisJSON)}}
		cpSup := newControlPlane(t, Options{
			CardsDir: t.TempDir(),
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, qSup)),
		})
		_, err = cpSup.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-sup", Prompt: "P", DesignSource: &supplied,
		})
		require.NoError(t, err)
		require.Len(t, qSup.requests, 1)
		assert.Contains(t, qSup.requests[0].Messages[1].Parts[0].(model.TextPart).Text, "explicit design")
	})

	t.Run("revise_case_R2_reread_and_case_R3_unbound_decode", func(t *testing.T) {
		wsDir := setupWorkspaceWithFiles(t, map[string]string{"design/design.go": "package design\n// v1 design\n"})
		cardsDir := t.TempDir()
		q := &queuedModelClient{
			responses: []queuedModelTurn{
				toolUseTurn("call_1", "read_design", `{}`),
				jsonAnswerTurn(validAnalysisJSON),
				toolUseTurn("call_r2", "read_design", `{}`),
				jsonAnswerTurn(validRevisionAnalysisJSON),
			},
		}
		cp := newControlPlane(t, Options{
			CardsDir: cardsDir, WorkspaceDir: wsDir,
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg-rev", Prompt: "Add refunds"})
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(filepath.Join(wsDir, "design/design.go"), []byte("package design\n// v2 design\n"), 0o644))
		revised, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
			ChangeID: "chg-rev", RequestRevision: card.RequestRevision, Prompt: "Updated prompt",
		})
		require.NoError(t, err)
		assert.Equal(t, "Revised order refund processing.", revised.Summary)
		r2Phase2Text := q.requests[3].Messages[1].Parts[0].(model.TextPart).Text
		assert.Contains(t, r2Phase2Text, "--- design/design.go ---\npackage design\n// v2 design")
		assert.NotContains(t, r2Phase2Text, "v1 design")

		require.NoError(t, cp.Close())
		qUnbound := &queuedModelClient{
			responses: []queuedModelTurn{
				toolUseTurn("call_r3", "read_design", `{}`),
				jsonAnswerTurn(validRevisionAnalysisJSON),
			},
		}
		cpUnbound := newControlPlane(t, Options{
			CardsDir: cardsDir,
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, qUnbound)),
		})
		got, err := cpUnbound.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-rev"})
		require.NoError(t, err)
		revisedUnbound, err := cpUnbound.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
			ChangeID: "chg-rev", RequestRevision: got.RequestRevision, Prompt: "Unbound revise",
		})
		require.NoError(t, err)
		assert.Equal(t, "Revised order refund processing.", revisedUnbound.Summary)
		require.Len(t, qUnbound.requests, 2)
		r3Phase2Text := qUnbound.requests[1].Messages[1].Parts[0].(model.TextPart).Text
		assert.Contains(t, r3Phase2Text, "--- design/design.go ---\npackage design\n// v2 design")
	})
}

// TestCP1TwoPhaseReadDesign_RealProviderAdapters exercises Phase 1 read_design
// tool encoding and Phase 2 toolless structured completion through the real
// Goa-AI Anthropic Messages and OpenAI Responses adapters with offline SDK
// transports (zero network calls).
func TestCP1TwoPhaseReadDesign_RealProviderAdapters(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	wsDir := setupWorkspaceWithFiles(t, map[string]string{
		"design/api.go":    "package design\n// edge API contract\n",
		"design/orders.go": "package design\n// unread orders internal contract\n",
	})

	t.Run("anthropic_messages_adapter", func(t *testing.T) {
		msgs := &scriptedAnthropicMessages{
			responses: []*anthropicsdk.Message{
				mustAnthropicMessage(t, []map[string]any{{
					"type":  "tool_use",
					"id":    "toolu_01",
					"name":  "read_design",
					"input": map[string]any{"path": "design/api.go"},
				}}, "tool_use"),
				mustAnthropicMessage(t, []map[string]any{{
					"type": "text",
					"text": "Inspected the relevant edge API contract.",
				}}, "end_turn"),
				mustAnthropicMessage(t, []map[string]any{{
					"type": "text",
					"text": validAnalysisJSON,
				}}, "end_turn"),
			},
		}
		client, err := anthropic.New(msgs, anthropic.Options{
			DefaultModel: string(anthropicsdk.ModelClaudeSonnet4_5_20250929),
		})
		require.NoError(t, err)

		cp := newControlPlane(t, Options{
			CardsDir:     t.TempDir(),
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(client),
		})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-anthropic",
			Prompt:   "Add order refunds",
		})
		require.NoError(t, err)
		assert.Equal(t, "Order refund processing.", card.Summary)
		require.Len(t, msgs.requests, 3)

		// Phase 1 turns advertise read_design; Turn 1 replays tool_use + tool_result.
		require.Len(t, msgs.requests[0].Tools, 1)
		require.Len(t, msgs.requests[1].Tools, 1)
		require.Len(t, msgs.requests[1].Messages, 3)

		// Phase 2 sends a clean single-user-message structured completion with zero tools.
		phase2 := msgs.requests[2]
		assert.Empty(t, phase2.Tools)
		require.Len(t, phase2.Messages, 1)
		phase2Wire, err := json.Marshal(phase2)
		require.NoError(t, err)
		assert.Contains(t, string(phase2Wire), "edge API contract")
		assert.NotContains(t, string(phase2Wire), "unread orders internal contract")
		assert.NotContains(t, string(phase2Wire), "toolu_01")
	})

	t.Run("openai_responses_adapter", func(t *testing.T) {
		// OpenAI strict schema projects optional read_design.path to nullable, so
		// omit-path arrives from OpenAI as `{"path":null}` and is normalized to `{}`.
		oai := &scriptedOpenAIResponses{
			responses: []*responses.Response{
				mustOpenAIResponse(t, `[{
					"id":"fc_01",
					"type":"function_call",
					"call_id":"call_01",
					"name":"read_design",
					"arguments":"{\"path\":null}",
					"status":"completed"
				}]`),
				mustOpenAIResponse(t, fmt.Sprintf(`[{
					"id":"msg_02",
					"type":"message",
					"role":"assistant",
					"status":"completed",
					"content":[{"type":"output_text","text":%q,"annotations":[],"logprobs":[]}]
				}]`, validOpenAIStrictAnalysisJSON)),
			},
		}
		client, err := openai.New(openai.Options{
			Client:       oai,
			DefaultModel: "gpt-5.4",
		})
		require.NoError(t, err)

		cp := newControlPlane(t, Options{
			CardsDir:     t.TempDir(),
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(client),
		})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-openai",
			Prompt:   "Add order refunds",
		})
		require.NoError(t, err)
		assert.Equal(t, "Order refund processing.", card.Summary)
		require.Len(t, oai.requests, 2)

		// Phase 1 advertises strict read_design function tool.
		require.Len(t, oai.requests[0].Tools, 1)

		// Phase 2 has zero tools, zero function_call history items, and both read files.
		phase2 := oai.requests[1]
		assert.Empty(t, phase2.Tools)
		phase2Wire, err := json.Marshal(phase2)
		require.NoError(t, err)
		assert.Contains(t, string(phase2Wire), "edge API contract")
		assert.Contains(t, string(phase2Wire), "unread orders internal contract")
		assert.NotContains(t, string(phase2Wire), "call_01")
	})
}

// TestCP1DecodersRejectInvalidInputs verifies boundary rejection on both
// decodeReadDesignPayload and decodeSavedWorkspaceCP1Design.
func TestCP1DecodersRejectInvalidInputs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{name: "empty_object", raw: `{}`, want: ""},
		{name: "spaced_empty_object", raw: `{ }`, want: ""},
		{name: "valid_unicode_path", raw: `{"path":"design/注文.go"}`, want: "design/注文.go"},
		{name: "empty_raw", raw: ``, wantErr: true},
		{name: "whitespace_raw", raw: `   `, wantErr: true},
		{name: "literal_null", raw: `null`, wantErr: true},
		{name: "null_path", raw: `{"path":null}`, wantErr: true},
		{name: "empty_path", raw: `{"path":""}`, wantErr: true},
		{name: "unknown_field", raw: `{"other":"x"}`, wantErr: true},
		{name: "trailing_tokens", raw: `{}{}`, wantErr: true},
	} {
		t.Run("read_design_payload/"+tc.name, func(t *testing.T) {
			got, err := decodeReadDesignPayload(rawjson.Message(tc.raw))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	for _, tc := range []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{name: "empty_string", raw: ""},
		{name: "valid_object", raw: `{"design/design.go":"package design\n"}`},
		{name: "literal_null", raw: "null", wantErr: true},
		{name: "wrong_value_type", raw: `{"design/design.go":123}`, wantErr: true},
		{name: "trailing_tokens", raw: `{"design/design.go":"x"}{}`, wantErr: true},
	} {
		t.Run("saved_workspace_design/"+tc.name, func(t *testing.T) {
			_, err := decodeSavedWorkspaceCP1Design(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// New records body and returns the next scripted Anthropic response.
func (m *scriptedAnthropicMessages) New(
	_ context.Context,
	body anthropicsdk.MessageNewParams,
	_ ...anthropicoption.RequestOption,
) (*anthropicsdk.Message, error) {
	idx := len(m.requests)
	m.requests = append(m.requests, body)
	if idx >= len(m.responses) {
		return nil, fmt.Errorf("unexpected anthropic call %d", idx)
	}
	return m.responses[idx], nil
}

// NewStreaming is unused because the CP1 analyzer only calls Complete.
func (m *scriptedAnthropicMessages) NewStreaming(
	context.Context,
	anthropicsdk.MessageNewParams,
	...anthropicoption.RequestOption,
) *anthropicstream.Stream[anthropicsdk.MessageStreamEventUnion] {
	panic("streaming not used by CP1 analyzer")
}

// CountTokens is unused because the CP1 analyzer does not count tokens.
func (m *scriptedAnthropicMessages) CountTokens(
	context.Context,
	anthropicsdk.MessageCountTokensParams,
	...anthropicoption.RequestOption,
) (*anthropicsdk.MessageTokensCount, error) {
	return nil, errors.New("token counting not used by CP1 analyzer")
}

// New records body and returns the next scripted OpenAI Responses API payload.
func (m *scriptedOpenAIResponses) New(
	_ context.Context,
	body responses.ResponseNewParams,
	_ ...openaioption.RequestOption,
) (*responses.Response, error) {
	idx := len(m.requests)
	m.requests = append(m.requests, body)
	if idx >= len(m.responses) {
		return nil, fmt.Errorf("unexpected openai call %d", idx)
	}
	return m.responses[idx], nil
}

// NewStreaming is unused because the CP1 analyzer only calls Complete.
func (m *scriptedOpenAIResponses) NewStreaming(
	context.Context,
	responses.ResponseNewParams,
	...openaioption.RequestOption,
) *openaistream.Stream[responses.ResponseStreamEventUnion] {
	panic("streaming not used by CP1 analyzer")
}

// toolUseTurn constructs a scripted Phase 1 tool-call response turn.
func toolUseTurn(id, name, inputJSON string) queuedModelTurn {
	return queuedModelTurn{
		resp: &model.Response{
			Content: []model.Message{{
				Role: model.ConversationRoleAssistant,
				Parts: []model.Part{model.ToolUsePart{
					ID:    id,
					Name:  name,
					Input: rawjson.Message(inputJSON),
				}},
			}},
			StopReason: "tool_use",
		},
	}
}

// jsonAnswerTurn constructs a scripted assistant text response turn.
func jsonAnswerTurn(text string) queuedModelTurn {
	return queuedModelTurn{
		resp: &model.Response{
			Content: []model.Message{{
				Role:  model.ConversationRoleAssistant,
				Parts: []model.Part{model.TextPart{Text: text}},
			}},
			StopReason: "stop",
		},
	}
}

// setupWorkspaceWithFiles creates a valid Go module workspace containing files.
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

// mustAnthropicMessage constructs a provider-shaped Anthropic Message value.
func mustAnthropicMessage(t *testing.T, content []map[string]any, stopReason string) *anthropicsdk.Message {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id":          "msg_test",
		"type":        "message",
		"role":        "assistant",
		"model":       "claude-sonnet-4-5-20250929",
		"content":     content,
		"stop_reason": stopReason,
		"usage":       map[string]any{"input_tokens": 12, "output_tokens": 18},
	})
	require.NoError(t, err)
	var msg anthropicsdk.Message
	require.NoError(t, json.Unmarshal(raw, &msg))
	return &msg
}

// mustOpenAIResponse constructs a provider-shaped OpenAI Responses API value.
func mustOpenAIResponse(t *testing.T, outputArrayJSON string) *responses.Response {
	t.Helper()
	raw := fmt.Sprintf(`{
		"id":"resp_test",
		"model":"gpt-5.4",
		"status":"completed",
		"usage":{
			"input_tokens":12,
			"input_tokens_details":{"cached_tokens":0},
			"output_tokens":18,
			"output_tokens_details":{"reasoning_tokens":0},
			"total_tokens":30
		},
		"output":%s
	}`, outputArrayJSON)
	var resp responses.Response
	require.NoError(t, json.Unmarshal([]byte(raw), &resp))
	return &resp
}
