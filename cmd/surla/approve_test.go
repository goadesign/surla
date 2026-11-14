package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/eval"

	"github.com/goadesign/surla/internal/verify"
)

// TestRunApproveChecksTheSavedReportAgainstTheGivenChange verifies that
// `surla approve` prints a summary only when the saved report passed and
// matches every flag, and otherwise prints nothing and fails.
func TestRunApproveChecksTheSavedReportAgainstTheGivenChange(t *testing.T) {
	digest := strings.Repeat("d", 64)
	binding := verify.Binding{
		CandidateCommit: "1111111111111111111111111111111111111111", RequestRevision: "rev-1",
		SuiteID: "orders_acceptance", SuiteCommit: "2222222222222222222222222222222222222222",
		Environment: "sim", Artifacts: map[string]string{"orders": digest},
	}
	passing := saveReport(t, binding, true)
	failing := saveReport(t, binding, false)
	flags := func(report string) []string {
		return []string{
			"-report", report,
			"-candidate-commit", binding.CandidateCommit,
			"-request-revision", binding.RequestRevision,
			"-suite-id", binding.SuiteID,
			"-suite-commit", binding.SuiteCommit,
			"-environment", binding.Environment,
			"-artifact", "orders=" + digest,
		}
	}

	t.Run("matching passed report", func(t *testing.T) {
		var out bytes.Buffer
		require.NoError(t, runApprove(flags(passing.path), &out))
		var got approveSummary
		require.NoError(t, json.Unmarshal(out.Bytes(), &got))
		assert.Equal(t, approveSummary{ReportDigest: passing.digest, Binding: binding,
			Counts: verify.Counts{Required: 2, Passed: 2}}, got)
	})

	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no flags names every required flag", nil,
			"approve requires -report, -candidate-commit, -request-revision, -suite-id, -suite-commit, -environment, -artifact"},
		{"missing artifact", without(flags(passing.path), "-artifact"), "approve requires -artifact"},
		{"artifact is not a sha256", append(flags(passing.path), "-artifact", "worker=abc"), "must be name=<64 lowercase hex sha256>"},
		{"repeated artifact", append(flags(passing.path), "-artifact", "orders="+digest), `artifact name "orders" is repeated`},
		{"different request revision", append(flags(passing.path), "-request-revision", "rev-2"), "changed request revision"},
		{"different suite ID", append(flags(passing.path), "-suite-id", "other_acceptance"), "changed evaluator suite"},
		{"different suite commit", append(flags(passing.path), "-suite-commit", "3333333333333333333333333333333333333333"), "changed evaluator suite"},
		{"different environment", append(flags(passing.path), "-environment", "staging"), "changed environment"},
		{"different measured binary", append(without(flags(passing.path), "-artifact"), "-artifact", "orders="+strings.Repeat("e", 64)), "changed candidate binaries"},
		{"report did not pass", flags(failing.path), "report did not pass"},
		{"missing report file", flags(filepath.Join(t.TempDir(), "missing.json")), "open report"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			require.ErrorContains(t, runApprove(c.args, &out), c.wantErr)
			assert.Empty(t, out.String())
		})
	}
}

// savedReport is a report written to disk the way surla verify writes it.
type savedReport struct {
	// path is the report file.
	path string
	// digest is the report's digest.
	digest string
}

// saveReport assembles a two-scenario report (one holdout) bound to binding,
// whose holdout passed when passed is true, and writes it as indented JSON.
func saveReport(t *testing.T, binding verify.Binding, passed bool) savedReport {
	t.Helper()
	var artifacts []eval.Artifact
	for name, digest := range binding.Artifacts {
		artifacts = append(artifacts, eval.Artifact{Name: "candidate:" + name, URI: "sha256:" + digest})
	}
	ran := func(id string, ok bool) eval.ScenarioReport {
		return eval.ScenarioReport{ID: id, StartedAt: time.Now(), Passed: ok,
			Result: &eval.Result{Checks: []eval.Check{{Name: "check", Passed: ok}}, Artifacts: artifacts}}
	}
	suite := verify.Suite{ID: binding.SuiteID, Environment: binding.Environment,
		Scenarios: []verify.Scenario{{ID: "a"}, {ID: "h", Holdout: true}}}
	r, err := verify.Assemble(binding, suite, eval.Report{SuiteID: binding.SuiteID,
		Scenarios: []eval.ScenarioReport{ran("a", true), ran("h", passed)}}, 0)
	require.NoError(t, err)
	b, err := json.MarshalIndent(r, "", "  ")
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), r.Digest+".json")
	require.NoError(t, os.WriteFile(path, b, 0o600))
	return savedReport{path: path, digest: r.Digest}
}
