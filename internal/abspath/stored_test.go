package abspath

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

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

// TestFDPathRejectsUnlinkedFileAndPreservesDeletedSuffixInRealName verifies
// that FDPath detects an unlinked open file by its link count and returns
// ErrUnlinked, while preserving a real file whose name ends with " (deleted)".
func TestFDPathRejectsUnlinkedFileAndPreservesDeletedSuffixInRealName(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	// 1. A real file whose name ends with " (deleted)" must be returned verbatim.
	realDeletedName := filepath.Join(base, "report (deleted)")
	require.NoError(t, os.WriteFile(realDeletedName, []byte("ok\n"), 0o600))
	fd, err := unix.Open(realDeletedName, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, unix.Close(fd)) })

	gotFDPath, err := FDPath(fd)
	require.NoError(t, err)
	assert.Equal(t, realDeletedName, gotFDPath)

	gotStored, err := Stored(realDeletedName)
	require.NoError(t, err)
	assert.Equal(t, realDeletedName, gotStored)

	// 2. Once the file is unlinked, its link count drops to 0 and FDPath must
	// return ErrUnlinked rather than "/.../report (deleted) (deleted)".
	require.NoError(t, os.Remove(realDeletedName))
	_, err = FDPath(fd)
	require.ErrorIs(t, err, ErrUnlinked)
}
