package testwriter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/designeval"
)

// TestTheStoryTableIsSurlas plays a test writer who changes the Story table
// Surla writes in its working copy. Prepare replaces a symbolic link at the
// table's path without touching its target. Check requires the Story's flows
// whatever the author left at that path (nothing, a table requiring nothing,
// a link) and records Surla's table; a sibling file that tries to empty the
// rows does not compile, and a folder at the path is the author's finding.
// Prepare at the next report restores an edited table, and after Record the
// copy has no change left. A later Story without a flow drops that flow's
// requirement while its method stays and still builds.
func TestTheStoryTableIsSurlas(t *testing.T) {
	skipWithoutSandbox(t)
	repo, markers := newProject(t, fixtureProject(t))
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	module := filepath.Join(wc.Project(), "eval")
	tablePath := filepath.Join(module, filepath.FromSlash(designeval.StoryTestsPath))
	want, _, err := designeval.StoryTests(os.DirFS(module), refundStories)
	require.NoError(t, err)
	outside := filepath.Join(t.TempDir(), "outside.go")
	require.NoError(t, os.WriteFile(outside, []byte("package outside\n"), 0o600))
	assertOutsideUntouched := func(t *testing.T) {
		t.Helper()
		b, err := os.ReadFile(outside)
		require.NoError(t, err)
		assert.Equal(t, "package outside\n", string(b), "the link's target is untouched")
	}
	assertSurlasTable := func(t *testing.T) {
		t.Helper()
		info, err := os.Lstat(tablePath)
		require.NoError(t, err)
		assert.True(t, info.Mode().IsRegular(), "the table is a file, not a link")
		b, err := os.ReadFile(tablePath)
		require.NoError(t, err)
		assert.Equal(t, string(want), string(b))
		left, err := filepath.Glob(filepath.Join(module, "design", ".story_tests-*"))
		require.NoError(t, err)
		assert.Empty(t, left, "no temporary file is left")
	}

	t.Run("Prepare replaces a link", func(t *testing.T) {
		require.NoError(t, os.Symlink(outside, tablePath))
		required, err := testChecker.Prepare(t.Context(), wc, fixtureSuite, refundStories)
		require.NoError(t, err)
		assert.Len(t, required, 2)
		assertSurlasTable(t)
		assertOutsideUntouched(t)
		suiteGo, err := os.ReadFile(filepath.Join(module, generatedSuite))
		require.NoError(t, err)
		assert.Contains(t, string(suiteGo), "RefundOverLimit(context.Context) (eval.Result, error)")
	})

	writeFiles(t, module, map[string]string{
		"acceptance/acceptance.go": fixtureAcceptance(quietInit(filepath.Join(t.TempDir(), "unused")),
			"RefundOverLimit", "StoreCreditWithinLimit"),
	})
	noRows, _, err := designeval.StoryTests(os.DirFS(module), nil)
	require.NoError(t, err)
	for _, step := range []struct {
		name string
		// leave changes the table's path as the author would.
		leave func(t *testing.T)
		// wantFinding lists fragments of the one generation finding; empty
		// means the copy is accepted.
		wantFinding []string
	}{
		{name: "the table deleted", leave: func(t *testing.T) { require.NoError(t, os.Remove(tablePath)) }},
		{name: "the table edited to require nothing", leave: func(t *testing.T) {
			require.NoError(t, os.WriteFile(tablePath, noRows, 0o600))
		}},
		{name: "a link at the table's path", leave: func(t *testing.T) {
			require.NoError(t, os.Remove(tablePath))
			require.NoError(t, os.Symlink(outside, tablePath))
		}},
		{name: "a sibling file that empties the rows", leave: func(t *testing.T) {
			writeFiles(t, module, map[string]string{"design/aaa.go": "package design\n\nfunc init() { storyScenarios = nil }\n"})
		}, wantFinding: []string{"storyScenarios"}},
		{name: "a folder at the table's path", leave: func(t *testing.T) {
			require.NoError(t, os.Remove(tablePath))
			writeFiles(t, module, map[string]string{designeval.StoryTestsPath + "/kept.txt": "mine\n"})
		}, wantFinding: []string{"eval/design/story_tests.go is a folder", "delete the folder"}},
	} {
		t.Run(step.name, func(t *testing.T) {
			step.leave(t)
			t.Cleanup(func() {
				require.NoError(t, os.RemoveAll(tablePath))
				require.NoError(t, os.RemoveAll(filepath.Join(module, "design", "aaa.go")))
				require.NoError(t, os.WriteFile(tablePath, want, 0o600))
			})
			v, err := testChecker.Check(t.Context(), wc, fixtureSuite, refundStories)
			require.NoError(t, err)
			assertNoAbsolutePath(t, v.Findings)
			if len(step.wantFinding) > 0 {
				require.Len(t, v.Findings, 1, "%+v", v.Findings)
				assert.Equal(t, FindingGeneration, v.Findings[0].Kind)
				for _, w := range step.wantFinding {
					assert.Contains(t, v.Findings[0].Message, w)
				}
				return
			}
			require.True(t, v.Accepted(), "%+v", v.Findings)
			commit, err := Record(t.Context(), v, "Add the accepted tests")
			require.NoError(t, err)
			assert.Equal(t, strings.TrimSpace(string(want)), runGit(t, repo, "show", commit+":eval/"+designeval.StoryTestsPath),
				"the commit holds Surla's table, not the author's")
			assertOutsideUntouched(t)
		})
	}

	t.Run("Prepare at a report restores an edited table", func(t *testing.T) {
		require.NoError(t, os.WriteFile(tablePath, noRows, 0o600))
		_, err := testChecker.Prepare(t.Context(), wc, fixtureSuite, refundStories)
		require.NoError(t, err)
		assertSurlasTable(t)
		v, err := testChecker.Check(t.Context(), wc, fixtureSuite, refundStories)
		require.NoError(t, err)
		require.True(t, v.Accepted(), "%+v", v.Findings)
		_, err = Record(t.Context(), v, "Add the accepted tests")
		require.NoError(t, err)
		assert.Empty(t, runGit(t, wc.Dir(), "status", "--porcelain", "--untracked-files=all"), "the copy holds what was recorded")
	})

	t.Run("a revision removes a finished flow", func(t *testing.T) {
		fewer := []designeval.StorySummary{{Name: "Refunds", Flows: refundStories[0].Flows[:1]}}
		required, err := testChecker.Prepare(t.Context(), wc, fixtureSuite, fewer)
		require.NoError(t, err)
		assert.Equal(t, []designeval.StoryScenario{
			{Story: "Refunds", Key: "refund-over-limit", Scenario: "refund_over_limit", Hook: "RefundOverLimit"},
		}, required)
		table, err := os.ReadFile(tablePath)
		require.NoError(t, err)
		assert.NotContains(t, string(table), "store_credit_within_limit")
		suiteGo, err := os.ReadFile(filepath.Join(module, generatedSuite))
		require.NoError(t, err)
		assert.NotContains(t, string(suiteGo), "StoreCreditWithinLimit")
		v, err := testChecker.Check(t.Context(), wc, fixtureSuite, fewer)
		require.NoError(t, err)
		assert.True(t, v.Accepted(), "the left-over method still builds: %+v", v.Findings)
	})
	assert.Empty(t, ranHooks(t, markers))
}
