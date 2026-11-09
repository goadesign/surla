// Package abspath resolves existing and not-yet-created filesystem paths
// without changing which directory a symlink followed by a parent component
// selects.
package abspath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// maxSymlinkExpansions bounds symlink resolution to match filepath.EvalSymlinks.
const maxSymlinkExpansions = 255

// Eval returns an absolute, symlink-free path to an existing file or directory.
// Candidate and live-process callers resolve relative paths from their working
// directory, including a
// per-drive working directory on Windows. It preserves the supplied components
// until symlinks have been resolved. An empty path returns os.ErrNotExist;
// filesystem and working-directory errors are returned to the caller.
func Eval(path string) (string, error) {
	if path == "" {
		return "", os.ErrNotExist
	}
	if !filepath.IsAbs(path) {
		volume := filepath.VolumeName(path)
		var base string
		var err error
		if volume == "" {
			base, err = os.Getwd()
		} else {
			base, err = filepath.Abs(volume + ".")
		}
		if err != nil {
			return "", err
		}
		relative := strings.TrimPrefix(path, volume)
		if len(relative) > 0 && os.IsPathSeparator(relative[0]) {
			path = filepath.VolumeName(base) + relative
		} else {
			// Join would clean away parent components before following links.
			path = base + string(os.PathSeparator) + relative
		}
	}
	return filepath.EvalSymlinks(path)
}

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

// EvalMissing returns an absolute, symlink-free path for a file or directory
// that may or may not exist yet, resolving relative paths from the current
// working directory. Every existing or dangling symbolic link is expanded
// left-to-right before applying parent ("..") components. An empty path returns
// os.ErrNotExist.
func EvalMissing(path string) (string, error) {
	if path == "" {
		return "", os.ErrNotExist
	}
	if filepath.IsAbs(path) {
		vol := filepath.VolumeName(path)
		root := vol + string(filepath.Separator)
		return evalComponents(root, path[len(vol):], path)
	}
	volume := filepath.VolumeName(path)
	start := "."
	if volume != "" {
		start = volume + "."
	}
	base, err := Eval(start)
	if err != nil {
		return "", err
	}
	relative := strings.TrimPrefix(path, volume)
	if len(relative) > 0 && os.IsPathSeparator(relative[0]) {
		root := filepath.VolumeName(base) + string(filepath.Separator)
		return evalComponents(root, relative, path)
	}
	return evalComponents(base, relative, path)
}

// EvalFrom resolves path (which may or may not exist yet) relative to baseDir
// when path is relative, or from the filesystem root when path is absolute,
// expanding symbolic links left-to-right before applying parent ("..")
// components. An empty path or empty baseDir (for a relative path) returns
// os.ErrNotExist.
func EvalFrom(baseDir, path string) (string, error) {
	if path == "" {
		return "", os.ErrNotExist
	}
	if filepath.IsAbs(path) {
		return EvalMissing(path)
	}
	if baseDir == "" {
		return "", os.ErrNotExist
	}
	base, err := EvalMissing(baseDir)
	if err != nil {
		return "", err
	}
	return evalComponents(base, path, path)
}

// Within reports whether path names root or a descendant of root, comparing
// actual filesystem identities so differently spelled aliases also match.
// Storage admission callers supply an existing directory as root; path may
// have a missing suffix. Symlinks are expanded before parent components, as in
// EvalMissing. Missing roots, non-directory roots, and filesystem errors other
// than missing path components return errors. The result describes the named
// paths during this call, without protecting them against concurrent renames.
func Within(root, path string) (bool, error) {
	resolvedRoot, err := Eval(root)
	if err != nil {
		return false, fmt.Errorf("resolve root %s: %w", root, err)
	}
	rootInfo, err := os.Stat(resolvedRoot)
	if err != nil {
		return false, err
	}
	if !rootInfo.IsDir() {
		return false, fmt.Errorf("root %s is not a directory", root)
	}
	resolvedPath, err := EvalMissing(path)
	if err != nil {
		return false, fmt.Errorf("resolve path %s: %w", path, err)
	}
	for cur := resolvedPath; ; cur = filepath.Dir(cur) {
		info, err := os.Stat(cur)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		if err == nil && os.SameFile(rootInfo, info) {
			return true, nil
		}
		if filepath.Dir(cur) == cur {
			return false, nil
		}
	}
}

// evalComponents walks rel's path components left-to-right from the resolved
// physical directory cur, expanding symbolic links before applying "..".
func evalComponents(cur, rel, origPath string) (string, error) {
	queue := splitComponents(rel)
	links := 0
	for len(queue) > 0 {
		elem := queue[0]
		queue = queue[1:]
		if elem == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, elem)
		info, err := os.Lstat(next)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				cur = next
				continue
			}
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > maxSymlinkExpansions {
				return "", &os.PathError{Op: "EvalMissing", Path: origPath, Err: syscall.ELOOP}
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", err
			}
			if filepath.IsAbs(target) {
				vol := filepath.VolumeName(target)
				cur = vol + string(filepath.Separator)
				target = target[len(vol):]
			}
			queue = append(splitComponents(target), queue...)
			continue
		}
		if len(queue) > 0 && !info.IsDir() {
			return "", &os.PathError{Op: "lstat", Path: next, Err: syscall.ENOTDIR}
		}
		cur = next
	}
	return cur, nil
}

// splitComponents splits p on path separators, omitting empty and "." segments
// while preserving ".." segments in order.
func splitComponents(p string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || os.IsPathSeparator(p[i]) || p[i] == '/' {
			part := p[start:i]
			if part != "" && part != "." {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}
