// These tests keep command storage inside test-owned roots and exercise path
// selection independently of compiler and provider availability.
package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// starterVersionForWorkTest is parsed by the CLI but never downloaded.
const starterVersionForWorkTest = "v0.0.0-20261104215521-580509970fb6"

// TestLocalWorkStaysOutsideStartupAndTargets includes a builder-writable
// TMPDIR and an arbitrary CLI target unrelated to the startup directory.
func TestLocalWorkStaysOutsideStartupAndTargets(t *testing.T) {
	operator := isolateLocalWork(t)
	workspace, destination := t.TempDir(), t.TempDir()
	t.Chdir(workspace)
	tmp := filepath.Join(workspace, "tmp")
	require.NoError(t, os.Mkdir(tmp, 0o700))
	t.Setenv("TMPDIR", tmp)
	for _, target := range []string{workspace, destination} {
		work, err := localInitWorkDir(target)
		require.NoError(t, err)
		want, err := abspath.Eval(filepath.Join(operator, "surla", "operator", "init-stage"))
		require.NoError(t, err)
		assert.Equal(t, want, work)
	}
	work, err := localEvalWorkDir(destination)
	require.NoError(t, err)
	assert.Equal(t, "designeval-work", filepath.Base(work))
	left, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, left)
}

// TestProtectedDirectoryRejectsWorkspaceAliases checks physical ancestors
// before creation, including a symlink that points into the writable grant.
func TestProtectedDirectoryRejectsWorkspaceAliases(t *testing.T) {
	workspace, outside := t.TempDir(), t.TempDir()
	physical, err := abspath.Eval(workspace)
	require.NoError(t, err)
	alias := filepath.Join(outside, "alias")
	require.NoError(t, os.Symlink(workspace, alias))
	for _, candidate := range []string{workspace, filepath.Join(workspace, "new"), filepath.Join(alias, "new")} {
		_, err := createProtectedDir(candidate, []string{physical})
		assert.ErrorContains(t, err, "must be outside workspace")
	}
	assert.NoDirExists(t, filepath.Join(workspace, "new"))
}

// TestRunInitKeepsFailedLogsOutsideTheWorkspace runs the real CLI composition
// with a failing trusted Go-command fixture; generation and downloads never run.
func TestRunInitKeepsFailedLogsOutsideTheWorkspace(t *testing.T) {
	operator := isolateLocalWork(t)
	workspace := t.TempDir()
	t.Chdir(workspace)
	tmp := filepath.Join(workspace, "tmp")
	require.NoError(t, os.Mkdir(tmp, 0o700))
	t.Setenv("TMPDIR", tmp)
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\necho protected > \"$TMPDIR/visited\"\necho controlled-init-failure >&2\nexit 1\n"), 0o700))
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	err := runInit([]string{"-dir", "shop", "-module", "example.com/shop", "-surla-version", starterVersionForWorkTest}, io.Discard)
	require.ErrorContains(t, err, "controlled-init-failure")
	assert.NoDirExists(t, filepath.Join(workspace, "shop"))
	left, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, left)
	markers, err := filepath.Glob(filepath.Join(operator, "surla", "operator", "init-stage", ".surla-init-logs-*", "visited"))
	require.NoError(t, err)
	require.Len(t, markers, 1, "the failed command's protected logs are retained")
	stages, err := filepath.Glob(filepath.Join(operator, "surla", "operator", "init-stage", ".surla-init-*"))
	require.NoError(t, err)
	assert.Len(t, stages, 1, "only the failed log directory remains")
}

// isolateLocalWork supplies a test-owned platform configuration parent. Callers
// are serial command tests; remote-client and snapshot tests never use it.
func isolateLocalWork(t *testing.T) string {
	t.Helper()
	parent := t.TempDir()
	prior := userConfigDir
	userConfigDir = func() (string, error) { return parent, nil }
	t.Cleanup(func() { userConfigDir = prior })
	return parent
}
