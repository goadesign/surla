package testwriter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/testmodule"
)

// TestPrepareAndCheckRefuseALinkedFolder places a symbolic link, absolute and
// relative, at each folder on the suite module's path in the working copy of
// a project at services/shop, pointing at the matching folder of the
// repository, so that following it would reach the project's own suite
// module. Prepare refuses with ErrLinkedFolder and Check returns one finding,
// each naming the link, and the project is byte-identical afterwards.
func TestPrepareAndCheckRefuseALinkedFolder(t *testing.T) {
	t.Parallel()
	module := testmodule.Module(t, "example.com/shop/eval", "", map[string]string{"design/design.go": fixtureDesign})
	files := map[string]string{}
	for p, content := range module {
		files["services/shop/eval/"+p] = content
	}
	suite := Suite{Module: "eval", Command: "./cmd/shop-evals"}
	cases := []struct {
		name string
		// check runs Check instead of Prepare.
		check bool
		// link is the linked folder's path below the project. The error names
		// it relative to the copy, as its author sees it, from Prepare as from
		// Check; the copy's own folder is named as the project has it.
		link string
		// target is the repository folder the link points at.
		target string
	}{
		{name: "Prepare, the copy itself", link: Folder, target: "."},
		{name: "Prepare, services", link: Folder + "/services", target: "services"},
		{name: "Prepare, shop", link: Folder + "/services/shop", target: "services/shop"},
		{name: "Prepare, eval", link: Folder + "/services/shop/eval", target: "services/shop/eval"},
		{name: "Check, services", check: true, link: Folder + "/services", target: "services"},
		{name: "Check, shop", check: true, link: Folder + "/services/shop", target: "services/shop"},
		{name: "Check, eval", check: true, link: Folder + "/services/shop/eval", target: "services/shop/eval"},
	}
	for _, tc := range cases {
		for _, relative := range []bool{false, true} {
			name := tc.name + ", absolute"
			if relative {
				name = tc.name + ", relative"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				repo, _ := newProject(t, files)
				project := filepath.Join(repo, "services", "shop")
				wc, err := OpenWorkingCopy(t.Context(), project)
				require.NoError(t, err)
				link := filepath.Join(project, filepath.FromSlash(tc.link))
				target := filepath.Join(repo, filepath.FromSlash(tc.target))
				if relative {
					target, err = filepath.Rel(filepath.Dir(link), target)
					require.NoError(t, err)
				}
				require.NoError(t, os.RemoveAll(link))
				require.NoError(t, os.Symlink(target, link))
				before := projectFiles(t, repo)

				named := Folder
				if tc.link != Folder {
					rel, err := filepath.Rel(filepath.Join(project, Folder), link)
					require.NoError(t, err)
					named = filepath.ToSlash(rel)
				}
				if tc.check {
					v, err := testChecker.Check(t.Context(), wc, suite, refundStories)
					assert.Equal(t, before, projectFiles(t, repo), "the project is byte-identical")
					require.NoError(t, err)
					require.Len(t, v.Findings, 1)
					assert.Equal(t, FindingGeneration, v.Findings[0].Kind)
					assert.Contains(t, v.Findings[0].Message, ErrLinkedFolder.Error())
					assert.Contains(t, v.Findings[0].Message, ": "+named+" is a symbolic link")
					return
				}
				_, err = testChecker.Prepare(t.Context(), wc, suite, nil)
				assert.Equal(t, before, projectFiles(t, repo), "the project is byte-identical")
				require.ErrorIs(t, err, ErrLinkedFolder)
				var author *AuthorError
				require.ErrorAs(t, err, &author, "a link the author placed is the author's to fix")
				assert.Equal(t, FindingGeneration, author.Finding.Kind)
				assert.Contains(t, author.Finding.Message, ": "+named+" is a symbolic link",
					"the link is named relative to the copy, as Check names it")
				assert.Contains(t, author.Finding.Message, "The suite module services/shop/eval could not be opened",
					"the module is named relative to the copy")
				if tc.link != Folder {
					assert.NotContains(t, author.Finding.Message, Folder, "no path is given from the project folder")
				}
			})
		}
	}
}

// TestPrepareSeparatesFailuresTheAuthorCannotFix runs Prepare where the
// failure is not the copy's author's: the caller's context already ended, or
// the evaluator was closed. Each is a plain error, not an *AuthorError, and
// the copy's generated folder is not written.
func TestPrepareSeparatesFailuresTheAuthorCannotFix(t *testing.T) {
	t.Parallel()
	module := testmodule.Module(t, "example.com/shop/eval", "", map[string]string{"design/design.go": fixtureDesign})
	files := map[string]string{}
	for p, content := range module {
		files["eval/"+p] = content
	}
	closed := designeval.NewEvaluatorInDir(t.TempDir())
	require.NoError(t, closed.Close())
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	cases := []struct {
		name    string
		checker *Checker
		ctx     context.Context
	}{
		{name: "the caller's context ended", checker: testChecker, ctx: ended},
		{name: "the evaluator was closed", checker: NewChecker(closed, t.TempDir()), ctx: t.Context()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, _ := newProject(t, files)
			wc, err := OpenWorkingCopy(t.Context(), repo)
			require.NoError(t, err)
			_, err = tc.checker.Prepare(tc.ctx, wc, Suite{Module: "eval", Command: "./cmd/shop-evals"}, nil)
			require.Error(t, err)
			var author *AuthorError
			assert.False(t, errors.As(err, &author), "not the author's to fix: %v", err)
			assert.NoDirExists(t, filepath.Join(wc.Project(), "eval", "gen"))
		})
	}
}
