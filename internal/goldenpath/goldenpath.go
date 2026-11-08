// Package goldenpath embeds and serves the public Golden Path architecture
// playbook (`GOLDEN_PATH.md`) for building multi-service agentic platforms on
// the Goa Design suite (goa, goa-ai, pulse, clue, model, and chorus).
//
// Surla serves the embedded playbook at the `surla://golden-path` MCP
// resource and injects it into Checkpoint 1 (`CP1`) analysis so the architect
// agent decomposes requirements along clean trust, transport, inference, and
// state-ownership boundaries.
package goldenpath

import (
	_ "embed"
)

const (
	// ResourceURI is the canonical MCP resource URI serving the Golden Path playbook.
	ResourceURI = "surla://golden-path"

	// cp1InstructionHeader precedes the embedded Golden Path Markdown in the
	// CP1 architect system prompt.
	cp1InstructionHeader = "Analyze the operator feature request against the Goa design source. Return structured JSON matching the cp1_change_card_analysis schema.\n\n" +
		cp1CardContentRules +
		"Apply the following Golden Path architectural playbook when identifying ambiguities, examples, and preserved behaviors:\n\n"

	// cp1ReviseInstructionHeader precedes the embedded Golden Path Markdown in
	// the CP1 architect revision system prompt.
	cp1ReviseInstructionHeader = "Revise the existing Checkpoint 1 Change Card against the updated operator feature request and Goa design source. Return structured JSON matching the cp1_revise_change_card_analysis schema.\n\n" +
		"Apply these revision rules strictly:\n" +
		"1. Confirmed decisions (`confirmed_answer` non-empty) that are not contradicted by the `Revised Request` or `Design` remain settled requirements: do not re-ask them in `ambiguities`, and materialize them into the resulting `summary`, `examples`, and `preserved_behaviors`.\n" +
		"2. Explicit instructions in the `Revised Request` or `Design` supersede affected prior decisions, examples, and `preserved_behaviors`. When a revised instruction is unambiguous, do not emit an ambiguity for it; when it introduces a genuine new domain question, include that question in `ambiguities`.\n" +
		"3. Unanswered prior questions (`confirmed_answer` empty) must remain in `ambiguities` unless the `Revised Request` or `Design` explicitly resolves them or removes the scope they asked about.\n" +
		"4. Account for every prior example from the prior card: to keep or edit a prior example, include it in `examples` with `prior_id` set to its prior `id`; to remove or supersede a prior example, list its `id` in `removed_example_ids`; for brand-new examples, omit `prior_id`. Every prior example `id` must appear in either `examples[].prior_id` or `removed_example_ids`, never in both, and zero prior IDs may be omitted.\n" +
		"5. Successive revisions (`R2 -> R3`): the prior card's `summary`, `examples`, and `preserved_behaviors` are authoritative settled requirements even when they came from an earlier revision's confirmed decisions that are no longer listed in `decisions`. Preserve all of them unless the `Revised Request` or `Design` explicitly changes them.\n\n" +
		cp1CardContentRules +
		"Apply the following Golden Path architectural playbook when identifying ambiguities, examples, and preserved behaviors:\n\n"

	// cp1CardContentRules gives the CP1 model, in both the proposal and the
	// revision prompt, the rules that span fields: who answers the questions
	// and where already-settled facts go. What each field means, including
	// what a concrete example is, is in cp1_change_card_analysis's
	// descriptions.
	cp1CardContentRules = "Write the card for the requester, who answers its questions and confirms its examples:\n" +
		"- `ambiguities`: ask only about product, domain, or operational choices the requester must make that change observable behavior or service boundaries. Do not ask about details the request or the design already settle; reflect them in `summary`, or in `examples` when they change an observable outcome. In a revision, the revision rules above decide which earlier questions stay open.\n\n"

	// cp1CompositionHeader introduces the decision component catalog in the
	// CP1 architect system prompt.
	cp1CompositionHeader = "\n\nOptionally, set ui_composition to an OpenUI Lang program built only from the components below that helps the operator decide the open questions. Refer to an ambiguity or example only by its 1-based position in the ambiguities or examples list you return (1 is the first one you list); never invent identifiers. Omit ui_composition when a plain card is clearer.\n\n"
)

// playbookMarkdown holds the embedded contents of GOLDEN_PATH.md.
//
//go:embed GOLDEN_PATH.md
var playbookMarkdown string

// PlaybookMarkdown returns the embedded Golden Path architecture playbook
// Markdown served by the `surla://golden-path` MCP resource.
func PlaybookMarkdown() string {
	return playbookMarkdown
}

// decisionCatalog holds the embedded contents of decision_catalog.md: the
// prompt describing the OpenUI decision components the checkpoint viewer can
// render. The checkpoint UI build writes it from the same component
// definitions the viewer renders with; it is not edited by hand.
//
//go:embed decision_catalog.md
var decisionCatalog string

// ArchitectSystemPrompt returns the CP1 system prompt combining the structured
// output instruction, the embedded Golden Path architectural playbook, and the
// decision component catalog for the optional ui_composition.
func ArchitectSystemPrompt() string {
	return cp1InstructionHeader + playbookMarkdown + cp1CompositionHeader + decisionCatalog
}

// ArchitectReviseSystemPrompt returns the CP1 revision system prompt combining
// the revision rules, the embedded Golden Path architectural playbook, and the
// decision component catalog for the optional ui_composition.
func ArchitectReviseSystemPrompt() string {
	return cp1ReviseInstructionHeader + playbookMarkdown + cp1CompositionHeader + decisionCatalog
}
