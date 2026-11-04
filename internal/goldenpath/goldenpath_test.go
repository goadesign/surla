// Package goldenpath tests the embedded Golden Path playbook and CP1 architect prompt.
package goldenpath

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPlaybookMarkdownAndArchitectSystemPromptEmbedSingleSourceOfTruth verifies
// that PlaybookMarkdown and ArchitectSystemPrompt both serve the embedded
// GOLDEN_PATH.md file without divergence.
func TestPlaybookMarkdownAndArchitectSystemPromptEmbedSingleSourceOfTruth(t *testing.T) {
	t.Parallel()

	md := PlaybookMarkdown()
	require.NotEmpty(t, strings.TrimSpace(md))

	prompt := ArchitectSystemPrompt()
	assert.Contains(t, prompt, "cp1_change_card_analysis")
	assert.Contains(t, prompt, md, "ArchitectSystemPrompt must embed PlaybookMarkdown verbatim")
	assert.True(t, strings.HasSuffix(prompt, decisionCatalog), "ArchitectSystemPrompt must end with the embedded decision catalog verbatim")
	assert.Contains(t, prompt, "1-based position", "the composition instruction must tell the model to reference items by position, not by invented IDs")

	revisePrompt := ArchitectReviseSystemPrompt()
	assert.Contains(t, revisePrompt, "cp1_change_card_analysis")
	assert.Contains(t, revisePrompt, "Confirmed decisions (`confirmed_answer` non-empty)")
	assert.Contains(t, revisePrompt, "Unanswered prior questions (`confirmed_answer` empty)")
	assert.Contains(t, revisePrompt, "Successive revisions")
	assert.Contains(t, revisePrompt, md, "ArchitectReviseSystemPrompt must embed PlaybookMarkdown verbatim")
	assert.True(t, strings.HasSuffix(revisePrompt, decisionCatalog), "ArchitectReviseSystemPrompt must end with the embedded decision catalog verbatim")
	assert.Contains(t, revisePrompt, "1-based position")
}

// TestPlaybookRegeneratesWithSurlaGenAndKeepsTheGRPCRoute verifies that the
// playbook's executable regeneration step, which both CP1 prompts and the
// surla://golden-path resource carry, names the sandboxed surla gen, and that
// the module-pinned goa gen route remains for gRPC designs surla gen refuses.
func TestPlaybookRegeneratesWithSurlaGenAndKeepsTheGRPCRoute(t *testing.T) {
	t.Parallel()

	md := PlaybookMarkdown()
	assert.Contains(t, md, "surla gen -dir")
	assert.Contains(t, md, "gRPC")
	assert.Contains(t, md, "go run goa.design/goa/v3/cmd/goa gen")
}
