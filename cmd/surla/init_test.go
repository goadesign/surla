package main

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitNextStepsGiveAWorkingFirstRunPath verifies the CLI handoff matches
// the starter's private verification paths and documented change checkpoints.
func TestInitNextStepsGiveAWorkingFirstRunPath(t *testing.T) {
	out := fmt.Sprintf(initNextSteps, shellQuote("/tmp/my shop"), "bakery")
	assert.Contains(t, out, `cd '/tmp/my shop'`)
	assert.Contains(t, out, `VERIFY_DIR="$(mktemp -d)"`)
	assert.Contains(t, out, `WORK_DIR="$VERIFY_DIR/work"`)
	assert.Contains(t, out, `REPORT_DIR="$VERIFY_DIR/reports"`)
	assert.Contains(t, out, `-work-dir "$WORK_DIR" -report-dir "$REPORT_DIR" -builder-view`)
	assert.Contains(t, out, "$WORK_DIR/evidence/")
	assert.Contains(t, out, "-builder-view hides holdout scenario names and checks in the completed report\nJSON and summary line (if a run fails before writing a report, stderr prints\noperator setup/execution failure diagnostics).")
	assert.Contains(t, out, "check_project before committing")
	assert.Contains(t, out, "confirmed Change Card's request_revision")
	assert.Contains(t, out, "surla://golden-path")
	assert.Contains(t, out, "docs/REFERENCE.md#mcp-tools")
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
