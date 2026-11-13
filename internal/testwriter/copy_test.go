package testwriter

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/shquote"
)

// TestOpenWorkingCopyCreatesOneCopy opens the working copy of a project at
// its repository's top level and of one in a subfolder, twice each. The copy
// is created once, in a .surla-prefixed folder of the project (the prefix
// check_project skips; check_copy_test covers that rule), on its own branch
// at the project's commit, the repository's own exclude file names it once,
// the project's status stays clean, and no hook runs.
func TestOpenWorkingCopyCreatesOneCopy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// project is the project's folder inside the repository.
		project string
	}{
		{name: "project at the repository's top level", project: "."},
		{name: "project in a subfolder", project: "services/shop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, markers := newProject(t, map[string]string{
				filepath.ToSlash(filepath.Join(tc.project, "README.md")): "shop\n",
			})
			project := filepath.Join(repo, tc.project)
			start := runGit(t, repo, "rev-parse", "HEAD")

			wc, err := OpenWorkingCopy(t.Context(), project)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(Folder, ".surla"), "check_project skips .surla-prefixed folders")
			assert.Equal(t, filepath.Join(project, Folder), wc.Dir())
			assert.Equal(t, filepath.Join(wc.Dir(), tc.project), wc.Project())
			assert.FileExists(t, filepath.Join(wc.Project(), "README.md"), "the copy holds the project's files")
			assert.Equal(t, "refs/heads/"+Branch, runGit(t, wc.Dir(), "symbolic-ref", "HEAD"))
			assert.Equal(t, start, runGit(t, wc.Dir(), "rev-parse", "HEAD"), "the branch starts at the project's commit")
			assert.Empty(t, runGit(t, repo, "status", "--porcelain", "--untracked-files=all"), "the new copy is not in the project's status")

			again, err := OpenWorkingCopy(t.Context(), project)
			require.NoError(t, err)
			assert.Equal(t, wc, again, "opening again returns the same copy")
			listed := runGit(t, repo, "worktree", "list", "--porcelain")
			assert.Equal(t, 2, strings.Count(listed, "worktree "), "one linked copy beside the project:\n%s", listed)
			exclude, err := os.ReadFile(runGit(t, repo, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude"))
			require.NoError(t, err)
			assert.Equal(t, 1, strings.Count(string(exclude), "\n"+Folder+"/\n"), "one exclude entry:\n%s", exclude)
			assert.Empty(t, runGit(t, repo, "status", "--porcelain", "--untracked-files=all"), "the project's status stays clean")
			assert.Empty(t, runGit(t, repo, "ls-files", "--others", "--exclude-standard"), "git add --all would add nothing")
			assert.Empty(t, ranHooks(t, markers))
		})
	}
}

// TestOpenWorkingCopyRefuses checks each named refusal and that a refused
// project gets no copy and no exclude entry.
func TestOpenWorkingCopyRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// project returns the project folder to open and the steps the
		// error must name, with any path in them.
		project func(t *testing.T) (string, []string)
		wantErr error
		// notSteps are steps the error must not name, because they would not
		// work in that case or would touch more than the one registration.
		notSteps []string
	}{
		{
			name: "a folder outside git",
			project: func(t *testing.T) (string, []string) {
				return t.TempDir(), []string{"run git init in the project folder"}
			},
			wantErr: ErrNotRepository,
		},
		{
			name: "a repository without a commit",
			project: func(t *testing.T) (string, []string) {
				dir := t.TempDir()
				runGit(t, dir, "init", "--quiet")
				return dir, []string{"commit the project's files"}
			},
			wantErr: ErrNoCommit,
		},
		{
			name: "a folder of the same name the user made",
			project: func(t *testing.T) (string, []string) {
				repo, _ := newProject(t, map[string]string{"README.md": "shop\n"})
				writeFiles(t, repo, map[string]string{Folder + "/notes.txt": "mine\n"})
				return repo, []string{"move it out of the project"}
			},
			wantErr: ErrFolderInUse,
		},
		{
			name: "a file of the same name",
			project: func(t *testing.T) (string, []string) {
				repo, _ := newProject(t, map[string]string{"README.md": "shop\n"})
				writeFiles(t, repo, map[string]string{Folder: "mine\n"})
				return repo, []string{"move it out of the project"}
			},
			wantErr: ErrFolderInUse,
		},
		{
			name: "the reserved branch checked out in the project folder",
			project: func(t *testing.T) (string, []string) {
				repo, _ := newProject(t, map[string]string{"README.md": "shop\n"})
				runGit(t, repo, "switch", "--quiet", "--create", Branch)
				return repo, []string{repo + " has it checked out; switch that folder to another branch"}
			},
			wantErr:  ErrBranchInUse,
			notSteps: []string{"git worktree"},
		},
		{
			name: "the reserved branch held by a deleted working copy elsewhere",
			project: func(t *testing.T) (string, []string) {
				repo, _ := newProject(t, map[string]string{"README.md": "shop\n"})
				elsewhere := heldElsewhere(t, repo, false)
				return repo, []string{"remove that registration with " + removeStep(elsewhere) + " in the project"}
			},
			wantErr:  ErrBranchInUse,
			notSteps: []string{"switch that folder", "git worktree prune", "unlock"},
		},
		{
			name: "the reserved branch held by a locked, deleted working copy elsewhere",
			project: func(t *testing.T) (string, []string) {
				repo, _ := newProject(t, map[string]string{"README.md": "shop\n"})
				elsewhere := heldElsewhere(t, repo, true)
				return repo, []string{unlockStep(elsewhere) + " and then " + removeStep(elsewhere) + " in the project"}
			},
			wantErr:  ErrBranchInUse,
			notSteps: []string{"switch that folder", "git worktree prune"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			project, steps := tc.project(t)
			before := folderState(t, project)
			_, err := OpenWorkingCopy(t.Context(), project)
			require.ErrorIs(t, err, tc.wantErr)
			for _, step := range steps {
				assert.ErrorContains(t, err, step)
			}
			for _, step := range tc.notSteps {
				assert.NotContains(t, err.Error(), step)
			}
			assert.Equal(t, before, folderState(t, project), "a refused project is left as it was")
		})
	}
}

// TestTheBranchRefusalsStepsWork follows the step each refusal for a deleted
// working copy elsewhere names, unlocking first for a locked registration,
// while an unrelated registration of a deleted folder sits beside it: the copy
// then opens, and the unrelated registration is still there. The steps are
// run as the user runs them, as the refusal's own text given to a shell in
// the project, and the deleted folder's name holds shell syntax that would
// create a file if any of it were expanded: no such file appears.
func TestTheBranchRefusalsStepsWork(t *testing.T) {
	t.Parallel()
	for _, locked := range []bool{false, true} {
		t.Run(fmt.Sprintf("locked=%t", locked), func(t *testing.T) {
			t.Parallel()
			repo, _ := newProject(t, map[string]string{"README.md": "shop\n"})
			elsewhere := heldElsewhere(t, repo, locked)
			unrelated := filepath.Join(t.TempDir(), "unrelated")
			runGit(t, repo, "worktree", "add", "--quiet", "-b", "unrelated", unrelated)
			require.NoError(t, os.RemoveAll(unrelated))
			_, err := OpenWorkingCopy(t.Context(), repo)
			require.ErrorIs(t, err, ErrBranchInUse)

			steps := []string{removeStep(elsewhere)}
			if locked {
				steps = []string{unlockStep(elsewhere), removeStep(elsewhere)}
			}
			for _, step := range steps {
				require.ErrorContains(t, err, step, "the step that is run is the one the refusal names")
				runInShell(t, repo, step)
			}
			for _, planted := range []string{"INJECTED", "INJECTED2"} {
				assert.NoFileExists(t, filepath.Join(repo, planted), "the folder's name was read as shell syntax")
			}
			wc, err := OpenWorkingCopy(t.Context(), repo)
			require.NoError(t, err)
			assert.Equal(t, "refs/heads/"+Branch, runGit(t, wc.Dir(), "symbolic-ref", "HEAD"))
			assert.Contains(t, runGit(t, repo, "worktree", "list", "--porcelain"), "branch refs/heads/unrelated",
				"the step touches only the one registration")
		})
	}
}

// removeStep is the command a refusal names to remove the registration of the
// deleted folder at path, as the user types it in the project.
func removeStep(path string) string {
	return "git worktree remove " + shquote.Quote(path)
}

// unlockStep is the command a refusal names before removeStep when the
// registration of the deleted folder at path is locked.
func unlockStep(path string) string {
	return "git worktree unlock " + shquote.Quote(path)
}

// runInShell runs command in dir through sh, as a user who copies a step from
// a refusal does, with hooks off as runGit has them.
func runInShell(t *testing.T, dir, command string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s: %s", command, out)
}

// heldElsewhere makes a linked working copy of repo outside the project hold
// Branch, locks its registration when locked is set, deletes its folder with
// the file system, and returns that folder's path as git records it. The
// folder's name holds a space, a quote, a command substitution and a
// backquoted command, each of which would create a file in the folder a
// shell runs in if a step naming the path let the shell expand it.
func heldElsewhere(t *testing.T, repo string, locked bool) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	elsewhere := filepath.Join(parent, "tests $(touch INJECTED) it's `touch INJECTED2`")
	runGit(t, repo, "worktree", "add", "--quiet", "-b", Branch, elsewhere)
	if locked {
		runGit(t, repo, "worktree", "lock", "--reason", "on a removable disk", elsewhere)
	}
	require.NoError(t, os.RemoveAll(elsewhere))
	return elsewhere
}

// TestOpenWorkingCopyRecreatesADeletedCopy deletes the copy's folder with the
// file system after a commit on its branch, while git still registers it as
// holding the branch: opening again drops that registration of Surla's own
// copy and checks the same branch out again, so the accepted commit is kept,
// and restores the exclude entry the user removed.
func TestOpenWorkingCopyRecreatesADeletedCopy(t *testing.T) {
	t.Parallel()
	repo, markers := newProject(t, map[string]string{"README.md": "shop\n"})
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	writeFiles(t, wc.Dir(), map[string]string{"tests.txt": "accepted\n"})
	runGit(t, wc.Dir(), "add", "tests.txt")
	runGit(t, wc.Dir(), "commit", "--quiet", "-m", "Accepted tests")
	tip := runGit(t, wc.Dir(), "rev-parse", "HEAD")
	require.NoError(t, os.RemoveAll(wc.Dir()))
	listed := runGit(t, repo, "worktree", "list", "--porcelain")
	require.Contains(t, listed, "worktree "+wc.Dir()+"\nHEAD "+tip+"\nbranch refs/heads/"+Branch+"\nprunable",
		"git still registers the deleted copy as holding the branch")
	exclude := runGit(t, repo, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	require.NoError(t, os.WriteFile(exclude, nil, 0o644))

	again, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	assert.Equal(t, wc, again)
	assert.Equal(t, tip, runGit(t, again.Dir(), "rev-parse", "HEAD"), "the branch keeps its commits")
	assert.FileExists(t, filepath.Join(again.Dir(), "tests.txt"))
	assert.Empty(t, runGit(t, repo, "status", "--porcelain", "--untracked-files=all"))
	assert.Empty(t, ranHooks(t, markers))
}

// folderState returns every path under dir with its content, or "dir" for a
// folder, including git's own files, so a test can tell that nothing changed.
func folderState(t *testing.T, dir string) map[string]string {
	t.Helper()
	state := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			state[rel] = "dir"
			return nil
		}
		b, err := os.ReadFile(path)
		state[rel] = string(b)
		return err
	}))
	return state
}
