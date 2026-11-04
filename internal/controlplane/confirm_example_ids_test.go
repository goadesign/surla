package controlplane

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
)

// TestConfirmIntentKeepsCallerExampleIDs verifies the example id contract
// ExampleItem describes: CP1 assigns ids to the examples it proposes, and
// confirm_intent keeps an edited example's existing id and an added example's
// caller-chosen id exactly as sent, in a new revision.
func TestConfirmIntentKeepsCallerExampleIDs(t *testing.T) {
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	analysis := `{"summary":"Refunds up to $50 are instant.","ambiguities":[],` +
		`"examples":[{"scenario":"Small refund","input_summary":"$20 order","expected_outcome":"Instant credit","verification_kind":"deterministic"}],` +
		`"preserved_behaviors":[]}`
	cp := newControlPlane(t, Options{Analyzer: NewModelCP1Analyzer(newStubModelClient(t, analysis))})
	proposed, err := cp.ProposeChange(ctx, &gensurla.ProposeChangePayload{ChangeID: "refunds", Prompt: "Instant refunds up to $50"})
	require.NoError(t, err)
	require.Len(t, proposed.Examples, 1)
	assignedID := proposed.Examples[0].ID
	assert.Equal(t, "refunds-check-01", assignedID, "CP1 assigns ids to the examples it proposes")

	edited := *proposed.Examples[0]
	edited.ExpectedOutcome = "Instant store credit"
	added := &gensurla.ExampleItem{
		ID: "refund-at-limit", Scenario: "Refund at the limit", InputSummary: "$50 order",
		ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
	}
	confirmed, err := cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
		ChangeID: "refunds", RequestRevision: proposed.RequestRevision,
		Answers: map[string]string{}, Examples: []*gensurla.ExampleItem{&edited, added},
	})
	require.NoError(t, err)
	assert.NotEqual(t, proposed.RequestRevision, confirmed.RequestRevision, "changed examples make a new revision")
	ids := make([]string, 0, len(confirmed.Examples))
	for _, ex := range confirmed.Examples {
		ids = append(ids, ex.ID)
	}
	assert.Equal(t, []string{assignedID, "refund-at-limit"}, ids)
	assert.Equal(t, "Instant store credit", confirmed.Examples[0].ExpectedOutcome)

	saved, err := cp.GetChange(ctx, &gensurla.GetChangePayload{ChangeID: "refunds"})
	require.NoError(t, err)
	require.Len(t, saved.Examples, 2)
	assert.Equal(t, "refund-at-limit", saved.Examples[1].ID, "the caller's id is saved as sent")
}
