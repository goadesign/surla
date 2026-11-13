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
// LiveProgramEnv for each test source, from a server environment holding a
// variable of its own, both providers' keys and the live copy's settings.
func TestProgramEnvGivesWrittenTestsOnlyTheirRunsNames(t *testing.T) {
	server := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/Users/operator",
		"TMPDIR=/var/tmp/server",
		"SURLA_TEST_SERVER_VAR=present",
		"ANTHROPIC_API_KEY=ant-k",
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
				"PATH=/usr/bin:/bin", "ANTHROPIC_API_KEY=ant-k", "TMPDIR=/runs/live/tmp", suite.LiveKeyEnv + "=live-key",
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

// TestTestProgramSeesOnlyItsSourcesEnvironment runs a program through
// runSuite with the environment Run gives it, and checks what the program
// itself sees: the operator's evaluator sees a variable of the server's own
// and a provider key, and written tests see neither, but do see their run's
// temporary folder.
func TestTestProgramSeesOnlyItsSourcesEnvironment(t *testing.T) {
	t.Setenv("SURLA_TEST_SERVER_VAR", "present")
	t.Setenv("ANTHROPIC_API_KEY", "ant-k")
	cases := []struct {
		name       string
		tests      TestsSource
		sees       []string
		doesNotSee []string
	}{
		{name: "operator's evaluator", tests: TestsEvaluator, sees: []string{"SURLA_TEST_SERVER_VAR=present", "ANTHROPIC_API_KEY=ant-k"}},
		{name: "written tests", tests: TestsWritten, doesNotSee: []string{"SURLA_TEST_SERVER_VAR", "ANTHROPIC_API_KEY"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tmp := filepath.Join(dir, "tmp")
			envFile := filepath.Join(dir, "env.txt")
			program := filepath.Join(dir, "program")
			// The program writes its environment to the file it is given and
			// reports no scenarios.
			require.NoError(t, os.WriteFile(program, []byte("#!/bin/sh\nenv > \"$1\"\necho '{}'\n"), 0o700))
			r := &runner{evidenceDir: dir}

			_, _, err := r.runSuite(t.Context(), ProgramEnv(Plan{Tests: tc.tests}, os.Environ(), tmp), program, []string{envFile})
			require.NoError(t, err)
			raw, err := os.ReadFile(envFile)
			require.NoError(t, err)
			seen := strings.Split(strings.TrimSpace(string(raw)), "\n")
			for _, want := range tc.sees {
				assert.Contains(t, seen, want)
			}
			for _, name := range tc.doesNotSee {
				for _, kv := range seen {
					assert.False(t, strings.HasPrefix(kv, name+"="), "the program sees %s", kv)
				}
			}
			if tc.tests == TestsWritten {
				assert.Contains(t, seen, "TMPDIR="+tmp)
				assert.Contains(t, seen, "PATH="+os.Getenv("PATH"))
			}
		})
	}
}
