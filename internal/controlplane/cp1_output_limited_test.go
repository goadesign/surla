// This file tests CP1 rejection of invalid structured responses when the
// provider reports its output token limit, without saving rejected analysis.

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/runtime/agent/model"
)

// TestCP1OutputLimitedRejectionAcrossModelBoundaries verifies that invalid,
// missing, and schema-failing provider-limited responses have the same safe
// recovery message over Go, HTTP, and MCP, whether the model is local or behind
// the gRPC gateway. Proposals save nothing and revisions preserve saved bytes.
func TestCP1OutputLimitedRejectionAcrossModelBoundaries(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	for _, mode := range []struct {
		name      string
		makeModel func(*testing.T, *queuedModelClient) model.Client
	}{
		{name: "direct", makeModel: newQueuedModelClient},
		{name: "gateway", makeModel: func(t *testing.T, q *queuedModelClient) model.Client {
			return startGatewayModelClient(t, q)
		}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			q := &queuedModelClient{}
			cp := newControlPlane(t, Options{Analyzer: NewModelCP1Analyzer(mode.makeModel(t, q))})
			handler := NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{
				"alice-token": {ID: "alice@example.com"},
			}))
			adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)
			q.enqueueJSON(t, CP1SemanticAnalysis{
				Summary:     "Refunds up to $50 are instant.",
				Ambiguities: []CP1ModelAmbiguity{},
				Examples: []CP1ModelExample{{
					Scenario: "Refund", InputSummary: "$50", ExpectedOutcome: "Instant", VerificationKind: "deterministic",
				}},
				PreservedBehaviors: []string{},
			})
			seed, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{
				ChangeID: "chg-seed", Prompt: "Refunds.",
			})
			require.NoError(t, err)
			before, err := os.ReadFile(cp.cards.path(seed.ChangeID))
			require.NoError(t, err)

			for _, tc := range []struct {
				name    string
				content []model.Message
			}{
				{name: "malformed_json", content: []model.Message{{
					Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: "private-rejected-output"}},
				}}},
				{name: "missing_content"},
				{name: "schema_failure", content: []model.Message{{
					Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: `{"summary":"private-rejected-output"}`}},
				}}},
			} {
				for _, operation := range []string{"propose", "revise"} {
					for _, transport := range []string{"go", "http", "mcp"} {
						t.Run(tc.name+"/"+operation+"/"+transport, func(t *testing.T) {
							changeID := "chg-" + tc.name + "-" + transport
							input := map[string]string{"change_id": changeID, "prompt": "Refunds."}
							if operation == "revise" {
								changeID = seed.ChangeID
								input["change_id"] = changeID
								input["request_revision"] = seed.RequestRevision
							}
							body, err := json.Marshal(input)
							require.NoError(t, err)
							q.mu.Lock()
							q.responses = append(q.responses, queuedModelTurn{resp: &model.Response{
								OutputLimited: true, StopReason: "private-stop-marker", Content: tc.content,
							}})
							q.mu.Unlock()

							var message string
							switch transport {
							case "go":
								if operation == "propose" {
									_, err = cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: changeID, Prompt: input["prompt"]})
								} else {
									_, err = cp.ReviseChange(ctx, &gensurla.ReviseChangePayload{
										ChangeID: changeID, RequestRevision: seed.RequestRevision, Prompt: input["prompt"],
									})
								}
								require.Error(t, err)
								var ovErr *model.OutputValidationError
								require.ErrorAs(t, err, &ovErr)
								assert.Equal(t, model.OutputValidationOutputLimited, ovErr.Kind())
								se, ok := asAnalysisRejected(err)
								require.True(t, ok)
								assert.False(t, se.Fault)
								message = se.Message
							case "http":
								req := httptest.NewRequest(http.MethodPost, "/changes/"+operation, strings.NewReader(string(body)))
								req.Header.Set("Authorization", "Bearer alice-token")
								req.Header.Set("Content-Type", "application/json")
								rec := httptest.NewRecorder()
								handler.ServeHTTP(rec, req)
								assert.Equal(t, http.StatusBadGateway, rec.Code)
								var result struct {
									Name    string `json:"name"`
									Message string `json:"message"`
									Fault   bool   `json:"fault"`
								}
								require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &result))
								assert.Equal(t, "analysis_rejected", result.Name)
								assert.False(t, result.Fault)
								message = result.Message
							case "mcp":
								result, err := adapter.ToolsCall(ctx, &genmcpadapter.ToolsCallPayload{
									Name: operation + "_change", Arguments: body,
								})
								require.NoError(t, err)
								complete, ok := result.Outcome.AsComplete()
								require.True(t, ok)
								require.NotNil(t, complete.IsError)
								assert.True(t, *complete.IsError)
								require.NotEmpty(t, complete.Content)
								require.NotNil(t, complete.Content[0].Text)
								message = *complete.Content[0].Text
							}
							assert.Contains(t, message, "provider stopped at output limit; shorten the request before retrying")
							assert.Contains(t, message, "this "+operation+"_change request did not ")
							assert.NotContains(t, message, "private-rejected-output")
							assert.NotContains(t, message, "private-stop-marker")
							if operation == "propose" {
								_, err := os.Stat(cp.cards.path(changeID))
								assert.ErrorIs(t, err, os.ErrNotExist)
							} else {
								after, err := os.ReadFile(cp.cards.path(changeID))
								require.NoError(t, err)
								assert.Equal(t, before, after)
								assert.Contains(t, message, seed.RequestRevision)
							}
							assert.Equal(t, 16384, q.lastRequest().MaxTokens)
						})
					}
				}
			}
		})
	}
}

// TestCP1OutputLimitedErrorIdentityAndMixedFaults verifies that Surla retains
// the framework error rather than replacing its cause, and does not classify
// a token-limit rejection joined with an independent failure as a rejection.
func TestCP1OutputLimitedErrorIdentityAndMixedFaults(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	q := &queuedModelClient{responses: []queuedModelTurn{{resp: &model.Response{
		OutputLimited: true, StopReason: "private-stop-marker",
	}}}}
	cp := newControlPlane(t, Options{Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, q))})
	payload := &gensurla.ProposeChangePayload{ChangeID: "chg-limited", Prompt: "Refunds."}
	_, err := cp.ProposeChange(ctx, payload)
	var original *model.OutputValidationError
	require.ErrorAs(t, err, &original)
	require.Equal(t, model.OutputValidationOutputLimited, original.Kind())
	fault := errors.New("independent provider fault")
	for _, tc := range []struct {
		name     string
		err      error
		rejected bool
		other    error
	}{
		{name: "wrapped", err: fmt.Errorf("wrapped: %w", original), rejected: true},
		{name: "duplicate_join", err: errors.Join(original, original), rejected: true},
		{name: "independent_fault", err: errors.Join(original, fault), other: fault},
		{name: "cancellation", err: errors.Join(original, context.Canceled), other: context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q.mu.Lock()
			q.responses = append(q.responses, queuedModelTurn{err: tc.err})
			q.mu.Unlock()
			_, err := cp.ProposeChange(ctx, payload)
			require.Error(t, err)
			var got *model.OutputValidationError
			require.ErrorAs(t, err, &got)
			assert.Same(t, original, got)
			_, rejected := asAnalysisRejected(err)
			assert.Equal(t, tc.rejected, rejected)
			if tc.other != nil {
				assert.ErrorIs(t, err, tc.other)
			}
		})
	}
}
