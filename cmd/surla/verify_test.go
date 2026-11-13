package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/project"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/verify"
)

// TestParseVerifyFlagsBuildsTheExplicitPlan checks required inputs and every
// supported spelling of the report-view flag without starting a run.
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
		{name: "omitted builder view defaults to true", args: complete, wantBuilderView: true},
		{name: "bare builder view", args: append([]string{"-builder-view"}, complete...), wantBuilderView: true},
		{name: "explicit true builder view", args: append([]string{"-builder-view=true"}, complete...), wantBuilderView: true},
		{name: "explicit false prints full report", args: append([]string{"-builder-view=false"}, complete...)},
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
// on its own, and the parsed default never prints a holdout's name. Explicit
// -builder-view=false preserves the full report and summary.
func TestWriteVerifySummaryKeepsJSONOnStdoutAndHoldoutsHidden(t *testing.T) {
	args := []string{
		"-candidate-repo", "/repos/candidate",
		"-candidate-commit", strings.Repeat("1", 40),
		"-binary", "orders=./cmd/orders",
		"-suite-repo", "/repos/suite",
		"-suite-commit", strings.Repeat("2", 40),
		"-suite-command", "./cmd/orders-evals",
		"-request-revision", "rev-1",
		"-work-dir", "/private/work",
		"-report-dir", "/private/reports",
	}
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
		name      string
		suiteExit int
		flags     []string
		want      string
	}{
		{
			name:  "full report names every non-passing scenario",
			flags: []string{"-builder-view=false"},
			want: "surla: verification report did not pass: 1/3 scenarios passed; failed: 1, not run: 1, harness errors: 0; " +
				`not passing: "cross_customer_refund_refused", "slow\nrefund" (report abc123)` + "\n",
		},
		{
			name: "default builder view names holdouts only by number",
			want: "surla: verification report did not pass: 1/3 scenarios passed; failed: 1, not run: 1, harness errors: 0; " +
				`not passing: "holdout-1", "slow\nrefund" (report abc123)` + "\n",
		},
		{
			name: "a failed suite run is said", suiteExit: 2,
			want: "surla: verification report did not pass: 1/3 scenarios passed; failed: 1, not run: 1, harness errors: 0; " +
				`the suite run itself failed; not passing: "holdout-1", "slow\nrefund" (report abc123)` + "\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, builderView, err := parseVerifyFlags(append(tc.flags, args...))
			require.NoError(t, err)
			r := report
			r.SuiteExit = tc.suiteExit
			var stdout, stderr bytes.Buffer
			// runVerify writes the JSON first and the summary after it.
			require.Error(t, writeVerifyReport(&stdout, r, builderView), "every case is a report that did not pass")
			writeVerifySummary(&stderr, r, builderView)
			assert.Equal(t, tc.want, stderr.String())
			assert.Equal(t, 1, strings.Count(stderr.String(), "\n"), "an ID with a line break stays on the summary's one line")
			var wantReport any = r
			if builderView {
				wantReport = r.ForBuilders()
				assert.NotContains(t, stderr.String(), "cross_customer_refund_refused")
			}
			wantJSON, err := json.Marshal(wantReport)
			require.NoError(t, err)
			assert.JSONEq(t, string(wantJSON), stdout.String(), "stdout holds only the chosen report JSON")
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
		{name: "no arguments", args: nil},
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

		{name: "diff with a stray argument", args: []string{"diff", "-old-dir", "a", "-new-dir", "b", "c"}, wantErr: `unexpected arguments ["c"]`},
		{name: "preview with a bad flag", args: []string{"preview", "-nope"}, wantErr: "flag provided but not defined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			var err error
			if tc.args == nil {
				err = runWithRuntime(tc.args, &out, projectDispatchRuntime(t))
			} else {
				err = run(tc.args, &out)
			}
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.args == nil {
				assert.Contains(t, out.String(), "Surla is running for")
			} else if tc.wantUsage {
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

// projectDispatchRuntime proves no-argument dispatch through actual management HTTP
// while replacing terminal and browser effects and every state-root resolution.
func projectDispatchRuntime(t *testing.T) projectRuntime {
	t.Helper()
	root := t.TempDir()
	runtime := testProjectRuntimeAt(root)
	bindFixtureTeamEffects(&runtime)
	runtime.preflight = func() error { return nil }
	runtime.waitTerminal = func(ctx context.Context, id string) (bool, error) { <-ctx.Done(); return false, nil }
	starts, opens := 0, 0
	runtime.startTerminal = func(ctx context.Context, state *project.State) (string, error) {
		starts++
		cp, err := controlplane.New(controlplane.Options{CardsDir: state.CardsDir()})
		if err != nil {
			return "", err
		}
		t.Cleanup(func() { assert.NoError(t, cp.Close()) })
		listener, err := net.Listen("tcp", state.Address())
		if err != nil {
			return "", err
		}
		management, err := project.NewServer(state, func() {}, fixtureTeamDependencies(state))
		require.NoError(t, err)
		require.NoError(t, management.PublishRunning(os.Getpid(), "stand-in"))
		server := &http.Server{Handler: management.Handler(controlplane.NewHTTPHandler(cp, management.Authenticate)), ReadHeaderTimeout: time.Second}
		t.Cleanup(func() { assert.NoError(t, server.Close()) })
		go func() {
			if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				assert.NoError(t, err)
			}
		}()
		return "stand-in", nil
	}
	runtime.openViews = func(ctx context.Context, link string) error {
		opens++
		assert.Contains(t, link, "/views#link=")
		return nil
	}
	t.Cleanup(func() { assert.Equal(t, 1, starts); assert.Equal(t, 1, opens) })
	return runtime
}

// TestDefaultProjectEffectsAreBlocked prevents dispatch regressions from reaching
// user config, real terminals, or the browser when a test omits its stand-ins.
func TestDefaultProjectEffectsAreBlocked(t *testing.T) {
	runtime := testProjectRuntime()
	_, err := runtime.configRoot()
	require.ErrorContains(t, err, "test-owned configuration directory")
	_, err = runtime.startTerminal(t.Context(), nil)
	require.ErrorContains(t, err, "cannot start real")
	require.ErrorContains(t, runtime.openViews(t.Context(), "unused"), "cannot open real")
	require.ErrorContains(t, runtime.startTeamPrograms(t.Context()), "cannot start real team")
	_, err = runtime.teamDependencies(t.Context())
	require.ErrorContains(t, err, "cannot resolve or change real project teams")
	// The constructor fixtures bind neither reader nor sender: opting into startup
	// stand-ins must never opt a test into terminal observations or reminder typing.
	isolateServeWorkspace(t)
	state, err := fixtureProjectState(t.TempDir())
	require.NoError(t, err)
	dependencies := fixtureTeamDependencies(state)
	assert.Nil(t, dependencies.Observer)
	assert.Nil(t, dependencies.Sender)
	require.ErrorContains(t, run(nil, io.Discard), "cannot start real")
}
