// This file checks the generated CP1 completion contracts at the model boundary.
package controlplane

import (
	"context"
	"strings"
	"testing"

	gencompletions "github.com/goadesign/surla/gen/surla/completions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/runtime/agent/model"
	"goa.design/goa-ai/runtime/agent/tools"
)

// cp1CompletionJSON is valid for both completion contracts after a revision adds
// its required removed_example_ids collection.
const cp1CompletionJSON = `{"summary":"Summary","ambiguities":[],"examples":[{"scenario":"Scenario","input_summary":"Input","expected_outcome":"Outcome","verification_kind":"deterministic"}],"preserved_behaviors":[]}`

// TestCP1CompletionCodecsPreserveStructuralContract verifies presence, null,
// collection, enum, unknown-field, and single-document rules without inference.
func TestCP1CompletionCodecsPreserveStructuralContract(t *testing.T) {
	for _, contract := range []struct {
		name     string
		revision bool
		decode   func([]byte) error
	}{
		{name: "proposal", decode: func(raw []byte) error {
			_, err := gencompletions.SpecCp1ChangeCardAnalysis().Codec.FromJSON(raw)
			return err
		}},
		{name: "revision", revision: true, decode: func(raw []byte) error {
			_, err := gencompletions.SpecCp1ReviseChangeCardAnalysis().Codec.FromJSON(raw)
			return err
		}},
	} {
		t.Run(contract.name, func(t *testing.T) {
			base := cp1CompletionJSON
			if contract.revision {
				base = strings.Replace(base, `"summary":`, `"removed_example_ids":[],"summary":`, 1)
			}
			for _, tc := range []struct {
				name, old, replacement string
				valid                  bool
				revisionOnly           bool
			}{
				{name: "empty_optional_collections", valid: true},
				{name: "model_judged", old: `"deterministic"`, replacement: `"model_judged"`, valid: true},
				{name: "empty_ui", old: `"summary":`, replacement: `"ui_composition":"","summary":`, valid: true},
				{name: "invalid_ui_program", old: `"summary":`, replacement: `"ui_composition":"not an OpenUI program","summary":`, valid: true},
				{name: "summary_missing", old: `"summary":"Summary",`},
				{name: "summary_empty", old: `"Summary"`, replacement: `""`},
				{name: "summary_null", old: `"Summary"`, replacement: `null`},
				{name: "ambiguities_missing", old: `"ambiguities":[],`},
				{name: "ambiguities_null", old: `"ambiguities":[]`, replacement: `"ambiguities":null`},
				{name: "question_empty", old: `"ambiguities":[]`, replacement: `"ambiguities":[{"question":"","options":["a","b"]}]`},
				{name: "question_missing", old: `"ambiguities":[]`, replacement: `"ambiguities":[{"options":["a","b"]}]`},
				{name: "options_too_short", old: `"ambiguities":[]`, replacement: `"ambiguities":[{"question":"Q","options":["a"]}]`},
				{name: "option_empty", old: `"ambiguities":[]`, replacement: `"ambiguities":[{"question":"Q","options":["a",""]}]`},
				{name: "option_null", old: `"ambiguities":[]`, replacement: `"ambiguities":[{"question":"Q","options":["a",null]}]`},
				{name: "question_choices", old: `"ambiguities":[]`, replacement: `"ambiguities":[{"question":"Q","options":["a","b"]}]`, valid: true},
				{name: "unknown_question_id", old: `"ambiguities":[]`, replacement: `"ambiguities":[{"id":"a1","question":"Q","options":["a","b"]}]`},
				{name: "null_question", old: `"ambiguities":[]`, replacement: `"ambiguities":[null]`},
				{name: "examples_empty", old: `[{"scenario":"Scenario","input_summary":"Input","expected_outcome":"Outcome","verification_kind":"deterministic"}]`, replacement: `[]`},
				{name: "examples_null", old: `[{"scenario":"Scenario","input_summary":"Input","expected_outcome":"Outcome","verification_kind":"deterministic"}]`, replacement: `null`},
				{name: "scenario_missing", old: `"scenario":"Scenario",`},
				{name: "scenario_empty", old: `"Scenario"`, replacement: `""`},
				{name: "input_empty", old: `"Input"`, replacement: `""`},
				{name: "outcome_empty", old: `"Outcome"`, replacement: `""`},
				{name: "invalid_verification_kind", old: `"deterministic"`, replacement: `"other"`},
				{name: "unknown_example_id", old: `"scenario":`, replacement: `"id":"e1","scenario":`},
				{name: "preserved_missing", old: `,"preserved_behaviors":[]`},
				{name: "preserved_null", old: `"preserved_behaviors":[]`, replacement: `"preserved_behaviors":null`},
				{name: "preserved_item_empty", old: `"preserved_behaviors":[]`, replacement: `"preserved_behaviors":[""]`},
				{name: "unknown_root_field", old: `"summary":`, replacement: `"unknown":true,"summary":`},
				{name: "ui_null", old: `"summary":`, replacement: `"ui_composition":null,"summary":`},
				{name: "ui_wrong_type", old: `"summary":`, replacement: `"ui_composition":1,"summary":`},
				{name: "prior_id", old: `"scenario":`, replacement: `"prior_id":"check_01","scenario":`, valid: true, revisionOnly: true},
				{name: "prior_id_empty", old: `"scenario":`, replacement: `"prior_id":"","scenario":`, revisionOnly: true},
				{name: "prior_id_null", old: `"scenario":`, replacement: `"prior_id":null,"scenario":`, revisionOnly: true},
				{name: "removed_missing", old: `"removed_example_ids":[],`, revisionOnly: true},
				{name: "removed_null", old: `"removed_example_ids":[]`, replacement: `"removed_example_ids":null`, revisionOnly: true},
				{name: "removed_item_empty", old: `"removed_example_ids":[]`, replacement: `"removed_example_ids":[""]`, revisionOnly: true},
			} {
				if tc.revisionOnly && !contract.revision {
					continue
				}
				t.Run(tc.name, func(t *testing.T) {
					raw := base
					if tc.old != "" {
						require.Contains(t, base, tc.old)
						raw = strings.Replace(base, tc.old, tc.replacement, 1)
					}
					if tc.valid {
						assert.NoError(t, contract.decode([]byte(raw)))
					} else {
						assert.Error(t, contract.decode([]byte(raw)))
					}
				})
			}
			for _, raw := range []string{"null", "[]", base + " {}", base + " trailing"} {
				assert.Error(t, contract.decode([]byte(raw)), raw)
			}
		})
	}
}

// TestCP1CompletionDescriptionsPreserveModelContract checks the exact authored
// descriptions exposed by both generated specs, including revision accounting.
func TestCP1CompletionDescriptionsPreserveModelContract(t *testing.T) {
	proposal := gencompletions.SpecCp1ChangeCardAnalysis()
	revision := gencompletions.SpecCp1ReviseChangeCardAnalysis()
	for _, contract := range []struct {
		name     string
		fields   []tools.FieldMetadata
		examples string
	}{
		{name: "proposal", fields: proposal.Fields, examples: "Concrete acceptance examples drawn from the request and the design, never placeholder text; each becomes a check the change must pass."},
		{name: "revision", fields: revision.Fields, examples: "Concrete acceptance examples drawn from the revised request and the design. Each surviving or edited prior example must specify prior_id; brand-new examples omit prior_id."},
	} {
		t.Run(contract.name, func(t *testing.T) {
			descriptions := make(map[string]string, len(contract.fields))
			for _, field := range contract.fields {
				descriptions[tools.FieldPathString(field.Path)] = field.Description
			}
			for path, want := range map[string]string{
				"ambiguities":                  "Open product, domain, or operational choices that only the requester can make. Leave out details the request, design, or playbook already settle.",
				"ambiguities.*.question":       "One decision the requester must make, in plain language, saying what the choice changes.",
				"ambiguities.*.options":        "Distinct, self-contained choices; each states the behavior it selects and what follows from it.",
				"examples":                     contract.examples,
				"examples.*.scenario":          "Short name of the situation, in the requester's domain terms.",
				"examples.*.input_summary":     "The actual request, inputs, and starting state in this scenario.",
				"examples.*.expected_outcome":  "The observable result or refusal that must hold.",
				"examples.*.verification_kind": "deterministic when code can check the outcome directly; model_judged when checking it needs judgment.",
			} {
				assert.Equal(t, want, descriptions[path], path)
			}
			if contract.name == "revision" {
				assert.Equal(t, "ID of the prior Change Card example being kept or edited in place, exactly as shown in the prior card; omit for brand-new examples.", descriptions["examples.*.prior_id"])
				assert.Equal(t, "IDs of prior Change Card examples being superseded or removed. Every prior example ID must appear in either examples[].prior_id or removed_example_ids, never in both, and none may be omitted.", descriptions["removed_example_ids"])
			}
		})
	}
	assert.Empty(t, proposal.Description)
	assert.Empty(t, proposal.ExampleJSON)
	assert.Empty(t, revision.Description)
	assert.Empty(t, revision.ExampleJSON)
}

// TestCP1CompletionPreservesRequestSettings checks both entrypoints leave model
// choice and thinking unspecified while retaining the CP1 output allowance.
func TestCP1CompletionPreservesRequestSettings(t *testing.T) {
	for _, revision := range []bool{false, true} {
		name := "proposal"
		raw := cp1CompletionJSON
		if revision {
			name = "revision"
			raw = strings.Replace(raw, `"summary":`, `"removed_example_ids":[],"summary":`, 1)
		}
		t.Run(name, func(t *testing.T) {
			q := &queuedModelClient{responses: []queuedModelTurn{{resp: &model.Response{
				Content:    []model.Message{{Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: raw}}}},
				StopReason: "stop",
			}}}}
			analyzer := NewModelCP1Analyzer(newQueuedModelClient(t, q))
			var err error
			if revision {
				_, err = analyzer.Revise(context.Background(), CP1PriorCard{}, "Request", SuppliedCP1Design("Design"))
			} else {
				_, err = analyzer.Analyze(context.Background(), "Request", SuppliedCP1Design("Design"))
			}
			require.NoError(t, err)
			req := q.lastRequest()
			assert.Nil(t, req.Thinking)
			assert.Empty(t, req.Model)
			assert.Empty(t, req.ModelClass)
			assert.Empty(t, req.Tools)
			assert.Nil(t, req.ToolChoice)
			assert.Equal(t, cp1AnalysisMaxOutputTokens, req.MaxTokens)
			assert.Len(t, q.requests, 1)
		})
	}
}

// TestCP1CompletionRejectsInvalidResponseEnvelopes verifies generated completion
// decoding accepts one assistant answer, including adjacent text parts, and
// rejects extra messages or tools without exposing rejected response contents.
func TestCP1CompletionRejectsInvalidResponseEnvelopes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []model.Message
		valid   bool
	}{
		{name: "one_answer", content: []model.Message{{Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: cp1CompletionJSON}}}}, valid: true},
		{name: "adjacent_text", content: []model.Message{{Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: cp1CompletionJSON[:20]}, model.TextPart{Text: cp1CompletionJSON[20:]}}}}, valid: true},
		{name: "extra_assistant_message", content: []model.Message{
			{Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: cp1CompletionJSON}}},
			{Role: model.ConversationRoleAssistant, Parts: []model.Part{model.TextPart{Text: "private-extra-answer"}}},
		}},
		{name: "non_assistant_message", content: []model.Message{{Role: model.ConversationRoleUser, Parts: []model.Part{model.TextPart{Text: cp1CompletionJSON}}}}},
		{name: "tool_call", content: []model.Message{{Role: model.ConversationRoleAssistant, Parts: []model.Part{
			model.TextPart{Text: cp1CompletionJSON}, model.ToolUsePart{ID: "private-tool-id", Name: "private-tool-name"},
		}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := &queuedModelClient{responses: []queuedModelTurn{{resp: &model.Response{
				Content: tc.content, StopReason: "stop",
			}}}}
			_, err := NewModelCP1Analyzer(newQueuedModelClient(t, q)).Analyze(context.Background(), "Request", SuppliedCP1Design("Design"))
			if tc.valid {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			_, rejected := asAnalysisRejected(err)
			assert.True(t, rejected)
			assert.NotContains(t, err.Error(), "private-")
			assert.Len(t, q.requests, 1)
		})
	}
}
