// This file checks the environment each test program starts with: the
// operator's evaluator keeps its caller's, and a program agents wrote gets
// only the names its run needs.
package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/verify/suite"
)

// TestProgramEnvGivesWrittenTestsOnlyTheirRunsNames checks ProgramEnv and
// LiveProgramEnv for each test source, from an environment holding a
// variable of the server's own, both providers' keys, a request setting and
// an unlisted family member, and the live copy's settings. A live copy of
// written tests gets the chosen provider's request settings, no unlisted
// member and no other provider's.
func TestProgramEnvGivesWrittenTestsOnlyTheirRunsNames(t *testing.T) {
	server := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/Users/operator",
		"TMPDIR=/var/tmp/server",
		"SURLA_TEST_SERVER_VAR=present",
		"ANTHROPIC_API_KEY=ant-k",
		"ANTHROPIC_BASE_URL=https://gateway.invalid",
		"ANTHROPIC_ADMIN_KEY=admin-k",
		"OPENAI_API_KEY=oai-k",
	}
	// live returns a live copy's executor environment as LaunchLive makes
	// it: the server's without its TMPDIR, then the live copy's TMPDIR and
	// key, then the model connection in extra.
	live := func(extra ...string) []string {
		var env []string
		for _, kv := range server {
			if !strings.HasPrefix(kv, "TMPDIR=") {
				env = append(env, kv)
			}
		}
		env = append(env, "TMPDIR=/runs/live/tmp", suite.LiveKeyEnv+"=live-key")
		return append(env, extra...)
	}
	evaluator := Plan{Tests: TestsEvaluator}
	written := Plan{Tests: TestsWritten}
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{
			name: "verification of the operator's evaluator",
			got:  ProgramEnv(evaluator, server, "/runs/r1/work/tmp"),
			want: server,
		},
		{
			name: "verification of written tests",
			got:  ProgramEnv(written, server, "/runs/r1/work/tmp"),
			want: []string{"PATH=/usr/bin:/bin", "TMPDIR=/runs/r1/work/tmp"},
		},
		{
			name: "live copy with the operator's evaluator",
			got:  LiveProgramEnv(evaluator, live(LiveModelProviderEnv+"=anthropic")),
			want: live(LiveModelProviderEnv + "=anthropic"),
		},
		{
			name: "live copy of written tests with a direct provider",
			got:  LiveProgramEnv(written, live(LiveModelProviderEnv+"=anthropic", LiveModelNameEnv+"=claude")),
			want: []string{
				"PATH=/usr/bin:/bin", "ANTHROPIC_API_KEY=ant-k", "ANTHROPIC_BASE_URL=https://gateway.invalid", "TMPDIR=/runs/live/tmp", suite.LiveKeyEnv + "=live-key",
				LiveModelProviderEnv + "=anthropic", LiveModelNameEnv + "=claude",
			},
		},
		{
			name: "live copy of written tests through a gateway",
			got:  LiveProgramEnv(written, live(LiveModelGatewayEnv+"=127.0.0.1:9090")),
			want: []string{"PATH=/usr/bin:/bin", "TMPDIR=/runs/live/tmp", suite.LiveKeyEnv + "=live-key", LiveModelGatewayEnv + "=127.0.0.1:9090"},
		},
		{
			name: "live copy of written tests with a provider live copies cannot use",
			got:  LiveProgramEnv(written, live(LiveModelProviderEnv+"=other")),
			want: []string{"PATH=/usr/bin:/bin", "TMPDIR=/runs/live/tmp", suite.LiveKeyEnv + "=live-key", LiveModelProviderEnv + "=other"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.got)
		})
	}
}

// TestRunGivesEachStepOnlyItsSourcesEnvironment runs the describe step and
// the suite step of Run, after Prepare, with a stand-in program that writes
// its environment to a file at each step. From a server environment holding
// a variable of its own and both providers' keys, written tests see neither,
// but see PATH and their run's temporary folder, at both steps; the
// operator's evaluator sees the server's variable and keys at both steps.
func TestRunGivesEachStepOnlyItsSourcesEnvironment(t *testing.T) {
	server := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/Users/operator",
		"SURLA_TEST_SERVER_VAR=present",
		"ANTHROPIC_API_KEY=ant-k",
		"OPENAI_API_KEY=oai-k",
	}
	cases := []struct {
		name       string
		tests      TestsSource
		sees       []string
		doesNotSee []string
	}{
		{
			name: "operator's evaluator", tests: TestsEvaluator,
			sees: []string{"SURLA_TEST_SERVER_VAR=present", "ANTHROPIC_API_KEY=ant-k", "OPENAI_API_KEY=oai-k", "HOME=/Users/operator"},
		},
		{
			name: "written tests", tests: TestsWritten,
			doesNotSee: []string{"SURLA_TEST_SERVER_VAR", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "HOME"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tmp := filepath.Join(dir, "tmp")
			require.NoError(t, os.Mkdir(tmp, 0o700))
			describeEnv := filepath.Join(dir, "describe-env.txt")
			suiteEnv := filepath.Join(dir, "suite-env.txt")
			program := filepath.Join(dir, "program")
			// The program writes its environment to a file named for its step
			// and reports a suite without scenarios, which the report step
			// then refuses; this test judges only the two steps' environment.
			script := "#!/bin/sh\n" +
				"if [ \"$1\" = \"--describe\" ]; then env > '" + describeEnv + "'; echo '{\"id\":\"s\",\"environment\":\"sim\",\"scenarios\":[]}'; exit 0; fi\n" +
				"env > '" + suiteEnv + "'\necho '{}'\n"
			require.NoError(t, os.WriteFile(program, []byte(script), 0o700))
			prep := Prepared{runner: &runner{evidenceDir: dir}, SuiteBin: program, TmpDir: tmp}

			_, err := runPrepared(t.Context(), Plan{Tests: tc.tests, ReportDir: filepath.Join(dir, "reports")}, prep, server)
			var stage *StageError
			if assert.ErrorAs(t, err, &stage) {
				assert.Equal(t, StageReport, stage.Stage, "both steps ran")
			}
			for step, path := range map[string]string{"describe": describeEnv, "suite": suiteEnv} {
				raw, err := os.ReadFile(path)
				require.NoError(t, err, "the %s step ran", step)
				seen := strings.Split(strings.TrimSpace(string(raw)), "\n")
				for _, want := range tc.sees {
					assert.Contains(t, seen, want, "the %s step", step)
				}
				for _, name := range tc.doesNotSee {
					for _, kv := range seen {
						assert.False(t, strings.HasPrefix(kv, name+"="), "the %s step sees %s", step, kv)
					}
				}
				if tc.tests == TestsWritten {
					assert.Contains(t, seen, "TMPDIR="+tmp, "the %s step", step)
					assert.Contains(t, seen, "PATH="+os.Getenv("PATH"), "the %s step", step)
				}
			}
		})
	}
}
