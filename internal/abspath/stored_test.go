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
// " (deleted)" name the Linux kernel reports. FDPath may then report the
// surviving link b, because Darwin's F_GETPATH answers for a file with several
// links with an error or any remaining link, and the answer can differ from
// one call to the next. Whatever it answers, FDPath must never return a path
// that names another file: it returns ErrUnlinked or the surviving link.
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
	// ".../a (deleted)", which does not exist; on Darwin F_GETPATH reports
	// ENOENT or the surviving link b.
	assertNamesHeldFileOrUnlinked(t, fd, held, b, "after removing a")

	// 2. Creating another file named "a (deleted)" and another file named "a"
	// must never cause FDPath to return a path belonging to a different inode.
	otherDeleted := filepath.Join(base, "a (deleted)")
	require.NoError(t, os.WriteFile(otherDeleted, []byte("someone else's\n"), 0o600))
	require.NoError(t, os.WriteFile(a, []byte("replacement at a\n"), 0o600))
	assertNamesHeldFileOrUnlinked(t, fd, held, b, "after creating replacement files")
}

// assertNamesHeldFileOrUnlinked asserts that FDPath(fd) returns either
// ErrUnlinked with an empty path or exactly the surviving link, and that a
// returned path currently names the held file's device and inode.
func assertNamesHeldFileOrUnlinked(t *testing.T, fd int, held unix.Stat_t, surviving, step string) {
	t.Helper()
	got, err := FDPath(fd)
	if err != nil {
		require.ErrorIs(t, err, ErrUnlinked, step)
		assert.Empty(t, got, step)
		return
	}
	t.Logf("%s: FDPath reported the surviving link %s", step, got)
	require.Equal(t, surviving, got, step)
	var named unix.Stat_t
	require.NoError(t, unix.Lstat(got, &named), step)
	assert.Equal(t, held.Dev, named.Dev, step)
	assert.Equal(t, held.Ino, named.Ino, step)
}

// TestFDPathRemovedAndRecreatedDirectory opens a directory, removes it, and
// creates a new directory at the same name, verifying on both macOS (where
// F_GETPATH continues to return the old path for an open directory descriptor)
// and Linux that FDPath returns ErrUnlinked rather than naming the replacement
// directory.
func TestFDPathRemovedAndRecreatedDirectory(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	dir := filepath.Join(base, "dir")
	require.NoError(t, os.Mkdir(dir, 0o700))
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, unix.Close(fd)) })

	require.NoError(t, os.Remove(dir))
	got, err := FDPath(fd)
	require.ErrorIs(t, err, ErrUnlinked)
	assert.Empty(t, got)

	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Mkdir(dir+" (deleted)", 0o700))
	got, err = FDPath(fd)
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
