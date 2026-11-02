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
	cp1InstructionHeader = "Analyze the operator feature request against the Goa design source. Return structured JSON matching the cp1_change_card_analysis schema.\n\nApply the following Golden Path architectural playbook when identifying ambiguities, examples, and preserved behaviors:\n\n"

	// cp1ReviseInstructionHeader precedes the embedded Golden Path Markdown in
	// the CP1 architect revision system prompt.
	cp1ReviseInstructionHeader = "Revise the existing Checkpoint 1 Change Card against the updated operator feature request and Goa design source. Return structured JSON matching the cp1_change_card_analysis schema.\n\n" +
		"Apply these revision rules strictly:\n" +
		"1. Confirmed decisions (`confirmed_answer` non-empty) that are not contradicted by the `Revised Request` or `Design` remain settled requirements: do not re-ask them in `ambiguities`, and materialize them into the resulting `summary`, `examples`, and `preserved_behaviors`.\n" +
		"2. Explicit instructions in the `Revised Request` or `Design` supersede affected prior decisions, examples, and `preserved_behaviors`. When a revised instruction is unambiguous, do not emit an ambiguity for it; when it introduces a genuine new domain question, include that question in `ambiguities`.\n" +
		"3. Unanswered prior questions (`confirmed_answer` empty) must remain in `ambiguities` unless the `Revised Request` or `Design` explicitly resolves them or removes the scope they asked about.\n" +
		"4. Surviving prior `examples` (including operator-confirmed edits) whose behavior is not changed by the `Revised Request` or `Design` must be carried forward in `examples` alongside new or updated examples for the revised scope.\n" +
		"5. Successive revisions (`R2 -> R3`): the prior card's `summary`, `examples`, and `preserved_behaviors` are authoritative settled requirements even when they came from an earlier revision's confirmed decisions that are no longer listed in `decisions`. Preserve all of them unless the `Revised Request` or `Design` explicitly changes them.\n\n" +
		"Apply the following Golden Path architectural playbook when identifying ambiguities, examples, and preserved behaviors:\n\n"

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
