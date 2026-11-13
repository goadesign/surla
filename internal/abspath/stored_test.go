package abspath

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
// that FDPath detects an unlinked open file by comparing the returned path's
// device and inode against the descriptor and returns ErrUnlinked, while
// preserving a real file whose name ends with " (deleted)".
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

	// 2. Once the file is unlinked, FDPath must return ErrUnlinked rather than
	// "/.../report (deleted)" or "/.../report (deleted) (deleted)".
	require.NoError(t, os.Remove(realDeletedName))
	_, err = FDPath(fd)
	require.ErrorIs(t, err, ErrUnlinked)
}

// TestFDPathTwoHardLinks opens a file with two hard links through one name,
// removes that name, and creates new files at both the original name and the
// " (deleted)" name the Linux kernel reports, verifying that FDPath returns
// ErrUnlinked whenever the kernel's reported path no longer names the open
// descriptor's file.
func TestFDPathTwoHardLinks(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	a := filepath.Join(base, "a")
	b := filepath.Join(base, "b")
	require.NoError(t, os.WriteFile(a, []byte("mine\n"), 0o600))
	require.NoError(t, os.Link(a, b))
	fd, err := unix.Open(a, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, unix.Close(fd)) })
	require.NoError(t, os.Remove(a))

	var held unix.Stat_t
	require.NoError(t, unix.Fstat(fd, &held))
	assert.EqualValues(t, 1, held.Nlink)

	// 1. With no replacement file: on Linux /proc/self/fd/<fd> reports
	// ".../a (deleted)" which does not exist -> ErrUnlinked; on Darwin
	// F_GETPATH returns ENOENT when the opened hard link is unlinked -> ErrUnlinked.
	rawPath, rawErr := fdPath(fd)
	got, err := FDPath(fd)
	t.Logf("after removing a (no replacement): raw fdPath=%q (%v), FDPath=%q (%v)", rawPath, rawErr, got, err)
	require.ErrorIs(t, err, ErrUnlinked)
	assert.Empty(t, got)

	// 2. Creating another file named "a (deleted)" and another file named "a"
	// must never cause FDPath to return a path belonging to a different inode.
	otherDeleted := filepath.Join(base, "a (deleted)")
	require.NoError(t, os.WriteFile(otherDeleted, []byte("someone else's\n"), 0o600))
	require.NoError(t, os.WriteFile(a, []byte("replacement at a\n"), 0o600))

	rawPath, rawErr = fdPath(fd)
	got, err = FDPath(fd)
	t.Logf("after creating replacement files: raw fdPath=%q (%v), FDPath=%q (%v)", rawPath, rawErr, got, err)
	require.ErrorIs(t, err, ErrUnlinked)
	assert.Empty(t, got)
}

// TestFDPathWithoutProc verifies that FDPath and Stored do not report a live
// directory as ErrUnlinked when /proc is not mounted on Linux.
func TestFDPathWithoutProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-only /proc check")
	}
	if _, err := os.Stat("/proc/self"); err == nil {
		t.Skip("needs a root without /proc")
	}
	dir := t.TempDir()
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, unix.Close(fd)) })
	_, err = FDPath(fd)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrUnlinked), "a live directory is reported as unlinked")
	_, err = Stored(dir)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrUnlinked), "a live directory is reported as unlinked")
}
