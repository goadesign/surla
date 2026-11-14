//go:build e2e

package verify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writtenTestsProgram is a test program at the generated entry point's path
// that needs no dependency: it declares one scenario, and its run reports the
// candidate binaries it was given and passes that scenario only when it sees
// neither the server's own variable nor a provider key, and has a TMPDIR.
const writtenTestsProgram = `package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type artifact struct {
	Name string ` + "`json:\"name\"`" + `
	URI  string ` + "`json:\"uri\"`" + `
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--describe" {
		fmt.Println(` + "`" + `{"id":"shop_acceptance","environment":"sim","scenarios":[{"id":"environment","tags":[]}]}` + "`" + `)
		return
	}
	var artifacts []artifact
	for i := 1; i+1 < len(os.Args); i += 2 {
		if os.Args[i] != "--artifact" {
			continue
		}
		name, path, _ := strings.Cut(os.Args[i+1], "=")
		b, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		sum := sha256.Sum256(b)
		artifacts = append(artifacts, artifact{Name: "candidate:" + name, URI: "sha256:" + hex.EncodeToString(sum[:])})
	}
	_, server := os.LookupEnv("SURLA_TEST_SERVER_VAR")
	_, key := os.LookupEnv("ANTHROPIC_API_KEY")
	passed := !server && !key && os.Getenv("TMPDIR") != ""
	now := time.Now().UTC()
	report := map[string]any{
		"suite_id": "shop_acceptance", "started_at": now, "duration": 0, "passed": passed,
		"scenarios": []any{map[string]any{
			"id": "environment", "started_at": now, "duration": 0, "passed": passed,
			"result": map[string]any{
				"artifacts": artifacts,
				"output":    fmt.Sprintf("server variable seen: %t; provider key seen: %t", server, key),
			},
		}},
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		os.Exit(2)
	}
	if !passed {
		os.Exit(1)
	}
}
`

// TestRunGivesWrittenTestsOnlyTheirRunsEnvironment runs a whole verification
// of a commit with the layout `surla init` makes, whose test program sits at
// the generated entry point (./gen/cmd/shop-evals in eval). Run checks the
// commit out, builds the candidate and the test program in Surla's sandbox,
// describes and runs the program, and assembles the report. From a server
// environment holding a variable of its own and a provider key, written
// tests see neither, so the run passes; the operator's evaluator sees both,
// so the same program fails. It needs git and macOS. Run with: go test -tags
// e2e ./internal/verify
func TestRunGivesWrittenTestsOnlyTheirRunsEnvironment(t *testing.T) {
	t.Setenv("SURLA_TEST_SERVER_VAR", "present")
	t.Setenv("ANTHROPIC_API_KEY", "ant-k")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	writeTestFiles(t, repo, map[string]string{
		"go.mod":                          "module example.com/shop\n\ngo 1.27.3\n",
		"cmd/shop/main.go":                "package main\n\nfunc main() {}\n",
		"eval/go.mod":                     "module example.com/shop/eval\n\ngo 1.27.3\n",
		"eval/gen/cmd/shop-evals/main.go": writtenTestsProgram,
	})
	git(t, repo, "init", "-q")
	git(t, repo, "add", ".")
	git(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "starter")
	head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
	cases := []struct {
		name   string
		tests  TestsSource
		passed bool
		output string
	}{
		{name: "written tests", tests: TestsWritten, passed: true, output: "server variable seen: false; provider key seen: false"},
		{name: "operator's evaluator", tests: TestsEvaluator, passed: false, output: "server variable seen: true; provider key seen: true"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			work := t.TempDir()
			report, err := Run(t.Context(), Plan{
				CandidateRepo:   repo,
				CandidateCommit: head,
				CandidateModule: ".",
				Binaries:        map[string]string{"shop": "./cmd/shop"},
				SuiteRepo:       repo,
				SuiteCommit:     head,
				SuiteModule:     "eval",
				SuiteCommand:    "./gen/cmd/shop-evals",
				RequestRevision: "rev-test",
				WorkDir:         filepath.Join(work, "work"),
				ReportDir:       filepath.Join(work, "reports"),
				Tests:           tc.tests,
			}, os.Environ())
			require.NoError(t, err)
			assert.Equal(t, tc.passed, report.Passed)
			require.Len(t, report.Scenarios, 1)
			require.NotNil(t, report.Scenarios[0].Detail)
			require.NotNil(t, report.Scenarios[0].Detail.Result)
			assert.Equal(t, tc.output, report.Scenarios[0].Detail.Result.Output)
		})
	}
}

// writeTestFiles writes files, keyed by slash-separated path, under dir.
func writeTestFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	}
}
