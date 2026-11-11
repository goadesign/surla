// Package goldenpath embeds and serves the public Golden Path architecture
// playbook (`GOLDEN_PATH.md`) for building multi-service agentic platforms on
// the Goa Design suite (goa, goa-ai, pulse, clue, model, and chorus).
//
// Surla serves the embedded playbook at the `surla://golden-path` MCP
// resource, followed by the decision component catalog a caller uses to
// compose the optional ui_composition of a Checkpoint 1 (`CP1`) analysis. A
// coding agent reads it before it analyzes a feature request for
// propose_change or revise_change.
package goldenpath

import (
	_ "embed"
)

const (
	// ResourceURI is the canonical MCP resource URI serving the Golden Path playbook.
	ResourceURI = "surla://golden-path"

	// compositionHeader introduces the decision component catalog after the
	// playbook: it says which field the catalog serves, how a program refers
	// to the analysis it belongs to, and what Surla does with it.
	compositionHeader = "\n\n## Change Card decision views\n\n" +
		"`propose_change` and `revise_change` accept an optional `analysis.ui_composition`: an OpenUI Lang program built only from the components below that helps the requester decide the card's open questions. " +
		"Refer to an ambiguity or example only by its 1-based position in the `ambiguities` or `examples` list you send (1 is the first one you list); never invent identifiers. " +
		"Surla saves the program unchanged without checking it, and the checkpoint viewer validates and renders it. Omit `ui_composition` when a plain card is clearer.\n\n"
)

var (
	// playbookMarkdown holds the embedded contents of GOLDEN_PATH.md.
	//
	//go:embed GOLDEN_PATH.md
	playbookMarkdown string

	// decisionCatalog holds the embedded contents of decision_catalog.md: the
	// description of the OpenUI decision components the checkpoint viewer can
	// render. The checkpoint UI build writes it from the same component
	// definitions the viewer renders with; it is not edited by hand.
	//
	//go:embed decision_catalog.md
	decisionCatalog string
)

// ResourceMarkdown returns the Markdown the `surla://golden-path` MCP resource
// and the `GET /golden-path` route serve: the Golden Path playbook, then the
// decision component catalog for an analysis's optional ui_composition.
func ResourceMarkdown() string {
	return playbookMarkdown + compositionHeader + decisionCatalog
}
