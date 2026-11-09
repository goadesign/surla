// This file declares the model-authored CP1 responses decoded by generated completions.
package design

import . "goa.design/goa/v3/dsl"

// CP1AnalysisAmbiguity contains one requester decision and its choices.
var CP1AnalysisAmbiguity = Type("CP1AnalysisAmbiguity", func() {
	Attribute("question", String, "One decision the requester must make, in plain language, saying what the choice changes.", func() {
		MinLength(1)
	})
	Attribute("options", ArrayOf(String, func() {
		MinLength(1)
	}), "Distinct, self-contained choices; each states the behavior it selects and what follows from it.", func() {
		MinLength(2)
	})
	Required("question", "options")
})

// CP1AnalysisExample contains one proposed acceptance check before code assigns its ID.
var CP1AnalysisExample = Type("CP1AnalysisExample", func() {
	Attribute("scenario", String, "Short name of the situation, in the requester's domain terms.", func() {
		MinLength(1)
	})
	Attribute("input_summary", String, "The actual request, inputs, and starting state in this scenario.", func() {
		MinLength(1)
	})
	Attribute("expected_outcome", String, "The observable result or refusal that must hold.", func() {
		MinLength(1)
	})
	Attribute("verification_kind", String, "deterministic when code can check the outcome directly; model_judged when checking it needs judgment.", func() {
		Enum("deterministic", "model_judged")
	})
	Required("scenario", "input_summary", "expected_outcome", "verification_kind")
})

// CP1RevisionAnalysisExample contains one revised check and its optional prior example ID.
var CP1RevisionAnalysisExample = Type("CP1RevisionAnalysisExample", func() {
	Attribute("prior_id", String, "ID of the prior Change Card example being kept or edited in place, exactly as shown in the prior card; omit for brand-new examples.", func() {
		MinLength(1)
	})
	Attribute("scenario", String, "Short name of the situation, in the requester's domain terms.", func() {
		MinLength(1)
	})
	Attribute("input_summary", String, "The actual request, inputs, and starting state in this scenario.", func() {
		MinLength(1)
	})
	Attribute("expected_outcome", String, "The observable result or refusal that must hold.", func() {
		MinLength(1)
	})
	Attribute("verification_kind", String, "deterministic when code can check the outcome directly; model_judged when checking it needs judgment.", func() {
		Enum("deterministic", "model_judged")
	})
	Required("scenario", "input_summary", "expected_outcome", "verification_kind")
})

// CP1Analysis defines the final response consumed by ModelCP1Analyzer.Analyze.
var CP1Analysis = Type("CP1Analysis", func() {
	Attribute("summary", String, func() {
		MinLength(1)
	})
	Attribute("ambiguities", ArrayOf(CP1AnalysisAmbiguity), "Open product, domain, or operational choices that only the requester can make. Leave out details the request, design, or playbook already settle.")
	Attribute("examples", ArrayOf(CP1AnalysisExample), "Concrete acceptance examples drawn from the request and the design, never placeholder text; each becomes a check the change must pass.", func() {
		MinLength(1)
	})
	Attribute("preserved_behaviors", ArrayOf(String, func() {
		MinLength(1)
	}))
	Attribute("ui_composition", String)
	Required("summary", "ambiguities", "examples", "preserved_behaviors")
})

// CP1RevisionAnalysis defines the final response consumed by ModelCP1Analyzer.Revise.
var CP1RevisionAnalysis = Type("CP1RevisionAnalysis", func() {
	Attribute("summary", String, func() {
		MinLength(1)
	})
	Attribute("ambiguities", ArrayOf(CP1AnalysisAmbiguity), "Open product, domain, or operational choices that only the requester can make. Leave out details the request, design, or playbook already settle.")
	Attribute("examples", ArrayOf(CP1RevisionAnalysisExample), "Concrete acceptance examples drawn from the revised request and the design. Each surviving or edited prior example must specify prior_id; brand-new examples omit prior_id.", func() {
		MinLength(1)
	})
	Attribute("removed_example_ids", ArrayOf(String, func() {
		MinLength(1)
	}), "IDs of prior Change Card examples being superseded or removed. Every prior example ID must appear in either examples[].prior_id or removed_example_ids, never in both, and none may be omitted.")
	Attribute("preserved_behaviors", ArrayOf(String, func() {
		MinLength(1)
	}))
	Attribute("ui_composition", String)
	Required("summary", "ambiguities", "examples", "removed_example_ids", "preserved_behaviors")
})
