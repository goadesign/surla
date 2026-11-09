// This file chooses operator-owned temporary storage for local CLI work.
// Builders may write the startup workspace and target directories, so compiler
// work, unpublished starters and command logs must be stored elsewhere.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

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
	var failures []error
	for _, candidate := range localOperatorCandidates(startup) {
		work, err := createProtectedDir(filepath.Join(candidate, "designeval-work"), protected)
		if err == nil {
			return work, nil
		}
		failures = append(failures, err)
	}
	return "", fmt.Errorf("cannot find protected design evaluator storage outside the workspace: %w", errors.Join(failures...))
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
	candidates := localOperatorCandidates(startup)
	// A different destination mount may need protected storage beside its
	// parent, after every standard location has failed the complete check.
	candidates = append(candidates, filepath.Join(filepath.Dir(parent), fmt.Sprintf(".surla-operator-%d", os.Getuid())))
	var failures []error
	for _, candidate := range candidates {
		work, err := createProtectedDir(filepath.Join(candidate, "init-stage"), protected)
		if err == nil {
			err = requireSameDevice(parent, work)
		}
		if err == nil {
			return work, nil
		}
		failures = append(failures, err)
	}
	return "", fmt.Errorf("cannot find protected init storage on the destination filesystem: %w", errors.Join(failures...))
}

// localOperatorCandidates lists the existing operator locations in preference
// order. Each owner applies all its requirements before selecting a location.
func localOperatorCandidates(startup string) []string {
	var candidates []string
	if config, err := userConfigDir(); err == nil {
		candidates = append(candidates, filepath.Join(config, "surla", "operator"))
	}
	return append(candidates,
		filepath.Join(os.TempDir(), fmt.Sprintf("surla-operator-%d", os.Getuid())),
		filepath.Join(filepath.Dir(startup), fmt.Sprintf(".surla-operator-%d", os.Getuid())),
	)
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
// A returned directory permits the current effective user to read, write, and
// search its entries: Go removes populated temporary children through their
// parent, which requires read permission as well as allocation permission.
func createProtectedDir(candidate string, protected []string) (string, error) {
	physical, err := abspath.EvalMissing(candidate)
	if err != nil {
		return "", err
	}
	for _, root := range protected {
		inside, err := abspath.Within(root, physical)
		if err != nil {
			return "", err
		}
		if inside {
			return "", fmt.Errorf("operator directory %s must be outside workspace %s", physical, root)
		}
	}
	if err := os.MkdirAll(physical, 0o700); err != nil {
		return "", err
	}
	physical, err = abspath.Eval(physical)
	if err != nil {
		return "", err
	}
	if err := unix.Faccessat(unix.AT_FDCWD, physical, unix.R_OK|unix.W_OK|unix.X_OK, unix.AT_EACCESS); err != nil {
		return "", fmt.Errorf("operator directory %s must allow allocating and cleaning up temporary files: %w", physical, err)
	}
	return physical, nil
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
