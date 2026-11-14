package testwriter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/shquote"
	"github.com/goadesign/surla/internal/testmodule"
)

// TestPrepareAndCheckRefuseALinkedFolder places a symbolic link, absolute and
// relative, at each folder on the suite module's path in the working copy of
// a project at services/shop, pointing at the matching folder of the
// repository, so that following it would reach the project's own suite
// module. Prepare refuses with ErrLinkedFolder, naming the link. Check names
// the link too: as a change outside the test module for a folder above it,
// and as a link on the module's path for the module's own folder. The project
// is byte-identical afterwards.
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
					require.NotEmpty(t, v.Findings)
					if named != "services/shop/eval" {
						assert.Equal(t, FindingOutsideModule, v.Findings[0].Kind)
						assert.Contains(t, v.Findings[0].Message, named+" is changed outside the test module services/shop/eval")
						return
					}
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
// the evaluator was closed. Each is an error marked ErrGeneration, not an
// *AuthorError, and the copy's generated folder is not written.
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
			assert.ErrorIs(t, err, ErrGeneration, "the generator failed, not the writing of its result")
			assert.NoDirExists(t, filepath.Join(wc.Project(), "eval", "gen"))
		})
	}
}

// TestPrepareGivesAPlainErrorWhenTheCallerStopsDuringAModulePhase ends the
// caller's context while the generator waits on the toolchain's go in a
// phase that works on the module's own files. The killed command's error
// carries that phase, so designeval.FailedOnModule alone would blame the
// module; Prepare still returns a plain error, not an *AuthorError, because
// the context ended.
func TestPrepareGivesAPlainErrorWhenTheCallerStopsDuringAModulePhase(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Prepare refuses before generating where the sandbox does not exist")
	}
	repo, _ := newProject(t, map[string]string{
		"eval/go.mod":           "module example.com/shop/eval\n\ngo 1.24\n",
		"eval/go.sum":           "",
		"eval/design/design.go": "package design\n",
	})
	wc, err := OpenWorkingCopy(t.Context(), repo)
	require.NoError(t, err)

	// The toolchain's go records that it started, then hangs, as a slow
	// download of the module's dependencies would, until it is killed. The go
	// on PATH answers go env GOROOT GOMODCACHE with that toolchain.
	t.Setenv("TMPDIR", t.TempDir())
	goroot, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	reached := filepath.Join(goroot, "reached")
	require.NoError(t, os.Mkdir(filepath.Join(goroot, "bin"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(goroot, "bin", "go"),
		[]byte("#!/bin/sh\n: > "+shquote.Quote(reached)+"\nexec sleep 600\n"), 0o700))
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nprintf '%s\\n' "+
		shquote.Quote(goroot)+" "+shquote.Quote(filepath.Join(goroot, "modcache"))+"\n"), 0o700))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	evaluator := designeval.NewEvaluatorInDir(t.TempDir())
	t.Cleanup(func() { assert.NoError(t, evaluator.Close()) })

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := NewChecker(evaluator, t.TempDir()).Prepare(ctx, wc, Suite{Module: "eval", Command: "./gen/cmd/shop-evals"}, refundStories)
		done <- err
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(reached)
		return err == nil
	}, 20*time.Second, 10*time.Millisecond, "the generator reaches the toolchain's go")
	cancel()
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		require.FailNow(t, "Prepare did not return within 20 seconds of the cancellation")
	}
	require.Error(t, err)
	assert.True(t, designeval.FailedOnModule(err), "the killed command's error carries a module phase: %v", err)
	var author *AuthorError
	assert.False(t, errors.As(err, &author), "not the author's to fix: %v", err)
	assert.NoDirExists(t, filepath.Join(wc.Project(), "eval", "gen"))
}
