package main

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/verify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadVerificationConfigReadsTheOperatorsPlanStrictly(t *testing.T) {
	cfg, err := loadVerificationConfig("")
	require.NoError(t, err)
	assert.Nil(t, cfg, "no file means verification is not configured")

	complete := `{"runs_dir":"/srv/runs","report_dir":"/srv/reports","candidate_repo":"/srv/app.git",
		"candidate_module":".","binaries":{"app":"./cmd/app"},"suite_repo":"/srv/eval.git",
		"suite_commit":"` + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" + `","suite_module":".","suite_command":"./cmd/app-evals","timeout":"30m"}`
	write := func(t *testing.T, content string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "verification.json")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	cfg, err = loadVerificationConfig(write(t, complete))
	require.NoError(t, err)
	assert.Equal(t, "/srv/runs", cfg.RunsDir)
	assert.Equal(t, "/srv/reports", cfg.Plan.ReportDir)
	assert.Equal(t, map[string]string{"app": "./cmd/app"}, cfg.Plan.Binaries)
	assert.Equal(t, "./cmd/app-evals", cfg.Plan.SuiteCommand)
	assert.Empty(t, cfg.Plan.CandidateCommit, "the candidate commit comes from each verify_change call")
	assert.Empty(t, cfg.Plan.WorkDir, "each run gets its own work directory")
	require.Len(t, cfg.Command, 1)
	assert.Equal(t, 30*time.Minute, cfg.Timeout)

	for _, tc := range []struct {
		name, content, wantErr string
	}{
		{"unknown field", `{"runs_dir":"x","suite":"y"}`, "unknown field"},
		{"missing fields", `{"runs_dir":"/srv/runs"}`, "is missing report_dir, candidate_repo, candidate_module, suite_repo, suite_commit, suite_module, suite_command, timeout, binaries"},
		{"trailing data", complete + `{}`, "data after the configuration"},
		{"bad timeout", strings.Replace(complete, `"30m"`, `"soon"`, 1), "must be a positive duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadVerificationConfig(write(t, tc.content))
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
	_, err = loadVerificationConfig(filepath.Join(t.TempDir(), "missing.json"))
	assert.ErrorContains(t, err, "read -verification-config")
}

// TestLoadProjectVerificationConfigDerivesTheStarterPlanFromHEAD checks the
// configuration `surla serve -project` builds from a committed starter: the
// layout, modules and suite commit all come from one HEAD commit, whatever
// the directory is called, wherever the project sits in its repository, and
// whatever the working files or the environment say.
func TestLoadProjectVerificationConfigDerivesTheStarterPlanFromHEAD(t *testing.T) {
	cases := []struct {
		name        string
		module      string
		projectPath string // project directory relative to the repository root
		wantService string
		wantModule  string // candidate module directory relative to the repository root
	}{
		{"starter at the repository root", "example.com/shop", ".", "shop", "."},
		{"renamed directory with a space", "example.com/shop", "renamed shop", "shop", "renamed shop"},
		{"starter nested in a larger repository", "example.com/shop", "services/shop", "shop", "services/shop"},
		{"service name reserved in generated Go", "example.com/errors", ".", "errors", "."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newGitRepo(t)
			project := filepath.Join(repo, filepath.FromSlash(tc.projectPath))
			writeStarter(t, project, tc.module)
			head := gitCommitAll(t, repo, "starter")

			cfg, err := loadProjectConfig(t.Context(), project)
			require.NoError(t, err)
			suiteModule := "eval"
			if tc.wantModule != "." {
				suiteModule = tc.wantModule + "/eval"
			}
			assert.Equal(t, verify.Plan{
				CandidateRepo:   repo,
				CandidateModule: tc.wantModule,
				Binaries:        map[string]string{tc.wantService: "./cmd/" + tc.wantService},
				SuiteRepo:       repo,
				SuiteCommit:     head,
				SuiteModule:     suiteModule,
				SuiteCommand:    "./cmd/" + tc.wantService + "-evals",
				ReportDir:       filepath.Join(project, "reports"),
			}, cfg.Plan)
			assert.Equal(t, filepath.Join(project, ".surla", "runs"), cfg.RunsDir)
			assert.Equal(t, 30*time.Minute, cfg.Timeout)
			require.Len(t, cfg.Command, 1, "runs re-execute this surla executable")
		})
	}

	t.Run("relative paths resolve from the working directory", func(t *testing.T) {
		repo := newGitRepo(t)
		project := filepath.Join(repo, "services", "shop")
		writeStarter(t, project, "example.com/shop")
		gitCommitAll(t, repo, "starter")
		for _, start := range []struct{ dir, arg string }{{project, "."}, {repo, "services/shop"}} {
			t.Chdir(start.dir)
			cfg, err := resolveVerificationConfig(serveConfig{projectDir: start.arg, projectSet: true})
			require.NoError(t, err, "-project %s from %s", start.arg, start.dir)
			assert.Equal(t, repo, cfg.Plan.CandidateRepo)
			assert.Equal(t, "services/shop", cfg.Plan.CandidateModule)
			assert.Equal(t, "services/shop/eval", cfg.Plan.SuiteModule)
			assert.Equal(t, filepath.Join(project, ".surla", "runs"), cfg.RunsDir)
			assert.Equal(t, filepath.Join(project, "reports"), cfg.Plan.ReportDir)
		}
	})

	t.Run("committed layout wins over working files", func(t *testing.T) {
		repo := newGitRepo(t)
		writeStarter(t, repo, "example.com/shop")
		head := gitCommitAll(t, repo, "starter")
		require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/other\n"), 0o600))
		require.NoError(t, os.RemoveAll(filepath.Join(repo, "cmd")))

		cfg, err := loadProjectConfig(t.Context(), repo)
		require.NoError(t, err)
		assert.Equal(t, head, cfg.Plan.SuiteCommit)
		assert.Equal(t, map[string]string{"shop": "./cmd/shop"}, cfg.Plan.Binaries)
	})

	t.Run("a later commit changes nothing until the next load", func(t *testing.T) {
		repo := newGitRepo(t)
		writeStarter(t, repo, "example.com/shop")
		first := gitCommitAll(t, repo, "starter")
		cfg, err := loadProjectConfig(t.Context(), repo)
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(filepath.Join(repo, "eval", "NOTES"), []byte("new scenario\n"), 0o600))
		second := gitCommitAll(t, repo, "suite change")
		require.NotEqual(t, first, second)
		assert.Equal(t, first, cfg.Plan.SuiteCommit, "the loaded plan keeps the startup commit")

		restarted, err := loadProjectConfig(t.Context(), repo)
		require.NoError(t, err)
		assert.Equal(t, second, restarted.Plan.SuiteCommit, "a restart selects the new HEAD")
	})

	t.Run("linked worktree uses its own checkout and HEAD", func(t *testing.T) {
		repo := newGitRepo(t)
		writeStarter(t, repo, "example.com/shop")
		gitCommitAll(t, repo, "starter")
		worktree := filepath.Join(newGitRepo(t), "wt")
		runTestGit(t, repo, "worktree", "add", "-q", "-b", "feature", worktree)
		require.NoError(t, os.WriteFile(filepath.Join(worktree, "eval", "NOTES"), []byte("feature suite\n"), 0o600))
		head := gitCommitAll(t, worktree, "feature suite")

		cfg, err := loadProjectConfig(t.Context(), worktree)
		require.NoError(t, err)
		assert.Equal(t, worktree, cfg.Plan.CandidateRepo)
		assert.Equal(t, head, cfg.Plan.SuiteCommit, "the worktree's HEAD, not the main checkout's")
	})

	t.Run("GIT_DIR in the environment does not redirect the project", func(t *testing.T) {
		other := newGitRepo(t)
		writeStarter(t, other, "example.com/other")
		gitCommitAll(t, other, "other")
		repo := newGitRepo(t)
		writeStarter(t, repo, "example.com/shop")
		head := gitCommitAll(t, repo, "starter")
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))

		cfg, err := loadProjectConfig(t.Context(), repo)
		require.NoError(t, err)
		assert.Equal(t, head, cfg.Plan.SuiteCommit)
		assert.Equal(t, map[string]string{"shop": "./cmd/shop"}, cfg.Plan.Binaries)
	})
}

// TestLoadProjectVerificationConfigNamesTheNextStep checks that every project
// -project cannot use stops startup with an error saying what to do, instead
// of searching for another layout or starting without verification.
func TestLoadProjectVerificationConfigNamesTheNextStep(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T) string
		wantErr string
	}{
		{
			name:    "missing directory",
			prepare: func(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") },
			wantErr: "read -project",
		},
		{
			name: "regular file",
			prepare: func(t *testing.T) string {
				path := filepath.Join(t.TempDir(), "file")
				require.NoError(t, os.WriteFile(path, nil, 0o600))
				return path
			},
			wantErr: "is not a directory",
		},
		{
			name: "not a git repository",
			prepare: func(t *testing.T) string {
				dir := t.TempDir()
				writeStarter(t, dir, "example.com/shop")
				return dir
			},
			wantErr: "is not inside a git repository (run git init",
		},
		{
			name: "repository without a commit",
			prepare: func(t *testing.T) string {
				repo := newGitRepo(t)
				writeStarter(t, repo, "example.com/shop")
				return repo
			},
			wantErr: "has no committed HEAD (run git add -A && git commit",
		},
		{
			name: "evaluator module never committed",
			prepare: func(t *testing.T) string {
				repo := newGitRepo(t)
				writeStarter(t, repo, "example.com/shop")
				require.NoError(t, os.Remove(filepath.Join(repo, "eval", "go.mod")))
				gitCommitAll(t, repo, "starter")
				return repo
			},
			wantErr: "is missing committed eval/go.mod",
		},
		{
			name: "evaluator module from another project",
			prepare: func(t *testing.T) string {
				repo := newGitRepo(t)
				writeStarter(t, repo, "example.com/shop")
				require.NoError(t, os.WriteFile(filepath.Join(repo, "eval", "go.mod"), []byte("module example.com/other/eval\n"), 0o600))
				gitCommitAll(t, repo, "starter")
				return repo
			},
			wantErr: "does not match the surla init starter layout",
		},
		{
			name: "committed go.mod larger than a startup read allows",
			prepare: func(t *testing.T) string {
				repo := newGitRepo(t)
				writeStarter(t, repo, "example.com/shop")
				padding := "// " + strings.Repeat("x", gitReadMaxOutput) + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/shop\n"+padding), 0o600))
				gitCommitAll(t, repo, "starter")
				return repo
			},
			wantErr: "printed more output than a startup read allows",
		},
		{
			name: "candidate command never committed",
			prepare: func(t *testing.T) string {
				repo := newGitRepo(t)
				writeStarter(t, repo, "example.com/shop")
				require.NoError(t, os.RemoveAll(filepath.Join(repo, "cmd")))
				gitCommitAll(t, repo, "starter")
				return repo
			},
			wantErr: "is missing committed directory cmd/shop",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadProjectConfig(t.Context(), tc.prepare(t))
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// TestResolveVerificationConfigChoosesOneSource checks how serve picks its
// verification configuration: -project and -verification-config exclude each
// other, an explicitly empty -project is an error, and with neither,
// verification stays off.
func TestResolveVerificationConfigChoosesOneSource(t *testing.T) {
	cfg, err := resolveVerificationConfig(serveConfig{})
	require.NoError(t, err)
	assert.Nil(t, cfg, "with neither flag, verification is off")

	_, err = resolveVerificationConfig(serveConfig{projectDir: ".", projectSet: true, verificationPath: "v.json"})
	assert.ErrorContains(t, err, "-project and -verification-config cannot be used together")

	_, err = resolveVerificationConfig(serveConfig{projectSet: true})
	assert.ErrorContains(t, err, "-project requires a directory")
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

// writeStarter writes the files `surla serve -project` reads from a starter
// created by surla init with module modulePath: both go.mod files and one file
// in each command directory.
func writeStarter(t *testing.T, dir, modulePath string) {
	t.Helper()
	name := path.Base(modulePath)
	files := map[string]string{
		"go.mod":                              "module " + modulePath + "\n\ngo 1.26\n",
		"cmd/" + name + "/main.go":            "package main\n",
		"eval/go.mod":                         "module " + modulePath + "/eval\n\ngo 1.26\n",
		"eval/cmd/" + name + "-evals/main.go": "package main\n",
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

// runTestGit runs git in dir with a fixed test identity and no inherited
// GIT_* variables, and returns its stdout.
func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=Surla Test",
		"-c", "user.email=test@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(cleanGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.Output()
	require.NoError(t, err, "git %v", args)
	return string(out)
}
