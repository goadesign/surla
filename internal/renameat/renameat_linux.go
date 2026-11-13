package renameat

import "golang.org/x/sys/unix"

// noReplace invokes Linux's renameat2(2) with RENAME_NOREPLACE.
func noReplace(oldFD int, oldPath string, newFD int, newPath string) error {
	return unix.Renameat2(oldFD, oldPath, newFD, newPath, unix.RENAME_NOREPLACE)
}

// exchange invokes Linux's renameat2(2) with RENAME_EXCHANGE.
func exchange(oldFD int, oldPath string, newFD int, newPath string) error {
	return unix.Renameat2(oldFD, oldPath, newFD, newPath, unix.RENAME_EXCHANGE)
}
