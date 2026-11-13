// This file proves on the real project server that only the user declines a
// revision the guide proposed beside an agreed change: an agent that calls
// the tool anyway, although it was not offered, is refused.
package project

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
)

// TestAgentsCannotDeclineAProposalOverMCP agrees a change, has the guide
// propose a revision beside it, and sends tools/call decline_revision with
// the guide's, the builder's and the test writer's own credentials. Each
// call is refused by the operation table, and the proposal still waits
// beside the unchanged agreed revision afterwards.
func TestAgentsCannotDeclineAProposalOverMCP(t *testing.T) {
	vp := startViewsProject(t, controlplane.Options{})
	cp := vp.cp.Load()
	owner := vp.state.Record.Owner
	guide := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: owner, CallerID: "guide", Kind: controlplane.CallerGuide})
	user := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: owner, CallerID: "user-1", Kind: controlplane.CallerUser})
	example := &gensurla.CP1AnalysisExample{
		Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
		ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
	}
	proposed, err := cp.ProposeChange(guide, &gensurla.ProposeChangePayload{
		ChangeID: "chg", Prompt: "Refunds up to $50 are instant.",
		Analysis: &gensurla.CP1Analysis{
			Summary: "Refunds up to $50 are instant.", Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1AnalysisExample{example}, PreservedBehaviors: []string{},
		},
	})
	require.NoError(t, err)
	agreed, err := cp.ConfirmIntent(user, &gensurla.ConfirmIntentPayload{
		ChangeID: "chg", RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	kept := agreed.Examples[0].ID
	revised, err := cp.ReviseChange(guide, &gensurla.ReviseChangePayload{
		ChangeID: "chg", RequestRevision: agreed.RequestRevision, Prompt: "Refunds up to $60 are instant.",
		Analysis: &gensurla.CP1RevisionAnalysis{
			Summary: "Refunds up to $60 are instant.", Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1RevisionAnalysisExample{{
				PriorID: &kept, Scenario: example.Scenario, InputSummary: "Order total $60.00",
				ExpectedOutcome: example.ExpectedOutcome, VerificationKind: example.VerificationKind,
			}},
			RemovedExampleIds: []string{}, PreservedBehaviors: []string{},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, revised.ProposedRevision)
	proposal := revised.ProposedRevision.RequestRevision
	call := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"decline_revision","arguments":{"change_id":"chg","request_revision":%q}}}`, proposal)

	for _, role := range []string{"guide", "builder", "test_writer"} {
		t.Run(role, func(t *testing.T) {
			answer := testRequest(t, http.DefaultClient, http.MethodPost, vp.state.Origin()+"/mcp", call, vp.agent(t, role), false)
			require.Equal(t, http.StatusOK, answer.StatusCode)
			var envelope map[string]any
			require.NoError(t, json.NewDecoder(answer.Body).Decode(&envelope))
			require.NoError(t, answer.Body.Close())
			assert.Nil(t, envelope["error"])
			result, ok := envelope["result"].(map[string]any)
			require.True(t, ok, "tools/call answered %v", envelope)
			assert.Equal(t, true, result["isError"], "the %s's decline was not refused", role)
			assert.Contains(t, fmt.Sprint(result["content"]), "may not call decline_revision")

			change, err := cp.GetChange(user, &gensurla.GetChangePayload{ChangeID: "chg"})
			require.NoError(t, err)
			assert.Equal(t, agreed.RequestRevision, change.RequestRevision, "the %s's refused decline changed the agreed revision", role)
			require.NotNil(t, change.ProposedRevision, "the %s's refused decline removed the proposal", role)
			assert.Equal(t, proposal, change.ProposedRevision.RequestRevision)
		})
	}
}
