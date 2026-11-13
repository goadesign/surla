// Package testwriter prepares, checks and records the work of the test
// writer: the coding agent that writes one test per finished Story flow in its
// own linked git working copy of the project. Surla, not the agent, decides
// whether that work counts. OpenWorkingCopy creates the copy, Checker.Prepare
// writes the required tests into it from a Story, Checker.Check judges it, and
// Record commits an accepted result on the copy's branch, which the user
// merges.
//
// Nothing here trusts the copy's author. Its files are compiled and run only
// in the verify/candidate sandbox, which exists only on macOS: elsewhere
// Prepare and Check fail with candidate.ErrIsolationUnavailable before any of
// the author's code runs. Git runs on the copy with hooks and the file system
// monitor disabled, so no script the author can write runs either.
package testwriter

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/verify"
)

// WorkingCopy is the test writer's linked git working copy of one project.
// Only OpenWorkingCopy creates one, so its folders are always a working copy
// Surla created or verified.
type WorkingCopy struct {
	// dir is the absolute, symlink-free root of the linked working copy: a
	// checkout of the whole repository at <project>/.surla-tests.
	dir string
	// project is the project's folder inside dir: dir itself for a project at
	// the repository's top level, dir/services/shop for one at services/shop.
	project string
}

const (
	// Folder is the working copy's folder name inside the project. Its
	// ".surla" prefix makes the project's own checks (check_project) skip it,
	// and the local exclude entry keeps it out of the project's git status.
	Folder = ".surla-tests"
	// Branch is the branch checked out in the working copy. Record commits
	// accepted tests on it; the user merges it.
	Branch = "surla/tests"
	// excludePattern is the line OpenWorkingCopy adds to the repository's own
	// info/exclude file. It is not anchored, so it also covers a project in a
	// subfolder of its repository.
	excludePattern = Folder + "/"
)

var (
	// ErrNotRepository refuses a project folder that is in no git working
	// tree. Its message is the step to take.
	ErrNotRepository = errors.New("the project is not in a git repository: run git init in the project folder and commit its files, then try again")
	// ErrNoCommit refuses a project whose repository has no commit to start
	// the working copy's branch from. Its message is the step to take.
	ErrNoCommit = errors.New("the project's git repository has no commit yet: commit the project's files, then try again")
	// ErrFolderInUse refuses a project whose .surla-tests entry is not the
	// working copy Surla created. Its message is the step to take.
	ErrFolderInUse = errors.New("the project's " + Folder + " entry is not Surla's working copy: move it out of the project, then try again")
)

// OpenWorkingCopy returns the test writer's working copy of the project in
// projectDir, creating it when missing: a linked git working copy (git
// worktree) at <project>/.surla-tests on branch surla/tests. The branch starts
// at the project's current commit and is kept when it already exists, so
// commits Record made survive a deleted copy. Before creating anything it
// adds ".surla-tests/" to the repository's own info/exclude file, never a
// tracked file, so the project's git status stays clean; no tracked file of
// the project is written.
//
// It is safe to repeat: an existing copy is verified and returned as it is.
// Callers serialize calls for one project. A project in no git working tree
// is refused with ErrNotRepository, one whose repository has no commit with
// ErrNoCommit, and one whose .surla-tests entry is something else with
// ErrFolderInUse; nothing is created in these cases. Other errors are git or
// file system failures.
func OpenWorkingCopy(ctx context.Context, projectDir string) (WorkingCopy, error) {
	project, err := abspath.Eval(projectDir)
	if err != nil {
		return WorkingCopy{}, fmt.Errorf("resolve the project folder: %w", err)
	}
	top, err := verify.EnclosingWorktreeRoot(ctx, project)
	if err != nil {
		return WorkingCopy{}, fmt.Errorf("find the project's git repository: %w", err)
	}
	if top == "" {
		return WorkingCopy{}, ErrNotRepository
	}
	if _, err := git(ctx, project, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		if exitCode(err) == 1 {
			return WorkingCopy{}, ErrNoCommit
		}
		return WorkingCopy{}, err
	}
	rel, err := filepath.Rel(top, project)
	if err != nil || !filepath.IsLocal(rel) {
		return WorkingCopy{}, fmt.Errorf("project %s is not inside its git working tree %s", project, top)
	}
	dir := filepath.Join(project, Folder)
	wc := WorkingCopy{dir: dir, project: filepath.Join(dir, rel)}
	info, err := os.Lstat(dir)
	switch {
	case err == nil && !info.IsDir():
		return WorkingCopy{}, fmt.Errorf("%w (%s is not a folder)", ErrFolderInUse, dir)
	case err == nil:
		if err := verifyCopy(ctx, project, dir); err != nil {
			return WorkingCopy{}, err
		}
		if err := excludeFolder(ctx, project); err != nil {
			return WorkingCopy{}, err
		}
		return wc, nil
	case !errors.Is(err, fs.ErrNotExist):
		return WorkingCopy{}, fmt.Errorf("inspect %s: %w", dir, err)
	}
	if err := excludeFolder(ctx, project); err != nil {
		return WorkingCopy{}, err
	}
	if err := addCopy(ctx, project, dir); err != nil {
		return WorkingCopy{}, err
	}
	return wc, nil
}

// Dir returns the absolute root of the working copy, a checkout of the whole
// repository. The test writer works here.
func (w WorkingCopy) Dir() string {
	return w.dir
}

// Project returns the project's folder inside the working copy, where the
// project's modules, such as its evaluator module, are found.
func (w WorkingCopy) Project() string {
	return w.project
}

// excludeFolder adds excludePattern to the repository's own info/exclude
// file unless a line already holds it.
func excludeFolder(ctx context.Context, project string) error {
	path, err := git(ctx, project, nil, "rev-parse", "--path-format=absolute", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read the repository's exclude file: %w", err)
	}
	if slices.Contains(strings.Split(string(raw), "\n"), excludePattern) {
		return nil
	}
	entry := "# The test writer's working copy, created by Surla.\n" + excludePattern + "\n"
	if len(raw) > 0 && !strings.HasSuffix(string(raw), "\n") {
		entry = "\n" + entry
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create the repository's info folder: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open the repository's exclude file: %w", err)
	}
	_, writeErr := f.WriteString(entry)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return fmt.Errorf("add %s to the repository's exclude file: %w", excludePattern, err)
	}
	return nil
}

// verifyCopy checks that the existing folder dir is the root of a linked
// working copy of the project's repository with Branch checked out, and
// returns ErrFolderInUse otherwise.
func verifyCopy(ctx context.Context, project, dir string) error {
	inUse := func(reason string) error {
		return fmt.Errorf("%w (%s %s)", ErrFolderInUse, dir, reason)
	}
	top, err := git(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		return inUse(fmt.Sprintf("is not a git working copy: %v", err))
	}
	if same, err := samePath(top, dir); err != nil || !same {
		return inUse("is not the root of a git working copy")
	}
	copyCommon, err := git(ctx, dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	projectCommon, err := git(ctx, project, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if same, err := samePath(copyCommon, projectCommon); err != nil || !same {
		return inUse("belongs to another git repository")
	}
	head, err := git(ctx, dir, nil, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || head != "refs/heads/"+Branch {
		return inUse("does not have branch " + Branch + " checked out")
	}
	return nil
}

// addCopy creates the linked working copy at dir, which does not exist. A
// registration git kept for dir after its folder was deleted is removed first;
// Branch is checked out when it exists and otherwise created at the project's
// current commit.
func addCopy(ctx context.Context, project, dir string) error {
	listed, err := git(ctx, project, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	for _, field := range strings.Split(listed, "\x00") {
		path, ok := strings.CutPrefix(field, "worktree ")
		if !ok {
			continue
		}
		if same, err := samePath(path, dir); err == nil && same {
			if _, err := git(ctx, project, nil, "worktree", "remove", dir); err != nil {
				return fmt.Errorf("remove the registration of the deleted working copy: %w", err)
			}
		}
	}
	_, err = git(ctx, project, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+Branch)
	switch {
	case err == nil:
		_, err = git(ctx, project, nil, "worktree", "add", "--quiet", dir, Branch)
	case exitCode(err) == 1:
		_, err = git(ctx, project, nil, "worktree", "add", "--quiet", "-b", Branch, dir, "HEAD")
	}
	if err != nil {
		return fmt.Errorf("create the test writer's working copy: %w", err)
	}
	return nil
}

// samePath reports whether a and b name the same location once symbolic
// links are resolved. Either may name a missing path.
func samePath(a, b string) (bool, error) {
	ra, err := abspath.EvalMissing(a)
	if err != nil {
		return false, err
	}
	rb, err := abspath.EvalMissing(b)
	if err != nil {
		return false, err
	}
	return filepath.Clean(ra) == filepath.Clean(rb), nil
}

// exitCode returns the exit status of a git command that ran and failed, or
// -1 for any other error.
func exitCode(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// opened returns an error unless OpenWorkingCopy made w: git run on a zero
// WorkingCopy's empty folder would act on the server's own folder instead.
func (w WorkingCopy) opened() error {
	if w.dir == "" {
		return errors.New("the working copy was not opened with OpenWorkingCopy")
	}
	return nil
}
