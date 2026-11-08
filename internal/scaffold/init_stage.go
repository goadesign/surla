// This file owns the unpublished starter tree and command logs. The operator
// supplies their protected parent; only a completed tree reaches the target.
package scaffold

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/goadesign/surla/internal/abspath"
)

// initStage keeps the one build tree and its separate logs under the protected
// parent. The opened destination parent fixes where publication takes place.
type initStage struct {
	// parent is the operator-owned staging parent, held through publication.
	parent *os.File
	// destination is the opened parent of the new project.
	destination *os.File
	// path and logs name the build tree and command output directories.
	path, logs string
	// target is the resolved destination path shown in errors.
	target string
}

// newInitStage allocates a single unpublished project and its logs. The work
// parent must be outside the destination parent and on the same filesystem.
func newInitStage(target, workDir string) (*initStage, error) {
	if workDir == "" {
		return nil, errors.New("init requires an operator-owned work directory")
	}
	parentPath, err := abspath.Eval(filepath.Dir(target))
	if err != nil {
		return nil, fmt.Errorf("resolve project parent: %w", err)
	}
	workPath, err := abspath.Eval(workDir)
	if err != nil {
		return nil, fmt.Errorf("resolve init work directory: %w", err)
	}
	rel, err := filepath.Rel(parentPath, workPath)
	if err != nil {
		return nil, err
	}
	if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("init work directory %s must be outside project parent %s", workPath, parentPath)
	}
	parent, err := os.Open(workPath)
	if err != nil {
		return nil, err
	}
	destination, err := os.Open(parentPath)
	if err != nil {
		return nil, errors.Join(err, parent.Close())
	}
	s := &initStage{parent: parent, destination: destination, target: filepath.Join(parentPath, filepath.Base(target))}
	if err := s.checkDevice(); err != nil {
		return nil, errors.Join(err, s.close())
	}
	s.path, err = os.MkdirTemp(workPath, ".surla-init-*")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create staging directory: %w", err), s.close())
	}
	s.logs, err = os.MkdirTemp(workPath, ".surla-init-logs-*")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create log directory: %w", err), os.RemoveAll(s.path), s.close())
	}
	return s, nil
}

// syncInitTree synchronizes regular files and then directories from children
// to parents before the completed project is made visible.
func syncInitTree(root string) error {
	var dirs []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			dirs = append(dirs, path)
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("starter path %s is not a regular file", path)
		}
		return syncInitPath(path)
	})
	if err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := syncInitPath(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// syncInitPath flushes file data or directory entries and reports close errors.
func syncInitPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// close releases only directory handles; Init owns file retention decisions.
func (s *initStage) close() error {
	return errors.Join(s.parent.Close(), s.destination.Close())
}

// checkDevice rejects an unusable work parent before any project is built.
func (s *initStage) checkDevice() error {
	var source, destination unix.Stat_t
	if err := unix.Fstat(int(s.parent.Fd()), &source); err != nil {
		return err
	}
	if err := unix.Fstat(int(s.destination.Fd()), &destination); err != nil {
		return err
	}
	if source.Mode&syscall.S_IFMT != syscall.S_IFDIR {
		return errors.New("init work parent is not a directory")
	}
	if source.Dev != destination.Dev {
		return errors.New("init work directory must share the project's filesystem; choose protected storage on that filesystem")
	}
	return nil
}

// publish moves the synchronized tree without replacing any existing target.
// After rename succeeds, failures leave the published project intact.
func (s *initStage) publish() error {
	if err := os.Chmod(s.path, 0o755); err != nil {
		return fmt.Errorf("set project permissions: %w", err)
	}
	if err := syncInitTree(s.path); err != nil {
		return fmt.Errorf("sync starter project: %w", err)
	}
	if err := unix.RenameatxNp(int(s.parent.Fd()), filepath.Base(s.path), int(s.destination.Fd()), filepath.Base(s.target), unix.RENAME_EXCL); err != nil {
		return fmt.Errorf("move project into %s: %w", s.target, err)
	}
	if err := errors.Join(s.destination.Sync(), s.parent.Sync()); err != nil {
		return fmt.Errorf("project created at %s but directory synchronization failed: %w", s.target, err)
	}
	return nil
}
