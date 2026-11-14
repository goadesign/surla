package testwriter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// unreachable is a module no proxy serves, and the requirement that names it.
const (
	unreachable            = "example.com/unreachable"
	unreachableRequirement = "require " + unreachable + " v1.0.0\n"
)

// failingDownload is a stand-in for the dependency download that fails as
// with no network: for every module, or only for one whose go.mod holds
// marker when marker is not empty. It counts its calls in calls.
func failingDownload(marker string, calls *int) func(context.Context, string, string) error {
	return func(_ context.Context, _, dir string) error {
		*calls++
		mod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			return err
		}
		if marker == "" || strings.Contains(string(mod), marker) {
			return errors.New("exit status 1")
		}
		return nil
	}
}

// TestALoadFailureOutsideTheModuleIsNotTheAuthorsFinding prepares a copy
// whose design imports a module that cannot be loaded, so generation fails
// while loading the module's dependencies, and decides whose failure it is
// from what the download can do. When the copy's go.mod and go.sum are the
// project's own, or the project's own cannot be downloaded either, the
// failure is ErrDependencies, for the user. When the project's own download
// and only the author's do not, or everything downloads and the module's
// files are at fault, it stays the author's finding: the author cannot hide a
// defect of its own module behind a failed download.
func TestALoadFailureOutsideTheModuleIsNotTheAuthorsFinding(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Prepare refuses before generating where the sandbox does not exist")
	}
	// No go command of this test may reach the network.
	t.Setenv("GOPROXY", "off")
	const (
		plainMod   = "module example.com/shop/eval\n\ngo 1.24\n"
		plain      = "package design\n"
		importing  = "package design\n\nimport _ \"" + unreachable + "/dsl\"\n"
		unloadable = plainMod + "\n" + unreachableRequirement
	)
	cases := []struct {
		name string
		// project is the test module the project commits; edits are the
		// author's uncommitted changes to it in the copy.
		project, edits map[string]string
		// failsFor is the go.mod text whose download fails; empty fails all,
		// and "nothing" none.
		failsFor string
		// wantAuthor is whether the failure is the author's finding.
		wantAuthor bool
	}{
		{
			name:    "the project's own lists cannot be downloaded",
			project: map[string]string{"go.mod": unloadable, "go.sum": "", "design/design.go": importing},
		},
		{
			name:       "only the lists the author changed cannot be downloaded",
			project:    map[string]string{"go.mod": plainMod, "go.sum": "", "design/design.go": plain},
			edits:      map[string]string{"go.mod": unloadable, "design/design.go": importing},
			failsFor:   unreachable,
			wantAuthor: true,
		},
		{
			name:    "the author changed the lists and the project's own cannot be downloaded either",
			project: map[string]string{"go.mod": plainMod, "go.sum": "", "design/design.go": plain},
			edits:   map[string]string{"go.mod": unloadable, "design/design.go": importing},
		},
		{
			name:       "everything downloads, so the module's files are at fault",
			project:    map[string]string{"go.mod": unloadable, "go.sum": "", "design/design.go": importing},
			failsFor:   "nothing",
			wantAuthor: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			for p, content := range tc.project {
				files["eval/"+p] = content
			}
			repo, _ := newProject(t, files)
			base := runGit(t, repo, "rev-parse", "HEAD")
			wc, err := OpenWorkingCopy(t.Context(), repo)
			require.NoError(t, err)
			writeFiles(t, filepath.Join(wc.Project(), "eval"), tc.edits)
			var downloads int
			c := *testChecker
			c.download = failingDownload(tc.failsFor, &downloads)

			_, err = c.Prepare(t.Context(), wc, Suite{Module: "eval", Command: "./gen/cmd/shop-evals"}, nil)
			require.Error(t, err)
			assert.Positive(t, downloads, "the download decides whose failure it is")
			var author *AuthorError
			if tc.wantAuthor {
				require.ErrorAs(t, err, &author, "the author's to fix: %v", err)
				assert.Equal(t, FindingGeneration, author.Finding.Kind)
				assert.Contains(t, author.Finding.Message, "loading the design module dependencies")
				assert.NotErrorIs(t, err, ErrDependencies)
				return
			}
			require.ErrorIs(t, err, ErrDependencies)
			assert.False(t, errors.As(err, &author), "not the author's to fix: %v", err)
			assert.Contains(t, err.Error(), "commit "+base, "the reason names the project's commit")
			assert.NotRegexp(t, absolutePath, err.Error())
			assert.NotContains(t, err.Error(), "exit status", "nothing of the download's own failure is passed on")
		})
	}
}

// TestADownloadFailureOutsideTheModuleIsNotTheAuthorsFinding checks a copy
// whose required tests generate and whose dependencies then cannot be
// downloaded. With the project's own go.mod and go.sum the check could not
// run, with ErrDependencies. Once the author has changed go.mod, the check
// downloads the project's own lists to compare: when those download, the
// finding is the author's and names the commit it was compared with; when
// they do not, the check could not run.
func TestADownloadFailureOutsideTheModuleIsNotTheAuthorsFinding(t *testing.T) {
	skipWithoutSandbox(t)
	const edit = "\n// The author's change.\n"
	cases := []struct {
		name string
		// edited is whether the author changed go.mod in the copy.
		edited bool
		// failsFor is the go.mod text whose download fails; empty fails all.
		failsFor string
		// wantAuthor is whether the failure is the author's finding.
		wantAuthor bool
	}{
		{name: "the project's own lists"},
		{name: "only the lists the author changed", edited: true, failsFor: edit, wantAuthor: true},
		{name: "the author's lists and the project's own", edited: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := newProject(t, fixtureProject(t))
			base := runGit(t, repo, "rev-parse", "HEAD")
			wc, err := OpenWorkingCopy(t.Context(), repo)
			require.NoError(t, err)
			if tc.edited {
				goMod := filepath.Join(wc.Project(), "eval", "go.mod")
				raw, err := os.ReadFile(goMod)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(goMod, append(raw, edit...), 0o600))
			}
			var downloads int
			c := *testChecker
			c.download = failingDownload(tc.failsFor, &downloads)

			v, err := c.Check(t.Context(), wc, fixtureSuite, refundStories)
			if tc.wantAuthor {
				require.NoError(t, err)
				require.Len(t, v.Findings, 1, "%+v", v.Findings)
				assert.Equal(t, FindingDependencies, v.Findings[0].Kind)
				assert.Contains(t, v.Findings[0].Message, "commit "+base)
				assertNoAbsolutePath(t, v.Findings)
				assert.Equal(t, 2, downloads, "the copy's lists, then the project's own")
				return
			}
			require.ErrorIs(t, err, ErrDependencies)
			assert.Empty(t, v.Findings, "no finding for the test writer")
			assert.Contains(t, err.Error(), "commit "+base)
			assert.NotRegexp(t, absolutePath, err.Error())
		})
	}
}

// TestDependencyFailureComparesWithTheProjectsOwnLists decides whose failure
// a failed download is for each relation between the copy's go.mod and go.sum
// and the project's own. Lists that are the project's own, and lists the
// author changed while the project's own cannot be downloaded either, are
// ErrDependencies. Lists the author changed while the project's own download
// are the author's, compared with the base commit. When the project's commit
// gives nothing to compare with, the failure stays the author's and nothing
// is downloaded. The folder a comparison downloads in is removed.
func TestDependencyFailureComparesWithTheProjectsOwnLists(t *testing.T) {
	t.Parallel()
	const (
		plainMod = "module example.com/shop/eval\n\ngo 1.24"
		edited   = plainMod + "\n\n" + unreachableRequirement
	)
	cases := []struct {
		name string
		// project is what the project commits.
		project map[string]string
		// current is the copy's lists.
		current dependencyLists
		// failsFor is the go.mod text whose download fails; empty fails all.
		failsFor string
		// wantOutside is whether the cause is outside the author's changes.
		wantOutside bool
		// wantCompared is whether the author's failure names the base.
		wantCompared bool
		// wantDownloads counts the downloads of the project's own lists.
		wantDownloads int
	}{
		{
			name:        "the project's own lists",
			project:     map[string]string{"eval/go.mod": plainMod + "\n", "eval/go.sum": "sum\n"},
			current:     dependencyLists{mod: plainMod, sum: "sum", hasSum: true},
			wantOutside: true,
		},
		{
			name:     "lists the author changed, while the project's own download",
			project:  map[string]string{"eval/go.mod": plainMod + "\n", "eval/go.sum": "sum\n"},
			current:  dependencyLists{mod: strings.TrimSuffix(edited, "\n"), sum: "sum", hasSum: true},
			failsFor: unreachable, wantCompared: true, wantDownloads: 1,
		},
		{
			name:        "lists the author changed, while the project's own do not download either",
			project:     map[string]string{"eval/go.mod": plainMod + "\n", "eval/go.sum": "sum\n"},
			current:     dependencyLists{mod: strings.TrimSuffix(edited, "\n"), sum: "sum", hasSum: true},
			wantOutside: true, wantDownloads: 1,
		},
		{
			name:     "a go.sum the author added to a project without one",
			project:  map[string]string{"eval/go.mod": plainMod + "\n"},
			current:  dependencyLists{mod: plainMod, sum: "sum", hasSum: true},
			failsFor: unreachable, wantCompared: true, wantDownloads: 1,
		},
		{
			name:    "a module the project does not have",
			project: map[string]string{"README.md": "shop\n"},
			current: dependencyLists{mod: plainMod},
		},
		{
			name: "a project whose go.mod replaces a module with a local folder",
			project: map[string]string{
				"eval/go.mod": plainMod + "\n\nreplace example.com/shop => ../\n", "eval/go.sum": "",
			},
			current: dependencyLists{mod: plainMod, hasSum: true},
		},
		{
			name:    "a project whose go.mod cannot be parsed",
			project: map[string]string{"eval/go.mod": "not a go.mod\n", "eval/go.sum": ""},
			current: dependencyLists{mod: plainMod, hasSum: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo, _ := newProject(t, tc.project)
			base := runGit(t, repo, "rev-parse", "HEAD")
			wc, err := OpenWorkingCopy(t.Context(), repo)
			require.NoError(t, err)
			var downloads int
			c := *testChecker
			c.workDir = t.TempDir()
			c.download = failingDownload(tc.failsFor, &downloads)

			compared, err := c.dependencyFailure(t.Context(), wc, "go", "eval", tc.current)
			assert.Equal(t, tc.wantDownloads, downloads)
			left, readErr := os.ReadDir(c.workDir)
			require.NoError(t, readErr)
			assert.Empty(t, left, "the folder the comparison downloaded in is removed")
			if tc.wantOutside {
				require.ErrorIs(t, err, ErrDependencies)
				assert.Contains(t, err.Error(), "commit "+base)
				assert.Empty(t, compared)
				return
			}
			require.NoError(t, err)
			if tc.wantCompared {
				assert.Equal(t, base, compared)
			} else {
				assert.Empty(t, compared, "nothing to compare with")
			}
		})
	}
}

// TestDownloadDependenciesFailsForAModuleItCannotFetch runs the real download
// with fetching turned off and an empty module cache: a module that requires
// nothing downloads, and one that requires a module it cannot fetch fails.
func TestDownloadDependenciesFailsForAModuleItCannotFetch(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	t.Setenv("GOMODCACHE", t.TempDir())
	tc, err := resolveToolchain(t.Context(), t.TempDir())
	require.NoError(t, err)
	for requirement, wantErr := range map[string]bool{"": false, "\n" + unreachableRequirement: true} {
		dir := t.TempDir()
		writeFiles(t, dir, map[string]string{"go.mod": "module example.com/shop/eval\n\ngo 1.24\n" + requirement})
		err := downloadDependencies(t.Context(), tc.goBin(), dir)
		assert.Equal(t, wantErr, err != nil, "requirement %q: %v", requirement, err)
	}
}
