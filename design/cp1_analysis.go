// This file declares the Checkpoint 1 analysis a caller sends with
// propose_change and revise_change: the summary, open questions, examples and
// preserved behaviors that Surla checks, numbers and saves as a Change Card.
package design

import . "goa.design/goa/v3/dsl"

// nonBlank is the pattern every required analysis text matches: it contains at
// least one character that is not whitespace. Surla saves the text exactly as
// sent, surrounding whitespace included.
//
// Whitespace is exactly what Go's unicode.IsSpace reports, the characters
// strings.TrimSpace removes. The class writes them out instead of using \s,
// whose meaning differs between Go's regexp, which the generated validators
// use, and ECMA-262, which JSON Schema consumers of the MCP tool schemas and
// the OpenAPI document use; written out, both accept and reject the same
// strings. Zero-width characters such as U+200B are not whitespace.
const nonBlank = "[^\\t\\n\\v\\f\\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]"

// CP1AnalysisAmbiguity is one open question the requester must answer.
var CP1AnalysisAmbiguity = Type("CP1AnalysisAmbiguity", func() {
	Description("One product, domain, or operational choice only the requester can make. Surla gives it the ID <change_id>-ambiguity-NN, numbered in list order.")
	Attribute("question", String, "The decision in plain language, saying what the choice changes.", func() {
		Pattern(nonBlank)
	})
	Attribute("options", ArrayOfRequired(String, func() {
		Pattern(nonBlank)
	}), "At least two distinct, self-contained choices; each states the behavior it selects and what follows from it. The requester's answer must be exactly one of these strings.", func() {
		MinLength(2)
	})
	Required("question", "options")
})

// CP1AnalysisExample is one acceptance example of a proposal, before Surla
// assigns its ID.
var CP1AnalysisExample = Type("CP1AnalysisExample", func() {
	Description("One concrete acceptance example that becomes a check the change must pass. Surla gives it the ID check_01, check_02, ... in list order.")
	Attribute("scenario", String, "Short name of the situation, in the requester's domain terms.", func() {
		Pattern(nonBlank)
	})
	Attribute("input_summary", String, "The actual request, inputs, and starting state in this scenario, with real values from the request and the design, never placeholders.", func() {
		Pattern(nonBlank)
	})
	Attribute("expected_outcome", String, "The observable result or refusal that must hold.", func() {
		Pattern(nonBlank)
	})
	Attribute("verification_kind", String, "deterministic when code can check the outcome directly; model_judged when checking it needs judgment.", func() {
		Enum("deterministic", "model_judged")
	})
	Required("scenario", "input_summary", "expected_outcome", "verification_kind")
})

// CP1RevisionAnalysisExample is one acceptance example of a revision, kept from
// the current card through prior_id or new.
var CP1RevisionAnalysisExample = Type("CP1RevisionAnalysisExample", func() {
	Description("One concrete acceptance example of the revised card: a current example kept or edited in place (prior_id set), or a new one.")
	Attribute("prior_id", String, "ID of the current card's example this one keeps or edits, exactly as the card shows it; the example keeps that ID. Omit it for a new example, which gets the lowest check_NN ID the current and revised cards do not use.", func() {
		MinLength(1)
	})
	Attribute("scenario", String, "Short name of the situation, in the requester's domain terms.", func() {
		Pattern(nonBlank)
	})
	Attribute("input_summary", String, "The actual request, inputs, and starting state in this scenario, with real values from the request and the design, never placeholders.", func() {
		Pattern(nonBlank)
	})
	Attribute("expected_outcome", String, "The observable result or refusal that must hold.", func() {
		Pattern(nonBlank)
	})
	Attribute("verification_kind", String, "deterministic when code can check the outcome directly; model_judged when checking it needs judgment.", func() {
		Enum("deterministic", "model_judged")
	})
	Required("scenario", "input_summary", "expected_outcome", "verification_kind")
})

// CP1Analysis is the caller's analysis of a new feature request.
var CP1Analysis = Type("CP1Analysis", func() {
	Description("Your analysis of the feature request against the target's current Goa design (its design/*.go files) and the Golden Path playbook served at surla://golden-path. Surla saves it as the Change Card after checking its structure and cross-field rules; it cannot judge whether the analysis is complete or correct.")
	Attribute("summary", String, "Plain-English description of the requested behavior, including the details the request or the design already settle.", func() {
		Pattern(nonBlank)
	})
	Attribute("ambiguities", ArrayOfRequired(CP1AnalysisAmbiguity), "Open questions only the requester can answer: product, domain, or operational choices that change observable behavior or service boundaries. Do not ask about details the request or the design already settle; state them in summary, or in examples when they change an observable outcome. Send an empty list when nothing is open.")
	Attribute("examples", ArrayOfRequired(CP1AnalysisExample), "Concrete acceptance examples drawn from the request and the design, never placeholder text; each becomes a check the change must pass.", func() {
		MinLength(1)
	})
	Attribute("preserved_behaviors", ArrayOfRequired(String, func() {
		Pattern(nonBlank)
	}), "Existing behaviors and contract rules of the target that the change must keep, each stated concretely; empty when none apply.")
	Attribute("ui_composition", String, "Optional OpenUI Lang program that helps the requester decide the open questions, built only from the decision components the surla://golden-path resource lists. Refer to an ambiguity or example only by its 1-based position in this analysis (1 is the first); never invent identifiers. Surla saves it unchanged without checking it; the views page validates and renders it. Omit it when a plain card is clearer.")
	Required("summary", "ambiguities", "examples", "preserved_behaviors")
})

// CP1RevisionAnalysis is the caller's analysis of a revised feature request
// against the current Change Card.
var CP1RevisionAnalysis = Type("CP1RevisionAnalysis", func() {
	Description("Your analysis of the revised request against the current Change Card (as get_change returns it at request_revision), the target's current Goa design, and the Golden Path playbook served at surla://golden-path. A confirmed answer stays settled unless the revised request or design contradicts it: do not ask it again, and restate it in summary, examples or preserved_behaviors, because the revised card drops every answer. An explicit instruction in the revised request or design replaces the decisions, examples and preserved behaviors it affects. An unanswered question stays in ambiguities unless the revised request or design answers it or removes its scope. Everything else on the current card stays unless the revised request or design changes it. Surla checks the structure and the example accounting; it cannot judge whether the analysis is complete or correct.")
	Attribute("summary", String, "Plain-English description of the revised behavior, including the details the request, the design or confirmed answers already settle.", func() {
		Pattern(nonBlank)
	})
	Attribute("ambiguities", ArrayOfRequired(CP1AnalysisAmbiguity), "Open questions only the requester can answer: product, domain, or operational choices that change observable behavior or service boundaries. Do not ask about details the request, the design or a confirmed answer already settle; state them in summary, or in examples when they change an observable outcome. Send an empty list when nothing is open.")
	Attribute("examples", ArrayOfRequired(CP1RevisionAnalysisExample), "Concrete acceptance examples drawn from the revised request and the design. Give prior_id for each current example you keep or edit; omit it for new ones.", func() {
		MinLength(1)
	})
	Attribute("removed_example_ids", ArrayOfRequired(String, func() {
		MinLength(1)
	}), "IDs of the current card's examples this revision drops. Every current example ID appears exactly once, either as an examples[].prior_id or here; send an empty list when none are dropped.")
	Attribute("preserved_behaviors", ArrayOfRequired(String, func() {
		Pattern(nonBlank)
	}), "Existing behaviors and contract rules of the target that the change must keep, each stated concretely; empty when none apply.")
	Attribute("ui_composition", String, "Optional OpenUI Lang program that helps the requester decide the open questions, built only from the decision components the surla://golden-path resource lists. Refer to an ambiguity or example only by its 1-based position in this analysis (1 is the first); never invent identifiers. Surla saves it unchanged without checking it; the views page validates and renders it. Omit it when a plain card is clearer.")
	Required("summary", "ambiguities", "examples", "removed_example_ids", "preserved_behaviors")
})
