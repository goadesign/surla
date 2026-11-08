// This file tests Checkpoint 1 (CP1) example ID allocation and revision accounting
// under Decision 1A (revise_change prior-example accounting) and Decision 2A (check_01
// default example IDs).

package controlplane

import (
	"context"
	"regexp"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/runtime/agent/model"
)

// newReviseTestControlPlane creates a test ControlPlane with a queued model client
// returning the specified serialized JSON responses in order.
func newReviseTestControlPlane(t *testing.T, responses ...string) (*ControlPlane, string) {
	t.Helper()
	cardsDir := t.TempDir()
	qc := &queuedModelClient{}
	for _, r := range responses {
		qc.responses = append(qc.responses, queuedModelTurn{
			resp: &model.Response{
				Content: []model.Message{{
					Role:  model.ConversationRoleAssistant,
					Parts: []model.Part{model.TextPart{Text: r}},
				}},
				StopReason: "stop",
			},
		})
	}
	cp := newControlPlane(t, Options{
		CardsDir: cardsDir,
		Analyzer: NewModelCP1Analyzer(newQueuedModelClient(t, qc)),
	})
	return cp, cardsDir
}

// TestProposeChangeAssignsDefaultExampleIDs_Decision2A verifies that propose_change
// assigns check_01, check_02, ... IDs matching the goa-ai/eval validateID regex
// even when change_id contains hyphens and digits.
func TestProposeChangeAssignsDefaultExampleIDs_Decision2A(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	proposalJSON := `{
		"summary": "Process refunds under $50 instantly.",
		"ambiguities": [],
		"examples": [
			{"scenario": "First check", "input_summary": "$10 order", "expected_outcome": "Instant refund", "verification_kind": "deterministic"},
			{"scenario": "Second check", "input_summary": "$30 order", "expected_outcome": "Instant refund", "verification_kind": "deterministic"},
			{"scenario": "Third check", "input_summary": "$50 order", "expected_outcome": "Instant refund", "verification_kind": "model_judged"}
		],
		"preserved_behaviors": ["Orders over $50 require manager approval."]
	}`
	cp, _ := newReviseTestControlPlane(t, proposalJSON)

	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "feat-refunds-1",
		Prompt:   "Instant refunds under $50.",
	})
	require.NoError(t, err)
	require.Len(t, card.Examples, 3)

	evalScenarioIDRegex := regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)
	expectedIDs := []string{"check_01", "check_02", "check_03"}
	for i, ex := range card.Examples {
		assert.Equal(t, expectedIDs[i], ex.ID, "examples[%d].id must match check_%02d", i, i+1)
		assert.True(t, evalScenarioIDRegex.MatchString(ex.ID), "example ID %q must satisfy goa-ai/eval validateID regex", ex.ID)
	}
}

// TestReviseChangePreservesSurvivingPriorExamples_Decision1A verifies that revise_change
// preserves surviving prior examples' exact IDs via prior_id (both code-assigned check_01
// and caller-confirmed custom IDs like ex-search).
func TestReviseChangePreservesSurvivingPriorExamples_Decision1A(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	proposalJSON := `{
		"summary": "Search products.",
		"ambiguities": [{"question": "Include out of stock?", "options": ["yes", "no"]}],
		"examples": [
			{"scenario": "Standard search", "input_summary": "query=shoes", "expected_outcome": "List results", "verification_kind": "deterministic"}
		],
		"preserved_behaviors": []
	}`
	revisionJSON := `{
		"summary": "Search products with fast filtering.",
		"ambiguities": [],
		"examples": [
			{"prior_id": "check_01", "scenario": "Standard search", "input_summary": "query=shoes", "expected_outcome": "List results within 50ms", "verification_kind": "deterministic"},
			{"prior_id": "ex-search", "scenario": "Out of stock filter", "input_summary": "query=shoes&in_stock=true", "expected_outcome": "Only in-stock results", "verification_kind": "deterministic"}
		],
		"removed_example_ids": [],
		"preserved_behaviors": []
	}`
	cp, _ := newReviseTestControlPlane(t, proposalJSON, revisionJSON)

	card1, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "search-feature",
		Prompt:   "Search products.",
	})
	require.NoError(t, err)
	require.Len(t, card1.Examples, 1)
	assert.Equal(t, "check_01", card1.Examples[0].ID)

	// Confirm card1 with caller-chosen ID ex-search added.
	confirmed, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID:        "search-feature",
		RequestRevision: card1.RequestRevision,
		Answers:         map[string]string{card1.Ambiguities[0].ID: "yes"},
		Examples: []*gensurla.ExampleItem{
			card1.Examples[0],
			{
				ID:               "ex-search",
				Scenario:         "Out of stock filter",
				InputSummary:     "query=shoes&in_stock=true",
				ExpectedOutcome:  "Only in-stock results",
				VerificationKind: "deterministic",
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, confirmed.Examples, 2)
	assert.Equal(t, "check_01", confirmed.Examples[0].ID)
	assert.Equal(t, "ex-search", confirmed.Examples[1].ID)

	// Revise the confirmed card.
	revised, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "search-feature",
		RequestRevision: confirmed.RequestRevision,
		Prompt:          "Search products with fast filtering.",
	})
	require.NoError(t, err)
	require.Len(t, revised.Examples, 2)
	assert.Equal(t, "check_01", revised.Examples[0].ID)
	assert.Equal(t, "ex-search", revised.Examples[1].ID)
}

// TestReviseChangeAllocatesLowestUnusedIDAbsentFromBothTables verifies that brand-new
// examples in revise_change receive the lowest check_%02d absent from both input and output tables.
func TestReviseChangeAllocatesLowestUnusedIDAbsentFromBothTables(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	t.Run("surviving check_01 and check_05 allocate check_02 for new example", func(t *testing.T) {
		proposalJSON := `{
			"summary": "Feature.",
			"ambiguities": [],
			"examples": [
				{"scenario": "First", "input_summary": "In1", "expected_outcome": "Out1", "verification_kind": "deterministic"}
			],
			"preserved_behaviors": []
		}`
		// Revision keeps check_01 and check_05, and adds a brand-new example (prior_id omitted).
		revisionJSON := `{
			"summary": "Revised feature.",
			"ambiguities": [],
			"examples": [
				{"prior_id": "check_01", "scenario": "First", "input_summary": "In1", "expected_outcome": "Out1", "verification_kind": "deterministic"},
				{"prior_id": "check_05", "scenario": "Fifth", "input_summary": "In5", "expected_outcome": "Out5", "verification_kind": "deterministic"},
				{"scenario": "Brand new", "input_summary": "InNew", "expected_outcome": "OutNew", "verification_kind": "deterministic"}
			],
			"removed_example_ids": [],
			"preserved_behaviors": []
		}`
		cp, _ := newReviseTestControlPlane(t, proposalJSON, revisionJSON)

		card1, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-gap",
			Prompt:   "Feature.",
		})
		require.NoError(t, err)

		// Confirm with check_01 and custom check_05.
		card1Conf, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
			ChangeID:        "chg-gap",
			RequestRevision: card1.RequestRevision,
			Answers:         map[string]string{},
			Examples: []*gensurla.ExampleItem{
				card1.Examples[0],
				{
					ID:               "check_05",
					Scenario:         "Fifth",
					InputSummary:     "In5",
					ExpectedOutcome:  "Out5",
					VerificationKind: "deterministic",
				},
			},
		})
		require.NoError(t, err)

		revised, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
			ChangeID:        "chg-gap",
			RequestRevision: card1Conf.RequestRevision,
			Prompt:          "Revised feature.",
		})
		require.NoError(t, err)
		require.Len(t, revised.Examples, 3)
		assert.Equal(t, "check_01", revised.Examples[0].ID)
		assert.Equal(t, "check_05", revised.Examples[1].ID)
		assert.Equal(t, "check_02", revised.Examples[2].ID, "new example must fill lowest gap check_02")
	})

	t.Run("retired check_01 and surviving check_02 allocate check_03 for new example", func(t *testing.T) {
		proposalJSON := `{
			"summary": "Feature.",
			"ambiguities": [],
			"examples": [
				{"scenario": "First", "input_summary": "In1", "expected_outcome": "Out1", "verification_kind": "deterministic"},
				{"scenario": "Second", "input_summary": "In2", "expected_outcome": "Out2", "verification_kind": "deterministic"}
			],
			"preserved_behaviors": []
		}`
		// Revision removes check_01, keeps check_02, and adds a brand-new example.
		revisionJSON := `{
			"summary": "Revised feature.",
			"ambiguities": [],
			"examples": [
				{"prior_id": "check_02", "scenario": "Second", "input_summary": "In2", "expected_outcome": "Out2", "verification_kind": "deterministic"},
				{"scenario": "Brand new", "input_summary": "InNew", "expected_outcome": "OutNew", "verification_kind": "deterministic"}
			],
			"removed_example_ids": ["check_01"],
			"preserved_behaviors": []
		}`
		cp, _ := newReviseTestControlPlane(t, proposalJSON, revisionJSON)

		card1, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
			ChangeID: "chg-retired",
			Prompt:   "Feature.",
		})
		require.NoError(t, err)
		require.Len(t, card1.Examples, 2)
		assert.Equal(t, "check_01", card1.Examples[0].ID)
		assert.Equal(t, "check_02", card1.Examples[1].ID)

		revised, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
			ChangeID:        "chg-retired",
			RequestRevision: card1.RequestRevision,
			Prompt:          "Revised feature.",
		})
		require.NoError(t, err)
		require.Len(t, revised.Examples, 2)
		assert.Equal(t, "check_02", revised.Examples[0].ID)
		assert.Equal(t, "check_03", revised.Examples[1].ID, "new example must allocate check_03, never reusing retired check_01")
	})
}

// TestReviseChangeRejectsOmittedPriorExampleID_Decision1A verifies that revise_change
// rejects any prior example omitted from both examples[*].prior_id and removed_example_ids
// as analysis_rejected (HTTP 502), leaving the saved card untouched.
func TestReviseChangeRejectsOmittedPriorExampleID_Decision1A(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	proposalJSON := `{
		"summary": "Feature.",
		"ambiguities": [],
		"examples": [
			{"scenario": "First", "input_summary": "In1", "expected_outcome": "Out1", "verification_kind": "deterministic"},
			{"scenario": "Second", "input_summary": "In2", "expected_outcome": "Out2", "verification_kind": "deterministic"}
		],
		"preserved_behaviors": []
	}`
	// Silent omission of check_02: not in examples, not in removed_example_ids.
	omissionJSON := `{
		"summary": "Revised feature.",
		"ambiguities": [],
		"examples": [
			{"prior_id": "check_01", "scenario": "First", "input_summary": "In1", "expected_outcome": "Out1", "verification_kind": "deterministic"}
		],
		"removed_example_ids": [],
		"preserved_behaviors": []
	}`
	cp, _ := newReviseTestControlPlane(t, proposalJSON, omissionJSON)

	card1, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
		ChangeID: "chg-omission",
		Prompt:   "Feature.",
	})
	require.NoError(t, err)

	_, err = cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-omission",
		RequestRevision: card1.RequestRevision,
		Prompt:          "Revised feature.",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, `prior example ID "check_02" is omitted from both examples and removed_example_ids`)
	se, ok := asAnalysisRejected(err)
	require.True(t, ok, "omission must return analysis_rejected ServiceError")
	assert.False(t, se.Fault, "analysis_rejected must not be a server fault")

	// Verify saved card remains untouched.
	saved, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-omission"})
	require.NoError(t, err)
	assert.Equal(t, card1.RequestRevision, saved.RequestRevision, "saved card revision must remain untouched")
}

// TestReviseChangeRejectsInvalidPriorAccounting_Decision1A verifies that revise_change
// rejects unknown prior_id, duplicate prior_id, unknown removed_example_ids, duplicate
// removed_example_ids, ID in both lists, or empty/whitespace removed entries as analysis_rejected (HTTP 502).
func TestReviseChangeRejectsInvalidPriorAccounting_Decision1A(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	cases := []struct {
		name       string
		reviseJSON string
		wantErr    string
	}{
		{
			name: "unknown prior_id",
			reviseJSON: `{
				"summary": "Revised.",
				"ambiguities": [],
				"examples": [
					{"prior_id": "check_01", "scenario": "S1", "input_summary": "I1", "expected_outcome": "O1", "verification_kind": "deterministic"},
					{"prior_id": "check_unknown", "scenario": "S2", "input_summary": "I2", "expected_outcome": "O2", "verification_kind": "deterministic"}
				],
				"removed_example_ids": [],
				"preserved_behaviors": []
			}`,
			wantErr: `examples[1].prior_id references unknown prior example ID "check_unknown"`,
		},
		{
			name: "duplicate prior_id",
			reviseJSON: `{
				"summary": "Revised.",
				"ambiguities": [],
				"examples": [
					{"prior_id": "check_01", "scenario": "S1", "input_summary": "I1", "expected_outcome": "O1", "verification_kind": "deterministic"},
					{"prior_id": "check_01", "scenario": "S2", "input_summary": "I2", "expected_outcome": "O2", "verification_kind": "deterministic"}
				],
				"removed_example_ids": [],
				"preserved_behaviors": []
			}`,
			wantErr: `duplicate prior_id "check_01" at examples[1]`,
		},
		{
			name: "unknown removed_example_ids",
			reviseJSON: `{
				"summary": "Revised.",
				"ambiguities": [],
				"examples": [
					{"prior_id": "check_01", "scenario": "S1", "input_summary": "I1", "expected_outcome": "O1", "verification_kind": "deterministic"}
				],
				"removed_example_ids": ["unknown_retired"],
				"preserved_behaviors": []
			}`,
			wantErr: `removed_example_ids[0] references unknown prior example ID "unknown_retired"`,
		},
		{
			name: "duplicate removed_example_ids",
			reviseJSON: `{
				"summary": "Revised.",
				"ambiguities": [],
				"examples": [
					{"scenario": "S", "input_summary": "I", "expected_outcome": "O", "verification_kind": "deterministic"}
				],
				"removed_example_ids": ["check_01", "check_01"],
				"preserved_behaviors": []
			}`,
			wantErr: `duplicate ID "check_01" in removed_example_ids`,
		},
		{
			name: "ID in both prior_id and removed_example_ids",
			reviseJSON: `{
				"summary": "Revised.",
				"ambiguities": [],
				"examples": [
					{"prior_id": "check_01", "scenario": "S1", "input_summary": "I1", "expected_outcome": "O1", "verification_kind": "deterministic"}
				],
				"removed_example_ids": ["check_01"],
				"preserved_behaviors": []
			}`,
			wantErr: `prior example ID "check_01" appears in both examples[0].prior_id and removed_example_ids`,
		},
		{
			name: "blank removed_example_ids entry",
			reviseJSON: `{
				"summary": "Revised.",
				"ambiguities": [],
				"examples": [
					{"prior_id": "check_01", "scenario": "S1", "input_summary": "I1", "expected_outcome": "O1", "verification_kind": "deterministic"}
				],
				"removed_example_ids": ["   "],
				"preserved_behaviors": []
			}`,
			wantErr: `removed_example_ids[0] is empty`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proposalJSON := `{
				"summary": "Feature.",
				"ambiguities": [],
				"examples": [
					{"scenario": "First", "input_summary": "In1", "expected_outcome": "Out1", "verification_kind": "deterministic"}
				],
				"preserved_behaviors": []
			}`
			cp, _ := newReviseTestControlPlane(t, proposalJSON, tc.reviseJSON)

			card1, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{
				ChangeID: "chg-test",
				Prompt:   "Feature.",
			})
			require.NoError(t, err)

			_, err = cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
				ChangeID:        "chg-test",
				RequestRevision: card1.RequestRevision,
				Prompt:          "Revised feature.",
			})
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.wantErr)
			se, ok := asAnalysisRejected(err)
			require.True(t, ok, "must return analysis_rejected ServiceError")
			assert.False(t, se.Fault)

			saved, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-test"})
			require.NoError(t, err)
			assert.Equal(t, card1.RequestRevision, saved.RequestRevision, "prior card must remain untouched")
		})
	}
}

// TestReviseChangeLegacySavedCardWithHyphenatedIDs verifies that a legacy on-disk card
// with hyphenated IDs (e.g. feat-1-check-01) loads cleanly under DisallowUnknownFields,
// preserves the legacy ID on surviving examples, and allocates check_01 for new examples.
func TestReviseChangeLegacySavedCardWithHyphenatedIDs(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	revisionJSON := `{
		"summary": "Revised legacy feature.",
		"ambiguities": [],
		"examples": [
			{"prior_id": "feat-1-check-01", "scenario": "Kept legacy check", "input_summary": "Legacy In", "expected_outcome": "Legacy Out", "verification_kind": "deterministic"},
			{"scenario": "New check", "input_summary": "New In", "expected_outcome": "New Out", "verification_kind": "deterministic"}
		],
		"removed_example_ids": [],
		"preserved_behaviors": []
	}`
	cp, cardsDir := newReviseTestControlPlane(t, revisionJSON)

	// Construct and save a legacy card file with feat-1-check-01 on disk.
	legacyCard := &gensurla.ChangeCardResult{
		ChangeID:           "feat-legacy",
		Status:             StatusConfirmed,
		Prompt:             "Original legacy feature.",
		Summary:            "Original legacy summary.",
		Ambiguities:        []*gensurla.AmbiguityQuestion{},
		Answers:            map[string]string{},
		PreservedBehaviors: []string{"Preserved behavior."},
		Examples: []*gensurla.ExampleItem{
			{
				ID:               "feat-1-check-01",
				Scenario:         "Legacy check",
				InputSummary:     "Legacy In",
				ExpectedOutcome:  "Legacy Out",
				VerificationKind: "deterministic",
			},
		},
	}
	rev, err := computeCanonicalRevision(legacyCard, "alice@example.com", "")
	require.NoError(t, err)
	legacyCard.RequestRevision = rev

	// Save legacy card directly via cardStore.
	err = cp.cards.save(storedChangeCard{
		card:         legacyCard,
		owner:        "alice@example.com",
		designSource: "",
	})
	require.NoError(t, err)

	// Verify it loads cleanly from disk under DisallowUnknownFields.
	loaded, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "feat-legacy"})
	require.NoError(t, err)
	assert.Equal(t, rev, loaded.RequestRevision)
	require.Len(t, loaded.Examples, 1)
	assert.Equal(t, "feat-1-check-01", loaded.Examples[0].ID)

	// Revise the legacy card.
	revised, err := cp.ReviseChange(aliceCtx, &gensurla.ReviseChangePayload{
		ChangeID:        "feat-legacy",
		RequestRevision: loaded.RequestRevision,
		Prompt:          "Add another check to legacy feature.",
	})
	require.NoError(t, err)
	require.Len(t, revised.Examples, 2)
	assert.Equal(t, "feat-1-check-01", revised.Examples[0].ID, "surviving legacy example preserves its exact ID")
	assert.Equal(t, "check_01", revised.Examples[1].ID, "brand-new example gets lowest unused check_NN (check_01)")

	// Verify the newly revised card persists and loads cleanly from disk.
	reloaded, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "feat-legacy"})
	require.NoError(t, err)
	assert.Equal(t, revised.RequestRevision, reloaded.RequestRevision)
	assert.Equal(t, "feat-1-check-01", reloaded.Examples[0].ID)
	assert.Equal(t, "check_01", reloaded.Examples[1].ID)
	_ = cardsDir
}
