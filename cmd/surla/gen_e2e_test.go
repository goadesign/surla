//go:build e2e

package main

import (
	"io"
	"maps"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/testmodule"
)

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
