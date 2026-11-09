package abspath

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Stored returns the path the volume stores for an existing file or
// directory: Eval's symlink-free path in the spelling the kernel reports for
// the opened file (F_GETPATH). On a case- or normalization-insensitive
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
	buf := make([]byte, unix.PathMax)
	_, err = unix.FcntlInt(uintptr(fd), unix.F_GETPATH, int(uintptr(unsafe.Pointer(&buf[0]))))
	if err := errors.Join(err, unix.Close(fd)); err != nil {
		return "", fmt.Errorf("read the stored path of %s: %w", phys, err)
	}
	return unix.ByteSliceToString(buf), nil
}
