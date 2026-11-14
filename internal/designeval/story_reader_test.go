// This file runs the evaluation program's Story reader in process, against
// the Story module version Surla's go.mod requires.
package designeval

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa/v3/eval"

	storydsl "github.com/goadesign/surla/story/dsl"
	storyexpr "github.com/goadesign/surla/story/expr"
)

// TestStorySummariesReadTheRequiredStoryModule declares a Story with the
// Story language Surla requires, runs it, and checks that the reader the
// evaluation program uses copies every field, takes each flow's completeness
// from the Story module, and prints what the host decodes into the required
// tests: one scenario for the finished flow, none for the open one.
func TestStorySummariesReadTheRequiredStoryModule(t *testing.T) {
	previousContext, previousRoot := eval.Context, storyexpr.Root
	t.Cleanup(func() { eval.Context, storyexpr.Root = previousContext, previousRoot })
	eval.Reset()
	storyexpr.Root = new(storyexpr.RootExpr)
	require.NoError(t, eval.Register(storyexpr.Root))
	storydsl.Story("Refunds", func() {
		storydsl.Description("Customers get their money back.")
		storydsl.Actor("customer", "Customer")
		storydsl.Actor("manager", "Support manager", func() { storydsl.Description("Approves large refunds.") })
		storydsl.Flow("refund-over-limit", "Refund above the limit", func() {
			storydsl.StartsWith("request-refund", "customer", "Requests a refund")
			storydsl.Step("approve-refund", "manager", "Approves the refund")
			storydsl.Outcome("refund-paid", "customer", "Receives the refund")
		})
		storydsl.Flow("refund-without-receipt", "Refund without a receipt", func() {
			storydsl.StartsWith("request-refund", "customer", "Requests a refund", func() {
				storydsl.Question("proof-of-purchase", "Which proof of purchase is acceptable?", func() {
					storydsl.Option("Order number")
					storydsl.Option("Payment record")
				})
			})
		})
	})
	require.NoError(t, eval.RunDSL())

	stories := storySummaries()
	assert.Equal(t, []storySummary{{
		Name:        "Refunds",
		Description: "Customers get their money back.",
		Actors: []storyActor{
			{Key: "customer", Title: "Customer"},
			{Key: "manager", Title: "Support manager", Description: "Approves large refunds."},
		},
		Flows: []storyFlow{
			{Key: "refund-over-limit", Title: "Refund above the limit", Complete: true, Steps: []storyStep{
				{Key: "request-refund", ActorKey: "customer", Summary: "Requests a refund", Kind: "start"},
				{Key: "approve-refund", ActorKey: "manager", Summary: "Approves the refund", Kind: "step"},
				{Key: "refund-paid", ActorKey: "customer", Summary: "Receives the refund", Kind: "outcome"},
			}},
			{Key: "refund-without-receipt", Title: "Refund without a receipt", Steps: []storyStep{{
				Key: "request-refund", ActorKey: "customer", Summary: "Requests a refund", Kind: "start",
				Questions: []storyQuestion{{
					Key: "proof-of-purchase", Prompt: "Which proof of purchase is acceptable?",
					Options: []string{"Order number", "Payment record"},
				}},
			}}},
		},
	}}, stories)

	raw, err := json.Marshal(stories)
	require.NoError(t, err)
	var decoded []StorySummary
	require.NoError(t, json.Unmarshal(raw, &decoded))
	required, drafts, err := StoryScenarios(decoded)
	require.NoError(t, err)
	assert.Equal(t, []StoryScenario{{
		Story: "Refunds", Key: "refund-over-limit", Scenario: "refund_over_limit", Hook: "RefundOverLimit",
	}}, required)
	require.Len(t, drafts, 1)
	assert.Equal(t, "refund-without-receipt", drafts[0].Key)
}
