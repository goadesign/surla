package verify

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrepareNamesTheBuildLogOnlyInThePrivateError verifies that a failed
// candidate or suite build returns a build-stage error whose private Err
// carries the end of the compiler's output and names the build log Prepare
// kept under WorkDir/evidence, while the Summary that builders see stays free
// of paths and compiler output. It runs real sandboxed builds, so it needs
// macOS.
func TestPrepareNamesTheBuildLogOnlyInThePrivateError(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	if runtime.GOOS != "darwin" {
		t.Skip("the build sandbox is implemented with sandbox-exec on macOS")
	}
	const (
		goodMain = "package main\n\nfunc main() {}\n"
		badMain  = "package main\n\nfunc main() { undefinedName() }\n"
	)
	cases := []struct {
		name        string
		appMain     string
		suiteMain   string
		wantSummary string
		wantLog     string
	}{
		{
			name:        "candidate build fails",
			appMain:     badMain,
			suiteMain:   goodMain,
			wantSummary: summaryCandidateBuild,
			wantLog:     "build-app.log",
		},
		{
			name:        "suite build fails",
			appMain:     goodMain,
			suiteMain:   badMain,
			wantSummary: summarySuiteBuild,
			wantLog:     "build-suite.log",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			git(t, repo, "init", "--quiet")
			files := map[string]string{
				"go.mod":            "module example.com/app\n\ngo 1.24\n",
				"cmd/app/main.go":   tc.appMain,
				"cmd/suite/main.go": tc.suiteMain,
			}
			for name, content := range files {
				path := filepath.Join(repo, filepath.FromSlash(name))
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			}
			git(t, repo, "add", ".")
			git(t, repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "--quiet", "-m", "c")
			head := strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
			workDir := filepath.Join(t.TempDir(), "work")

			_, err := Prepare(t.Context(), Plan{
				CandidateRepo:   repo,
				CandidateCommit: head,
				CandidateModule: ".",
				Binaries:        map[string]string{"app": "./cmd/app"},
				SuiteRepo:       repo,
				SuiteCommit:     head,
				SuiteModule:     ".",
				SuiteCommand:    "./cmd/suite",
				WorkDir:         workDir,
				Tests:           TestsEvaluator,
			})
			var stage *StageError
			require.True(t, errors.As(err, &stage), "error %v is not a *StageError", err)
			assert.Equal(t, StageBuild, stage.Stage)
			assert.Equal(t, tc.wantSummary, stage.Summary)
			logPath := filepath.Join(workDir, "evidence", tc.wantLog)
			assert.Contains(t, stage.Err.Error(), "build output, full output in "+logPath+":")
			assert.Contains(t, stage.Err.Error(), "undefined: undefinedName", "the compiler's diagnostic is in the private error")
			assert.FileExists(t, logPath)
			assert.NotContains(t, stage.Summary, "evidence")
			assert.NotContains(t, stage.Summary, "undefinedName")
		})
	}
}
