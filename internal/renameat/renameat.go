// Package renameat performs atomic directory-relative renames that either
// refuse to overwrite an existing destination or exchange two existing paths,
// using each operating system's native kernel syscall without non-atomic
// fallbacks.
package renameat

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// ErrUnsupported reports that the kernel or target filesystem does not support
// the requested atomic rename operation.
var ErrUnsupported = errors.New("filesystem does not support atomic rename")

// NoReplace atomically renames oldPath in oldFD to newPath in newFD, refusing
// to overwrite an existing entry at newPath. Callers may pass unix.AT_FDCWD when
// oldPath or newPath is resolved from the current working directory.
//
// If newPath already exists, NoReplace returns an error matching unix.EEXIST
// and fs.ErrExist and leaves newPath untouched. If the kernel or target
// filesystem does not support atomic non-replacing rename, NoReplace returns
// an error wrapping both ErrUnsupported and the underlying errno.
func NoReplace(oldFD int, oldPath string, newFD int, newPath string) error {
	return wrapErr(noReplace(oldFD, oldPath, newFD, newPath))
}

// Exchange atomically swaps the existing entries oldPath in oldFD and newPath
// in newFD so neither name is missing at any point observable by concurrent
// readers. Callers may pass unix.AT_FDCWD when oldPath or newPath is resolved
// from the current working directory.
//
// If the kernel or target filesystem does not support atomic exchange, Exchange
// returns an error wrapping both ErrUnsupported and the underlying errno.
func Exchange(oldFD int, oldPath string, newFD int, newPath string) error {
	return wrapErr(exchange(oldFD, oldPath, newFD, newPath))
}

// wrapErr wraps unsupported-operation errnos with ErrUnsupported while
// preserving the original errno for errors.Is callers.
//
// In addition to ENOSYS, ENOTSUP, and EOPNOTSUPP, Linux renameat2(2) and
// Darwin renameatx_np(2) return EINVAL when the target filesystem does not
// support RENAME_NOREPLACE/RENAME_EXCL or RENAME_EXCHANGE/RENAME_SWAP; EINVAL
// can also indicate an invalid caller argument such as renaming a directory
// into its own subdirectory, so both ErrUnsupported and err are wrapped.
func wrapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ENOSYS) ||
		errors.Is(err, unix.ENOTSUP) ||
		errors.Is(err, unix.EOPNOTSUPP) ||
		errors.Is(err, unix.EINVAL) {
		return fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return err
}
