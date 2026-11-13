// This file opens a directory entry on Linux without following a symbolic link.
package controlplane

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// openEntry opens name in dirFD on Linux without following a symbolic link or
// blocking on a FIFO.
//
// Callers in check_copy.go use the returned descriptor as follows:
//   - isProtectedEntry -> sameEntry calls unix.Fstat(fd, &st) and abspath.FDPath(fd).
//   - copyEntry wraps fd in os.NewFile, calls f.Stat(), and branches on mode:
//     directories call requireWorkspace (Fstat) and readDirNames (Dup + Readdirnames),
//     regular files call copyFile (io.Copy from f), symbolic links read the raw target
//     via readlinkAt(dirFD, name) without reading from f, and other special entries
//     (FIFOs, sockets, devices) are skipped after f.Stat() without reading from f.
//
// On Linux, Openat with O_NOFOLLOW|O_RDONLY fails with ELOOP on a symbolic link.
// openEntry therefore opens the entry once with O_PATH|O_NOFOLLOW|O_CLOEXEC and
// inspects it with Fstat:
//   - For symbolic links and special entries (neither S_IFREG nor S_IFDIR), it
//     returns the O_PATH descriptor directly; that descriptor was resolved once
//     without following links or blocking on FIFOs and supports Fstat,
//     os.File.Stat(), and /proc/self/fd/<fd> readlink.
//   - For regular files and directories, it reopens the pinned kernel object
//     through the process's own descriptor table (/proc/self/fd/<pathFD>,
//     requiring /proc mounted on Linux) with O_RDONLY|O_NONBLOCK|O_CLOEXEC
//     (plus O_DIRECTORY for directories), closes pathFD, and verifies via Fstat
//     that (Dev, Ino, Mode&S_IFMT) match before returning the readable descriptor.
func openEntry(dirFD int, name string) (int, error) {
	pathFD, err := unix.Openat(dirFD, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	var st unix.Stat_t
	if err := unix.Fstat(pathFD, &st); err != nil {
		return -1, errors.Join(err, unix.Close(pathFD))
	}
	kind := st.Mode & unix.S_IFMT
	if kind != unix.S_IFREG && kind != unix.S_IFDIR {
		return pathFD, nil
	}
	flags := unix.O_RDONLY | unix.O_NONBLOCK | unix.O_CLOEXEC
	if kind == unix.S_IFDIR {
		flags |= unix.O_DIRECTORY
	}
	procPath := fmt.Sprintf("/proc/self/fd/%d", pathFD)
	fd, err := unix.Open(procPath, flags, 0)
	if err != nil && (errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR)) {
		var procSt unix.Stat_t
		if lerr := unix.Lstat(procPath, &procSt); lerr != nil {
			err = fmt.Errorf("open %s (/proc not mounted): %w", procPath, unix.ENOSYS)
		}
	}
	closePathErr := unix.Close(pathFD)
	if err != nil {
		return -1, errors.Join(err, closePathErr)
	}
	if closePathErr != nil {
		return -1, errors.Join(closePathErr, unix.Close(fd))
	}
	var reopened unix.Stat_t
	if err := unix.Fstat(fd, &reopened); err != nil {
		return -1, errors.Join(err, unix.Close(fd))
	}
	if reopened.Dev != st.Dev || reopened.Ino != st.Ino || reopened.Mode&unix.S_IFMT != kind {
		return -1, errors.Join(
			fmt.Errorf("reopened entry %q changed kernel identity", name),
			unix.Close(fd),
		)
	}
	return fd, nil
}
