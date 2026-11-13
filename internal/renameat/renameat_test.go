package renameat

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestNoReplaceMovesWhenDestinationIsAbsentAndRefusesExisting verifies that
// NoReplace moves an entry when the destination does not exist and refuses
// with EEXIST without modifying either entry when the destination exists.
func TestNoReplaceMovesWhenDestinationIsAbsentAndRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, unix.Close(dirFD)) })

	require.NoError(t, os.WriteFile(filepath.Join(dir, "src.txt"), []byte("fresh\n"), 0o600))
	require.NoError(t, NoReplace(dirFD, "src.txt", dirFD, "dst.txt"))

	got, err := os.ReadFile(filepath.Join(dir, "dst.txt"))
	require.NoError(t, err)
	assert.Equal(t, "fresh\n", string(got))
	_, err = os.Stat(filepath.Join(dir, "src.txt"))
	assert.ErrorIs(t, err, fs.ErrNotExist)

	// Attempting to overwrite an existing destination must fail with EEXIST
	// and leave both the existing destination and staged source untouched.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte("replacement\n"), 0o600))
	err = NoReplace(dirFD, "candidate.txt", dirFD, "dst.txt")
	require.Error(t, err)
	assert.ErrorIs(t, err, unix.EEXIST)
	assert.ErrorIs(t, err, fs.ErrExist)
	assert.False(t, errors.Is(err, ErrUnsupported))

	dstAfter, err := os.ReadFile(filepath.Join(dir, "dst.txt"))
	require.NoError(t, err)
	assert.Equal(t, "fresh\n", string(dstAfter))
	srcAfter, err := os.ReadFile(filepath.Join(dir, "candidate.txt"))
	require.NoError(t, err)
	assert.Equal(t, "replacement\n", string(srcAfter))
}

// TestExchangeSwapsDirectoriesWithoutMomentaryAbsence verifies that Exchange
// swaps two directories atomically so concurrent readers always observe either
// the old or new file at both names and never observe ENOENT.
func TestExchangeSwapsDirectoriesWithoutMomentaryAbsence(t *testing.T) {
	dir := t.TempDir()
	dirFD, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, unix.Close(dirFD)) })

	dirA := filepath.Join(dir, "a")
	dirB := filepath.Join(dir, "b")
	require.NoError(t, os.Mkdir(dirA, 0o755))
	require.NoError(t, os.Mkdir(dirB, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dirA, "state.txt"), []byte("alpha"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dirB, "state.txt"), []byte("beta"), 0o600))

	const swaps = 200
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, p := range []string{
					filepath.Join(dirA, "state.txt"),
					filepath.Join(dirB, "state.txt"),
				} {
					data, readErr := os.ReadFile(p)
					if !assert.NoError(t, readErr, "entry %s disappeared during Exchange", p) {
						return
					}
					s := string(data)
					if !assert.True(t, s == "alpha" || s == "beta", "unexpected content %q", s) {
						return
					}
				}
			}
		}()
	}

	for range swaps {
		require.NoError(t, Exchange(dirFD, "a", dirFD, "b"))
	}
	close(stop)
	wg.Wait()

	// Perform one final odd swap to verify the two trees exchanged places.
	require.NoError(t, Exchange(dirFD, "a", dirFD, "b"))
	gotA, err := os.ReadFile(filepath.Join(dirA, "state.txt"))
	require.NoError(t, err)
	gotB, err := os.ReadFile(filepath.Join(dirB, "state.txt"))
	require.NoError(t, err)
	assert.Equal(t, "beta", string(gotA))
	assert.Equal(t, "alpha", string(gotB))
}

// TestWrapErrPreservesBothErrUnsupportedAndErrno verifies that unsupported
// kernel/filesystem errnos wrap both ErrUnsupported and the original errno,
// while other errnos pass through unchanged.
func TestWrapErrPreservesBothErrUnsupportedAndErrno(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		unsupported bool
	}{
		{name: "ENOSYS", err: unix.ENOSYS, unsupported: true},
		{name: "ENOTSUP", err: unix.ENOTSUP, unsupported: true},
		{name: "EOPNOTSUPP", err: unix.EOPNOTSUPP, unsupported: true},
		{name: "EINVAL", err: unix.EINVAL, unsupported: true},
		{name: "EEXIST", err: unix.EEXIST, unsupported: false},
		{name: "ENOENT", err: unix.ENOENT, unsupported: false},
		{name: "EXDEV", err: syscall.EXDEV, unsupported: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wrapErr(tc.err)
			require.ErrorIs(t, got, tc.err)
			assert.Equal(t, tc.unsupported, errors.Is(got, ErrUnsupported))
		})
	}
	assert.NoError(t, wrapErr(nil))
}
