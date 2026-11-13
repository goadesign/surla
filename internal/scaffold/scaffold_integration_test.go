//go:build scaffold

package scaffold

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/eval"

	"github.com/goadesign/surla/internal/designeval"
)

// TestInitBuildsForNamesThatCollideWithTemplatesOrGo creates real projects
// (network needed) for names that match template identifiers or that Goa
// generates under another package name; Init vets both modules.
func TestInitBuildsForNamesThatCollideWithTemplatesOrGo(t *testing.T) {
	for _, name := range []string{"helper", "service", "catalog", "errors", "string"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), name)
			_, err := Init(t.Context(), Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/" + name, SurlaVersion: published}, RunCommand, designeval.NewEvaluator())
			require.NoError(t, err, "Init runs go vet on both modules")
		})
	}
}

// TestInitCreatesAProjectWhoseSuitePasses creates real projects with the
// published Surla version (it needs the network and, for the sandbox, macOS),
// builds the service and its evaluator suite, and runs the suite against the
// service: every scenario must pass. It then does what a build of a change
// does with the project's Story: it evaluates the design, generates the test
// module again with the Story's finished flows as required tests, and runs
// the result, so the flow the starter ships must pass through the method the
// starter ships for it, with nothing added by hand.
func TestInitCreatesAProjectWhoseSuitePasses(t *testing.T) {
	// errors is generated as package errors_, so it also proves the imports
	// follow the package goa gen produced.
	for _, service := range []string{"shop", "errors"} {
		t.Run(service, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), service)
			name, err := Init(t.Context(), Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/" + service, SurlaVersion: published}, RunCommand, designeval.NewEvaluator())
			require.NoError(t, err)
			bin := t.TempDir()
			build := func(moduleDir, out, pkg string) {
				cmd := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(bin, out), pkg)
				cmd.Dir = moduleDir
				cmd.Env = append(cmd.Environ(), "GOWORK=off")
				b, err := cmd.CombinedOutput()
				require.NoError(t, err, string(b))
			}
			test := exec.CommandContext(t.Context(), "go", "test", "./...")
			test.Dir = filepath.Join(dir, "eval")
			test.Env = append(test.Environ(), "GOWORK=off")
			testOut, err := test.CombinedOutput()
			require.NoError(t, err, "the generated evaluator's own tests pass:\n%s", testOut)
			build(dir, name, "./cmd/"+name)

			handWritten := []string{"prices_tea", "prompt_override_reaches_model", "unknown_item"}
			runSuite := func(want []string) {
				build(filepath.Join(dir, "eval"), name+"-evals", "./gen/cmd/"+name+"-evals")
				out, err := exec.CommandContext(t.Context(), filepath.Join(bin, name+"-evals"),
					"--artifact", name+"="+filepath.Join(bin, name)).Output()
				require.NoError(t, err, string(out))
				var report eval.Report
				require.NoError(t, json.Unmarshal(out, &report))
				assert.True(t, report.Passed)
				ids := make([]string, 0, len(report.Scenarios))
				for _, s := range report.Scenarios {
					ids = append(ids, s.ID)
				}
				assert.ElementsMatch(t, want, ids)
				for _, s := range report.Scenarios {
					require.NotNil(t, s.Result, s.ID)
					assert.True(t, s.Passed, "%s: %+v", s.ID, s.Result.Checks)
				}
			}
			runSuite(handWritten)

			ev := designeval.NewEvaluatorInDir(t.TempDir())
			t.Cleanup(func() { assert.NoError(t, ev.Close()) })
			design, err := designeval.ReadModuleSnapshot(dir)
			require.NoError(t, err)
			evaluated, err := ev.Evaluate(t.Context(), design)
			require.NoError(t, err, "the starter's design, with its Story, evaluates")
			require.Len(t, evaluated.Stories, 1)
			assert.Equal(t, "Prices", evaluated.Stories[0].Name)
			tests, err := designeval.ReadModuleSnapshot(filepath.Join(dir, "eval"))
			require.NoError(t, err)
			required, err := ev.GenerateWithStories(t.Context(), tests, evaluated.Stories)
			require.NoError(t, err)
			assert.Equal(t, []designeval.StoryScenario{
				{Story: "Prices", Key: "prices-cookie", Scenario: "prices_cookie", Hook: "PricesCookie"},
			}, required.Included)
			assert.Empty(t, required.Excluded, "the starter ships no unfinished flow")
			require.NoError(t, os.RemoveAll(filepath.Join(dir, "eval", "gen")))
			for rel, data := range required.Files {
				file := filepath.Join(dir, "eval", filepath.FromSlash(rel))
				require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o755))
				require.NoError(t, os.WriteFile(file, data, 0o644))
			}
			runSuite(append(handWritten, "prices_cookie"))
		})
	}
}
