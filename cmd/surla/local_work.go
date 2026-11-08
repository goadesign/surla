// This file chooses operator-owned temporary storage for local CLI work.
// Builders may write the startup workspace and target directories, so compiler
// work, unpublished starters and command logs must be stored elsewhere.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/goadesign/surla/internal/abspath"
)

// userConfigDir obtains the platform configuration parent. Command tests
// replace it with test-owned storage rather than writing into a user's home.
var userConfigDir = os.UserConfigDir

// localEvalWorkDir chooses a protected parent for a command's evaluator.
// roots are existing modules the command reads; startup is protected as well.
func localEvalWorkDir(roots ...string) (string, error) {
	startup, err := abspath.Eval(".")
	if err != nil {
		return "", err
	}
	protected, err := physicalLocalRoots(append(roots, startup))
	if err != nil {
		return "", err
	}
	root, err := localOperatorRoot(startup, protected)
	if err != nil {
		return "", err
	}
	return createProtectedDir(filepath.Join(root, "designeval-work"), protected)
}

// localInitWorkDir chooses protected staging on the destination filesystem.
// targetParent may be anywhere; the CLI does not impose MCP's child-name rule.
func localInitWorkDir(targetParent string) (string, error) {
	startup, err := abspath.Eval(".")
	if err != nil {
		return "", err
	}
	protected, err := physicalLocalRoots([]string{targetParent, startup})
	if err != nil {
		return "", err
	}
	parent := protected[0]
	var failures []error
	root, err := localOperatorRoot(startup, protected)
	if err == nil {
		work, err := createProtectedDir(filepath.Join(root, "init-stage"), protected)
		if err == nil {
			err = requireSameDevice(parent, work)
		}
		if err == nil {
			return work, nil
		}
		failures = append(failures, err)
	} else {
		failures = append(failures, err)
	}
	// A different destination mount needs protected storage beside its parent,
	// not a copy into another staging tree inside the writable workspace.
	fallback := filepath.Join(filepath.Dir(parent), fmt.Sprintf(".surla-operator-%d", os.Getuid()), "init-stage")
	work, err := createProtectedDir(fallback, protected)
	if err == nil {
		err = requireSameDevice(parent, work)
	}
	if err == nil {
		return work, nil
	}
	return "", fmt.Errorf("cannot find protected init storage on the destination filesystem: %w", errors.Join(append(failures, err)...))
}

// localOperatorRoot chooses the first operator directory outside every grant.
func localOperatorRoot(startup string, protected []string) (string, error) {
	var candidates []string
	if config, err := userConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(config, "surla", "operator"))
	}
	candidates = append(candidates,
		filepath.Join(os.TempDir(), fmt.Sprintf("surla-operator-%d", os.Getuid())),
		filepath.Join(filepath.Dir(startup), fmt.Sprintf(".surla-operator-%d", os.Getuid())),
	)
	var failures []error
	for _, candidate := range candidates {
		dir, err := createProtectedDir(candidate, protected)
		if err == nil {
			return dir, nil
		}
		failures = append(failures, err)
	}
	return "", fmt.Errorf("cannot find operator storage outside the workspace: %w", errors.Join(failures...))
}

// physicalLocalRoots resolves existing roots before any temporary allocation.
func physicalLocalRoots(roots []string) ([]string, error) {
	resolved := make([]string, len(roots))
	for i, root := range roots {
		physical, err := abspath.Eval(root)
		if err != nil {
			return nil, err
		}
		resolved[i] = physical
	}
	return resolved, nil
}

// createProtectedDir checks existing ancestors before creating storage, so a
// symlinked parent cannot place new compiler files inside a builder's grant.
func createProtectedDir(candidate string, protected []string) (string, error) {
	physical, err := resolveMissingPath(candidate)
	if err != nil {
		return "", err
	}
	for _, root := range protected {
		rel, err := filepath.Rel(root, physical)
		if err != nil {
			return "", err
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("operator directory %s must be outside workspace %s", physical, root)
		}
	}
	if err := os.MkdirAll(physical, 0o700); err != nil {
		return "", err
	}
	return abspath.Eval(physical)
}

// resolveMissingPath follows existing ancestors and appends only absent names.
func resolveMissingPath(candidate string) (string, error) {
	var missing []string
	for {
		physical, err := abspath.Eval(candidate)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				physical = filepath.Join(physical, missing[i])
			}
			return physical, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		missing = append(missing, filepath.Base(candidate))
		candidate = filepath.Dir(candidate)
	}
}

// requireSameDevice rejects staging that a single directory rename cannot use.
func requireSameDevice(destination, work string) error {
	var a, b syscall.Stat_t
	if err := syscall.Stat(destination, &a); err != nil {
		return err
	}
	if err := syscall.Stat(work, &b); err != nil {
		return err
	}
	if a.Dev != b.Dev {
		return fmt.Errorf("init storage %s is on a different filesystem from %s", work, destination)
	}
	return nil
}
