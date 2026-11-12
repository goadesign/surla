package designeval

import (
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	evalexpr "goa.design/goa-ai/eval/expr"
	"goa.design/goa/v3/codegen"
	"goa.design/goa/v3/eval"
)

// quickstartNames are Goa-AI's named AGENTS_QUICKSTART.md sections in render
// order, as documented in its docs/dsl.md.
var quickstartNames = []string{
	"header", "design", "liftoff", "liftoff-guide", "agents", "planner", "tools",
	"toolbox", "exports", "evaluations", "advanced", "golden-rules", "faq",
}

// TestSurlaQuickstartReplacesOnlyWorkflowSections checks which sections the
// plugin replaces for a design with and without evaluation suites, that
// Goa-AI's other sections and other files keep their source, and that a
// guide from a Goa-AI release without named sections is left alone.
func TestSurlaQuickstartReplacesOnlyWorkflowSections(t *testing.T) {
	withSuite := []eval.Root{&evalexpr.RootExpr{Suites: []*evalexpr.SuiteExpr{{Name: "quality"}}}}
	cases := []struct {
		name     string
		roots    []eval.Root
		replaced []string
	}{
		{"no eval root", nil, []string{"header", "liftoff", "golden-rules", "evaluations"}},
		{"eval root without suites", []eval.Root{&evalexpr.RootExpr{}}, []string{"header", "liftoff", "golden-rules", "evaluations"}},
		{"declared suite", withSuite, []string{"header", "liftoff", "golden-rules"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			guide := namedGuide(quickstartNames)
			other := &codegen.File{Path: "gen/svc/service.go", SectionTemplates: []*codegen.SectionTemplate{{Name: "agents-quickstart-header", Source: "keep"}}}
			out, err := surlaQuickstart("gen", tc.roots, []*codegen.File{guide, other})
			require.NoError(t, err)
			require.Len(t, out, 2)
			for _, s := range guide.SectionTemplates {
				name := strings.TrimPrefix(s.Name, "agents-quickstart-")
				want := "goa-ai " + name
				if slices.Contains(tc.replaced, name) {
					want = quickstartSections[name]
				}
				assert.Equal(t, want, s.Source, "section %s", name)
			}
			assert.Equal(t, "keep", other.SectionTemplates[0].Source, "only AGENTS_QUICKSTART.md changes")
		})
	}

	t.Run("single-section guide", func(t *testing.T) {
		legacy := &codegen.File{Path: quickstartPath, SectionTemplates: []*codegen.SectionTemplate{{Name: "agents-quickstart", Source: "goa-ai guide"}}}
		_, err := surlaQuickstart("gen", nil, []*codegen.File{legacy})
		require.NoError(t, err)
		assert.Equal(t, "goa-ai guide", legacy.SectionTemplates[0].Source)
	})

	t.Run("missing replaced section", func(t *testing.T) {
		guide := namedGuide([]string{"header", "liftoff", "evaluations"})
		_, err := surlaQuickstart("gen", nil, []*codegen.File{guide})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "0 agents-quickstart-golden-rules sections")
	})
}

// TestQuickstartSectionsKeepGoaAIsLayout checks that every replacement text
// keeps the heading and separator layout of the section it replaces, names
// Surla's tools instead of host commands, and ends with the blank line that
// precedes the next section.
func TestQuickstartSectionsKeepGoaAIsLayout(t *testing.T) {
	headings := map[string]string{
		"header":       "# Welcome to Your Goa-AI Agents! 👋\n",
		"liftoff":      "---\n\n## 2. 🚀 The 3-Step Liftoff: Your First Agent Run\n",
		"evaluations":  "---\n\n## 8. 🧪 Evaluating Your Agents\n",
		"golden-rules": "---\n\n## 10. 📜 The Golden Rules: Working with Codegen\n",
	}
	require.Len(t, quickstartSections, len(headings))
	for name, heading := range headings {
		text := quickstartSections[name]
		assert.True(t, strings.HasPrefix(text, heading), "%s starts with %q", name, heading)
		assert.True(t, strings.HasSuffix(text, "\n\n") && !strings.HasSuffix(text, "\n\n\n"), "%s ends with one blank line", name)
		assert.NotContains(t, text, "{{", "%s is static text", name)
		for _, cmd := range []string{"goa gen ", "go run ", "go test", "go build"} {
			assert.NotContains(t, text, cmd, "%s does not tell the reader to run %q", name, cmd)
		}
	}
	assert.Contains(t, quickstartSections["liftoff"], "`check_project`")
	assert.Contains(t, quickstartSections["header"], "section 2 names them", "the header points at liftoff, part 2")
	assert.Contains(t, quickstartSections["golden-rules"], "`generate_code`")
}

// TestGenerateProgramCompilesThePluginAsMain checks the files the sandboxed
// generation program is built from: the plugin with its package clause
// changed to main, and the section texts as Go literals equal to this
// package's embedded files.
func TestGenerateProgramCompilesThePluginAsMain(t *testing.T) {
	program := generateProgram()
	require.Len(t, program, 3)
	assert.Contains(t, program["main.go"], `codegen.RegisterPluginLast("surla-quickstart", "gen", nil, surlaQuickstart)`)

	fset := token.NewFileSet()
	plugin, err := parser.ParseFile(fset, "quickstart.go", program["quickstart.go"], 0)
	require.NoError(t, err)
	assert.Equal(t, "main", plugin.Name.Name)

	sections, err := parser.ParseFile(fset, "quickstart_sections.go", program["quickstart_sections.go"], 0)
	require.NoError(t, err)
	assert.Equal(t, "main", sections.Name.Name)
	for name, text := range quickstartSections {
		assert.Contains(t, program["quickstart_sections.go"], strconv.Quote(name)+": "+strconv.Quote(text))
	}
}

// namedGuide returns an AGENTS_QUICKSTART.md file whose sections carry
// Goa-AI's names and placeholder sources.
func namedGuide(names []string) *codegen.File {
	f := &codegen.File{Path: quickstartPath}
	for _, n := range names {
		f.SectionTemplates = append(f.SectionTemplates, &codegen.SectionTemplate{Name: "agents-quickstart-" + n, Source: "goa-ai " + n})
	}
	return f
}

func TestGenerateProgramWithStoriesParsesAsMain(t *testing.T) {
	assert.Equal(t, generateProgram(), generateProgramWithStories(nil))

	specs := []storyScenarioSpec{
		{name: "refund_over_limit", description: "Line 1: \"quoted\" `backtick` \\backslash\\\nLine 2"},
		{name: "blank_title_flow", description: "blank-title-flow"},
	}
	program := generateProgramWithStories(specs)
	require.Len(t, program, 5)

	fset := token.NewFileSet()
	for _, name := range []string{"story_scenarios.go", "story_scenarios_data.go"} {
		parsed, err := parser.ParseFile(fset, name, program[name], 0)
		require.NoError(t, err, "parse %s", name)
		assert.Equal(t, "main", parsed.Name.Name)
	}
}
