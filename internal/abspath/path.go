// Package abspath resolves existing filesystem paths without changing which
// directory a symlink followed by a parent component selects.
package abspath

import (
	"os"
	"path/filepath"
	"strings"
)

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
