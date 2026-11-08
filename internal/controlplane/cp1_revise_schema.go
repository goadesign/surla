// This file defines the Checkpoint 1 (CP1) revision semantic analysis types
// and canonical JSON Schema used by CP1Analyzer.Revise.

package controlplane

type (
	// CP1PriorExample is one concrete acceptance check from the Change Card revision
	// being revised, preserving its code- or operator-assigned ID for revision accounting.
	CP1PriorExample struct {
		// ID is the unique identifier of the example on the prior card (e.g. "check_01"
		// or a caller-chosen ID like "refund-at-limit").
		ID string `json:"id"`
		// Scenario is the plain-English scenario description.
		Scenario string `json:"scenario"`
		// InputSummary describes the representative user request and fixture state.
		InputSummary string `json:"input_summary"`
		// ExpectedOutcome describes the expected side effect and response behavior.
		ExpectedOutcome string `json:"expected_outcome"`
		// VerificationKind classifies the check ("deterministic" or "model_judged").
		VerificationKind string `json:"verification_kind"`
	}

	// CP1RevisionModelExample is a concrete input/outcome example returned by a
	// CP1Analyzer.Revise model call. It includes an optional PriorID referencing
	// the prior Change Card example being preserved or edited in place.
	CP1RevisionModelExample struct {
		// PriorID is the ID of the prior example being kept or edited, or empty
		// when this example is brand-new.
		PriorID string `json:"prior_id,omitempty"`
		// Scenario is the plain-English scenario description.
		Scenario string `json:"scenario"`
		// InputSummary describes the representative user request and fixture state.
		InputSummary string `json:"input_summary"`
		// ExpectedOutcome describes the expected side effect and response behavior.
		ExpectedOutcome string `json:"expected_outcome"`
		// VerificationKind classifies the check ("deterministic" or "model_judged").
		VerificationKind string `json:"verification_kind"`
	}

	// CP1RevisionSemanticAnalysis is the structured semantic output returned by a
	// CP1Analyzer.Revise model call.
	CP1RevisionSemanticAnalysis struct {
		// Summary is the plain-English description of the requested behavior.
		Summary string `json:"summary"`
		// Ambiguities lists genuine domain questions that require human confirmation.
		Ambiguities []CP1ModelAmbiguity `json:"ambiguities"`
		// Examples lists concrete input/outcome examples for the revised Change Card.
		Examples []CP1RevisionModelExample `json:"examples"`
		// RemovedExampleIDs lists prior example IDs that are superseded or removed.
		RemovedExampleIDs []string `json:"removed_example_ids"`
		// PreservedBehaviors lists existing contract invariants preserved by the change.
		PreservedBehaviors []string `json:"preserved_behaviors"`
		// UIComposition is an optional OpenUI Lang decision view composed from
		// the Surla decision catalog. Nil means the model composed none. Any
		// string the model returns, including an empty or invalid program, is
		// carried to the card unchanged: it is presentation only, so a bad
		// program never costs the operator the authoritative analysis.
		UIComposition *string `json:"ui_composition,omitempty"`
	}
)

var (
	// cp1ReviseAnalysisJSONSchema is the canonical JSON Schema (Draft 2020-12)
	// enforced at the goa-ai model.Client boundary for CP1 semantic revisions.
	// It requires removed_example_ids and allows an optional prior_id on each
	// item in examples.
	cp1ReviseAnalysisJSONSchema = []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "additionalProperties": false,
  "required": ["summary", "ambiguities", "examples", "removed_example_ids", "preserved_behaviors"],
  "properties": {
    "summary": {
      "type": "string",
      "minLength": 1
    },
    "ambiguities": {
      "type": "array",
      "description": "Open product, domain, or operational choices that only the requester can make. Leave out details the request, design, or playbook already settle.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["question", "options"],
        "properties": {
          "question": {
            "type": "string",
            "minLength": 1,
            "description": "One decision the requester must make, in plain language, saying what the choice changes."
          },
          "options": {
            "type": "array",
            "minItems": 2,
            "description": "Distinct, self-contained choices; each states the behavior it selects and what follows from it.",
            "items": {
              "type": "string",
              "minLength": 1
            }
          }
        }
      }
    },
    "examples": {
      "type": "array",
      "minItems": 1,
      "description": "Concrete acceptance examples drawn from the revised request and the design. Each surviving or edited prior example must specify prior_id; brand-new examples omit prior_id.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["scenario", "input_summary", "expected_outcome", "verification_kind"],
        "properties": {
          "prior_id": {
            "type": "string",
            "minLength": 1,
            "description": "ID of the prior Change Card example being kept or edited in place, exactly as shown in the prior card; omit for brand-new examples."
          },
          "scenario": {
            "type": "string",
            "minLength": 1,
            "description": "Short name of the situation, in the requester's domain terms."
          },
          "input_summary": {
            "type": "string",
            "minLength": 1,
            "description": "The actual request, inputs, and starting state in this scenario."
          },
          "expected_outcome": {
            "type": "string",
            "minLength": 1,
            "description": "The observable result or refusal that must hold."
          },
          "verification_kind": {
            "type": "string",
            "enum": ["deterministic", "model_judged"],
            "description": "deterministic when code can check the outcome directly; model_judged when checking it needs judgment."
          }
        }
      }
    },
    "removed_example_ids": {
      "type": "array",
      "description": "IDs of prior Change Card examples being superseded or removed. Every prior example ID must appear in either examples[].prior_id or removed_example_ids, never in both, and none may be omitted.",
      "items": {
        "type": "string",
        "minLength": 1
      }
    },
    "preserved_behaviors": {
      "type": "array",
      "items": {
        "type": "string",
        "minLength": 1
      }
    },
    "ui_composition": {
      "type": "string"
    }
  }
}`)
)
