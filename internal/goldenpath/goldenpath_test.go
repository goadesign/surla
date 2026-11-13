// Package goldenpath tests the embedded Golden Path playbook and the guidance
// resource that serves it.
package goldenpath

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResourceMarkdownServesThePlaybookThenTheDecisionCatalog verifies that the
// surla://golden-path resource starts with the embedded GOLDEN_PATH.md
// verbatim and ends with the generated decision catalog verbatim, so a caller
// composing an analysis's ui_composition reads the same component list the
// views page renders with.
func TestResourceMarkdownServesThePlaybookThenTheDecisionCatalog(t *testing.T) {
	t.Parallel()

	require.NotEmpty(t, strings.TrimSpace(playbookMarkdown))
	require.NotEmpty(t, strings.TrimSpace(decisionCatalog))
	md := ResourceMarkdown()
	assert.True(t, strings.HasPrefix(md, playbookMarkdown), "the resource must start with the playbook verbatim")
	assert.True(t, strings.HasSuffix(md, decisionCatalog), "the resource must end with the decision catalog verbatim")
	assert.Contains(t, md, "analysis.ui_composition", "the catalog section must name the field it serves")
	assert.Contains(t, md, "1-based position", "a composition must reference items by position, not by invented IDs")
	assert.Contains(t, md, "DecisionWorkspace(", "the component signatures must be served")
}

// TestPlaybookLeavesConfirmationToTheUser verifies that the playbook every
// role reads says the user confirms a Change Card on the views page and that
// an unlisted choice goes back to the guide's revise_change, so no agent is
// told to confirm a card itself.
func TestPlaybookLeavesConfirmationToTheUser(t *testing.T) {
	t.Parallel()

	md := ResourceMarkdown()
	assert.Contains(t, md, "The user answers each ambiguity question on the views page")
	assert.Contains(t, md, "no agent confirms a card")
	assert.Contains(t, md, "the guide calls `revise_change`")
	assert.NotContains(t, md, "and confirm the revised card")
}

// TestPlaybookRegeneratesWithSurlaGenAndKeepsTheGRPCRoute verifies that the
// playbook's executable regeneration step, which the surla://golden-path
// resource carries, names the sandboxed surla gen, and that the module-pinned
// goa gen route remains for gRPC designs surla gen refuses.
func TestPlaybookRegeneratesWithSurlaGenAndKeepsTheGRPCRoute(t *testing.T) {
	t.Parallel()

	md := ResourceMarkdown()
	assert.Contains(t, md, "surla gen -dir")
	assert.Contains(t, md, "gRPC")
	assert.Contains(t, md, "go run goa.design/goa/v3/cmd/goa gen")
}
