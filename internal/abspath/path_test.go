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
	_, err = EvalMissing("")
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = EvalFrom("", "rel")
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = EvalFrom(t.TempDir(), "")
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestEvalMissingAndEvalFromResolveSymlinksBeforeParentComponents verifies that
// EvalMissing and EvalFrom expand existing and dangling symlinks left-to-right
// before applying ".." even when the final target does not exist yet.
func TestEvalMissingAndEvalFromResolveSymlinksBeforeParentComponents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the symlink fixture requires permission to create Unix-style links")
	}
	base := t.TempDir()
	t.Chdir(base)
	require.NoError(t, os.MkdirAll("target/workspace/nested", 0o700))
	require.NoError(t, os.MkdirAll("outside", 0o700))
	require.NoError(t, os.Symlink("target/workspace/nested", "alias"))
	require.NoError(t, os.Symlink("target/workspace/missing-nested", "dangling-alias"))
	require.NoError(t, os.Symlink(filepath.Join(base, "target", "workspace", "cards"), "outside/cards-link"))

	physicalBase, err := filepath.EvalSymlinks(base)
	require.NoError(t, err)
	wantCards := filepath.Join(physicalBase, "target", "workspace", "cards")
	wantDeep := filepath.Join(physicalBase, "target", "workspace", "cards", "sub")

	for _, p := range []string{
		"alias/../cards",
		base + "/alias/../cards",
		"dangling-alias/../cards",
		base + "/dangling-alias/../cards",
		"outside/cards-link",
		base + "/outside/cards-link",
	} {
		got, err := EvalMissing(p)
		require.NoError(t, err, p)
		assert.Equal(t, wantCards, got, p)
	}

	gotFromRel, err := EvalFrom(filepath.Join(base, "alias"), "../cards/sub")
	require.NoError(t, err)
	assert.Equal(t, wantDeep, gotFromRel)

	gotFromAbs, err := EvalFrom(filepath.Join(base, "outside"), base+"/alias/../cards/sub")
	require.NoError(t, err)
	assert.Equal(t, wantDeep, gotFromAbs)

	// Symlink cycle returns an error without looping indefinitely.
	require.NoError(t, os.Symlink("loop-b", filepath.Join(base, "loop-a")))
	require.NoError(t, os.Symlink("loop-a", filepath.Join(base, "loop-b")))
	_, err = EvalMissing(filepath.Join(base, "loop-a", "child"))
	require.Error(t, err)
}

// TestStoredReturnsOneSpellingPerDirectory verifies that every spelling a
// case- and normalization-insensitive volume resolves to one directory
// yields that directory's stored path.
func TestStoredReturnsOneSpellingPerDirectory(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(base, "café", "Repo"), 0o755))
	if _, err := os.Stat(filepath.Join(base, "CAFÉ", "REPO")); err != nil {
		t.Skip("needs a case- and normalization-insensitive volume")
	}
	want := filepath.Join(base, "café", "Repo")
	for _, spelling := range []string{
		want,
		filepath.Join(base, "CAFÉ", "repo"),
		filepath.Join(base, "café", "REPO"),
	} {
		got, err := Stored(spelling)
		require.NoError(t, err)
		assert.Equal(t, want, got, spelling)
	}
	_, err = Stored(filepath.Join(base, "missing"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
