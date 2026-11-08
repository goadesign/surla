// This file owns the unpublished starter tree and command logs. The operator
// supplies their protected parent; only a completed tree reaches the target.
package scaffold

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/goadesign/surla/internal/abspath"
)

// initStage keeps the one build tree and its separate logs under the protected
// parent. Publication creates the requested target without replacing it.
type initStage struct {
	// path and logs name the build tree and command output directories.
	path, logs string
	// target is the resolved destination path used for publication.
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
	var source, destination unix.Stat_t
	if err := unix.Stat(workPath, &source); err != nil {
		return nil, err
	}
	if err := unix.Stat(parentPath, &destination); err != nil {
		return nil, err
	}
	if source.Mode&unix.S_IFMT != unix.S_IFDIR {
		return nil, errors.New("init work parent is not a directory")
	}
	if source.Dev != destination.Dev {
		return nil, errors.New("init work directory must share the project's filesystem; choose protected storage on that filesystem")
	}
	s := &initStage{target: filepath.Join(parentPath, filepath.Base(target))}
	s.path, err = os.MkdirTemp(workPath, ".surla-init-*")
	if err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}
	s.logs, err = os.MkdirTemp(workPath, ".surla-init-logs-*")
	if err != nil {
		return nil, errors.Join(fmt.Errorf("create log directory: %w", err), os.RemoveAll(s.path))
	}
	return s, nil
}

// publish moves the completed tree without replacing any existing target.
// Rename is the final fallible step: an error leaves the target unchanged.
func (s *initStage) publish() error {
	if err := os.Chmod(s.path, 0o755); err != nil {
		return fmt.Errorf("set project permissions: %w", err)
	}
	if err := unix.RenamexNp(s.path, s.target, unix.RENAME_EXCL); err != nil {
		return fmt.Errorf("move project into %s: %w", s.target, err)
	}
	return nil
}
