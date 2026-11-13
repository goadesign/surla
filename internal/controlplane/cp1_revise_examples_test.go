// This file tests Checkpoint 1 (CP1) example ID allocation and revision accounting
// under Decision 1A (revise_change prior-example accounting) and Decision 2A (check_01
// default example IDs).

package controlplane

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withProposalAnalysis returns p with its analysis decoded from analysisJSON,
// the wire form a coding agent sends, by the generated propose_change decoder,
// which applies the same structural validation as the MCP and HTTP transports.
func withProposalAnalysis(t *testing.T, p *gensurla.ProposeChangePayload, analysisJSON string) *gensurla.ProposeChangePayload {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"change_id": p.ChangeID, "prompt": p.Prompt, "analysis": json.RawMessage(analysisJSON)})
	require.NoError(t, err)
	decoded, err := gensurla.DecodeProposeChangePayload(raw)
	require.NoError(t, err)
	p.Analysis = decoded.Analysis
	return p
}

// withRevisionAnalysis returns p with its analysis decoded from analysisJSON
// by the generated revise_change decoder, as withProposalAnalysis does.
func withRevisionAnalysis(t *testing.T, p *gensurla.ReviseChangePayload, analysisJSON string) *gensurla.ReviseChangePayload {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"change_id": p.ChangeID, "request_revision": p.RequestRevision, "prompt": p.Prompt, "analysis": json.RawMessage(analysisJSON),
	})
	require.NoError(t, err)
	decoded, err := gensurla.DecodeReviseChangePayload(raw)
	require.NoError(t, err)
	p.Analysis = decoded.Analysis
	return p
}

// TestProposeChangeAssignsDefaultExampleIDs_Decision2A verifies that propose_change
// assigns check_01, check_02, ... IDs matching the goa-ai/eval validateID regex
// even when change_id contains hyphens and digits.
func TestProposeChangeAssignsDefaultExampleIDs_Decision2A(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
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
	cp := newControlPlane(t, Options{})

	card, err := cp.ProposeChange(aliceCtx, withProposalAnalysis(t, &gensurla.ProposeChangePayload{
		ChangeID: "feat-refunds-1",
		Prompt:   "Instant refunds under $50.",
	}, proposalJSON))
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
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
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
	cp := newControlPlane(t, Options{})

	card1, err := cp.ProposeChange(aliceCtx, withProposalAnalysis(t, &gensurla.ProposeChangePayload{
		ChangeID: "search-feature",
		Prompt:   "Search products.",
	}, proposalJSON))
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
	revised, err := cp.ReviseChange(aliceCtx, withRevisionAnalysis(t, &gensurla.ReviseChangePayload{
		ChangeID:        "search-feature",
		RequestRevision: confirmed.RequestRevision,
		Prompt:          "Search products with fast filtering.",
	}, revisionJSON))
	require.NoError(t, err)
	require.Len(t, revised.Examples, 2)
	assert.Equal(t, "check_01", revised.Examples[0].ID)
	assert.Equal(t, "ex-search", revised.Examples[1].ID)
}

// TestReviseChangeAllocatesLowestUnusedIDAbsentFromBothTables verifies that brand-new
// examples in revise_change receive the lowest check_%02d absent from both input and output tables.
func TestReviseChangeAllocatesLowestUnusedIDAbsentFromBothTables(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})

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
		cp := newControlPlane(t, Options{})

		card1, err := cp.ProposeChange(aliceCtx, withProposalAnalysis(t, &gensurla.ProposeChangePayload{
			ChangeID: "chg-gap",
			Prompt:   "Feature.",
		}, proposalJSON))
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

		revised, err := cp.ReviseChange(aliceCtx, withRevisionAnalysis(t, &gensurla.ReviseChangePayload{
			ChangeID:        "chg-gap",
			RequestRevision: card1Conf.RequestRevision,
			Prompt:          "Revised feature.",
		}, revisionJSON))
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
		cp := newControlPlane(t, Options{})

		card1, err := cp.ProposeChange(aliceCtx, withProposalAnalysis(t, &gensurla.ProposeChangePayload{
			ChangeID: "chg-retired",
			Prompt:   "Feature.",
		}, proposalJSON))
		require.NoError(t, err)
		require.Len(t, card1.Examples, 2)
		assert.Equal(t, "check_01", card1.Examples[0].ID)
		assert.Equal(t, "check_02", card1.Examples[1].ID)

		revised, err := cp.ReviseChange(aliceCtx, withRevisionAnalysis(t, &gensurla.ReviseChangePayload{
			ChangeID:        "chg-retired",
			RequestRevision: card1.RequestRevision,
			Prompt:          "Revised feature.",
		}, revisionJSON))
		require.NoError(t, err)
		require.Len(t, revised.Examples, 2)
		assert.Equal(t, "check_02", revised.Examples[0].ID)
		assert.Equal(t, "check_03", revised.Examples[1].ID, "new example must allocate check_03, never reusing retired check_01")
	})
}

// TestReviseChangeRejectsOmittedPriorExampleID_Decision1A verifies that revise_change
// rejects any prior example omitted from both examples[*].prior_id and removed_example_ids
// as unprocessable, leaving the saved card untouched.
func TestReviseChangeRejectsOmittedPriorExampleID_Decision1A(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
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
	cp := newControlPlane(t, Options{})

	card1, err := cp.ProposeChange(aliceCtx, withProposalAnalysis(t, &gensurla.ProposeChangePayload{
		ChangeID: "chg-omission",
		Prompt:   "Feature.",
	}, proposalJSON))
	require.NoError(t, err)

	_, err = cp.ReviseChange(aliceCtx, withRevisionAnalysis(t, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-omission",
		RequestRevision: card1.RequestRevision,
		Prompt:          "Revised feature.",
	}, omissionJSON))
	assertServiceError(t, err, "unprocessable")
	assert.ErrorContains(t, err, `prior example ID "check_02" is omitted from both examples and removed_example_ids`)

	// Verify saved card remains untouched.
	saved, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-omission"})
	require.NoError(t, err)
	assert.Equal(t, card1.RequestRevision, saved.RequestRevision, "saved card revision must remain untouched")
}

// TestReviseChangeRejectsInvalidPriorAccounting_Decision1A verifies that revise_change
// rejects unknown prior_id, duplicate prior_id, unknown removed_example_ids, duplicate
// removed_example_ids, ID in both lists, or a whitespace-only removed entry as unprocessable.
func TestReviseChangeRejectsInvalidPriorAccounting_Decision1A(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})

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
			wantErr: `removed_example_ids[0] references unknown prior example ID "   "`,
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
			cp := newControlPlane(t, Options{})

			card1, err := cp.ProposeChange(aliceCtx, withProposalAnalysis(t, &gensurla.ProposeChangePayload{
				ChangeID: "chg-test",
				Prompt:   "Feature.",
			}, proposalJSON))
			require.NoError(t, err)

			_, err = cp.ReviseChange(aliceCtx, withRevisionAnalysis(t, &gensurla.ReviseChangePayload{
				ChangeID:        "chg-test",
				RequestRevision: card1.RequestRevision,
				Prompt:          "Revised feature.",
			}, tc.reviseJSON))
			assertServiceError(t, err, "unprocessable")
			assert.ErrorContains(t, err, tc.wantErr)

			saved, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg-test"})
			require.NoError(t, err)
			assert.Equal(t, card1.RequestRevision, saved.RequestRevision, "prior card must remain untouched")
		})
	}
}

// TestReviseChangeWhitespaceManualIDRemovalAndCollision verifies that:
//  1. A confirmed manual example ID with surrounding whitespace (" custom ") can be
//     explicitly removed via removed_example_ids: [" custom "] without being rejected
//     by premature whitespace trimming.
//  2. Keeping a whitespace-padded manual alias of a code-assigned ID (" check_01 ")
//     reserves both exact and trimmed forms during ID allocation so a brand-new example
//     receives "check_02" and the resulting table is accepted unchanged by confirm_intent.
//  3. Removing " check_01 " reserves "check_01" while constructing that revision (R3),
//     and a later revision (R4) after " check_01 " is no longer in the prior table can
//     allocate "check_01" again.
func TestReviseChangeWhitespaceManualIDRemovalAndCollision(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	proposeR1JSON := `{
		"summary": "Initial feature.",
		"ambiguities": [],
		"examples": [
			{"scenario": "First check", "input_summary": "In 1", "expected_outcome": "Out 1", "verification_kind": "deterministic"},
			{"scenario": "Second check", "input_summary": "In 2", "expected_outcome": "Out 2", "verification_kind": "deterministic"}
		],
		"preserved_behaviors": []
	}`
	reviseR2JSON := `{
		"summary": "Revised feature keeping padded check_01, removing padded custom, and adding a new row.",
		"ambiguities": [],
		"examples": [
			{"prior_id": " check_01 ", "scenario": "First check kept", "input_summary": "In 1", "expected_outcome": "Out 1", "verification_kind": "deterministic"},
			{"scenario": "Brand-new row", "input_summary": "In 3", "expected_outcome": "Out 3", "verification_kind": "deterministic"}
		],
		"removed_example_ids": [" custom "],
		"preserved_behaviors": []
	}`
	reviseR3JSON := `{
		"summary": "Third revision removing padded check_01 and adding a new row.",
		"ambiguities": [],
		"examples": [
			{"prior_id": "check_02", "scenario": "Second row kept", "input_summary": "In 3", "expected_outcome": "Out 3", "verification_kind": "deterministic"},
			{"scenario": "Row added when padded check_01 is removed", "input_summary": "In 4", "expected_outcome": "Out 4", "verification_kind": "deterministic"}
		],
		"removed_example_ids": [" check_01 "],
		"preserved_behaviors": []
	}`
	reviseR4JSON := `{
		"summary": "Fourth revision adding a row after padded check_01 was already removed in R3.",
		"ambiguities": [],
		"examples": [
			{"prior_id": "check_02", "scenario": "Second row kept", "input_summary": "In 3", "expected_outcome": "Out 3", "verification_kind": "deterministic"},
			{"prior_id": "check_03", "scenario": "Third row kept", "input_summary": "In 4", "expected_outcome": "Out 4", "verification_kind": "deterministic"},
			{"scenario": "Row reusing freed check_01", "input_summary": "In 5", "expected_outcome": "Out 5", "verification_kind": "deterministic"}
		],
		"removed_example_ids": [],
		"preserved_behaviors": []
	}`

	cp := newControlPlane(t, Options{})

	r1, err := cp.ProposeChange(aliceCtx, withProposalAnalysis(t, &gensurla.ProposeChangePayload{
		ChangeID: "chg-ws-ids",
		Prompt:   "Initial feature.",
	}, proposeR1JSON))
	require.NoError(t, err)

	// Confirm R1 with whitespace-padded manual IDs " check_01 " and " custom ".
	confirmedR1, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID:        "chg-ws-ids",
		RequestRevision: r1.RequestRevision,
		Answers:         map[string]string{},
		Examples: []*gensurla.ExampleItem{
			{
				ID:               " check_01 ",
				Scenario:         "First check",
				InputSummary:     "In 1",
				ExpectedOutcome:  "Out 1",
				VerificationKind: "deterministic",
			},
			{
				ID:               " custom ",
				Scenario:         "Second check",
				InputSummary:     "In 2",
				ExpectedOutcome:  "Out 2",
				VerificationKind: "deterministic",
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, confirmedR1.Examples, 2)
	assert.Equal(t, " check_01 ", confirmedR1.Examples[0].ID)
	assert.Equal(t, " custom ", confirmedR1.Examples[1].ID)

	// Revise to R2: removes " custom ", keeps " check_01 ", and adds a brand-new row.
	r2, err := cp.ReviseChange(aliceCtx, withRevisionAnalysis(t, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-ws-ids",
		RequestRevision: confirmedR1.RequestRevision,
		Prompt:          "Remove custom and add a new row.",
	}, reviseR2JSON))
	require.NoError(t, err)
	require.Len(t, r2.Examples, 2)
	assert.Equal(t, " check_01 ", r2.Examples[0].ID, "kept example preserves exact whitespace-padded ID")
	assert.Equal(t, "check_02", r2.Examples[1].ID, "allocator reserves trimmed 'check_01' so new row gets 'check_02'")

	// Confirm R2 with the returned table unchanged: must succeed without a trimmed-ID duplicate error.
	confirmedR2, err := cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
		ChangeID:        "chg-ws-ids",
		RequestRevision: r2.RequestRevision,
		Answers:         map[string]string{},
		Examples:        r2.Examples,
	})
	require.NoError(t, err)
	assert.Equal(t, StatusConfirmed, confirmedR2.Status)

	// Revise to R3: removes " check_01 " and adds a new row; "check_01" is still reserved during R3 construction.
	r3, err := cp.ReviseChange(aliceCtx, withRevisionAnalysis(t, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-ws-ids",
		RequestRevision: confirmedR2.RequestRevision,
		Prompt:          "Remove padded check_01 and add a row.",
	}, reviseR3JSON))
	require.NoError(t, err)
	require.Len(t, r3.Examples, 2)
	assert.Equal(t, "check_02", r3.Examples[0].ID)
	assert.Equal(t, "check_03", r3.Examples[1].ID, "removed ' check_01 ' is reserved during R3 construction")

	// Revise to R4: " check_01 " is no longer in the immediately prior table (R3), so "check_01" is reusable.
	r4, err := cp.ReviseChange(aliceCtx, withRevisionAnalysis(t, &gensurla.ReviseChangePayload{
		ChangeID:        "chg-ws-ids",
		RequestRevision: r3.RequestRevision,
		Prompt:          "Add another row in R4.",
	}, reviseR4JSON))
	require.NoError(t, err)
	require.Len(t, r4.Examples, 3)
	assert.Equal(t, "check_02", r4.Examples[0].ID)
	assert.Equal(t, "check_03", r4.Examples[1].ID)
	assert.Equal(t, "check_01", r4.Examples[2].ID, "check_01 is reusable once absent from the immediately prior table")
}
