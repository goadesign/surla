package abspath

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// fdPath reads the kernel path of open descriptor fd from /proc/self/fd on Linux.
// Because FDPath validates fd with Fstat before calling fdPath, an ENOENT or
// ENOTDIR error from /proc/self/fd/<fd> means /proc is not mounted rather than
// that the target file was unlinked.
func fdPath(fd int) (string, error) {
	buf := make([]byte, unix.PathMax)
	procPath := fmt.Sprintf("/proc/self/fd/%d", fd)
	n, err := unix.Readlink(procPath, buf)
	if err != nil {
		if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) {
			return "", fmt.Errorf("read %s (/proc not mounted): %w", procPath, unix.ENOSYS)
		}
		return "", err
	}
	return string(buf[:n]), nil
}
