// This file opens a directory entry on Darwin without following a symbolic link.
package controlplane

import "golang.org/x/sys/unix"

// openEntryFlags opens any directory entry itself on Darwin: a symbolic link
// is opened rather than followed (O_SYMLINK), and a FIFO never blocks.
const openEntryFlags = unix.O_RDONLY | unix.O_SYMLINK | unix.O_NONBLOCK | unix.O_CLOEXEC

// openEntry opens name in dirFD without following a symbolic link or blocking
// on a FIFO. Callers in check_copy.go use the returned descriptor with
// unix.Fstat / os.File.Stat and abspath.FDPath on every entry kind, and read
// directory entries or regular file bytes when the entry is a directory or
// regular file.
func openEntry(dirFD int, name string) (int, error) {
	return unix.Openat(dirFD, name, openEntryFlags, 0)
}
