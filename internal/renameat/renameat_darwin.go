package renameat

import "golang.org/x/sys/unix"

// noReplace invokes Darwin's renameatx_np(2) with RENAME_EXCL.
func noReplace(oldFD int, oldPath string, newFD int, newPath string) error {
	return unix.RenameatxNp(oldFD, oldPath, newFD, newPath, unix.RENAME_EXCL)
}

// exchange invokes Darwin's renameatx_np(2) with RENAME_SWAP.
func exchange(oldFD int, oldPath string, newFD int, newPath string) error {
	return unix.RenameatxNp(oldFD, oldPath, newFD, newPath, unix.RENAME_SWAP)
}
