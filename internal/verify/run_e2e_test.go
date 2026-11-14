//go:build e2e

package verify

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunVerifiesTheOrdersReference runs the whole verifier against this
// repository's committed HEAD: it clones the candidate and the evaluator suite,
// builds the orders binaries, runs the evaluator-owned acceptance suite in the
// sandbox and assembles a bound report. It needs git, the temporal CLI and
// macOS. Run with: go test -tags e2e ./internal/verify
func TestRunVerifiesTheOrdersReference(t *testing.T) {
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	head, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	require.NoError(t, err)
	commit := strings.TrimSpace(string(head))
	work := t.TempDir()

	report, err := Run(t.Context(), Plan{
		CandidateRepo:   repo,
		CandidateCommit: commit,
		CandidateModule: "examples/orders",
		Binaries:        map[string]string{"orders": "./cmd/orders", "orders-worker": "./cmd/orders-worker"},
		SuiteRepo:       repo,
		SuiteCommit:     commit,
		SuiteModule:     "examples/orders-eval",
		SuiteCommand:    "./cmd/orders_acceptance-evals",
		RequestRevision: "rev-test",
		WorkDir:         filepath.Join(work, "work"),
		ReportDir:       filepath.Join(work, "reports"),
		Tests:           TestsEvaluator,
	}, os.Environ())
	require.NoError(t, err)
	for _, s := range report.Scenarios {
		if s.Outcome != OutcomePassed && s.Detail != nil {
			detail, _ := json.MarshalIndent(s.Detail, "", "  ")
			t.Logf("scenario %s %s:\n%s", s.ID, s.Outcome, detail)
		}
	}
	assert.True(t, report.Passed, "%+v", report.Counts)
	assert.Equal(t, Counts{Required: 5, Passed: 5}, report.Counts)
	assert.Equal(t, "orders_acceptance", report.Binding.SuiteID)
	assert.Equal(t, "sim", report.Binding.Environment, "the environment is what the suite declares it ran")
	assert.Len(t, report.Binding.Artifacts, 2)
	assert.FileExists(t, filepath.Join(work, "reports", report.Digest+".json"))
	require.NoError(t, Approve(report.Binding, report))
	view := report.ForBuilders()
	assert.Equal(t, "holdout-1", view.Scenarios[4].ID, "the holdout's name is hidden from builders")
	assert.Empty(t, view.Scenarios[4].Checks, "the holdout's checks are hidden from builders")
}
