package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitNextStepsGiveAWorkingFirstRunPath verifies what surla init prints
// after it creates a project: verify keeps a named work directory, surla serve
// uses -project . directly without a manual verification.json heredoc step,
// and the text after the serve command says how to connect a coding agent
// while it runs.
func TestInitNextStepsGiveAWorkingFirstRunPath(t *testing.T) {
	out := fmt.Sprintf(initNextSteps, shellQuote("/tmp/my shop"), "bakery")

	assert.Contains(t, out, `WORK_DIR="$(mktemp -d)/work"`)
	assert.Contains(t, out, `-work-dir "$WORK_DIR"`)
	assert.Contains(t, out, "$WORK_DIR/evidence/")
	assert.NotContains(t, out, "cat > .surla/verification.json", "first run uses -project . instead of writing verification.json by hand")
	assert.Contains(t, out, "surla serve -principal you@example.com -model-provider anthropic -model-name \"$MODEL_NAME\" \\\n  -project .\n")
	assert.Contains(t, out, "SURLA_AUTH_TOKEN")

	_, handoff, ok := strings.Cut(out, "  -project .\n")
	require.True(t, ok)
	assert.Contains(t, handoff, "Leave surla serve running.", "the agent connects while the server keeps running")
	assert.Contains(t, handoff, "export\nthe same SURLA_AUTH_TOKEN there")
	assert.Contains(t, handoff, "http://127.0.0.1:8090/mcp")
	assert.Contains(t, handoff, "docs/REFERENCE.md#register-surla-in-your-mcp-client")
}
