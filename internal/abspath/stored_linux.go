package abspath

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// fdPath reads the kernel path of open descriptor fd from /proc/self/fd on Linux.
func fdPath(fd int) (string, error) {
	buf := make([]byte, unix.PathMax)
	n, err := unix.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd), buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
