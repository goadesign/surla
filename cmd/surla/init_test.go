package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitNextStepsGiveAWorkingFirstRunPath verifies what surla init prints
// after it creates a project: verify keeps a named work directory and passes
// -builder-view, surla serve uses -project . directly for both Anthropic and
// OpenAI without a manual verification.json heredoc step, and the text after
// the serve commands says how to connect a coding agent and read
// surla://golden-path while it runs.
func TestInitNextStepsGiveAWorkingFirstRunPath(t *testing.T) {
	out := fmt.Sprintf(initNextSteps, shellQuote("/tmp/my shop"), "bakery")

	assert.Contains(t, out, `WORK_DIR="$(mktemp -d)/work"`)
	assert.Contains(t, out, `-work-dir "$WORK_DIR" -report-dir reports -builder-view`)
	assert.Contains(t, out, "$WORK_DIR/evidence/")
	assert.Contains(t, out, "-builder-view hides holdout scenario names and checks on stdout and stderr.")
	assert.NotContains(t, out, "cat > .surla/verification.json", "first run uses -project . instead of writing verification.json by hand")
	assert.Contains(t, out, "surla serve -principal you@example.com -model-provider anthropic -model-name \"$MODEL_NAME\" \\\n  -project .\n")
	assert.Contains(t, out, "surla serve -principal you@example.com -model-provider openai -model-name \"$MODEL_NAME\" \\\n  -project .\n")
	assert.Contains(t, out, "SURLA_AUTH_TOKEN")

	_, handoff, ok := strings.Cut(out, "# Or with OpenAI (OPENAI_API_KEY):\nsurla serve -principal you@example.com -model-provider openai -model-name \"$MODEL_NAME\" \\\n  -project .\n")
	require.True(t, ok)
	assert.Contains(t, handoff, "Leave surla serve running.", "the agent connects while the server keeps running")
	assert.Contains(t, handoff, "export\nthe same SURLA_AUTH_TOKEN there")
	assert.Contains(t, handoff, "http://127.0.0.1:8090/mcp")
	assert.Contains(t, handoff, "surla://golden-path")
	assert.Contains(t, handoff, "http://127.0.0.1:8090/golden-path")
	assert.Contains(t, handoff, "docs/REFERENCE.md#register-surla-in-your-mcp-client")
}

// TestRunVersionPrintsModuleVersionAndRejectsExtraArguments verifies `surla
// version`, `surla version -h`, `surla help version`, and `surla help`.
func TestRunVersionPrintsModuleVersionAndRejectsExtraArguments(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, run([]string{"version"}, &out))
	want := buildVersion()
	if want == "" {
		want = "(devel)"
	}
	assert.Equal(t, want+"\n", out.String())

	out.Reset()
	require.NoError(t, run([]string{"version", "-h"}, &out))

	out.Reset()
	require.NoError(t, run([]string{"help", "version"}, &out))

	out.Reset()
	require.NoError(t, run([]string{"help"}, &out))
	assert.Contains(t, out.String(), "version         print the module version this surla binary was built from")

	err := run([]string{"version", "extra"}, &out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `unexpected arguments ["extra"]`)
}
