// This file keeps the Story module in this repository and the published
// version Surla requires the same, so new starters, which copy that version,
// get the Story language this Surla evaluates.
package surla

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/modfile"
)

// storyModule is the portable Story module, kept in story/.
const storyModule = "github.com/goadesign/surla/story"

// TestStoryRequirementMatchesTheStoryFolder fails when the Story language in
// story/ differs from the Story module version Surla's go.mod requires:
// Surla's Story reader is compiled against that version, and every new
// starter requires it. It compares what the Go command compiles (Go sources
// and the module files). Markdown documents (files ending in .md) are left
// out, because a document cannot change the language and should not force a
// new version; that holds only while the module embeds no file, so a Go file
// in story/ that contains the text of an embed directive fails the test too.
// The published module carries the repository's LICENSE, which story/ does
// not.
func TestStoryRequirementMatchesTheStoryFolder(t *testing.T) {
	mod, err := modfile.Parse("go.mod", []byte(GoMod()), nil)
	require.NoError(t, err)
	version := ""
	for _, r := range mod.Require {
		if r.Mod.Path == storyModule {
			version = r.Mod.Version
		}
	}
	require.NotEmpty(t, version, "Surla's go.mod must require %s: the design evaluator's Story reader imports it", storyModule)

	download := exec.Command("go", "mod", "download", "-json", storyModule+"@"+version)
	download.Env = append(os.Environ(), "GOWORK=off")
	var stderr bytes.Buffer
	download.Stderr = &stderr
	out, err := download.Output()
	require.NoError(t, err, stderr.String())
	var published struct {
		Dir string
	}
	require.NoError(t, json.Unmarshal(out, &published))
	require.NotEmpty(t, published.Dir)

	want := moduleFiles(t, published.Dir)
	license, err := os.ReadFile("LICENSE")
	require.NoError(t, err)
	if published, ok := want["LICENSE"]; ok && bytes.Equal(published, license) {
		delete(want, "LICENSE")
	}
	got := moduleFiles(t, "story")
	var differ []string
	for name, content := range got {
		theirs, ok := want[name]
		switch {
		case !ok:
			differ = append(differ, "story/"+name+" (not in the published version)")
		case !bytes.Equal(content, theirs):
			differ = append(differ, "story/"+name)
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			differ = append(differ, "story/"+name+" (only in the published version)")
		}
	}
	slices.Sort(differ)
	assert.Empty(t, differ, "story/ differs from %s@%s, the version Surla's go.mod requires and new "+
		"starters copy. Push the Story change, then raise the requirement to the pushed commit: "+
		"go get %s@<commit> && go mod tidy", storyModule, version, storyModule)
}

// moduleFiles returns every regular file under dir that is not a Markdown
// document, by its slash-separated path relative to dir. It fails the test
// when a Go file contains the text of an embed directive: an embedded
// document would be part of the compiled module, and leaving documents out
// would then hide a difference. The check is on the text, so a comment or a
// string that only mentions the directive fails too, which errs on the safe
// side.
func moduleFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	require.NoError(t, filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if filepath.Ext(path) == ".md" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if filepath.Ext(path) == ".go" {
			assert.False(t, bytes.Contains(content, []byte("//go:embed")), "%s contains an embed "+
				"directive: compare embedded documents too before leaving Markdown files out of this test", path)
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = content
		return nil
	}))
	return files
}
