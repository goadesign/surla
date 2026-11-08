//go:build e2e

package main

import (
	"bytes"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/testmodule"
)

// starterSurlaVersion is a published Surla version the starter's evaluator
// module requires, as the scaffold tests use.
const starterSurlaVersion = "v0.0.0-20261104215521-580509970fb6"

// headerCommand matches the command line Goa writes in generated file
// headers, which names the program that generated them.
var headerCommand = regexp.MustCompile(`(?m)^// \$ .*$`)

// TestGenMatchesGoaGenByteForByte compares surla gen with the module-pinned
// host goa gen on Surla's own trusted test design: every path and byte must
// match, apart from the header's command line.
func TestGenMatchesGoaGenByteForByte(t *testing.T) {
	snapshot := testmodule.Snapshot(t, completeSourcesGoa, genDesign)
	hostDir := writeModule(t, snapshot)
	cmd := exec.CommandContext(t.Context(), "go", "run", "goa.design/goa/v3/cmd/goa", "gen", "example.com/design/design")
	cmd.Dir = hostDir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))

	surlaDir := writeModule(t, snapshot)
	require.NoError(t, runGen([]string{"-dir", surlaDir}, io.Discard, io.Discard))

	want, got := normalized(t, hostDir), normalized(t, surlaDir)
	assert.Equal(t, keys(want), keys(got), "the same paths")
	for p, w := range want {
		assert.Equal(t, w, got[p], p)
	}
}

// TestGenOnAFreshStarterChangesNothingAndKeepsModulesApart creates a
// starter with surla init's own code path, checks that surla gen in either
// module of the untouched project changes no byte anywhere in it, and that
// regenerating after a design edit in one module leaves the other module
// byte-for-byte unchanged.
func TestGenOnAFreshStarterChangesNothingAndKeepsModulesApart(t *testing.T) {
	isolateLocalWork(t)
	dir := filepath.Join(t.TempDir(), "shop")
	_, err := scaffold.Init(t.Context(), scaffold.Options{Dir: dir, WorkDir: t.TempDir(), Module: "example.com/shop", SurlaVersion: starterSurlaVersion},
		scaffold.RunCommand, designeval.NewEvaluator())
	require.NoError(t, err)
	evalDir := filepath.Join(dir, "eval")
	created := files(t, dir, ".")
	require.Contains(t, created, quickstartGuide, "the product design declares an agent, so goa-ai writes its guide")
	assert.Contains(t, created["gen/shop/service.go"], "// $ surla gen", "surla init generates with the same generator")

	for _, module := range []string{dir, evalDir} {
		var out bytes.Buffer
		require.NoError(t, runGen([]string{"-dir", module}, &out, io.Discard))
		assert.Contains(t, out.String(), "added: 0\nchanged: 0\nremoved: 0\n", module)
	}
	assert.Equal(t, created, files(t, dir, "."), "regenerating an untouched starter changes no byte")

	edits := []struct {
		// module is the module directory surla gen runs in.
		module string
		// design is the edited design file, relative to dir.
		design string
		// old and new are the replaced description text.
		old, new string
		// other is the other module's directory, relative to dir, which
		// must not change.
		other string
	}{
		{module: dir, design: "design/design.go", old: "Answers price questions", new: "Answers every price question", other: "eval"},
		{module: evalDir, design: "eval/design/design.go", old: "Proves through the public API", new: "Shows through the public API", other: "gen"},
	}
	for _, e := range edits {
		before := files(t, dir, e.other)
		path := filepath.Join(dir, filepath.FromSlash(e.design))
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Contains(t, string(src), e.old)
		require.NoError(t, os.WriteFile(path, []byte(strings.Replace(string(src), e.old, e.new, 1)), 0o644))
		var out bytes.Buffer
		require.NoError(t, runGen([]string{"-dir", e.module}, &out, io.Discard))
		assert.NotContains(t, out.String(), "changed: 0\n", "the edit changes generated files in %s", e.module)
		assert.Equal(t, before, files(t, dir, e.other), "regenerating %s leaves %s unchanged", e.module, e.other)
	}
}

// normalized returns every file under gen/ and the agent guide, with header
// command lines replaced by a placeholder.
func normalized(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := files(t, dir, "gen")
	if guide, err := os.ReadFile(dir + "/" + quickstartGuide); err == nil {
		out[quickstartGuide] = string(guide)
	}
	for p, content := range out {
		out[p] = headerCommand.ReplaceAllString(content, "// $ <command>")
	}
	return out
}

// keys returns m's keys, sorted.
func keys(m map[string]string) []string {
	return slices.Sorted(maps.Keys(m))
}
