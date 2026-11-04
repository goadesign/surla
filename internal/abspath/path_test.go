package abspath

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEvalPreservesFilesystemPaths distinguishes the physical directory from
// the directory that lexical cleanup would select before resolving a symlink.
func TestEvalPreservesFilesystemPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the symlink fixture requires permission to create Unix-style links")
	}
	base := t.TempDir()
	t.Chdir(base)
	require.NoError(t, os.MkdirAll("target/nested", 0o700))
	require.NoError(t, os.Mkdir("target/state", 0o700))
	require.NoError(t, os.Mkdir("state", 0o700))
	require.NoError(t, os.Symlink("target/nested", "alias"))
	physicalBase, err := filepath.EvalSymlinks(base)
	require.NoError(t, err)
	for _, path := range []string{"alias/../state", base + "/alias/../state"} {
		got, err := Eval(path)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(physicalBase, "target", "state"), got)
	}

	t.Run("logical_working_directory", func(t *testing.T) {
		t.Chdir("alias")
		t.Setenv("PWD", base+"/alias")
		got, err := Eval("../state")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(physicalBase, "target", "state"), got)
	})
	t.Run("logical_working_directory_parent", func(t *testing.T) {
		t.Chdir("target")
		t.Setenv("PWD", base+"/alias/..")
		got, err := Eval("state")
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(physicalBase, "target", "state"), got)
	})
}

// TestEvalRejectsEmptyPath preserves required-path validation at caller entry.
func TestEvalRejectsEmptyPath(t *testing.T) {
	_, err := Eval("")
	require.ErrorIs(t, err, os.ErrNotExist)
}
