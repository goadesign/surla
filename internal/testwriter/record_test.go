package testwriter

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecordCommitsTheCheckedTreeOnce records a verdict made from the copy's
// snapshot. It makes one commit on the copy's branch, with the user's own
// identity and no hook run, holding the files as they were judged: an edit
// made after the verdict stays an uncommitted change in the copy. Recording
// the unchanged copy again returns the same commit.
func TestRecordCommitsTheCheckedTreeOnce(t *testing.T) {
	t.Parallel()
	repo, markers := newProject(t, map[string]string{"eval/go.mod": "module example.com/shop/eval\n"})
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	start := runGit(t, repo, "rev-parse", Branch)
	writeFiles(t, wc.Project(), map[string]string{"eval/refund_test.txt": "judged\n"})
	judged := snapshotVerdict(t, wc)
	writeFiles(t, wc.Project(), map[string]string{"eval/refund_test.txt": "edited after the check\n"})

	commit, err := Record(t.Context(), judged, "Add the accepted tests")
	require.NoError(t, err)
	assert.Equal(t, commit, runGit(t, repo, "rev-parse", Branch))
	assert.Equal(t, start, runGit(t, repo, "rev-parse", commit+"^"), "one commit on top of the branch")
	assert.Equal(t, "judged", runGit(t, repo, "show", commit+":eval/refund_test.txt"))
	assert.Equal(t, "Test User <test@example.com> Test User <test@example.com> Add the accepted tests",
		runGit(t, repo, "log", "-1", "--format=%an <%ae> %cn <%ce> %B", commit))
	assert.Equal(t, "M eval/refund_test.txt", runGit(t, wc.Dir(), "status", "--porcelain"),
		"the copy shows only the edit made after the check")
	assert.Empty(t, runGit(t, repo, "status", "--porcelain", "--untracked-files=all"))
	assert.Empty(t, ranHooks(t, markers), "no hook runs, including the copy's own")

	writeFiles(t, wc.Project(), map[string]string{"eval/refund_test.txt": "judged\n"})
	again, err := Record(t.Context(), snapshotVerdict(t, wc), "Add the accepted tests")
	require.NoError(t, err)
	assert.Equal(t, commit, again, "an unchanged copy returns the existing commit")
	assert.Equal(t, commit, runGit(t, repo, "rev-parse", Branch))
	assert.Empty(t, runGit(t, wc.Dir(), "status", "--porcelain"))
}

// TestRecordRefusesAVerdictWithFindingsOrNoMessage records nothing for a
// copy that was not accepted, a verdict Check did not make, or without a
// commit message.
func TestRecordRefusesAVerdictWithFindingsOrNoMessage(t *testing.T) {
	t.Parallel()
	repo, _ := newProject(t, map[string]string{"README.md": "shop\n"})
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	start := runGit(t, repo, "rev-parse", Branch)
	accepted := snapshotVerdict(t, wc)
	cases := []struct {
		name    string
		verdict Verdict
		message string
		wantErr string
	}{
		{
			name:    "findings",
			verdict: Verdict{Findings: []Finding{{Kind: FindingBuild, Message: "missing method"}}, copy: wc},
			message: "Add the accepted tests",
			wantErr: "only an accepted working copy is recorded",
		},
		{name: "empty message", verdict: accepted, message: " ", wantErr: "the commit message is empty"},
		{name: "a verdict Check did not make", verdict: Verdict{}, message: "Add the accepted tests", wantErr: "did not come from Check"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Record(t.Context(), tc.verdict, tc.message)
			require.ErrorContains(t, err, tc.wantErr)
			assert.Equal(t, start, runGit(t, repo, "rev-parse", Branch))
		})
	}
}

// snapshotVerdict returns an accepted verdict for the copy's current files,
// made by the snapshot steps Check uses, without generating or building.
func snapshotVerdict(t *testing.T, wc WorkingCopy) Verdict {
	t.Helper()
	dir := t.TempDir()
	index, src := filepath.Join(dir, "index"), filepath.Join(dir, "src")
	require.NoError(t, stageCopy(t.Context(), wc, index, src))
	tree, err := writeTree(t.Context(), wc, index, src)
	require.NoError(t, err)
	return Verdict{copy: wc, tree: tree}
}
