package main

import (
	"context"
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
	"github.com/goadesign/surla/internal/verify"
)

// capturingCP1Analyzer is a CP1Analyzer test double that records designSource.
type capturingCP1Analyzer struct {
	// onAnalyze is called with the designSource passed to Analyze.
	onAnalyze func(designSource string)
}

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
	isolateGitConfig(t)
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
			runsDir := filepath.Join(t.TempDir(), "runs")
			reportDir := filepath.Join(t.TempDir(), "reports")

			cfg, err := starterVerificationAtHEAD(t.Context(), project, runsDir, reportDir)
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
				ReportDir:       reportDir,
			}, cfg.Plan)
			assert.Equal(t, runsDir, cfg.RunsDir)
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
			isolateServeWorkspace(t)
			t.Chdir(start.dir)
			ws, err := configureServeWorkspace(serveConfig{checkTimeout: defaultCheckTimeout, projectDir: start.arg, projectSet: true})
			require.NoError(t, err, "-project %s from %s", start.arg, start.dir)
			require.NotNil(t, ws.verification)
			assert.Equal(t, repo, ws.verification.Plan.CandidateRepo)
			assert.Equal(t, "services/shop", ws.verification.Plan.CandidateModule)
			assert.Equal(t, "services/shop/eval", ws.verification.Plan.SuiteModule)
			assertOutsideDirectory(t, ws.verification.RunsDir, project)
			assertOutsideDirectory(t, ws.verification.Plan.ReportDir, project)
			require.NoError(t, ws.closeProjectLock())
		}
	})

	t.Run("symlink parent paths select the linked project", func(t *testing.T) {
		repo := newGitRepo(t)
		writeStarter(t, repo, "example.com/shop")
		head := gitCommitAll(t, repo, "starter")
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
			ws, err := configureServeWorkspace(serveConfig{checkTimeout: defaultCheckTimeout, projectDir: arg, projectSet: true})
			require.NoError(t, err, "-project %s", arg)
			require.NotNil(t, ws.verification)
			assert.Equal(t, repo, ws.verification.Plan.CandidateRepo, "-project %s", arg)
			assert.Equal(t, repo, ws.verification.Plan.SuiteRepo, "-project %s", arg)
			assert.Equal(t, head, ws.verification.Plan.SuiteCommit, "-project %s", arg)
			assert.Equal(t, map[string]string{"shop": "./cmd/shop"}, ws.verification.Plan.Binaries)
			assertOutsideDirectory(t, ws.verification.RunsDir, repo)
			assertOutsideDirectory(t, ws.verification.Plan.ReportDir, repo)
			require.NoError(t, ws.closeProjectLock())
		}
	})

	t.Run("committed layout wins over working files", func(t *testing.T) {
		repo := newGitRepo(t)
		writeStarter(t, repo, "example.com/shop")
		head := gitCommitAll(t, repo, "starter")
		require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/other\n"), 0o600))
		require.NoError(t, os.RemoveAll(filepath.Join(repo, "cmd")))

		cfg, err := starterVerificationAtHEAD(t.Context(), repo, filepath.Join(t.TempDir(), "runs"), filepath.Join(t.TempDir(), "reports"))
		require.NoError(t, err)
		assert.Equal(t, head, cfg.Plan.SuiteCommit)
		assert.Equal(t, map[string]string{"shop": "./cmd/shop"}, cfg.Plan.Binaries)
	})

	t.Run("a later commit changes nothing until the next load", func(t *testing.T) {
		repo := newGitRepo(t)
		writeStarter(t, repo, "example.com/shop")
		first := gitCommitAll(t, repo, "starter")
		runsDir := filepath.Join(t.TempDir(), "runs")
		reportDir := filepath.Join(t.TempDir(), "reports")
		cfg, err := starterVerificationAtHEAD(t.Context(), repo, runsDir, reportDir)
		require.NoError(t, err)

		require.NoError(t, os.WriteFile(filepath.Join(repo, "eval", "NOTES"), []byte("new scenario\n"), 0o600))
		second := gitCommitAll(t, repo, "suite change")
		require.NotEqual(t, first, second)
		assert.Equal(t, first, cfg.Plan.SuiteCommit, "the loaded plan keeps the startup commit")

		restarted, err := starterVerificationAtHEAD(t.Context(), repo, runsDir, reportDir)
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

		cfg, err := starterVerificationAtHEAD(t.Context(), worktree, filepath.Join(t.TempDir(), "runs"), filepath.Join(t.TempDir(), "reports"))
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

		cfg, err := starterVerificationAtHEAD(t.Context(), repo, filepath.Join(t.TempDir(), "runs"), filepath.Join(t.TempDir(), "reports"))
		require.NoError(t, err)
		assert.Equal(t, head, cfg.Plan.SuiteCommit)
		assert.Equal(t, map[string]string{"shop": "./cmd/shop"}, cfg.Plan.Binaries)
	})
}

// TestLoadProjectVerificationConfigRejectsUnusableProjects checks that every
// project -project cannot use stops startup with an error, naming the next
// step (git init, a commit, or the starter layout) where there is one, instead
// of searching for another layout or starting without verification.
func TestLoadProjectVerificationConfigRejectsUnusableProjects(t *testing.T) {
	isolateGitConfig(t)
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
			name: "git older than 2.45",
			prepare: func(t *testing.T) string {
				realGit, err := exec.LookPath("git")
				require.NoError(t, err)
				fakeBin := t.TempDir()
				// Git 2.44 and older print this for the unknown --no-lazy-fetch top-level option.
				script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = \"--no-lazy-fetch\" ]; then\n    printf 'unknown option: --no-lazy-fetch\\nusage: git [-v | --version] [-h | --help]\\n' >&2\n    exit 129\n  fi\ndone\nexec \"" + realGit + "\" \"$@\"\n"
				require.NoError(t, os.WriteFile(filepath.Join(fakeBin, "git"), []byte(script), 0o700))
				t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
				return t.TempDir()
			},
			wantErr: "needs Git 2.45 or newer",
		},
		{
			name: "partial clone without the committed go.mod",
			prepare: func(t *testing.T) string {
				src := newGitRepo(t)
				writeStarter(t, src, "example.com/shop")
				gitCommitAll(t, src, "starter")
				runTestGit(t, src, "config", "uploadpack.allowfilter", "true")
				clone := filepath.Join(newGitRepo(t), "partial")
				runTestGit(t, src, "clone", "-q", "--filter=blob:none", "--no-checkout", "file://"+src, clone)
				t.Cleanup(func() {
					missing := exec.Command("git", "-C", clone, "--no-lazy-fetch", "cat-file", "-e", "HEAD:go.mod")
					missing.Env = append(cleanGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null")
					assert.Error(t, missing.Run(), "startup must not have fetched the missing object")
				})
				return clone
			},
			wantErr: "check out HEAD there first",
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
			isolateServeWorkspace(t)
			dir := tc.prepare(t)
			_, err := starterVerificationAtHEAD(t.Context(), dir, filepath.Join(t.TempDir(), "runs"), filepath.Join(t.TempDir(), "reports"))
			require.ErrorContains(t, err, tc.wantErr)

			ws, wsErr := configureServeWorkspace(serveConfig{checkTimeout: defaultCheckTimeout, projectDir: dir, projectSet: true})
			if wsErr != nil {
				require.ErrorContains(t, wsErr, tc.wantErr)
				return
			}
			t.Cleanup(func() {
				if ws.closeProjectLock != nil {
					assert.NoError(t, ws.closeProjectLock())
				}
			})
			assert.Nil(t, ws.verification)
			require.NotNil(t, ws.resolveVerification)
			_, resolveErr := ws.resolveVerification(t.Context(), ws.workspaceDir, "")
			require.ErrorContains(t, resolveErr, tc.wantErr)
		})
	}
}

// TestConfigureServeWorkspaceSupportsIndependentProjectAndVerificationConfig
// verifies that -project (workspace binding) and -verification-config (external
// evaluator suite) compose independently, and that an explicitly empty
// -project="" flag is rejected.
func TestConfigureServeWorkspaceSupportsIndependentProjectAndVerificationConfig(t *testing.T) {
	isolateServeWorkspace(t)

	_, err := configureServeWorkspace(serveConfig{checkTimeout: defaultCheckTimeout, projectSet: true})
	assert.ErrorContains(t, err, "-project requires a directory")

	root := t.TempDir()
	opsSessionDir := filepath.Join(root, "ops", "session")
	shopDir := filepath.Join(root, "work", "shop")
	require.NoError(t, os.MkdirAll(opsSessionDir, 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(shopDir, "design"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(shopDir, "design", "design.go"), []byte("package design\n// shop workspace design\n"), 0o644))
	runTestGit(t, filepath.Join(root, "work"), "init", "-q")

	// Simulate Git <= 2.44 (rejecting --no-lazy-fetch) to verify that explicit
	// -verification-config and CP1 flows succeed without requiring Git 2.45.
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	fakeBin := filepath.Join(root, "fake-git-244")
	require.NoError(t, os.MkdirAll(fakeBin, 0o700))
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = \"--no-lazy-fetch\" ]; then\n    printf 'unknown option: --no-lazy-fetch\\nusage: git [-v | --version] [-h | --help]\\n' >&2\n    exit 129\n  fi\ndone\nexec \"" + realGit + "\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(fakeBin, "git"), []byte(script), 0o700))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	verPath := filepath.Join(root, "ops", "shop-eval.json")
	verContent := `{"runs_dir":"` + filepath.Join(root, "ops", "runs") + `","report_dir":"` + filepath.Join(root, "ops", "reports") + `",` +
		`"candidate_repo":"` + shopDir + `","candidate_module":".","binaries":{"shop":"./cmd/shop"},` +
		`"suite_repo":"` + filepath.Join(root, "ops", "eval-repo") + `","suite_commit":"` + strings.Repeat("d", 40) + `",` +
		`"suite_module":".","suite_command":"./cmd/shop-evals","timeout":"15m"}`
	require.NoError(t, os.WriteFile(verPath, []byte(verContent), 0o600))

	t.Chdir(opsSessionDir)
	opts, err := serveOptions(
		serveConfig{
			checkTimeout:     defaultCheckTimeout,
			projectDir:       shopDir,
			projectSet:       true,
			verificationPath: verPath,
		},
		func(string) string { return "" },
		nil,
	)
	require.NoError(t, err)
	require.NotNil(t, opts.Verification)
	assert.Nil(t, opts.ResolveVerification, "explicit -verification-config must not fall back to starter verification resolution")
	assert.Equal(t, filepath.Join(root, "ops", "eval-repo"), opts.Verification.Plan.SuiteRepo)
	assert.Equal(t, 15*time.Minute, opts.Verification.Timeout)

	var seenDesignSource string
	opts.Analyzer = &capturingCP1Analyzer{
		onAnalyze: func(designSource string) { seenDesignSource = designSource },
	}
	cp, err := controlplane.New(opts)
	require.NoError(t, err)
	defer cp.Close()

	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com"})
	_, err = cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID: "chg-shop-with-external-eval",
		Prompt:   "Add coupon validation",
	})
	require.NoError(t, err)
	assert.Contains(t, seenDesignSource, "shop workspace design", "-project must bind shopDir for CP1 workspace design reads even when -verification-config is set")
}

// Analyze records designSource and returns a confirmed CP1SemanticAnalysis.
func (a *capturingCP1Analyzer) Analyze(_ context.Context, _, designSource string) (controlplane.CP1SemanticAnalysis, error) {
	if a.onAnalyze != nil {
		a.onAnalyze(designSource)
	}
	return controlplane.CP1SemanticAnalysis{
		Summary: "Add coupon validation.",
		Examples: []controlplane.CP1ModelExample{{
			Scenario:         "Apply valid coupon",
			InputSummary:     "Valid coupon code",
			ExpectedOutcome:  "Discounts order total",
			VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{"existing checkout remains unchanged"},
	}, nil
}

// Revise satisfies controlplane.CP1Analyzer for capturingCP1Analyzer.
func (a *capturingCP1Analyzer) Revise(_ context.Context, _ controlplane.CP1PriorCard, _, designSource string) (controlplane.CP1RevisionSemanticAnalysis, error) {
	if a.onAnalyze != nil {
		a.onAnalyze(designSource)
	}
	return controlplane.CP1RevisionSemanticAnalysis{}, nil
}

// isolateGitConfig points HOME and XDG_CONFIG_HOME at empty directories for
// the test, so the developer's own Git configuration cannot change what the
// startup Git reads see.
func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
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
