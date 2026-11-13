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
// It verifies via Lstat that the returned path exists and has the same device
// and inode as fd; if the path no longer exists or resolves to a different
// file (for example after the opened name was unlinked or replaced), FDPath
// returns ErrUnlinked rather than a stale or kernel-annotated path.
//
// The returned path names fd's file when FDPath checks it, but it is not
// necessarily the name fd was opened through. A file with several hard links
// may be reported under any of them: after the opened link is unlinked, Darwin
// answers either with an error or with a surviving link's path, and the answer
// can change between calls. Callers that need a particular name compare the
// result with it and treat a mismatch, like ErrUnlinked, as a changed path;
// callers that need a particular file compare device and inode.
func FDPath(fd int) (string, error) {
	var fdSt unix.Stat_t
	if err := unix.Fstat(fd, &fdSt); err != nil {
		return "", err
	}
	p, err := fdPath(fd)
	if err != nil {
		return "", err
	}
	var pathSt unix.Stat_t
	if err := unix.Lstat(p, &pathSt); err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return "", fmt.Errorf("%w: %w", ErrUnlinked, err)
		}
		return "", err
	}
	if pathSt.Dev != fdSt.Dev || pathSt.Ino != fdSt.Ino {
		return "", fmt.Errorf(
			"%w: path %s resolves to (%d:%d), want (%d:%d)",
			ErrUnlinked,
			p,
			uint64(pathSt.Dev),
			pathSt.Ino,
			uint64(fdSt.Dev),
			fdSt.Ino,
		)
	}
	return p, nil
}
