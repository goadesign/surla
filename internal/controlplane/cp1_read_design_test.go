// This file tests the Checkpoint 1 two-phase `read_design` inspection loop,
// strict payload and saved-workspace decoders, combined output-limit plus
// tool-validation rejections, and offline Anthropic and OpenAI request encoding
// across direct and gRPC gateway model clients.
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

// TestCP1TwoPhaseReadDesign_SelectiveParallelAndLargeWorkspaces verifies
// selective single-file inspection, parallel multi-file reads in one turn, and
// multi-file workspaces larger than maxReadDesignTurnsCap (10 files across 8
// valid inspection turns).
func TestCP1TwoPhaseReadDesign_SelectiveParallelAndLargeWorkspaces(t *testing.T) {
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
					assert.Equal(t, cp1AnalysisMaxOutputTokens, req.MaxTokens)
					if i < 3 {
						require.Len(t, req.Tools, 1)
						assert.Equal(t, "read_design", req.Tools[0].Name)
						phase1Sys := req.Messages[0].Parts[0].(model.TextPart).Text
						assert.Contains(t, phase1Sys, "Workspace design manifest:")
						assert.Contains(t, phase1Sys, "Do not write a Change Card")
						assert.NotContains(t, phase1Sys, "omit path")
						assert.NotContains(t, phase1Sys, "Return structured JSON")
						assert.NotContains(t, phase1Sys, "ui_composition")
					}
				}
				// Turn 1 receives Turn 0's tool result with remaining_turns = 4.
				turn1Last := q.requests[1].Messages[len(q.requests[1].Messages)-1]
				resMap := turn1Last.Parts[0].(model.ToolResultPart).Content.(map[string]any)
				assert.Equal(t, "design/orders.go", resMap["path"])
				assert.Contains(t, resMap["content"], "orders design")
				assert.Equal(t, "4", fmt.Sprint(resMap["remaining_turns"]))

				// Phase 2 is a clean 2-message [System, User] structured request with
				// Tools empty, full card-writing instructions, sorted read files, and
				// unread file bodies omitted.
				phase2 := q.requests[3]
				assert.Empty(t, phase2.Tools)
				require.NotNil(t, phase2.StructuredOutput)
				require.Len(t, phase2.Messages, 2)
				assert.Equal(t, model.ConversationRoleSystem, phase2.Messages[0].Role)
				phase2Sys := phase2.Messages[0].Parts[0].(model.TextPart).Text
				assert.Contains(t, phase2Sys, "Return structured JSON")
				assert.Contains(t, phase2Sys, "ui_composition")
				assert.Equal(t, model.ConversationRoleUser, phase2.Messages[1].Role)
				require.Len(t, phase2.Messages[1].Parts, 1)
				phase2Text := phase2.Messages[1].Parts[0].(model.TextPart).Text
				assert.Contains(t, phase2Text, "Workspace design manifest:")
				assert.Contains(t, phase2Text, "Inspected design files (other manifest files were not read during inspection):")
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

			t.Run("parallel_calls_reading_all_files_transition_immediately", func(t *testing.T) {
				wsDir := setupWorkspaceWithFiles(t, map[string]string{
					"design/design.go": "package design\n// main design\n",
					"design/orders.go": "package design\n// orders design\n",
				})
				q := &queuedModelClient{
					responses: []queuedModelTurn{
						{
							resp: &model.Response{
								Content: []model.Message{{
									Role: model.ConversationRoleAssistant,
									Parts: []model.Part{
										model.ToolUsePart{
											ID:    "call_orders",
											Name:  "read_design",
											Input: rawjson.Message(`{"path":"design/orders.go"}`),
										},
										model.ToolUsePart{
											ID:    "call_design",
											Name:  "read_design",
											Input: rawjson.Message(`{"path":"design/design.go"}`),
										},
									},
								}},
								StopReason: "tool_use",
							},
						},
						jsonAnswerTurn(validAnalysisJSON),
					},
				}
				cp := newControlPlane(t, Options{
					CardsDir:     t.TempDir(),
					WorkspaceDir: wsDir,
					Analyzer:     NewModelCP1Analyzer(cf.buildFn(t, q)),
				})

				card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
					ChangeID: "chg-parallel",
					Prompt:   "Add order refunds",
				})
				require.NoError(t, err)
				assert.Equal(t, "Order refund processing.", card.Summary)
				require.Len(t, q.requests, 2)
				phase2 := q.requests[1]
				assert.Empty(t, phase2.Tools)
				require.Len(t, phase2.Messages, 2)
				phase2Text := phase2.Messages[1].Parts[0].(model.TextPart).Text
				assert.Contains(t, phase2Text, "\nInspected design files:\n")
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
// ThoughtSignature preservation without slice aliasing, output-limit cutoffs
// (including combined output-limited invalid tool arguments across direct and
// gateway clients), RecoveryCorrection framing, and unresolved-error budget
// exhaustion.
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

		// 1. OutputLimited response flag checked across valid tool input, invalid
		// tool_arguments (where RequestContract classifies as tool_arguments with
		// RecoveryCorrection), and unadvertised tool names across BOTH direct
		// model.NewClient and gateway.NewGRPCClient boundaries without retrying.
		for _, cf := range clientFactories {
			for _, limCase := range []struct {
				name     string
				toolName string
				rawInput string
			}{
				{name: "valid_tool_args", toolName: "read_design", rawInput: `{"path":"design/design.go"}`},
				{name: "empty_path_tool_args", toolName: "read_design", rawInput: `{"path":""}`},
				{name: "missing_path_tool_args", toolName: "read_design", rawInput: `{}`},
				{name: "unadvertised_tool", toolName: "other_tool", rawInput: `{}`},
			} {
				t.Run(cf.name+"/"+limCase.name, func(t *testing.T) {
					qLim := &queuedModelClient{
						responses: []queuedModelTurn{
							{
								resp: &model.Response{
									OutputLimited: true,
									StopReason:    "max_tokens",
									Content: []model.Message{{
										Role: model.ConversationRoleAssistant,
										Parts: []model.Part{model.ToolUsePart{
											ID:    "call_lim",
											Name:  limCase.toolName,
											Input: rawjson.Message(limCase.rawInput),
										}},
									}},
								},
							},
							// Queued valid fallback proves runPhase1Inspection never retries after OutputLimited.
							toolUseTurn("call_retry", "read_design", `{"path":"design/design.go"}`),
							jsonAnswerTurn(validAnalysisJSON),
						},
					}
					cpLim := newControlPlane(t, Options{
						CardsDir:     t.TempDir(),
						WorkspaceDir: wsDir,
						Analyzer:     NewModelCP1Analyzer(cf.buildFn(t, qLim)),
					})
					_, err := cpLim.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
						ChangeID: "chg-lim",
						Prompt:   "P",
					})
					require.Error(t, err)
					assert.Contains(t, err.Error(), "provider stopped at output limit; shorten the request before retrying")
					kind, ok := cp1RejectionKind(err)
					assert.True(t, ok)
					assert.Equal(t, "output_limited", kind)
					assert.Len(t, qLim.requests, 1, "OutputLimited tool turn must reject immediately without recovery retry")
					_, getErr := cpLim.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-lim"})
					require.Error(t, getErr)
				})
			}
		}

		// 2. Non-output-limited schema failure recovers with framed guidance within budget.
		qRec := &queuedModelClient{
			responses: []queuedModelTurn{
				toolUseTurn("call_bad", "read_design", `{}`),
				toolUseTurn("call_ok", "read_design", `{"path":"design/design.go"}`),
				jsonAnswerTurn(validAnalysisJSON),
			},
		}
		cpRec := newControlPlane(t, Options{
			CardsDir: t.TempDir(), WorkspaceDir: wsDir,
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, qRec)),
		})
		_, err := cpRec.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg-rec", Prompt: "P"})
		require.NoError(t, err)
		assertAlternatingRoles(t, qRec.requests[1].Messages)
		recMsg := qRec.requests[1].Messages[len(qRec.requests[1].Messages)-1]
		reminderText := recMsg.Parts[1].(model.TextPart).Text
		assert.Contains(t, reminderText, "Your previous read_design tool call was rejected before it could run.")
		assert.Contains(t, reminderText, "path")

		// 3. Unknown manifest path or recoverable tool_arguments error on the final
		// inspection turn proceeds to Phase 2 with the manifest and already-read files.
		twoFileWS := setupWorkspaceWithFiles(t, map[string]string{
			"design/design.go": "package design\n// read on turn 0\n",
			"design/other.go":  "package design\n// unread\n",
		})
		// 2 files -> maxInspectionTurns = 4.
		qLastTurnPath := &queuedModelClient{
			responses: []queuedModelTurn{
				toolUseTurn("c1", "read_design", `{"path":"design/design.go"}`),
				toolUseTurn("c2", "read_design", `{"path":"design/bad1.go"}`),
				toolUseTurn("c3", "read_design", `{"path":"design/bad2.go"}`),
				toolUseTurn("c4", "read_design", `{"path":"design/bad3.go"}`),
				jsonAnswerTurn(validAnalysisJSON),
			},
		}
		cpLastTurnPath := newControlPlane(t, Options{
			CardsDir: t.TempDir(), WorkspaceDir: twoFileWS,
			Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, qLastTurnPath)),
		})
		_, err = cpLastTurnPath.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg-last-path", Prompt: "P"})
		require.NoError(t, err)
		require.Len(t, qLastTurnPath.requests, 5)
		lastPathPhase2 := qLastTurnPath.requests[4].Messages[1].Parts[0].(model.TextPart).Text
		assert.Contains(t, lastPathPhase2, "--- design/design.go ---\npackage design\n// read on turn 0")
		assert.NotContains(t, lastPathPhase2, "// unread")
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
				toolUseTurn("call_1", "read_design", `{"path":"design/design.go"}`),
				jsonAnswerTurn(validAnalysisJSON),
				toolUseTurn("call_r2", "read_design", `{"path":"design/design.go"}`),
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
				toolUseTurn("call_r3", "read_design", `{"path":"design/design.go"}`),
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

// TestCP1TwoPhaseReadDesign_RealProviderAdapters exercises multi-turn Phase 1
// read_design tool history encoding and Phase 2 toolless structured completion
// through the real Goa-AI Anthropic Messages and OpenAI Responses adapters with
// offline SDK transports (zero network calls).
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
		oai := &scriptedOpenAIResponses{
			responses: []*responses.Response{
				mustOpenAIResponse(t, `[{
					"id":"fc_01",
					"type":"function_call",
					"call_id":"call_01",
					"name":"read_design",
					"arguments":"{\"path\":\"design/api.go\"}",
					"status":"completed"
				}]`),
				mustOpenAIResponse(t, `[{
					"id":"msg_01",
					"type":"message",
					"role":"assistant",
					"status":"completed",
					"content":[{"type":"output_text","text":"Inspected design/api.go.","annotations":[],"logprobs":[]}]
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
		require.Len(t, oai.requests, 3)

		// Phase 1 Turn 0 advertises strict read_design function tool; Turn 1 replays
		// function_call and function_call_output items through the OpenAI encoder.
		require.Len(t, oai.requests[0].Tools, 1)
		require.Len(t, oai.requests[1].Tools, 1)
		turn1Wire, err := json.Marshal(oai.requests[1])
		require.NoError(t, err)
		assert.Contains(t, string(turn1Wire), `"function_call"`)
		assert.Contains(t, string(turn1Wire), `"function_call_output"`)
		assert.Contains(t, string(turn1Wire), `"call_01"`)

		// Phase 2 has zero tools, zero function_call history items, and only the read file.
		phase2 := oai.requests[2]
		assert.Empty(t, phase2.Tools)
		phase2Wire, err := json.Marshal(phase2)
		require.NoError(t, err)
		assert.Contains(t, string(phase2Wire), "edge API contract")
		assert.NotContains(t, string(phase2Wire), "unread orders internal contract")
		assert.NotContains(t, string(phase2Wire), "call_01")
	})
}

// TestCP1DecodersRejectInvalidInputs verifies that Goa-AI's model.Client owns
// Phase 1 tool-name and schema validation (immediately rejecting unadvertised
// tool names and rejecting invalid read_design payloads with recovery reminders
// before proceeding to Phase 2 on the final inspection turn) and that
// decodeSavedWorkspaceCP1Design strictly rejects malformed stored workspace JSON.
func TestCP1DecodersRejectInvalidInputs(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	wsDir := setupWorkspaceWithFiles(t, map[string]string{"design/design.go": "package design\n// body\n"})

	t.Run("model_client_tool_validation/unadvertised_tool", func(t *testing.T) {
		q := &queuedModelClient{
			responses: []queuedModelTurn{
				toolUseTurn("c1", "other_tool", `{"path":"design/design.go"}`),
			},
		}
		cp := newControlPlane(t, Options{
			CardsDir:     t.TempDir(),
			WorkspaceDir: wsDir,
			Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
		})
		_, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-unadvertised",
			Prompt:   "Test validation",
		})
		require.Error(t, err)
		kind, ok := cp1RejectionKind(err)
		require.True(t, ok)
		assert.Equal(t, string(model.OutputValidationToolIdentity), kind)
		assert.Len(t, q.requests, 1)
	})

	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "empty_raw", raw: ``},
		{name: "literal_null", raw: `null`},
		{name: "missing_path", raw: `{}`},
		{name: "null_path", raw: `{"path":null}`},
		{name: "empty_path", raw: `{"path":""}`},
		{name: "unknown_field", raw: `{"path":"design/design.go","other":"x"}`},
	} {
		t.Run("model_client_tool_validation/"+tc.name, func(t *testing.T) {
			// 1 file -> maxInspectionTurns = 3; invalid read_design payloads are rejected
			// by model.Client (never reading design.go), send recovery reminders on turns
			// 0 and 1, and proceed to Phase 2 with no read files on the final turn.
			q := &queuedModelClient{
				responses: []queuedModelTurn{
					toolUseTurn("c1", "read_design", tc.raw),
					toolUseTurn("c2", "read_design", tc.raw),
					toolUseTurn("c3", "read_design", tc.raw),
					jsonAnswerTurn(validAnalysisJSON),
				},
			}
			cp := newControlPlane(t, Options{
				CardsDir:     t.TempDir(),
				WorkspaceDir: wsDir,
				Analyzer:     NewModelCP1Analyzer(newQueuedModelClient(t, q)),
			})
			_, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
				ChangeID: "chg-" + tc.name,
				Prompt:   "Test validation",
			})
			require.NoError(t, err)
			require.Len(t, q.requests, 4)
			phase2Text := q.requests[3].Messages[1].Parts[0].(model.TextPart).Text
			assert.Contains(t, phase2Text, "(no design files were read during inspection)")
			assert.NotContains(t, phase2Text, "// body")
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
func mustAnthropicMessage(
	t *testing.T, content []map[string]any, stopReason string,
) *anthropicsdk.Message {
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
