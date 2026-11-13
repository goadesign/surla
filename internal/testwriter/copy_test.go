package testwriter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		// project returns the project folder to open.
		project func(t *testing.T) string
		wantErr error
		// wantStep is a fragment of the step the error names.
		wantStep string
	}{
		{
			name:     "a folder outside git",
			project:  func(t *testing.T) string { return t.TempDir() },
			wantErr:  ErrNotRepository,
			wantStep: "run git init in the project folder",
		},
		{
			name: "a repository without a commit",
			project: func(t *testing.T) string {
				dir := t.TempDir()
				runGit(t, dir, "init", "--quiet")
				return dir
			},
			wantErr:  ErrNoCommit,
			wantStep: "commit the project's files",
		},
		{
			name: "a folder of the same name the user made",
			project: func(t *testing.T) string {
				repo, _ := newProject(t, map[string]string{"README.md": "shop\n"})
				writeFiles(t, repo, map[string]string{Folder + "/notes.txt": "mine\n"})
				return repo
			},
			wantErr:  ErrFolderInUse,
			wantStep: "move it out of the project",
		},
		{
			name: "a file of the same name",
			project: func(t *testing.T) string {
				repo, _ := newProject(t, map[string]string{"README.md": "shop\n"})
				writeFiles(t, repo, map[string]string{Folder: "mine\n"})
				return repo
			},
			wantErr:  ErrFolderInUse,
			wantStep: "move it out of the project",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			project := tc.project(t)
			before := folderState(t, project)
			_, err := OpenWorkingCopy(t.Context(), project)
			require.ErrorIs(t, err, tc.wantErr)
			assert.ErrorContains(t, err, tc.wantStep)
			assert.Equal(t, before, folderState(t, project), "a refused project is left as it was")
		})
	}
}

// TestOpenWorkingCopyRecreatesADeletedCopy deletes the copy's folder after a
// commit on its branch: opening again checks the same branch out again, so
// the commit is kept, and restores the exclude entry the user removed.
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
