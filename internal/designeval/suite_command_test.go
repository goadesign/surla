package designeval

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	evalexpr "goa.design/goa-ai/eval/expr"
	"goa.design/goa/v3/codegen"
	"goa.design/goa/v3/eval"
)

// TestTestModuleServiceRecognizesSurlasLayout checks the one rule that names
// a Surla test module and the program written for it, below, at and beside
// its boundaries.
func TestTestModuleServiceRecognizesSurlasLayout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		module, service string
		ok              bool
	}{
		{module: "example.com/shop/eval", service: "shop", ok: true},
		{module: "github.com/acme/orders2/eval", service: "orders2", ok: true},
		{module: "example.com/shop", ok: false},
		{module: "eval", ok: false},
		{module: "example.com/eval", ok: false},
		{module: "example.com/my-shop/eval", ok: false},
		{module: "example.com/Shop/eval", ok: false},
		{module: "example.com/2shop/eval", ok: false},
		{module: "example.com/main/eval", ok: false},
		{module: "example.com/func/eval", ok: false},
		{module: "example.com/shop/eval/x", ok: false},
		{module: "example.com/shop/evals", ok: false},
	} {
		t.Run(tc.module, func(t *testing.T) {
			service, ok := TestModuleService(tc.module)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.service, service)
		})
	}
	assert.Equal(t, "./gen/cmd/shop-evals", SuiteCommandPackage("shop"))
}

// TestSurlaSuiteCommandWritesTheProgramOfASurlaTestModule calls the plugin as
// Goa does. A Surla test module with exactly one suite gets the program at
// its package and the quickstart section, added to Goa-AI's guide or as the
// guide's only section; any other module is left as it is; a suite whose
// package Goa-AI did not plan is an error naming the file.
func TestSurlaSuiteCommandWritesTheProgramOfASurlaTestModule(t *testing.T) {
	t.Parallel()
	suiteFile := func(name string) *codegen.File { return &codegen.File{Path: "gen/evals/" + name + "/suite.go"} }
	roots := func(names ...string) []eval.Root {
		root := &evalexpr.RootExpr{}
		for _, n := range names {
			root.Suites = append(root.Suites, &evalexpr.SuiteExpr{Name: n})
		}
		return []eval.Root{root}
	}
	t.Run("a Surla test module with one suite", func(t *testing.T) {
		guide := namedGuide([]string{"header", "faq"})
		files, err := surlaSuiteCommand("example.com/shop/eval/gen", roots("shop_acceptance"), []*codegen.File{suiteFile("shop_acceptance"), guide})
		require.NoError(t, err)
		require.Len(t, files, 3)
		program := files[2]
		assert.Equal(t, "gen/cmd/shop-evals/main.go", program.Path)
		require.Len(t, program.SectionTemplates, 2)
		header := program.SectionTemplates[0]
		imports := header.Data.(map[string]any)["Imports"].([]*codegen.ImportSpec)
		paths := map[string]string{}
		for _, spec := range imports {
			paths[spec.Path] = spec.Name
		}
		assert.Contains(t, paths, "example.com/shop/eval/acceptance")
		assert.Equal(t, "genevals", paths["example.com/shop/eval/gen/evals/shop_acceptance"])
		assert.Contains(t, paths, "github.com/goadesign/surla/verify/suite")
		body := program.SectionTemplates[1]
		for _, call := range []string{"acceptance.Name", "&acceptance.Hooks{}", "acceptance.New(", "acceptance.Live(", "acceptance.LiveThroughGateway("} {
			assert.Contains(t, body.Source, call)
		}
		assert.Equal(t, suiteCommandData{Service: "shop", Suite: "shop_acceptance", Command: "./gen/cmd/shop-evals", Environment: "scripted-model"}, body.Data)
		last := guide.SectionTemplates[len(guide.SectionTemplates)-1]
		assert.Equal(t, "surla-test-program", last.Name, "the section is added to Goa-AI's guide")
		assert.Equal(t, testProgramSection, last.Source)
	})
	t.Run("a Surla test module without Goa-AI's guide", func(t *testing.T) {
		files, err := surlaSuiteCommand("example.com/shop/eval/gen", roots("shop_acceptance"), []*codegen.File{suiteFile("shop_acceptance")})
		require.NoError(t, err)
		require.Len(t, files, 3)
		assert.Equal(t, quickstartPath, files[2].Path)
		require.Len(t, files[2].SectionTemplates, 1)
		assert.Equal(t, "surla-test-program", files[2].SectionTemplates[0].Name)
	})
	for _, tc := range []struct {
		name, genpkg string
		roots        []eval.Root
	}{
		{name: "no suite", genpkg: "example.com/shop/eval/gen", roots: roots()},
		{name: "two suites", genpkg: "example.com/shop/eval/gen", roots: roots("a", "b")},
		{name: "no eval root", genpkg: "example.com/shop/eval/gen"},
		{name: "a module outside Surla's layout", genpkg: "example.com/shop/gen", roots: roots("shop_acceptance")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := []*codegen.File{suiteFile("a"), suiteFile("shop_acceptance")}
			files, err := surlaSuiteCommand(tc.genpkg, tc.roots, in)
			require.NoError(t, err)
			assert.Equal(t, in, files, "nothing is written")
		})
	}
	t.Run("a suite Goa-AI planned elsewhere", func(t *testing.T) {
		_, err := surlaSuiteCommand("example.com/shop/eval/gen", roots("shop_acceptance"), nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "gen/evals/shop_acceptance/suite.go")
	})
}

// TestTestProgramSectionNamesTheFiveNames checks that the quickstart section
// names every name the generated program calls in the author's package.
func TestTestProgramSectionNamesTheFiveNames(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"`Name`", "`Hooks`", "`New`", "`Live`", "`LiveThroughGateway`", "{{ .Command }}", "{{ .Environment }}"} {
		assert.Contains(t, testProgramSection, name)
	}
	assert.True(t, strings.HasPrefix(testProgramSection, "\n---\n\n## "), "it starts at a separator, as Goa-AI's sections do")
}

// TestGeneratedProgramRunsTheStarterSuite generates the starter's test module
// with a Story: it gets its program and the quickstart section naming what the
// acceptance package provides. With the required methods written, the program
// builds and lists the generated scenarios, and its live mode refuses mixed or
// incomplete model settings with the messages the starter's own program gave.
func TestGeneratedProgramRunsTheStarterSuite(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	evalInput, fullTree := starterEvalModule(t)
	stories := []StorySummary{{Name: "Refunds", Flows: []StoryFlow{{Key: "refund-over-limit", Title: "A refund over the limit.", Complete: true}}}}
	gen, err := testEvaluator.GenerateWithStories(t.Context(), evalInput, stories)
	require.NoError(t, err)
	program := string(gen.Files["gen/cmd/shop-evals/main.go"])
	require.NotEmpty(t, program, "the program is generated at "+SuiteCommandPackage("shop"))
	assert.Contains(t, program, "$ surla gen")
	assert.Contains(t, program, `genevals "example.com/shop/eval/gen/evals/shop_acceptance"`)
	assert.Contains(t, program, `const environment = "scripted-model"`)
	guide := string(gen.Files[quickstartPath])
	assert.Contains(t, guide, "## The test program Surla builds")
	assert.Contains(t, guide, "`./gen/cmd/shop-evals`")
	assert.NotContains(t, guide, "{{", "the section is rendered")

	fullTree["acceptance/acceptance.go"] += `
func (h *Hooks) RefundOverLimit(ctx context.Context) (eval.Result, error) {
	return eval.Result{}, nil
}
`
	dir := stageStarterEvalTree(t, fullTree, gen.Files)
	bin, out, err := buildEvalCommand(t, dir)
	require.NoError(t, err, "build the generated program:\n%s", out)
	assert.Equal(t, []string{"prices_tea", "prompt_override_reaches_model", "unknown_item", "refund_over_limit"},
		scenarioIDs(describeEvalSuite(t, bin)))
	declared := describeEvalSuite(t, bin)
	assert.Equal(t, "scripted-model", declared.Environment)
	for _, sc := range declared.Scenarios {
		assert.NotContains(t, sc.Tags, "holdout", "the starter hides no scenario")
	}

	artifact := filepath.Join(t.TempDir(), "shop")
	require.NoError(t, os.WriteFile(artifact, []byte("stand-in program under test"), 0o700))
	for _, tc := range []struct {
		name    string
		env     []string
		wantErr string
	}{
		{name: "none", wantErr: "no model for the live shop"},
		{name: "gateway and provider", env: []string{"SURLA_LIVE_MODEL_GATEWAY=127.0.0.1:9090", "SURLA_LIVE_MODEL_PROVIDER=anthropic"},
			wantErr: "both a model gateway and a model provider"},
		{name: "gateway and name", env: []string{"SURLA_LIVE_MODEL_GATEWAY=127.0.0.1:9090", "SURLA_LIVE_MODEL_NAME=m"},
			wantErr: "both a model gateway and a model provider"},
		{name: "provider without name", env: []string{"SURLA_LIVE_MODEL_PROVIDER=anthropic"}, wantErr: "needs both a model provider and a model name"},
		{name: "name without provider", env: []string{"SURLA_LIVE_MODEL_NAME=m"}, wantErr: "needs both a model provider and a model name"},
		{name: "unknown provider", env: []string{"SURLA_LIVE_MODEL_PROVIDER=other", "SURLA_LIVE_MODEL_NAME=m"}, wantErr: `unsupported model provider "other"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), bin, "--live", "--artifact", "shop="+artifact)
			cmd.Env = append([]string{"TMPDIR=" + t.TempDir(), "SURLA_LIVE_TARGET_KEY=k", "PATH=" + os.Getenv("PATH")}, tc.env...)
			out, err := cmd.CombinedOutput()
			require.Error(t, err)
			assert.Contains(t, string(out), tc.wantErr)
		})
	}
}
