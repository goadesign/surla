//go:build scaffold

package scaffold

import (
	"encoding/json"
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
// service: both scenarios must pass.
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
			build(filepath.Join(dir, "eval"), name+"-evals", "./cmd/"+name+"-evals")

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
			assert.ElementsMatch(t, []string{"prices_tea", "prompt_override_reaches_model", "unknown_item"}, ids)
			for _, s := range report.Scenarios {
				require.NotNil(t, s.Result, s.ID)
				assert.True(t, s.Passed, "%s: %+v", s.ID, s.Result.Checks)
			}
		})
	}
}
