package abspath

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// fdPath reads the kernel path of open descriptor fd via F_GETPATH on Darwin.
// FcntlFlock routes the buffer through unix.fcntlPtr (unsafe.Pointer) rather
// than unix.FcntlInt (int) so a stack split on entry to the x/sys/unix wrapper
// updates the pointer before the nosplit libc call.
func fdPath(fd int) (string, error) {
	buf := make([]byte, unix.PathMax)
	if err := unix.FcntlFlock(
		uintptr(fd),
		unix.F_GETPATH,
		(*unix.Flock_t)(unsafe.Pointer(&buf[0])),
	); err != nil {
		return "", err
	}
	return unix.ByteSliceToString(buf), nil
}
