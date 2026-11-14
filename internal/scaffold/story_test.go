// This file checks that a starter ships its Story and requires the Story
// language at the version this Surla evaluates.
package scaffold

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/modfile"

	"github.com/goadesign/surla"
	"github.com/goadesign/surla/internal/designeval"
)

// TestStarterRequiresAndShipsItsStory checks that newData copies the Story
// module version Surla's go.mod requires, that the starter's go.mod requires
// exactly that version, and that design/story.go, whose import of the Story
// language is what keeps the requirement through go mod tidy, reaches the
// generator with the other design files. It also checks that the starter
// holds the test its Story's finished flow requires and that its README names
// both. The Go command and the generator are stand-ins.
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

	// A build requires the method PricesCookie for flow prices-cookie; the
	// starter's real suite test proves that pairing with the generator.
	hooks, err := os.ReadFile(filepath.Join(dir, "eval", "acceptance", "acceptance.go"))
	require.NoError(t, err)
	assert.Contains(t, string(hooks), "func (h *Hooks) PricesCookie(ctx context.Context) (eval.Result, error)",
		"the starter ships the test its Story's finished flow requires")
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	for _, named := range []string{"`design/story.go`", "`prices-cookie`", "`PricesCookie`", "`eval/design/story_tests.go`"} {
		assert.Contains(t, string(readme), named, "the README says where the Story and its test are")
	}

	// The test module's design holds the Story table rendered from the
	// starter's Story, and its generation sees it.
	require.Len(t, gen.inputs, 2)
	want, requiredTests, err := designeval.StoryTests(os.DirFS(filepath.Join(dir, "eval")), starterStory)
	require.NoError(t, err)
	assert.Equal(t, []designeval.StoryScenario{
		{Story: "Prices", Key: "prices-cookie", Scenario: "prices_cookie", Hook: "PricesCookie"},
	}, requiredTests)
	assert.Equal(t, string(want), gen.inputs[1][designeval.StoryTestsPath], "the test module is generated with its table")
	shippedTable, err := os.ReadFile(filepath.Join(dir, "eval", filepath.FromSlash(designeval.StoryTestsPath)))
	require.NoError(t, err)
	assert.Equal(t, string(want), string(shippedTable), "the project holds the table the generator saw")
	assert.NotContains(t, gen.inputs[0], designeval.StoryTestsPath, "the service module has no table")
}

// TestStarterStoryMatchesTheShippedStory checks the literal summary Init
// renders the starter's Story table from against the Story template, without
// evaluating it: each of its flows is declared there with its key and title,
// and the template declares no other flow. The starter's scaffold-tagged
// suite compares the literal with the evaluated Story.
func TestStarterStoryMatchesTheShippedStory(t *testing.T) {
	raw, err := templatesFS.ReadFile("templates/candidate/design/story.go.tmpl")
	require.NoError(t, err)
	story := string(raw)
	require.Len(t, starterStory, 1)
	assert.Contains(t, story, fmt.Sprintf("Story(%q,", starterStory[0].Name))
	assert.Equal(t, len(starterStory[0].Flows), strings.Count(story, "Flow("))
	for _, flow := range starterStory[0].Flows {
		assert.Contains(t, story, fmt.Sprintf("Flow(%q, %q,", flow.Key, flow.Title))
		assert.True(t, flow.Complete, "the starter's flow has a start, an outcome and no question")
	}
	assert.NotContains(t, story, "Question(", "no flow of the starter is unfinished")
}
