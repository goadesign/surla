// This file resolves the enclosing Git working-tree root of a registered
// builder workspace directory so callers can keep private operator storage
// outside both the bound workspace and its owning Git working tree.
package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/goadesign/surla/internal/abspath"
)

type (
	// gitOutputBuffer bounds one read-only git query stream so runaway output
	// fails the query immediately.
	gitOutputBuffer struct {
		// buf holds the bytes written so far, up to limit.
		buf bytes.Buffer
		// limit is the maximum number of bytes buf may hold.
		limit int
		// overflow reports whether a write exceeded limit.
		overflow bool
	}
)

const (
	// gitQueryMaxStdout bounds stdout for a read-only git repository query.
	gitQueryMaxStdout = 1 << 20
	// gitQueryMaxStderr bounds stderr for a read-only git repository query.
	gitQueryMaxStderr = 4 << 10
)

// WorkspaceBoundaryRoots resolves workspaceDir to its physical path and returns
// the deduplicated slice of protected builder roots: the physical workspace
// directory itself and, when workspaceDir lies inside a Git working tree, the
// physical top-level root of that working tree. A non-Git or bare-repository
// workspace returns only the physical workspace directory; malformed Git
// metadata, unsupported Git versions, timeouts, and filesystem permission
// failures return an error.
func WorkspaceBoundaryRoots(ctx context.Context, workspaceDir string) ([]string, error) {
	if workspaceDir == "" {
		return nil, nil
	}
	physWS, err := abspath.EvalMissing(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace %s: %w", workspaceDir, err)
	}
	physWS = filepath.Clean(physWS)
	roots := []string{physWS}
	top, err := EnclosingWorktreeRoot(ctx, physWS)
	if err != nil {
		return nil, err
	}
	if top != "" && top != physWS {
		roots = append(roots, top)
	}
	return roots, nil
}

// EnclosingWorktreeRoot returns the physical top-level working-tree directory
// containing dir when dir (or its deepest existing ancestor) lies inside a Git
// working tree. When dir is not inside a Git repository or lies inside a bare
// repository or `.git` metadata directory with no working tree, it returns
// ("", nil). Any other Git or filesystem error is returned to the caller.
func EnclosingWorktreeRoot(ctx context.Context, dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	physDir, err := abspath.EvalMissing(dir)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", dir, err)
	}
	existingDir, err := deepestExistingDir(filepath.Clean(physDir))
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(
		ctx,
		"git",
		"-C",
		existingDir,
		"--no-optional-locks",
		"--no-replace-objects",
		"--no-lazy-fetch",
		"--literal-pathspecs",
		"-c",
		"core.fsmonitor=false",
		"-c",
		"core.hooksPath=/dev/null",
		"-c",
		"core.quotePath=false",
		"rev-parse",
		"--show-toplevel",
	)
	cmd.Env = cleanVerifyGitEnv()
	cmd.WaitDelay = time.Second
	stdout := &gitOutputBuffer{limit: gitQueryMaxStdout}
	stderr := &gitOutputBuffer{limit: gitQueryMaxStderr}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	stderrStr := stderr.buf.String()
	if strings.Contains(stderrStr, "unknown option: --no-lazy-fetch") {
		return "", errors.New(
			"surla needs Git 2.45 or newer (for --no-lazy-fetch, so startup never downloads objects); upgrade Git",
		)
	}
	if stdout.overflow || stderr.overflow {
		return "", fmt.Errorf("git rev-parse in %s printed more output than allowed", existingDir)
	}
	if runErr != nil {
		if strings.Contains(stderrStr, "not a git repository (or any ") ||
			strings.Contains(stderrStr, "this operation must be run in a work tree") {
			return "", nil
		}
		if msg := strings.TrimSpace(stderrStr); msg != "" {
			return "", fmt.Errorf("git rev-parse in %s: %s: %w", existingDir, msg, runErr)
		}
		return "", fmt.Errorf("git rev-parse in %s: %w", existingDir, runErr)
	}
	top := strings.TrimSuffix(stdout.buf.String(), "\n")
	if top == "" {
		return "", nil
	}
	physTop, err := abspath.EvalMissing(top)
	if err != nil {
		return "", fmt.Errorf("resolve git working tree root %s: %w", top, err)
	}
	return filepath.Clean(physTop), nil
}

// Write appends p to g.buf or fails once g.limit would be exceeded.
func (g *gitOutputBuffer) Write(p []byte) (int, error) {
	if g.buf.Len()+len(p) > g.limit {
		g.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	return g.buf.Write(p)
}

// deepestExistingDir walks dir upward until it finds an existing directory,
// returning non-ErrNotExist stat errors or an error if dir itself is a regular
// file.
func deepestExistingDir(dir string) (string, error) {
	cur := dir
	for {
		info, err := os.Stat(cur)
		if err == nil {
			if !info.IsDir() {
				return "", fmt.Errorf("%s is not a directory", cur)
			}
			return cur, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("stat %s: %w", cur, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("stat %s: %w", cur, err)
		}
		cur = parent
	}
}

// cleanVerifyGitEnv strips inherited GIT_* variables so the target directory
// alone selects the repository, disables terminal prompts and system config,
// restricts protocols to file, and forces English diagnostic messages.
func cleanVerifyGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(
		env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ALLOW_PROTOCOL=file",
		"LC_ALL=C",
	)
}
