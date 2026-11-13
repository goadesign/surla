// This file checks start-only refreshes against real temporary Git repositories.
package testwriter

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRefreshUsesTheProjectsCommit covers a project at either repository depth,
// preserving local author work and commits not contained in the project branch.
func TestRefreshUsesTheProjectsCommit(t *testing.T) {
	t.Parallel()
	for _, subdir := range []string{".", "services/shop"} {
		for _, state := range []string{"clean", "tracked change", "untracked", "not contained"} {
			t.Run(subdir+"/"+state, func(t *testing.T) {
				t.Parallel()
				repo, _ := newProject(t, map[string]string{filepath.ToSlash(filepath.Join(subdir, "README.md")): "original\n"})
				project := filepath.Join(repo, subdir)
				wc, err := OpenWorkingCopy(t.Context(), project)
				require.NoError(t, err)
				head := runGit(t, repo, "rev-parse", Branch)
				first, err := wc.Refresh(t.Context())
				require.NoError(t, err)
				assert.Equal(t, RefreshResult{State: RefreshAtProjectCommit, Commit: head}, first)
				want := RefreshAtProjectCommit
				switch state {
				case "tracked change":
					writeFiles(t, wc.Project(), map[string]string{"README.md": "author edit\n"})
					want = RefreshDirty
				case "untracked":
					writeFiles(t, wc.Project(), map[string]string{"new.txt": "author work\n"})
					want = RefreshDirty
				case "not contained":
					writeFiles(t, wc.Project(), map[string]string{"tests.txt": "accepted work\n"})
					runGit(t, wc.Dir(), "add", "--all")
					runGit(t, wc.Dir(), "commit", "--quiet", "-m", "Write tests")
					head = runGit(t, repo, "rev-parse", Branch)
					want = RefreshNotContained
				}
				writeFiles(t, project, map[string]string{"README.md": "project advanced\n"})
				runGit(t, repo, "add", "--all")
				runGit(t, repo, "commit", "--quiet", "-m", "Advance the project")
				target := runGit(t, repo, "rev-parse", "HEAD")
				result, err := wc.Refresh(t.Context())
				require.NoError(t, err)
				assert.Equal(t, want, result.State)
				if want == RefreshAtProjectCommit {
					assert.Equal(t, target, result.Commit)
					assert.Equal(t, target, runGit(t, repo, "rev-parse", Branch))
					content, err := os.ReadFile(filepath.Join(wc.Project(), "README.md"))
					require.NoError(t, err)
					assert.Equal(t, "project advanced\n", string(content))
				} else {
					assert.Equal(t, head, result.Commit)
					assert.Equal(t, head, runGit(t, repo, "rev-parse", Branch))
				}
			})
		}
	}
}

// TestRefreshPreservesAnEditAfterTheCleanRead forces the exact check/write race:
// a late tracked or untracked edit blocks checkout and survives unchanged.
func TestRefreshPreservesAnEditAfterTheCleanRead(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"README.md", "new.txt"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			repo, _ := newProject(t, map[string]string{"README.md": "original\n"})
			wc, err := OpenWorkingCopy(t.Context(), repo)
			require.NoError(t, err)
			head := runGit(t, repo, "rev-parse", Branch)
			writeFiles(t, repo, map[string]string{file: "project change\n"})
			runGit(t, repo, "add", "--all")
			runGit(t, repo, "commit", "--quiet", "-m", "Advance the project")
			result, err := wc.refresh(t.Context(), func() {
				writeFiles(t, wc.Project(), map[string]string{file: "late author edit\n"})
			})
			require.NoError(t, err)
			assert.Equal(t, RefreshResult{State: RefreshDirty, Commit: head}, result)
			content, err := os.ReadFile(filepath.Join(wc.Project(), file))
			require.NoError(t, err)
			assert.Equal(t, "late author edit\n", string(content))
			assert.Equal(t, head, runGit(t, repo, "rev-parse", Branch))
		})
	}
}

// TestRefreshIgnoresTheAuthorsGit uses the trusted original administration even
// after the author replaces .git with a different configured repository.
func TestRefreshIgnoresTheAuthorsGit(t *testing.T) {
	t.Parallel()
	repo, _ := newProject(t, map[string]string{"README.md": "original\n"})
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "filter-ran")
	replaceCopysGit(t, wc.Dir(), marker)
	writeFiles(t, repo, map[string]string{"README.md": "project advanced\n"})
	runGit(t, repo, "add", "--all")
	runGit(t, repo, "commit", "--quiet", "-m", "Advance the project")
	result, err := wc.Refresh(t.Context())
	require.NoError(t, err)
	assert.Equal(t, RefreshAtProjectCommit, result.State)
	assert.Equal(t, runGit(t, repo, "rev-parse", "HEAD"), result.Commit)
	assert.NoFileExists(t, marker)
}

// TestIdentityCheckMatchesRecord exercises the same Git-resolved identity with
// both repository configuration and explicit author/committer environment.
func TestIdentityCheckMatchesRecord(t *testing.T) {
	repo, _ := newProject(t, map[string]string{"README.md": "original\n"})
	assert.NoError(t, CheckIdentity(t.Context(), repo))
	runGit(t, repo, "config", "user.useConfigOnly", "true")
	for _, key := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(key, "")
	}
	assert.ErrorIs(t, CheckIdentity(t.Context(), repo), ErrIdentityMissing)
	t.Setenv("GIT_AUTHOR_NAME", "Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "author@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "committer@example.com")
	require.NoError(t, CheckIdentity(t.Context(), repo))
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	writeFiles(t, wc.Project(), map[string]string{"tests.txt": "accepted\n"})
	commit, err := Record(t.Context(), snapshotVerdict(t, wc), "Write tests")
	require.NoError(t, err)
	assert.Equal(t, "Author <author@example.com> Committer <committer@example.com>", runGit(t, repo, "log", "-1", "--format=%an <%ae> %cn <%ce>", commit))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, CheckIdentity(ctx, repo), context.Canceled)
}
