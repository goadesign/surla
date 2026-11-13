package designeval

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/modfile"

	"github.com/goadesign/surla"
	"github.com/goadesign/surla/internal/testmodule"
)

func TestEvaluateTreatsArchiveLookingTextAsPlainGo(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	design := strings.Replace(minimalDesign, `Service("orders"`, "Service(\"orders\", func() {\n\tDescription(`first line\n-- example.go --\nlast line`)\n})\n\nvar _ = Service(\"orders2\"", 1)
	res, err := testEvaluator.Evaluate(t.Context(), testmodule.Snapshot(t, "", design))
	require.NoError(t, err)
	assert.Contains(t, res.Declarations, "Service:orders2/Method:get_order")
}

func TestEvaluateRejectsAnIncompleteModule(t *testing.T) {
	files := testmodule.Snapshot(t, "", minimalDesign)
	files["go.sum"] = ""
	_, err := testEvaluator.Evaluate(t.Context(), files)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target module is incomplete or unresolvable")
}

func TestEvaluateRefusesToUpgradeTheTargetsGenerator(t *testing.T) {
	// The target requires only goa, at a version older than the evaluator's
	// goa-ai needs; building the evaluator against it would upgrade goa.
	old := "v3.21.0"
	files := testmodule.Tidy(t, map[string]string{
		"go.mod":           "module example.com/design\n\ngo 1.24\n\nrequire goa.design/goa/v3 " + old + "\n",
		"design/design.go": minimalDesign,
	})
	require.Contains(t, files["go.mod"], "goa.design/goa/v3 "+old, "tidy keeps the target's own goa version")
	_, err := testEvaluator.Evaluate(t.Context(), files)
	require.Error(t, err, "goa %s is older than the evaluator needs, so evaluating it would upgrade goa", old)
	assert.Contains(t, err.Error(), "goa.design/goa/v3: target selects "+old)
}

func TestEvaluateKeepsTheTargetsOwnCoreVersions(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	// A target pinned at an older goa than Surla's own is evaluated with
	// its own version, not upgraded to Surla's and then rejected.
	for _, goa := range []string{preCompleteSourcesGoa, ""} {
		res, err := testEvaluator.Evaluate(t.Context(), testmodule.Snapshot(t, goa, minimalDesign))
		require.NoError(t, err, "goa %q", goa)
		assert.Contains(t, res.Declarations, "Service:orders/Method:get_order", "goa %q", goa)
	}
}

func TestEvaluateUsesTheTargetsOwnGeneratorVersion(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	res, err := testEvaluator.Evaluate(t.Context(), testmodule.Snapshot(t, "", minimalDesign))
	require.NoError(t, err)
	assert.Contains(t, res.Declarations, "Service:orders/Method:get_order")
}

func TestEvaluateUsesTheBuildToolchainToResolveModules(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the sandboxed Go toolchain; the full suite (go test without -short) covers it")
	}
	// Module resolution must run with the toolchain the sandboxed build
	// compiles with, never whatever go is first on PATH: a design may require
	// that toolchain's exact Go version while PATH holds an older go.
	tc, err := resolveToolchainEnv(t.Context())
	require.NoError(t, err)
	raw, err := os.ReadFile(filepath.Join(tc.goroot, "VERSION"))
	require.NoError(t, err)
	version := strings.TrimPrefix(strings.SplitN(string(raw), "\n", 2)[0], "go")
	files := testmodule.Snapshot(t, "", minimalDesign)
	files["go.mod"] = strings.Replace(files["go.mod"], "go 1.24", "go "+version, 1)
	require.Contains(t, files["go.mod"], "go "+version)

	// The go on PATH only reports where the build toolchain and module cache
	// are, as `go env` does; any module command run with it fails.
	fakeBin := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1 $2 $3\" = \"env GOROOT GOMODCACHE\" ]; then printf '%s\\n%s\\n' '" + tc.goroot + "' '" + tc.modCache + "'; exit 0; fi\n" +
		"echo 'go on PATH must not be used' >&2\nexit 1\n"
	require.NoError(t, os.WriteFile(filepath.Join(fakeBin, "go"), []byte(script), 0o700))
	t.Setenv("PATH", fakeBin+":/usr/bin:/bin")

	res, err := testEvaluator.Evaluate(t.Context(), files)
	require.NoError(t, err)
	assert.Contains(t, res.Declarations, "Service:orders/Method:get_order")
}

// TestEvaluatorGoModIgnoresTheWorkingDirectory checks that the generator
// versions come from the go.mod compiled into Surla even when surla runs
// inside another module, such as a user's application that does not require
// goa.design/model.
func TestEvaluatorGoModIgnoresTheWorkingDirectory(t *testing.T) {
	app := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(app, "go.mod"), []byte("module example.com/app\n\ngo 1.24\n"), 0o600))
	sourceRoot, err := filepath.Abs("../..")
	require.NoError(t, err)

	onDisk, err := os.ReadFile(filepath.Join(sourceRoot, "go.mod"))
	require.NoError(t, err)
	t.Chdir(app)

	mod, err := evaluatorGoMod()
	require.NoError(t, err)
	assert.Equal(t, string(onDisk), surla.GoMod(), "the compiled-in go.mod is Surla's own")
	assert.Equal(t, "github.com/goadesign/surla", mod.Module.Mod.Path)
	for _, core := range []string{harnessGoaModule, harnessGoaAIModule, harnessModelModule} {
		version, ok := requiredVersion(mod, core)
		assert.True(t, ok, "Surla's go.mod requires %s", core)
		assert.NotEmpty(t, version)
	}
}

// TestALoadFailureNamesTheStoryRequirementStepOnlyWhenItIsMissing checks
// which failures to load a design's dependencies name the step that requires
// the Story module, from the snapshot alone, and that the step names the
// version Surla's own go.mod requires.
func TestALoadFailureNamesTheStoryRequirementStepOnlyWhenItIsMissing(t *testing.T) {
	evalMod, err := evaluatorGoMod()
	require.NoError(t, err)
	surlaStory, ok := requiredVersion(evalMod, harnessStoryModule)
	require.True(t, ok, "Surla's go.mod requires the Story module")
	step := "\nThe design imports the Story language, but its module's go.mod does not require it. " +
		"In the folder that holds that go.mod, run: go get github.com/goadesign/surla/story@" + surlaStory +
		", commit go.mod and go.sum, then try again."
	const (
		plainMod   = "module example.com/orders\n\ngo 1.24\n"
		storyFile  = "package design\n\nimport . \"github.com/goadesign/surla/story/dsl\"\n\nvar _ = Story(\"Orders\", func() {})\n"
		plainFile  = "package design\n\nimport . \"goa.design/goa/v3/dsl\"\n\nvar _ = API(\"orders\", func() {})\n"
		olderStory = "v0.0.0-20261110000000-000000000000"
	)
	for _, test := range []struct {
		name  string
		files map[string]string
		step  bool
	}{
		{
			name:  "the design imports the language and go.mod does not require it",
			files: map[string]string{"go.mod": plainMod, "design/design.go": plainFile, "design/story.go": storyFile},
			step:  true,
		},
		{
			name: "a package under design imports the language and go.mod does not require it",
			files: map[string]string{
				"go.mod": plainMod, "design/design.go": plainFile, "design/stories/orders.go": storyFile,
			},
			step: true,
		},
		{
			name: "the design imports the language and go.mod requires it",
			files: map[string]string{
				"go.mod":          plainMod + "\nrequire github.com/goadesign/surla/story " + surlaStory + "\n",
				"design/story.go": storyFile,
			},
		},
		{
			name:  "the design does not import the language",
			files: map[string]string{"go.mod": plainMod, "design/design.go": plainFile},
		},
		{
			// Only a package outside design/ imports the language: the
			// evaluator does not load it, so it is not why loading failed.
			name:  "only a package outside the design imports the language",
			files: map[string]string{"go.mod": plainMod, "design/design.go": plainFile, "stories/orders.go": storyFile},
		},
		{
			// The requirement resolves, so loading does not fail for want
			// of it, and the evaluation uses the project's version. Whether
			// that version fits is the compiler's to say, in a later phase
			// whose diagnostic carries the compiler's output.
			name: "the design imports the language and go.mod requires another version",
			files: map[string]string{
				"go.mod":          plainMod + "\nrequire github.com/goadesign/surla/story " + olderStory + "\n",
				"design/story.go": storyFile,
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			target, err := modfile.Parse("go.mod", []byte(test.files["go.mod"]), nil)
			require.NoError(t, err)
			cause := errors.New("go list -mod=readonly -deps ./design/...: exit status 1")
			loadErr := withStoryRequirementStep(cause, test.files, target, evalMod)
			assert.ErrorIs(t, loadErr, cause)
			assert.Equal(t, cause.Error(), loadErr.Error(), "the CLI and trace text is unchanged")
			diagnostic := GenerationDiagnostic(&phaseError{phase: phaseLoadModules, err: loadErr})
			want := "Code generation failed while loading the design module dependencies."
			if test.step {
				want += step
			}
			assert.Equal(t, want, diagnostic)
		})
	}
}
