package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/testmodule"
)

// completeSourcesGoa is the first goa.design/goa/v3 with
// generator.CompleteSources.
const completeSourcesGoa = "v3.35.1-0.20261103205041-35e789e8f9b1"

// preCompleteSourcesGoa is the goa.design/goa/v3 version starters created
// before generator.CompleteSources require.
const preCompleteSourcesGoa = "v3.35.1-0.20261030163856-3e08a6128171"

// genDesign is a small design surla gen regenerates.
const genDesign = `package design

import . "goa.design/goa/v3/dsl"

var _ = Service("orders", func() {
	Method("get_order", func() {
		Payload(String)
		Result(String)
		HTTP(func() { GET("/orders/{id}") })
	})
})
`

func TestGenPublishesGenAndLeavesNothingBehind(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := writeModule(t, testmodule.Snapshot(t, completeSourcesGoa, genDesign))
	layout(t, dir, map[string]string{"gen/stale/old.go": "package stale\n", "gen/NOTES.md": "mine\n"})

	var stdout, stderr bytes.Buffer
	require.NoError(t, runGen([]string{"-dir", dir}, &stdout, &stderr))
	assert.Contains(t, stdout.String(), "gen/ updated")
	assert.Contains(t, stdout.String(), "removed: 1\n  gen/stale/old.go")
	got := files(t, dir, "gen")
	assert.Contains(t, got["gen/orders/service.go"], "$ surla gen")
	assert.Contains(t, got, "gen/goa.json")
	assert.Equal(t, "mine\n", got["gen/NOTES.md"], "a top-level user file is kept")
	assert.NotContains(t, got, "gen/stale/old.go")
	assertNothingLeft(t, dir, tmp)
}

func TestGenRefusesAGoaWithoutCompleteSourcesAndChangesNothing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := writeModule(t, testmodule.Snapshot(t, preCompleteSourcesGoa, genDesign))
	layout(t, dir, map[string]string{"gen/orders/service.go": "old\n"})

	err := runGen([]string{"-dir", dir}, io.Discard, io.Discard)
	require.ErrorContains(t, err, "surla gen needs a goa.design/goa/v3 that provides generator.CompleteSources")
	assert.Equal(t, map[string]string{"gen/orders/service.go": "old\n"}, files(t, dir, "gen"))
	assertNothingLeft(t, dir, tmp)
}

// writeModule writes a module snapshot to a new directory and returns it.
func writeModule(t *testing.T, snapshot map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	layout(t, dir, snapshot)
	return dir
}

// assertNothingLeft checks that no staging directory is left in the module
// and no evaluator workspace in tmp.
func assertNothingLeft(t *testing.T, dir, tmp string) {
	t.Helper()
	stages, err := filepath.Glob(filepath.Join(dir, ".surla-gen-*"))
	require.NoError(t, err)
	assert.Empty(t, stages, "no staging directory is left")
	workspaces, err := filepath.Glob(filepath.Join(tmp, "surla-designeval-*"))
	require.NoError(t, err)
	assert.Empty(t, workspaces, "the evaluator's workspace is removed")
	_, err = os.Stat(filepath.Join(dir, "go.mod"))
	require.NoError(t, err)
}
