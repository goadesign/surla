package testwriter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWithinLimitIsInclusiveAtTheDeadline runs a stand-in that sleeps just
// below, exactly at and just above a limit: only below is the limit not
// reached, and a caller's context that ended first is never the limit.
func TestWithinLimitIsInclusiveAtTheDeadline(t *testing.T) {
	t.Parallel()
	const limit = 300 * time.Millisecond
	sleep := func(d time.Duration) func(context.Context) error {
		return func(ctx context.Context) error {
			timer := time.NewTimer(d)
			defer timer.Stop()
			select {
			case <-timer.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	for _, tc := range []struct {
		name  string
		sleep time.Duration
		want  bool
	}{
		{name: "just below", sleep: limit - 100*time.Millisecond, want: false},
		{name: "at", sleep: limit, want: true},
		{name: "just above", sleep: limit + 100*time.Millisecond, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reached, _ := withinLimit(t.Context(), limit, sleep(tc.sleep))
			assert.Equal(t, tc.want, reached)
		})
	}
	t.Run("the caller's context ends first", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), limit/3)
		defer cancel()
		reached, err := withinLimit(ctx, limit, sleep(limit+100*time.Millisecond))
		assert.False(t, reached)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})
	assert.Equal(t, "10 minutes", limitText(buildProgramLimit))
	assert.Equal(t, "60 seconds", limitText(listTestsLimit))
}

// TestWithoutAbsolutePathsKeepsURLsImportsAndRelativePaths replaces absolute
// file paths wherever a word starts and leaves URLs, import paths and
// relative paths untouched.
func TestWithoutAbsolutePathsKeepsURLsImportsAndRelativePaths(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in, want string
	}{
		{in: "see https://example.com/a/b and http://127.0.0.1:8080/x", want: "see https://example.com/a/b and http://127.0.0.1:8080/x"},
		{in: "import github.com/goadesign/surla/verify/suite", want: "import github.com/goadesign/surla/verify/suite"},
		{in: "build ./gen/cmd/shop-evals and gen/evals/x/suite.go", want: "build ./gen/cmd/shop-evals and gen/evals/x/suite.go"},
		{in: "open /Users/someone/project/x.h: operation not permitted", want: "open <absolute path>: operation not permitted"},
		{in: "/private/var/folders/x/main.go:12:3: undefined: Missing", want: "<absolute path>:12:3: undefined: Missing"},
		{in: `read "/tmp/a b"`, want: `read "<absolute path> b"`},
		{in: "(at /opt/go/src/x.go)", want: "(at <absolute path>)"},
		{in: "line one\n/abs/second/line", want: "line one\n<absolute path>"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			assert.Equal(t, tc.want, WithoutAbsolutePaths(tc.in))
		})
	}
}

// TestCheckNamesFilesChangedOutsideTheTestModule changes files outside the
// copy's test module, uncommitted and committed on the tests branch as its
// author could: Check names each one before generating anything, while a
// change inside the module is not named, and past the bound counts the rest.
func TestCheckNamesFilesChangedOutsideTheTestModule(t *testing.T) {
	t.Parallel()
	suite := Suite{Module: "eval", Command: "./gen/cmd/shop-evals"}
	t.Run("each change outside", func(t *testing.T) {
		t.Parallel()
		repo, _ := newProject(t, map[string]string{
			"README.md": "shop\n", "docs/a.md": "a\n", "docs/b.md": "b\n", "eval/go.mod": "module example.com/shop/eval\n",
		})
		wc, err := OpenWorkingCopy(t.Context(), repo)
		require.NoError(t, err)
		writeFiles(t, wc.Dir(), map[string]string{"docs/b.md": "committed by the author\n"})
		runGit(t, wc.Dir(), "commit", "--quiet", "-am", "The author's own commit")
		writeFiles(t, wc.Dir(), map[string]string{
			"README.md": "edited\n", "notes.txt": "added\n", "eval/hooks.go": "package eval\n",
		})
		require.NoError(t, os.Remove(filepath.Join(wc.Dir(), "docs", "a.md")))

		v, err := testChecker.Check(t.Context(), wc, suite, nil)
		require.NoError(t, err)
		var named []string
		for _, f := range v.Findings {
			assert.Equal(t, FindingOutsideModule, f.Kind)
			named = append(named, f.Message)
		}
		require.Len(t, named, 4, "%v", named)
		for _, p := range []string{"README.md", "docs/a.md", "docs/b.md", "notes.txt"} {
			assert.Contains(t, fmt.Sprint(named), p+" is changed outside the test module eval")
		}
		assert.NotContains(t, fmt.Sprint(named), "eval/hooks.go")
		assert.Contains(t, named[0], "If you never changed it, the project's history was rewritten",
			"a finding also explains a file the test writer never touched")
		assertNoAbsolutePath(t, v.Findings)
	})
	t.Run("a replace ref", func(t *testing.T) {
		t.Parallel()
		repo, _ := newProject(t, map[string]string{"README.md": "shop\n", "eval/go.mod": "module example.com/shop/eval\n"})
		base := runGit(t, repo, "rev-parse", "HEAD")
		wc, err := OpenWorkingCopy(t.Context(), repo)
		require.NoError(t, err)
		writeFiles(t, wc.Dir(), map[string]string{"README.md": "committed by the author\n"})
		runGit(t, wc.Dir(), "commit", "--quiet", "-am", "The author's own commit")
		// The author makes the base commit stand for one that already holds
		// its change, so a git that honours replace refs sees no change.
		fake := runGit(t, wc.Dir(), "commit-tree", "--no-gpg-sign", "HEAD^{tree}", "-m", "Start the project")
		runGit(t, wc.Dir(), "replace", base, fake)

		v, err := testChecker.Check(t.Context(), wc, suite, nil)
		require.NoError(t, err)
		require.Len(t, v.Findings, 1, "%+v", v.Findings)
		assert.Equal(t, FindingOutsideModule, v.Findings[0].Kind)
		assert.Contains(t, v.Findings[0].Message, "README.md is changed outside the test module eval")
	})
	t.Run("more than the bound", func(t *testing.T) {
		t.Parallel()
		repo, _ := newProject(t, map[string]string{"README.md": "shop\n", "eval/go.mod": "module example.com/shop/eval\n"})
		wc, err := OpenWorkingCopy(t.Context(), repo)
		require.NoError(t, err)
		added := map[string]string{}
		for i := range maxOutsideFindings + 5 {
			added[fmt.Sprintf("extra/%02d.txt", i)] = "added\n"
		}
		writeFiles(t, wc.Dir(), added)
		v, err := testChecker.Check(t.Context(), wc, suite, nil)
		require.NoError(t, err)
		require.Len(t, v.Findings, maxOutsideFindings+1)
		assert.Contains(t, v.Findings[maxOutsideFindings].Message, "5 more files outside the test module eval")
	})
}

// TestCheckStopsWhenTheHistoryIsReplaced replaces the project's history with
// an unrelated one, as rewriting it from its first commit does: Check stops
// with ErrNoCommonBase, whose text names the step.
func TestCheckStopsWhenTheHistoryIsReplaced(t *testing.T) {
	t.Parallel()
	repo, _ := newProject(t, map[string]string{"README.md": "shop\n", "eval/go.mod": "module example.com/shop/eval\n"})
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)
	runGit(t, repo, "switch", "--quiet", "--orphan", "rewritten")
	writeFiles(t, repo, map[string]string{"README.md": "shop\n", "eval/go.mod": "module example.com/shop/eval\n"})
	runGit(t, repo, "add", "README.md", "eval/go.mod")
	runGit(t, repo, "commit", "--quiet", "-m", "A new history")

	_, err = testChecker.Check(t.Context(), wc, Suite{Module: "eval", Command: "./gen/cmd/shop-evals"}, nil)
	require.ErrorIs(t, err, ErrNoCommonBase)
}
