// This file defines the Checkpoint 1 (CP1) revision semantic analysis types
// used by CP1Analyzer.Revise.

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
