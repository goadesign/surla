package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
)

// TestVerificationSettingsReadsTheOperatorsPlanStrictly exercises named-file setup and startup reload.
func TestVerificationSettingsReadsTheOperatorsPlanStrictly(t *testing.T) {
	isolateServeWorkspace(t)
	complete := `{"candidate_repo":"/srv/app.git","candidate_module":".","binaries":{"app":"./cmd/app"},"suite_repo":"/srv/eval.git","suite_commit":"` + strings.Repeat("b", 40) + `","suite_module":".","suite_command":"./cmd/app-evals","timeout":"30m"}`
	path := filepath.Join(t.TempDir(), "verification.json")
	require.NoError(t, os.WriteFile(path, []byte(complete), 0o600))
	require.NoError(t, runProjectSettings("verification", []string{"set", path}, io.Discard))
	ws, err := configureServeWorkspace()
	require.NoError(t, err)
	cfg := ws.verification
	require.NotNil(t, cfg)
	assert.Equal(t, ws.runsDir, cfg.RunsDir)
	assert.Equal(t, filepath.Join(filepath.Dir(ws.cardsDir), "reports"), cfg.Plan.ReportDir)
	assert.Equal(t, map[string]string{"app": "./cmd/app"}, cfg.Plan.Binaries)
	assert.Equal(t, "./cmd/app-evals", cfg.Plan.SuiteCommand)
	assert.Empty(t, cfg.Plan.CandidateCommit)
	assert.Empty(t, cfg.Plan.WorkDir)
	require.Len(t, cfg.Command, 1)
	assert.Equal(t, 30*time.Minute, cfg.Timeout)
	for _, tc := range []struct{ name, content, wantErr string }{
		{"unknown field", `{"runs_dir":"x","suite":"y"}`, "unknown field"},
		{"missing fields", `{}`, "are missing candidate_repo, candidate_module, suite_repo, suite_commit, suite_module, suite_command, timeout, binaries"},
		{"trailing data", complete + `{}`, "data after the configuration"},
		{"bad timeout", strings.Replace(complete, `"30m"`, `"soon"`, 1), "must be a positive duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NoError(t, os.WriteFile(path, []byte(tc.content), 0o600))
			assert.ErrorContains(t, runProjectSettings("verification", []string{"set", path}, io.Discard), tc.wantErr)
			current, err := configureServeWorkspace()
			require.NoError(t, err)
			assert.Equal(t, cfg, current.verification, "invalid replacement preserves stored plan")
		})
	}
	assert.ErrorIs(t, runProjectSettings("verification", []string{"set", filepath.Join(t.TempDir(), "missing.json")}, io.Discard), os.ErrNotExist)
}

// TestConfigureServeWorkspaceBindsTheNamedProject checks which project folder
// the project server binds, from a relative path or through a link's parent,
// and that without stored verification settings it verifies with the tests
// accepted for each change, keeping runs and reports outside the project.
// Which commits a run uses is the control plane's choice, at each call.
func TestConfigureServeWorkspaceBindsTheNamedProject(t *testing.T) {
	isolateGitConfig(t)
	// assertWrittenTestRuns checks that ws runs the tests accepted for each
	// change, with this surla executable, outside project.
	assertWrittenTestRuns := func(t *testing.T, ws workspaceServeConfig, project string) {
		t.Helper()
		assert.Nil(t, ws.verification)
		require.NotNil(t, ws.writtenTestRuns)
		require.Len(t, ws.writtenTestRuns.Command, 1, "runs re-execute this surla executable")
		assert.Equal(t, defaultProjectVerifyTimeout, ws.writtenTestRuns.Timeout)
		assertOutsideDirectory(t, ws.runsDir, project)
		assertOutsideDirectory(t, ws.writtenTestRuns.ReportDir, project)
	}

	t.Run("relative paths resolve from the working directory", func(t *testing.T) {
		repo := newGitRepo(t)
		project := filepath.Join(repo, "services", "shop")
		writeStarter(t, project, "example.com/shop")
		gitCommitAll(t, repo, "starter")
		for _, start := range []struct{ dir, arg string }{{project, "."}, {repo, "services/shop"}} {
			isolateServeWorkspace(t)
			t.Chdir(start.dir)
			ws, err := configureServeWorkspace(start.arg)
			require.NoError(t, err, "project %s from %s", start.arg, start.dir)
			assert.Equal(t, project, ws.workspaceDir)
			assertWrittenTestRuns(t, ws, project)
		}
	})

	t.Run("symlink parent paths select the linked project", func(t *testing.T) {
		repo := newGitRepo(t)
		writeStarter(t, repo, "example.com/shop")
		gitCommitAll(t, repo, "starter")
		nested := filepath.Join(repo, "nested")
		require.NoError(t, os.Mkdir(nested, 0o700))

		other := newGitRepo(t)
		writeStarter(t, other, "example.com/other")
		gitCommitAll(t, other, "other starter")
		require.NoError(t, os.Symlink(nested, filepath.Join(other, "alias")))

		// Keep the raw parent component: filepath.Join would erase the link.
		for _, arg := range []string{"alias/..", other + "/alias/.."} {
			isolateServeWorkspace(t)
			t.Chdir(other)
			ws, err := configureServeWorkspace(arg)
			require.NoError(t, err, "project %s", arg)
			assert.Equal(t, repo, ws.workspaceDir, "project %s", arg)
			assertWrittenTestRuns(t, ws, repo)
		}
	})
}

// TestStoredVerificationSupportsIndependentEvaluatorRepositories verifies project
// workspace binding and externally stored evaluator input compose on older Git.
func TestStoredVerificationSupportsIndependentEvaluatorRepositories(t *testing.T) {
	isolateServeWorkspace(t)

	root := t.TempDir()
	opsSessionDir := filepath.Join(root, "ops", "session")
	shopDir := filepath.Join(root, "work", "shop")
	require.NoError(t, os.MkdirAll(opsSessionDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(shopDir, "design"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(shopDir, "design", "design.go"), []byte("package design\n// shop workspace design\n"), 0o644))
	runTestGit(t, filepath.Join(root, "work"), "init", "-q")

	// Simulate Git <= 2.44 (rejecting --no-lazy-fetch) to verify that explicit
	// stored verification settings and CP1 flows succeed without requiring Git 2.45.
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	fakeBin := filepath.Join(root, "fake-git-244")
	require.NoError(t, os.MkdirAll(fakeBin, 0o700))
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = \"--no-lazy-fetch\" ]; then\n    printf 'unknown option: --no-lazy-fetch\\nusage: git [-v | --version] [-h | --help]\\n' >&2\n    exit 129\n  fi\ndone\nexec \"" + realGit + "\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(fakeBin, "git"), []byte(script), 0o700))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	verPath := filepath.Join(root, "ops", "shop-eval.json")
	verContent := `{"candidate_repo":"` + shopDir + `","candidate_module":".","binaries":{"shop":"./cmd/shop"},` +
		`"suite_repo":"` + filepath.Join(root, "ops", "eval-repo") + `","suite_commit":"` + strings.Repeat("d", 40) + `",` +
		`"suite_module":".","suite_command":"./cmd/shop-evals","timeout":"15m"}`
	require.NoError(t, os.WriteFile(verPath, []byte(verContent), 0o600))

	t.Chdir(shopDir)
	require.NoError(t, runProjectSettings("verification", []string{"set", verPath}, io.Discard))
	opts, err := serveOptions(
		func(string) string { return "" },
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, opts.Verification)
	assert.Nil(t, opts.WrittenTestRuns, "stored verification settings leave no runs of written tests")
	expectedSuite, err := filepath.EvalSymlinks(filepath.Join(root, "ops"))
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(expectedSuite, "eval-repo"), opts.Verification.Plan.SuiteRepo)
	assert.Equal(t, 15*time.Minute, opts.Verification.Timeout)

	cp, err := controlplane.New(opts)
	require.NoError(t, err)
	defer cp.Close()

	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: controlplane.CallerUser})
	_, err = cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID: "chg-shop-with-external-eval",
		Prompt:   "Add coupon validation",
		Analysis: &gensurla.CP1Analysis{
			Summary:     "Add coupon validation.",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1AnalysisExample{{
				Scenario:         "Apply valid coupon",
				InputSummary:     "Valid coupon code SAVE10 on a $50.00 order",
				ExpectedOutcome:  "Discounts the order total to $45.00",
				VerificationKind: "deterministic",
			}},
			PreservedBehaviors: []string{"Checkout without a coupon is unchanged."},
		},
	})
	require.NoError(t, err)
	// The saved card records the design source it was proposed against.
	sum := sha256.Sum256([]byte("chg-shop-with-external-eval"))
	raw, err := os.ReadFile(filepath.Join(opts.CardsDir, hex.EncodeToString(sum[:])+".json"))
	require.NoError(t, err)
	var saved struct {
		DesignSource string `json:"design_source"`
	}
	require.NoError(t, json.Unmarshal(raw, &saved))
	assert.Contains(t, saved.DesignSource, "shop workspace design", "project must bind shopDir for CP1 workspace design reads even when verification settings are stored")
}

// isolateGitConfig disables system/global Git configuration without changing HOME.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

// newGitRepo creates an empty Git repository in a new temporary directory and
// returns its symlink-free path.
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	runTestGit(t, dir, "init", "-q")
	return dir
}

// writeStarter writes the files the project server reads from a starter
// created by surla init with module modulePath: both go.mod files, one file in
// the candidate's command directory and one in the evaluator suite's
// generated program.
func writeStarter(t *testing.T, dir, modulePath string) {
	t.Helper()
	name := path.Base(modulePath)
	files := map[string]string{
		"go.mod":                   "module " + modulePath + "\n\ngo 1.26\n",
		"cmd/" + name + "/main.go": "package main\n",
		"eval/go.mod":              "module " + modulePath + "/eval\n\ngo 1.26\n",
		"eval/gen/cmd/" + name + "-evals/main.go": "package main\n",
	}
	for rel, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
}

// gitCommitAll commits every file in repo and returns the new HEAD commit ID.
func gitCommitAll(t *testing.T, repo, message string) string {
	t.Helper()
	runTestGit(t, repo, "add", "-A")
	runTestGit(t, repo, "commit", "-q", "-m", message)
	return strings.TrimSpace(runTestGit(t, repo, "rev-parse", "HEAD"))
}

// runTestGit runs git in dir with a fixed test identity, no inherited GIT_*
// variables and no system or global configuration, and returns its stdout.
func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Surla Test",
		"-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	cmd.Env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	require.NoError(t, err, "git %v", args)
	return string(out)
}
