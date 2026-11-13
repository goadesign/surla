// This file checks that a starter ships its Story and requires the Story
// language at the version this Surla evaluates.
package scaffold

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/modfile"

	"github.com/goadesign/surla"
)

// TestStarterRequiresAndShipsItsStory checks that newData copies the Story
// module version Surla's go.mod requires, that the starter's go.mod requires
// exactly that version, and that design/story.go, whose import of the Story
// language is what keeps the requirement through go mod tidy, reaches the
// generator with the other design files. The Go command and the generator are
// stand-ins.
func TestStarterRequiresAndShipsItsStory(t *testing.T) {
	surlaMod, err := modfile.Parse("go.mod", []byte(surla.GoMod()), nil)
	require.NoError(t, err)
	required := ""
	for _, r := range surlaMod.Require {
		if r.Mod.Path == storyModule {
			required = r.Mod.Version
		}
	}
	require.NotEmpty(t, required, "Surla's go.mod requires the Story module")
	d, err := newData(Options{Module: "example.com/shop", SurlaVersion: published})
	require.NoError(t, err)
	assert.Equal(t, required, d.StoryVersion)

	dir := filepath.Join(t.TempDir(), "shop")
	run := func(_ context.Context, moduleDir, _ string, args ...string) error { return fakeGo(moduleDir, args) }
	gen := &fakeGenerator{pkgs: []string{"shop"}, extra: map[string]string{quickstartGuide: "guide\n"}}
	_, err = Init(t.Context(), Options{
		Dir: dir, WorkDir: t.TempDir(), Module: "example.com/shop", SurlaVersion: published,
	}, run, gen)
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	require.NoError(t, err)
	starterMod, err := modfile.Parse("go.mod", raw, nil)
	require.NoError(t, err)
	versions := map[string]string{}
	for _, r := range starterMod.Require {
		versions[r.Mod.Path] = r.Mod.Version
	}
	assert.Equal(t, required, versions[storyModule], "the starter requires the Story language Surla evaluates")

	require.NotEmpty(t, gen.inputs)
	story := gen.inputs[0]["design/story.go"]
	assert.Contains(t, story, `import . "github.com/goadesign/surla/story/dsl"`)
	assert.Contains(t, story, `Flow("prices-cookie", "Price a cookie"`, "one finished flow, apart from the hand-written tests")
	assert.NotContains(t, story, "[[", "every template field is rendered")
	shipped, err := os.ReadFile(filepath.Join(dir, "design", "story.go"))
	require.NoError(t, err)
	assert.Equal(t, story, string(shipped), "the project holds the Story the generator saw")
}
