package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitNextStepsGiveAWorkingFirstRunPath verifies what surla init prints
// after it creates a project: verify keeps a named work directory, and the
// verification configuration written after the first commit is a heredoc a
// shell ends correctly, holding a configuration that loadVerificationConfig,
// which surla serve -verification-config uses, accepts; and the text after
// the serve command says how to connect a coding agent while it runs.
func TestInitNextStepsGiveAWorkingFirstRunPath(t *testing.T) {
	out := fmt.Sprintf(initNextSteps, shellQuote("/tmp/my shop"), "bakery")

	assert.Contains(t, out, `WORK_DIR="$(mktemp -d)/work"`)
	assert.Contains(t, out, `-work-dir "$WORK_DIR"`)
	assert.Contains(t, out, "$WORK_DIR/evidence/")

	_, after, ok := strings.Cut(out, "\ncat > .surla/verification.json <<EOF\n")
	require.True(t, ok, "the heredoc starts at the beginning of a line")
	body, _, ok := strings.Cut(after, "\nEOF\n")
	require.True(t, ok, "the heredoc's EOF line starts at the beginning of a line")
	body = strings.ReplaceAll(body, "$(git rev-parse HEAD)", strings.Repeat("a", 40))
	path := filepath.Join(t.TempDir(), "verification.json")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	cfg, err := loadVerificationConfig(path)
	require.NoError(t, err, "surla serve loads the configuration init prints:\n%s", body)
	assert.Equal(t, map[string]string{"bakery": "./cmd/bakery"}, cfg.Plan.Binaries)
	assert.Equal(t, "./cmd/bakery-evals", cfg.Plan.SuiteCommand)
	assert.Equal(t, "eval", cfg.Plan.SuiteModule)
	assert.Equal(t, strings.Repeat("a", 40), cfg.Plan.SuiteCommit)
	assert.Contains(t, out, "-verification-config .surla/verification.json")
	assert.Contains(t, out, "SURLA_AUTH_TOKEN")

	_, handoff, ok := strings.Cut(out, "-verification-config .surla/verification.json\n")
	require.True(t, ok)
	assert.Contains(t, handoff, "Leave surla serve running.", "the agent connects while the server keeps running")
	assert.Contains(t, handoff, "export\nthe same SURLA_AUTH_TOKEN there")
	assert.Contains(t, handoff, "http://127.0.0.1:8090/mcp")
	assert.Contains(t, handoff, "docs/REFERENCE.md#register-surla-in-your-mcp-client")
}
