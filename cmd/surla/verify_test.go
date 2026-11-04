package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/verify"
)

func TestParseVerifyFlagsBuildsTheExplicitPlan(t *testing.T) {
	complete := []string{
		"-candidate-repo", "/repos/candidate",
		"-candidate-commit", "1111111111111111111111111111111111111111",
		"-candidate-module", "examples/orders",
		"-binary", "orders=./cmd/orders",
		"-binary", "orders-worker=./cmd/orders-worker",
		"-suite-repo", "/repos/suite",
		"-suite-commit", "2222222222222222222222222222222222222222",
		"-suite-module", "examples/orders-eval",
		"-suite-command", "./cmd/orders_acceptance-evals",
		"-request-revision", "rev-1",
		"-work-dir", "/private/work",
		"-report-dir", "/private/reports",
	}
	wantPlan := verify.Plan{
		CandidateRepo:   "/repos/candidate",
		CandidateCommit: "1111111111111111111111111111111111111111",
		CandidateModule: "examples/orders",
		Binaries:        map[string]string{"orders": "./cmd/orders", "orders-worker": "./cmd/orders-worker"},
		SuiteRepo:       "/repos/suite",
		SuiteCommit:     "2222222222222222222222222222222222222222",
		SuiteModule:     "examples/orders-eval",
		SuiteCommand:    "./cmd/orders_acceptance-evals",
		RequestRevision: "rev-1",
		WorkDir:         "/private/work",
		ReportDir:       "/private/reports",
	}

	cases := []struct {
		name            string
		args            []string
		wantBuilderView bool
		wantErr         string
	}{
		{name: "complete plan", args: complete},
		{name: "builder view", args: append([]string{"-builder-view"}, complete...), wantBuilderView: true},
		{
			name:    "no flags names every required flag",
			args:    nil,
			wantErr: "verify requires -candidate-repo, -candidate-commit, -suite-repo, -suite-commit, -suite-command, -request-revision, -work-dir, -report-dir, -binary",
		},
		{name: "missing request revision", args: without(complete, "-request-revision"), wantErr: "verify requires -request-revision"},
		{name: "missing binaries", args: without(complete, "-binary"), wantErr: "verify requires -binary"},
		{name: "repeated binary name", args: append(complete, "-binary", "orders=./cmd/other"), wantErr: `binary name "orders" is repeated`},
		{name: "binary without package", args: append(complete, "-binary", "tool="), wantErr: `binary "tool=" must be name=package`},
		{name: "binary without name", args: append(complete, "-binary", "=./cmd/tool"), wantErr: `binary "=./cmd/tool" must be name=package`},
		{name: "binary without separator", args: append(complete, "-binary", "tool"), wantErr: `binary "tool" must be name=package`},
		{name: "positional argument", args: append(complete, "extra"), wantErr: `unexpected arguments ["extra"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, timeout, builderView, err := parseVerifyFlags(tc.args)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, wantPlan, plan)
			assert.Zero(t, timeout, "surla verify imposes no time limit unless -timeout is given")
			assert.Equal(t, tc.wantBuilderView, builderView)
		})
	}
	t.Run("module directories default to the repository root", func(t *testing.T) {
		plan, _, _, err := parseVerifyFlags(without(without(complete, "-candidate-module"), "-suite-module"))
		require.NoError(t, err)
		want := wantPlan
		want.CandidateModule, want.SuiteModule = ".", "."
		assert.Equal(t, want, plan)
	})
}

func TestWriteVerifyReportPrintsBeforeFailing(t *testing.T) {
	report := verify.Report{
		Digest: "abc123",
		Counts: verify.Counts{Required: 2, Passed: 1, Failed: 1},
		Scenarios: []verify.ScenarioResult{
			{ID: "refund_within_limit", Outcome: verify.OutcomePassed},
			{ID: "cross_customer_refund_refused", Holdout: true, Outcome: verify.OutcomeFailed},
		},
	}
	cases := []struct {
		name        string
		passed      bool
		builderView bool
		wantIDs     []string
		wantErr     string
	}{
		{name: "passed full report", passed: true, wantIDs: []string{"refund_within_limit", "cross_customer_refund_refused"}},
		{name: "failed full report", wantIDs: []string{"refund_within_limit", "cross_customer_refund_refused"}, wantErr: "verification did not pass (report abc123)"},
		{name: "failed builder view hides holdout names", builderView: true, wantIDs: []string{"refund_within_limit", "holdout-1"}, wantErr: "verification did not pass (report abc123)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := report
			r.Passed = tc.passed
			var out bytes.Buffer
			err := writeVerifyReport(&out, r, tc.builderView)
			if tc.wantErr != "" {
				require.EqualError(t, err, tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			var printed struct {
				Scenarios []struct {
					ID string `json:"id"`
				} `json:"scenarios"`
			}
			require.NoError(t, json.Unmarshal(out.Bytes(), &printed))
			ids := make([]string, 0, len(printed.Scenarios))
			for _, s := range printed.Scenarios {
				ids = append(ids, s.ID)
			}
			assert.Equal(t, tc.wantIDs, ids)
			if tc.builderView {
				assert.NotContains(t, out.String(), "cross_customer_refund_refused")
			}
		})
	}
}

// TestWriteVerifySummaryKeepsJSONOnStdoutAndHoldoutsHidden verifies the one
// human-readable line surla verify writes to stderr: it gives the verdict,
// counts and non-passing scenarios with each ID quoted, so an ID containing a
// line break keeps it on one line; the report on stdout stays decodable JSON
// on its own, and the builder view never prints a holdout's name.
func TestWriteVerifySummaryKeepsJSONOnStdoutAndHoldoutsHidden(t *testing.T) {
	report := verify.Report{
		Digest: "abc123",
		Counts: verify.Counts{Required: 3, Passed: 1, Failed: 1, NotRun: 1},
		Scenarios: []verify.ScenarioResult{
			{ID: "refund_within_limit", Outcome: verify.OutcomePassed},
			{ID: "cross_customer_refund_refused", Holdout: true, Outcome: verify.OutcomeFailed},
			{ID: "slow\nrefund", Outcome: verify.OutcomeNotRun},
		},
	}
	cases := []struct {
		name        string
		suiteExit   int
		builderView bool
		want        string
	}{
		{
			name: "full report names every non-passing scenario",
			want: "surla: verification report did not pass: 1/3 scenarios passed; failed: 1, not run: 1, harness errors: 0; " +
				`not passing: "cross_customer_refund_refused", "slow\nrefund" (report abc123)` + "\n",
		},
		{
			name: "builder view names holdouts only by number", builderView: true,
			want: "surla: verification report did not pass: 1/3 scenarios passed; failed: 1, not run: 1, harness errors: 0; " +
				`not passing: "holdout-1", "slow\nrefund" (report abc123)` + "\n",
		},
		{
			name: "a failed suite run is said", suiteExit: 2, builderView: true,
			want: "surla: verification report did not pass: 1/3 scenarios passed; failed: 1, not run: 1, harness errors: 0; " +
				`the suite run itself failed; not passing: "holdout-1", "slow\nrefund" (report abc123)` + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := report
			r.SuiteExit = tc.suiteExit
			var stdout, stderr bytes.Buffer
			// runVerify writes the JSON first and the summary after it.
			require.Error(t, writeVerifyReport(&stdout, r, tc.builderView), "every case is a report that did not pass")
			writeVerifySummary(&stderr, r, tc.builderView)
			assert.Equal(t, tc.want, stderr.String())
			assert.Equal(t, 1, strings.Count(stderr.String(), "\n"), "an ID with a line break stays on the summary's one line")
			var printed map[string]any
			require.NoError(t, json.Unmarshal(stdout.Bytes(), &printed), "stdout holds only the JSON report")
			if tc.builderView {
				assert.NotContains(t, stderr.String(), "cross_customer_refund_refused")
			}
		})
	}

	// A passing report can also be the partial report of a run stopped after
	// its suite passed; the line describes the report, and runVerify's
	// interruption error follows it.
	t.Run("a passing report has no not-passing list", func(t *testing.T) {
		passing := verify.Report{
			Digest: "def456", Passed: true,
			Counts:    verify.Counts{Required: 1, Passed: 1},
			Scenarios: []verify.ScenarioResult{{ID: "refund_within_limit", Outcome: verify.OutcomePassed}},
		}
		var stderr bytes.Buffer
		writeVerifySummary(&stderr, passing, false)
		assert.Equal(t, "surla: verification report passed: 1/1 scenarios passed; failed: 0, not run: 0, harness errors: 0 (report def456)\n", stderr.String())
	})
}

func TestRunPrintsHelpAndDispatchesCommands(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantUsage bool
		wantErr   string
	}{
		{name: "no arguments", args: nil, wantUsage: true},
		{name: "help", args: []string{"help"}, wantUsage: true},
		{name: "-h", args: []string{"-h"}, wantUsage: true},
		{name: "--help", args: []string{"--help"}, wantUsage: true},
		{name: "help help", args: []string{"help", "help"}, wantUsage: true},
		{name: "help -h", args: []string{"help", "-h"}, wantUsage: true},
		{name: "help for a command", args: []string{"help", "verify"}},
		{name: "command -h", args: []string{"preview", "-h"}},
		{name: "command --help", args: []string{"approve", "--help"}},
		{name: "help for an unknown command", args: []string{"help", "deploy"}, wantErr: `unknown command "deploy"`},
		{name: "help with two commands", args: []string{"help", "verify", "approve"}, wantErr: "unexpected arguments"},
		{name: "unknown command", args: []string{"unknown"}, wantErr: `unknown command "unknown"; run "surla help"`},
		{name: "verify without flags", args: []string{"verify"}, wantErr: "verify requires -candidate-repo"},
		{name: "approve without flags", args: []string{"approve"}, wantErr: "approve requires -report"},
		{name: "init without flags", args: []string{"init"}, wantErr: "init requires -dir and -module"},
		{name: "help for init", args: []string{"help", "init"}},
		{name: "init with a mutable version", args: []string{"init", "-dir", "x", "-module", "example.com/x", "-surla-version", "latest"}, wantErr: "not an exact module version"},
		{name: "serve without credentials", args: []string{"serve"}, wantErr: "serve requires -principal and -auth-token"},
		{name: "serve with a stray argument before a flag", args: []string{"serve", "-principal", "op", "x", "-promotion-config", "p.json"}, wantErr: `unexpected arguments ["x" "-promotion-config" "p.json"]`},
		{name: "diff with a stray argument", args: []string{"diff", "-old-dir", "a", "-new-dir", "b", "c"}, wantErr: `unexpected arguments ["c"]`},
		{name: "preview with a bad flag", args: []string{"preview", "-nope"}, wantErr: "flag provided but not defined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SURLA_AUTH_TOKEN", "")
			var out bytes.Buffer
			err := run(tc.args, &out)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.wantUsage {
				assert.Equal(t, usage, out.String())
			} else {
				assert.Empty(t, out.String())
			}
		})
	}
}

func TestShellQuoteMakesOneShellWord(t *testing.T) {
	for in, want := range map[string]string{
		"./shop":      `'./shop'`,
		"my dir":      `'my dir'`,
		"it's":        `'it'\''s'`,
		"$(rm -rf /)": `'$(rm -rf /)'`,
	} {
		assert.Equal(t, want, shellQuote(in), in)
	}
}

func TestResolveAuthTokenPrefersTheFlag(t *testing.T) {
	cases := []struct {
		name, flag, env, want string
	}{
		{name: "flag only", flag: "from-flag", want: "from-flag"},
		{name: "environment only", env: "from-env", want: "from-env"},
		{name: "flag beats environment", flag: "from-flag", env: "from-env", want: "from-flag"},
		{name: "whitespace trimmed", env: "  from-env\n", want: "from-env"},
		{name: "neither", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(name string) string {
				require.Equal(t, "SURLA_AUTH_TOKEN", name)
				return tc.env
			}
			assert.Equal(t, tc.want, resolveAuthToken(tc.flag, getenv))
		})
	}
}

func TestServeHelpNeverPrintsTheEnvironmentToken(t *testing.T) {
	const token = "synthetic-token-7f3a"
	t.Setenv("SURLA_AUTH_TOKEN", token)
	for _, args := range [][]string{{"serve", "-h"}, {"help", "serve"}} {
		stderr := captureStderr(t, func() {
			var stdout bytes.Buffer
			require.NoError(t, run(args, &stdout))
			assert.NotContains(t, stdout.String(), token)
		})
		assert.Contains(t, stderr, "-auth-token")
		assert.NotContains(t, stderr, token, "help for %v", args)
	}
	// The environment token still satisfies serve's credential check, so
	// serve gets past it and fails later, on the invalid listen address.
	err := run([]string{"serve", "-addr", "256.0.0.1:0", "-principal", "op", "-cards-dir", t.TempDir()}, &bytes.Buffer{})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "requires -principal and -auth-token")
}

// captureStderr runs fn with os.Stderr redirected to a pipe and returns what
// fn wrote. The pipe is read only after fn returns, so fn must write less than
// the pipe buffer (about 16 KiB); help text is about 1 KiB. Tests using it
// must not run in parallel because os.Stderr is process-wide.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = r.Close()
		_ = w.Close()
	})
	saved := os.Stderr
	os.Stderr = w
	func() {
		defer func() { os.Stderr = saved }()
		fn()
	}()
	require.NoError(t, w.Close())
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(out)
}

// without returns a copy of args with every occurrence of the flag named name
// and its value removed.
func without(args []string, name string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}
