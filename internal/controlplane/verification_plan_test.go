// This file checks that the plan of a run of written tests or a live copy
// comes only from the commits it names: never from the working files, the
// server's GIT_* variables or another checkout, without fetching objects a
// partial clone lacks, and with a refusal saying what is missing and who acts
// when a repository or commit cannot be used.
package controlplane

import (
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/verify"
)

// TestStarterPlanReadsTheCommittedLayout checks the plan starterPlanAt makes
// from one commit, wherever the project sits in its repository, whatever its
// folder is called, and whatever the working files, the checkout or the
// server's environment say.
func TestStarterPlanReadsTheCommittedLayout(t *testing.T) {
	cases := []struct {
		name        string
		module      string
		projectPath string // project folder relative to the repository root
		wantService string
		wantModule  string // service module folder relative to the repository root
	}{
		{name: "project at the repository root", module: "example.com/shop", projectPath: ".", wantService: "shop", wantModule: "."},
		{name: "renamed folder with a space", module: "example.com/shop", projectPath: "renamed shop", wantService: "shop", wantModule: "renamed shop"},
		{name: "project nested in a larger repository", module: "example.com/shop", projectPath: "services/shop", wantService: "shop", wantModule: "services/shop"},
		{name: "service name reserved in generated Go", module: "example.com/errors", projectPath: ".", wantService: "errors", wantModule: "."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := realTempDir(t)
			gitForLayoutTest(t, repo, "init", "-q")
			project := filepath.Join(repo, filepath.FromSlash(tc.projectPath))
			writeStarterAt(t, project, tc.module)
			head := commitAllForLayoutTest(t, repo)

			plan, err := planServer(t, project).starterPlanAt(t.Context(), head, head, testWriterActs)
			require.NoError(t, err)
			assert.Equal(t, verify.Plan{
				CandidateRepo:   repo,
				CandidateCommit: head,
				CandidateModule: tc.wantModule,
				Binaries:        map[string]string{tc.wantService: "./cmd/" + tc.wantService},
				SuiteRepo:       repo,
				SuiteCommit:     head,
				SuiteModule:     path.Join(tc.wantModule, "eval"),
				SuiteCommand:    "./gen/cmd/" + tc.wantService + "-evals",
				Tests:           verify.TestsWritten,
			}, plan)
		})
	}

	t.Run("the candidate's programs come from its own commit", func(t *testing.T) {
		repo := realTempDir(t)
		gitForLayoutTest(t, repo, "init", "-q")
		writeStarterAt(t, repo, "example.com/orders")
		require.NoError(t, os.RemoveAll(filepath.Join(repo, "cmd")))
		tests := commitAllForLayoutTest(t, repo)
		writeTree(t, repo, map[string]string{
			"cmd/orders/main.go":   "package main\n",
			"cmd/orders/refund.go": "package main\n",
		})
		require.NoError(t, os.RemoveAll(filepath.Join(repo, "eval", "gen")))
		candidate := commitAllForLayoutTest(t, repo)

		plan, err := planServer(t, repo).starterPlanAt(t.Context(), candidate, tests, testWriterActs)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"orders": "./cmd/orders"}, plan.Binaries, "the tests' commit has no cmd/orders")
		assert.Equal(t, "./gen/cmd/orders-evals", plan.SuiteCommand, "the candidate commit has no eval/gen")
		assert.Equal(t, candidate, plan.CandidateCommit)
		assert.Equal(t, tests, plan.SuiteCommit)
	})

	t.Run("candidate module rename is refused because it changes eval/go.mod", func(t *testing.T) {
		repo := realTempDir(t)
		gitForLayoutTest(t, repo, "init", "-q")
		writeStarterAt(t, repo, "example.com/orders")
		tests := commitAllForLayoutTest(t, repo)
		require.NoError(t, os.RemoveAll(filepath.Join(repo, "cmd")))
		require.NoError(t, os.RemoveAll(filepath.Join(repo, "eval")))
		writeStarterAt(t, repo, "example.com/shop")
		candidate := commitAllForLayoutTest(t, repo)

		_, err := planServer(t, repo).starterPlanAt(t.Context(), candidate, tests, testWriterActs)
		var refusal *layoutRefusal
		require.ErrorAs(t, err, &refusal)
		assert.Equal(t, refusedTestModuleChanged, refusal.reason)
		assert.Equal(t, tests, refusal.baseCommit)
		assert.Equal(t, "eval", refusal.testModule)
		assert.Equal(t, []changedGitFile{{path: "eval/go.mod"}}, refusal.changed)
		assert.ErrorContains(t, err,
			"candidate commit "+candidate+" changes hand-written files in the test module eval since base commit "+tests+" (eval/go.mod);"+
				" tests in eval belong to the test writer, not the builder;"+
				" renaming the service module is the user's to do on their own branch between changes, not during a build:"+
				" restore eval to base commit "+tests+" (git restore --source="+tests+" --staged --worktree -- ':(top,literal)eval/go.mod'), commit, and call verify_change or start_live_target again")
	})

	t.Run("committed layout wins over working files", func(t *testing.T) {
		repo := realTempDir(t)
		gitForLayoutTest(t, repo, "init", "-q")
		writeStarterAt(t, repo, "example.com/shop")
		head := commitAllForLayoutTest(t, repo)
		require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/other\n"), 0o600))
		require.NoError(t, os.RemoveAll(filepath.Join(repo, "cmd")))

		plan, err := planServer(t, repo).starterPlanAt(t.Context(), head, head, testWriterActs)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"shop": "./cmd/shop"}, plan.Binaries)
	})

	t.Run("linked worktree reads its own checkout's repository", func(t *testing.T) {
		repo := realTempDir(t)
		gitForLayoutTest(t, repo, "init", "-q")
		writeStarterAt(t, repo, "example.com/shop")
		commitAllForLayoutTest(t, repo)
		worktree := filepath.Join(realTempDir(t), "wt")
		gitForLayoutTest(t, repo, "worktree", "add", "-q", "-b", "feature", worktree)
		require.NoError(t, os.WriteFile(filepath.Join(worktree, "eval", "NOTES"), []byte("feature tests\n"), 0o600))
		head := commitAllForLayoutTest(t, worktree)

		plan, err := planServer(t, worktree).starterPlanAt(t.Context(), head, head, testWriterActs)
		require.NoError(t, err)
		assert.Equal(t, worktree, plan.CandidateRepo)
		assert.Equal(t, head, plan.SuiteCommit)
	})

	t.Run("GIT_DIR in the environment does not redirect the project", func(t *testing.T) {
		other := realTempDir(t)
		gitForLayoutTest(t, other, "init", "-q")
		writeStarterAt(t, other, "example.com/other")
		commitAllForLayoutTest(t, other)
		repo := realTempDir(t)
		gitForLayoutTest(t, repo, "init", "-q")
		writeStarterAt(t, repo, "example.com/shop")
		head := commitAllForLayoutTest(t, repo)
		cp := planServer(t, repo)
		t.Setenv("GIT_DIR", filepath.Join(other, ".git"))

		plan, err := cp.starterPlanAt(t.Context(), head, head, testWriterActs)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{"shop": "./cmd/shop"}, plan.Binaries)
	})
}

// TestStarterPlanRefusesUnusableRepositories checks that each repository or
// commit starterPlanAt cannot use is refused with what is missing and who
// acts, or, when git itself fails, with git's reason, instead of searching
// for another layout.
func TestStarterPlanRefusesUnusableRepositories(t *testing.T) {
	// fakeOldGit is a git that, as Git 2.44 and older do, rejects the
	// --no-lazy-fetch top-level option.
	fakeOldGit := func(t *testing.T, _ string) {
		realGit, err := exec.LookPath("git")
		require.NoError(t, err)
		bin := realTempDir(t)
		script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = \"--no-lazy-fetch\" ]; then\n" +
			"    printf 'unknown option: --no-lazy-fetch\\nusage: git [-v | --version] [-h | --help]\\n' >&2\n    exit 129\n  fi\ndone\n" +
			"exec \"" + realGit + "\" \"$@\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o700))
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	// committedStarter returns a repository holding the starter for
	// example.com/shop, as edit leaves it, at its one commit.
	committedStarter := func(t *testing.T, edit func(repo string)) (string, string) {
		repo := realTempDir(t)
		gitForLayoutTest(t, repo, "init", "-q")
		writeStarterAt(t, repo, "example.com/shop")
		if edit != nil {
			edit(repo)
		}
		return repo, commitAllForLayoutTest(t, repo)
	}
	cases := []struct {
		name    string
		prepare func(t *testing.T) (workspace, commit string)
		// after runs on the workspace once the server is open, before the
		// plan is read.
		after   func(t *testing.T, workspace string)
		refusal *layoutRefusal
		wantErr string
	}{
		{
			name: "project folder replaced after the server started",
			prepare: func(t *testing.T) (string, string) {
				return committedStarter(t, nil)
			},
			after: func(t *testing.T, workspace string) {
				require.NoError(t, os.Rename(workspace, workspace+".old"))
				require.NoError(t, os.Mkdir(workspace, 0o755))
			},
			refusal: &layoutRefusal{reason: refusedReplacedFolder},
			wantErr: "was replaced or removed after Surla started, so the user restarts it: stop and start Surla in the project folder, then try again",
		},
		{
			name: "not a git repository",
			prepare: func(t *testing.T) (string, string) {
				dir := realTempDir(t)
				writeStarterAt(t, dir, "example.com/shop")
				return dir, strings.Repeat("a", 40)
			},
			refusal: &layoutRefusal{reason: refusedNoRepository},
			wantErr: "is not inside a git repository, so it has no commits to verify: make the project folder a git repository and commit it",
		},
		{
			name: "test module never committed",
			prepare: func(t *testing.T) (string, string) {
				return committedStarter(t, func(repo string) { require.NoError(t, os.Remove(filepath.Join(repo, "eval", "go.mod"))) })
			},
			refusal: &layoutRefusal{reason: refusedNoLayout, actor: builderActs, missing: "it has no regular file eval/go.mod"},
		},
		{
			name: "test module of another project",
			prepare: func(t *testing.T) (string, string) {
				return committedStarter(t, func(repo string) {
					require.NoError(t, os.WriteFile(filepath.Join(repo, "eval", "go.mod"), []byte("module example.com/other/eval\n"), 0o600))
				})
			},
			refusal: &layoutRefusal{reason: refusedNoLayout, actor: builderActs,
				missing: `eval/go.mod declares module "example.com/other/eval", want "example.com/shop/eval"`},
		},
		{
			name: "go.mod committed as a link",
			prepare: func(t *testing.T) (string, string) {
				return committedStarter(t, func(repo string) {
					require.NoError(t, os.Rename(filepath.Join(repo, "go.mod"), filepath.Join(repo, "real.mod")))
					require.NoError(t, os.Symlink("real.mod", filepath.Join(repo, "go.mod")))
				})
			},
			refusal: &layoutRefusal{reason: refusedNoLayout, actor: builderActs, missing: "it has no regular file go.mod"},
		},
		{
			name: "service command never committed",
			prepare: func(t *testing.T) (string, string) {
				return committedStarter(t, func(repo string) { require.NoError(t, os.RemoveAll(filepath.Join(repo, "cmd"))) })
			},
			refusal: &layoutRefusal{reason: refusedNoLayout, actor: builderActs, missing: "it has no folder cmd/shop"},
		},
		{
			name: "committed go.mod larger than a layout read allows",
			prepare: func(t *testing.T) (string, string) {
				return committedStarter(t, func(repo string) {
					padding := "// " + strings.Repeat("x", gitReadMaxOutput) + "\n"
					require.NoError(t, os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/shop\n"+padding), 0o600))
				})
			},
			wantErr: "printed more output than a layout read allows",
		},
		{
			name: "git older than 2.45",
			prepare: func(t *testing.T) (string, string) {
				return committedStarter(t, nil)
			},
			after:   fakeOldGit,
			wantErr: "needs Git 2.45 or newer",
		},
		{
			name: "partial clone without the committed go.mod",
			prepare: func(t *testing.T) (string, string) {
				src, head := committedStarter(t, nil)
				gitForLayoutTest(t, src, "config", "uploadpack.allowfilter", "true")
				clone := filepath.Join(realTempDir(t), "partial")
				gitForLayoutTest(t, src, "clone", "-q", "--filter=blob:none", "--no-checkout", "file://"+src, clone)
				t.Cleanup(func() {
					missing := exec.Command("git", "-C", clone, "--no-lazy-fetch", "cat-file", "-e", head+":go.mod")
					missing.Env = append(cleanGitEnv(), "GIT_CONFIG_GLOBAL=/dev/null")
					assert.Error(t, missing.Run(), "the plan must not have fetched the missing object")
				})
				return clone, head
			},
			wantErr: "check out that commit there first",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace, commit := tc.prepare(t)
			cp := planServer(t, workspace)
			if tc.after != nil {
				tc.after(t, workspace)
			}
			_, err := cp.starterPlanAt(t.Context(), commit, commit, builderActs)
			require.Error(t, err)
			var refusal *layoutRefusal
			if tc.refusal == nil {
				assert.NotErrorAs(t, err, &refusal, "git's own failure is not a refusal")
				assert.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.ErrorAs(t, err, &refusal)
			want := *tc.refusal
			if want.reason == refusedNoRepository || want.reason == refusedReplacedFolder {
				want.folder = workspace
			} else {
				want.commit = commit
			}
			assert.Equal(t, want, *refusal)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
}

// planServer returns a control plane bound to workspace that verifies with
// the tests accepted for each change.
func planServer(t *testing.T, workspace string) *ControlPlane {
	t.Helper()
	ops := realTempDir(t)
	return newControlPlane(t, Options{
		WorkspaceDir:    workspace,
		RunsDir:         filepath.Join(ops, "runs"),
		SurlaVersion:    testSurlaVersion,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: filepath.Join(ops, "reports"), Timeout: time.Minute},
	})
}

// writeStarterAt writes the files the plan reads from a starter `surla init`
// made for modulePath in dir: both go.mod files, the service's command and
// the tests' generated entry point.
func writeStarterAt(t *testing.T, dir, modulePath string) {
	t.Helper()
	name := path.Base(modulePath)
	writeTree(t, dir, map[string]string{
		"go.mod":                   "module " + modulePath + "\n\ngo 1.26\n",
		"cmd/" + name + "/main.go": "package main\n",
		"eval/go.mod":              "module " + modulePath + "/eval\n\ngo 1.26\n",
		"eval/gen/cmd/" + name + "-evals/main.go": "package main\n",
	})
}

// commitAllForLayoutTest commits every file in repo and returns the new
// commit.
func commitAllForLayoutTest(t *testing.T, repo string) string {
	t.Helper()
	gitForLayoutTest(t, repo, "add", "-A")
	gitForLayoutTest(t, repo, "commit", "-q", "-m", "fixture")
	return strings.TrimSpace(gitForLayoutTest(t, repo, "rev-parse", "HEAD"))
}
