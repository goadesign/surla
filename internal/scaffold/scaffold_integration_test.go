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
	"github.com/goadesign/surla/internal/testwriter"
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
// service: every scenario must pass, including the one the starter's Story
// table requires for its finished flow, through the method the starter ships
// for it. It then does what the first build of a change does with the
// project's Story: it evaluates the design and prepares a test writer's
// working copy from the evaluated Story. The Story table rendered from the
// evaluated Story is the one the starter ships, and preparing changes no file
// of the copy, so the starter's generated test program is what a build
// prepares.
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

			scenarios := []string{"prices_tea", "prompt_override_reaches_model", "unknown_item", "prices_cookie"}
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
			runSuite(scenarios)

			ev := designeval.NewEvaluatorInDir(t.TempDir())
			t.Cleanup(func() { assert.NoError(t, ev.Close()) })
			design, err := designeval.ReadModuleSnapshot(dir)
			require.NoError(t, err)
			evaluated, err := ev.Evaluate(t.Context(), design)
			require.NoError(t, err, "the starter's design, with its Story, evaluates")
			require.Len(t, evaluated.Stories, 1)
			assert.Equal(t, "Prices", evaluated.Stories[0].Name)
			table, required, err := designeval.StoryTests(os.DirFS(filepath.Join(dir, "eval")), evaluated.Stories)
			require.NoError(t, err)
			assert.Equal(t, []designeval.StoryScenario{
				{Story: "Prices", Key: "prices-cookie", Scenario: "prices_cookie", Hook: "PricesCookie"},
			}, required)
			shipped, err := os.ReadFile(filepath.Join(dir, "eval", filepath.FromSlash(designeval.StoryTestsPath)))
			require.NoError(t, err)
			assert.Equal(t, string(table), string(shipped), "the starter ships the table its evaluated Story gives")

			// The user makes the first commit, as the README says.
			for _, args := range [][]string{
				{"init", "--quiet"}, {"config", "user.name", "Test User"}, {"config", "user.email", "test@example.com"},
				{"config", "commit.gpgsign", "false"}, {"add", "--all"}, {"commit", "--quiet", "-m", "Start from surla init"},
			} {
				out, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
				require.NoError(t, err, "git %v: %s", args, out)
			}
			wc, err := testwriter.OpenWorkingCopy(t.Context(), dir)
			require.NoError(t, err)
			prepared, err := testwriter.NewChecker(ev, t.TempDir()).Prepare(t.Context(), wc,
				testwriter.Suite{Module: SuiteDir, Command: designeval.SuiteCommandPackage(name)}, evaluated.Stories)
			require.NoError(t, err)
			assert.Equal(t, required, prepared)
			status := exec.CommandContext(t.Context(), "git", "-C", wc.Dir(), "status", "--porcelain", "--untracked-files=all")
			out, err := status.Output()
			require.NoError(t, err)
			assert.Empty(t, string(out), "preparing the first build changes no file of the starter")
		})
	}
}
