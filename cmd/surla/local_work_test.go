// These tests keep command storage inside test-owned roots and exercise path
// selection independently of compiler and provider availability.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// starterVersionForWorkTest is parsed by the CLI but never downloaded.
const starterVersionForWorkTest = "v0.0.0-20261110163737-d9beac313142"

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
		require.NoError(t, os.Chmod(work, 0o750))
		reused, err := localInitWorkDir(target)
		require.NoError(t, err)
		assert.Equal(t, work, reused, "an accessible existing parent remains eligible")
	}
	work, err := localEvalWorkDir(destination)
	require.NoError(t, err)
	assert.Equal(t, "designeval-work", filepath.Base(work))
	require.NoError(t, os.Chmod(work, 0o750))
	reused, err := localEvalWorkDir(destination)
	require.NoError(t, err)
	assert.Equal(t, work, reused, "an accessible existing evaluator parent remains eligible")
	left, err := os.ReadDir(tmp)
	require.NoError(t, err)
	assert.Empty(t, left)
}

// TestInitConsidersTheCompleteLocationBeforeSelecting proves that a creatable
// configuration root cannot hide a usable temporary location when its actual
// init-stage directory is unusable. The fallback also stays test-owned.
func TestInitConsidersTheCompleteLocationBeforeSelecting(t *testing.T) {
	config := isolateLocalWork(t)
	workspace, destination, tmp := t.TempDir(), t.TempDir(), t.TempDir()
	t.Chdir(workspace)
	t.Setenv("TMPDIR", tmp)
	configRoot := filepath.Join(config, "surla", "operator")
	require.NoError(t, os.MkdirAll(configRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configRoot, "init-stage"), []byte("keep"), 0o600))
	parent := filepath.Join(destination, "projects")
	require.NoError(t, os.Mkdir(parent, 0o700))
	work, err := localInitWorkDir(parent)
	require.NoError(t, err)
	want, err := abspath.Eval(filepath.Join(tmp, fmt.Sprintf("surla-operator-%d", os.Getuid()), "init-stage"))
	require.NoError(t, err)
	assert.Equal(t, want, work, "the next complete usable location wins before the destination fallback")
	assert.FileExists(t, filepath.Join(configRoot, "init-stage"))
}

// TestLocalWorkSkipsDirectoriesWithoutRequiredPermissions preserves unusable
// leaves and selects a location that supports allocation and cleanup.
func TestLocalWorkSkipsDirectoriesWithoutRequiredPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("UID 0 bypasses these directory permission restrictions; this case requires an unprivileged process")
	}
	for _, tc := range []struct {
		// name is the operation whose actual work parent is selected.
		name string
		// leaf is the protected resource, not merely its operator base.
		leaf string
		// choose calls the operation's production selection entry point.
		choose func(string) (string, error)
	}{
		{name: "init", leaf: "init-stage", choose: localInitWorkDir},
		{name: "evaluator", leaf: "designeval-work", choose: func(target string) (string, error) {
			return localEvalWorkDir(target)
		}},
	} {
		for _, mode := range []os.FileMode{0o300, 0o500} {
			t.Run(fmt.Sprintf("%s/mode_%04o", tc.name, mode), func(t *testing.T) {
				config := isolateLocalWork(t)
				workspace, destination, tmp := t.TempDir(), t.TempDir(), t.TempDir()
				t.Chdir(workspace)
				t.Setenv("TMPDIR", tmp)
				blocked := filepath.Join(config, "surla", "operator", tc.leaf)
				require.NoError(t, os.MkdirAll(blocked, 0o700))
				require.NoError(t, os.Chmod(blocked, mode))
				t.Cleanup(func() { require.NoError(t, os.Chmod(blocked, 0o700)) })
				parent := filepath.Join(destination, "projects")
				require.NoError(t, os.Mkdir(parent, 0o700))
				work, err := tc.choose(parent)
				require.NoError(t, err)
				want, err := abspath.Eval(filepath.Join(tmp, fmt.Sprintf("surla-operator-%d", os.Getuid()), tc.leaf))
				require.NoError(t, err)
				assert.Equal(t, want, work, "the actual usable leaf wins before any later fallback")
				info, err := os.Stat(blocked)
				require.NoError(t, err)
				assert.Equal(t, mode, info.Mode().Perm(), "selection does not change an existing leaf's permissions")
			})
		}
	}
}

// TestInitUsesTemporaryStorageOnTheDestinationDevice covers a creatable config
// root on the startup disk followed by TMPDIR on the destination disk. A host
// without a writable second mounted filesystem skips only this physical case.
func TestInitUsesTemporaryStorageOnTheDestinationDevice(t *testing.T) {
	config := isolateLocalWork(t)
	startup := t.TempDir()
	var source syscall.Stat_t
	require.NoError(t, syscall.Stat(config, &source))
	volumes, err := os.ReadDir("/Volumes")
	if err != nil {
		t.Skip("no mounted volume directory available for a second-filesystem test")
	}
	secondTemp := ""
	for _, volume := range volumes {
		candidate := filepath.Join("/Volumes", volume.Name(), "tmp")
		var target syscall.Stat_t
		if syscall.Stat(candidate, &target) == nil && target.Mode&unix.S_IFMT == unix.S_IFDIR && target.Dev != source.Dev && unix.Access(candidate, unix.R_OK|unix.W_OK|unix.X_OK) == nil {
			secondTemp = candidate
			break
		}
	}
	if secondTemp == "" {
		t.Skip("requires a writable tmp directory on a second mounted filesystem; complete-location ordering is covered without a second mount")
	}
	t.Setenv("TMPDIR", secondTemp)
	t.Run("uses destination device before unwritable fallback", func(t *testing.T) {
		volumeRoot := t.TempDir()
		parent, tmp := filepath.Join(volumeRoot, "projects"), filepath.Join(volumeRoot, "tmp")
		require.NoError(t, os.Mkdir(parent, 0o700))
		require.NoError(t, os.Mkdir(tmp, 0o700))
		t.Chdir(startup)
		t.Setenv("TMPDIR", tmp)
		require.NoError(t, os.Chmod(volumeRoot, 0o500))
		t.Cleanup(func() { require.NoError(t, os.Chmod(volumeRoot, 0o700)) })
		work, err := localInitWorkDir(parent)
		require.NoError(t, err)
		want, err := abspath.Eval(filepath.Join(tmp, fmt.Sprintf("surla-operator-%d", os.Getuid()), "init-stage"))
		require.NoError(t, err)
		assert.Equal(t, want, work)
	})
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

// TestProtectedDirectoryRejectsCaseAliases stops before creating compiler or
// staging directories through a non-symlink alias of a builder directory.
func TestProtectedDirectoryRejectsCaseAliases(t *testing.T) {
	base := t.TempDir()
	workspace := filepath.Join(base, "workspace")
	alias := filepath.Join(base, "WORKSPACE")
	require.NoError(t, os.Mkdir(workspace, 0o700))
	a, err := os.Stat(workspace)
	require.NoError(t, err)
	b, err := os.Stat(alias)
	if os.IsNotExist(err) {
		t.Skip("filesystem does not provide this case alias")
	}
	require.NoError(t, err)
	require.True(t, os.SameFile(a, b))
	for _, pair := range [][2]string{{workspace, alias}, {alias, workspace}} {
		_, err := createProtectedDir(filepath.Join(pair[1], "new"), []string{pair[0]})
		require.ErrorContains(t, err, "must be outside workspace")
	}
	assert.NoDirExists(t, filepath.Join(workspace, "new"))
	_, err = createProtectedDir(filepath.Join(base, "outside"), []string{filepath.Join(base, "missing-root")})
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.NoDirExists(t, filepath.Join(base, "outside"))
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
	t.Setenv("SURLA_TEST_USER_CONFIG_DIR", parent)
	return parent
}
