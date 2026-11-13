package testwriter

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// authorsAttributes names, for every file of the copy, a filter only the
// copy's author defines.
const authorsAttributes = "* filter=authors\n"

// TestSnapshotAndRecordUseTheProjectsRepository rewrites the copy's .git as
// its author could, to a repository whose configuration defines the filter the
// copy's attributes name, and snapshots and records the copy as Check and
// Record do. The filter never runs, the copy is still opened, and the commit
// holds exactly the bytes of the snapshot the check compiles. An attributes
// file naming a filter no configuration defines runs nothing either.
func TestSnapshotAndRecordUseTheProjectsRepository(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// rewrite changes the copy at dir as its author could; a filter that
		// runs writes marker.
		rewrite func(t *testing.T, dir, marker string)
	}{
		{name: "the copy's .git replaced by the author's repository", rewrite: replaceCopysGit},
		{name: "the copy's .git file pointed at a repository inside the copy", rewrite: pointCopysGitInside},
		{
			name:    "an attributes file naming a filter no configuration defines",
			rewrite: func(*testing.T, string, string) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, markers := newProject(t, map[string]string{"eval/go.mod": "module example.com/shop/eval\n"})
			wc, err := OpenWorkingCopy(t.Context(), repo)
			require.NoError(t, err)
			writeFiles(t, wc.Dir(), map[string]string{
				".gitattributes":       authorsAttributes,
				"eval/refund_test.txt": "judged\n",
			})
			marker := filepath.Join(t.TempDir(), "filter-ran")
			tc.rewrite(t, wc.Dir(), marker)

			snapshot := t.TempDir()
			index, src := filepath.Join(snapshot, "index"), filepath.Join(snapshot, "src")
			require.NoError(t, stageCopy(t.Context(), wc, index, src))
			tree, err := writeTree(t.Context(), wc, index, src)
			require.NoError(t, err)
			assert.NoFileExists(t, marker, "no program named inside the copy runs while snapshotting")
			commit, err := Record(t.Context(), Verdict{copy: wc, tree: tree}, "Add the accepted tests")
			require.NoError(t, err)

			assert.NoFileExists(t, marker, "no program named inside the copy runs while recording")
			assert.Equal(t, snapshotFiles(t, src), commitFiles(t, repo, commit),
				"the commit holds exactly the files the check compiles")
			assert.Contains(t, commitFiles(t, repo, commit), "eval/refund_test.txt")
			again, err := OpenWorkingCopy(t.Context(), repo)
			require.NoError(t, err, "the copy is found through the project's repository")
			assert.Equal(t, wc, again)
			assert.Empty(t, ranHooks(t, markers))
		})
	}
}

// replaceCopysGit replaces the .git file of the copy at dir with a repository
// of the author's, with one commit, that defines the author's filter.
func replaceCopysGit(t *testing.T, dir, marker string) {
	t.Helper()
	require.NoError(t, os.RemoveAll(filepath.Join(dir, ".git")))
	runGit(t, dir, "init", "--quiet")
	runGit(t, dir, "-c", "user.name=Author", "-c", "user.email=author@example.com",
		"commit", "--quiet", "--allow-empty", "-m", "The author's own history")
	defineAuthorsFilter(t, filepath.Join(dir, ".git"), marker)
}

// pointCopysGitInside points the .git file of the copy at dir at a
// repository the author keeps inside the copy, with one commit, that defines
// the author's filter.
func pointCopysGitInside(t *testing.T, dir, marker string) {
	t.Helper()
	authors := filepath.Join(dir, "authors.git")
	runGit(t, dir, "init", "--quiet", "--bare", authors)
	runGit(t, dir, "--git-dir="+authors, "config", "core.bare", "false")
	empty := runGit(t, dir, "--git-dir="+authors, "hash-object", "-t", "tree", "-w", "/dev/null")
	commit := runGit(t, dir, "--git-dir="+authors, "-c", "user.name=Author",
		"-c", "user.email=author@example.com", "commit-tree", empty, "-m", "The author's own history")
	runGit(t, dir, "--git-dir="+authors, "update-ref", "HEAD", commit)
	defineAuthorsFilter(t, authors, marker)
	require.NoError(t, os.RemoveAll(filepath.Join(dir, ".git")))
	writeFiles(t, dir, map[string]string{".git": "gitdir: " + authors + "\n"})
}

// TestRecordHoldsEveryCompiledFile writes, after the copy's snapshot, the
// files a check regenerates into it, while an ignore file in the copy names
// the generated folder, the generated guide, or a helper file of the
// author's. The recorded commit equals the snapshot the check compiles, byte
// for byte, regenerated files included; the helper the ignore file kept out of
// the snapshot was never compiled and is not recorded either.
func TestRecordHoldsEveryCompiledFile(t *testing.T) {
	t.Parallel()
	for _, ignored := range []string{"gen/", "AGENTS_QUICKSTART.md", "helper.go"} {
		t.Run(ignored, func(t *testing.T) {
			t.Parallel()
			repo, _ := newProject(t, map[string]string{"eval/go.mod": "module example.com/shop/eval\n"})
			wc, err := OpenWorkingCopy(t.Context(), repo)
			require.NoError(t, err)
			writeFiles(t, wc.Dir(), map[string]string{
				".gitignore":     ignored + "\n",
				"eval/helper.go": "package eval\n",
			})
			snapshot := t.TempDir()
			index, src := filepath.Join(snapshot, "index"), filepath.Join(snapshot, "src")
			require.NoError(t, stageCopy(t.Context(), wc, index, src))
			writeFiles(t, src, map[string]string{
				"eval/gen/evals/shop_acceptance/suite.go": "package shopacceptance\n",
				"eval/AGENTS_QUICKSTART.md":               "# Quickstart\n",
			})
			tree, err := writeTree(t.Context(), wc, index, src)
			require.NoError(t, err)
			commit, err := Record(t.Context(), Verdict{copy: wc, tree: tree}, "Add the accepted tests")
			require.NoError(t, err)

			compiled := snapshotFiles(t, src)
			assert.Equal(t, compiled, commitFiles(t, repo, commit), "the commit holds exactly the files the check compiles")
			assert.Contains(t, compiled, "eval/gen/evals/shop_acceptance/suite.go")
			assert.Contains(t, compiled, "eval/AGENTS_QUICKSTART.md")
			if ignored == "helper.go" {
				assert.NotContains(t, compiled, "eval/helper.go", "an ignored file of the author's is not compiled")
			}
		})
	}
}

// defineAuthorsFilter makes the repository at gitDir define the filter
// authorsAttributes names, as a program that writes marker.
func defineAuthorsFilter(t *testing.T, gitDir, marker string) {
	t.Helper()
	for _, step := range []string{"clean", "smudge"} {
		runGit(t, gitDir, "--git-dir="+gitDir, "config", "filter.authors."+step, "touch '"+marker+"'; cat")
	}
}

// snapshotFiles returns every file under dir, with its bytes, keyed by slash
// path.
func snapshotFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		files[filepath.ToSlash(rel)] = string(b)
		return err
	}))
	return files
}

// commitFiles returns every file of commit in repo, with its exact bytes,
// keyed by slash path.
func commitFiles(t *testing.T, repo, commit string) map[string]string {
	t.Helper()
	files := map[string]string{}
	listed := runGit(t, repo, "ls-tree", "-r", "-z", "--name-only", commit)
	for _, name := range strings.Split(strings.TrimSuffix(listed, "\x00"), "\x00") {
		cmd := exec.Command("git", "-C", repo, "cat-file", "blob", commit+":"+name)
		b, err := cmd.Output()
		require.NoError(t, err, name)
		files[name] = string(b)
	}
	return files
}
