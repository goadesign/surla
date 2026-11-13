package abspath

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// ErrUnlinked reports that an open file descriptor no longer has a stored path
// because its directory entry was unlinked.
var ErrUnlinked = errors.New("open file has been unlinked")

// Stored returns the path the volume stores for an existing file or
// directory: Eval's symlink-free path in the spelling the kernel reports for
// the opened file (FDPath). On a case- or normalization-insensitive
// volume, /Users/dev/REPO and /Users/dev/repo name one directory; Stored
// returns the same string for both, so callers that hash, store or compare
// paths treat every spelling of one directory alike.
func Stored(path string) (string, error) {
	phys, err := Eval(path)
	if err != nil {
		return "", err
	}
	fd, err := unix.Open(phys, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", phys, err)
	}
	stored, err := FDPath(fd)
	if err := errors.Join(err, unix.Close(fd)); err != nil {
		return "", fmt.Errorf("read the stored path of %s: %w", phys, err)
	}
	return stored, nil
}

// FDPath returns the kernel's stored path for the open file descriptor fd.
// If the file's directory entries have been unlinked (link count zero), FDPath
// returns ErrUnlinked rather than a stale or kernel-annotated path string.
func FDPath(fd int) (string, error) {
	p, err := fdPath(fd)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return "", fmt.Errorf("%w: %w", ErrUnlinked, err)
		}
		return "", err
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return "", err
	}
	if st.Nlink == 0 {
		return "", ErrUnlinked
	}
	return p, nil
}
