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
// the author's code runs. No file inside the working copy can make git run a
// program: git is always pointed at the copy's repository through the folder
// the project's repository keeps for it, never through the copy's own .git,
// and it runs with hooks and the file system monitor disabled. A program the
// project's or the user's own git configuration names, such as a filter
// driver, still runs, as it would for the user. Files are read and written in
// the copy only through real folders, never through a symbolic link.
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
	"github.com/goadesign/surla/internal/shquote"
	"github.com/goadesign/surla/internal/verify"
)

// listedCopy is one entry of the project repository's list of working copies
// (git worktree list), the project's own folder included.
type listedCopy struct {
	// path is the working copy's folder as git recorded it.
	path string
	// branch is the full name of the branch checked out there, such as
	// refs/heads/main, or empty when none is.
	branch string
	// locked is set when the registration is locked (git worktree lock), so
	// git worktree remove refuses it until it is unlocked.
	locked bool
}

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
	// gitDir is the administrative folder the project's repository keeps for
	// this linked working copy (<common git folder>/worktrees/<name>), found
	// from the project's side. Every git command on the copy names it, so the
	// copy's own .git, which its author can rewrite, is never read.
	gitDir string
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
	// ErrBranchInUse refuses to create the working copy while another working
	// copy of the repository holds branch surla/tests: git checks a branch out
	// in one folder at a time. The error wrapping it names that folder and the
	// step to take, which touches only that registration: switch the folder
	// to another branch when it exists, such as the project folder itself;
	// otherwise remove git's registration of the folder that is gone with git
	// worktree remove, after git worktree unlock when it is locked.
	ErrBranchInUse = errors.New("branch " + Branch + ", which Surla reserves for the test writer's working copy, " +
		"is held by another working copy")
)

// OpenWorkingCopy returns the test writer's working copy of the project in
// projectDir, creating it when missing: a linked git working copy (git
// worktree) at <project>/.surla-tests on branch surla/tests. The branch name is
// reserved for Surla: the branch starts at the project's current commit, and
// an existing branch of that name that no folder has checked out is reused,
// so the commits Record made survive a deleted copy. Before creating anything it
// adds ".surla-tests/" to the repository's own info/exclude file, never a
// tracked file, so the project's git status stays clean; no tracked file of
// the project is written.
//
// It is safe to repeat: an existing copy is verified and returned as it is.
// Callers serialize calls for one project. A project in no git working tree
// is refused with ErrNotRepository, one whose repository has no commit with
// ErrNoCommit, one whose .surla-tests entry is something else with
// ErrFolderInUse, and one where another working copy holds branch
// surla/tests with ErrBranchInUse, whose message names the step: switch that
// folder to another branch, or, when its folder is gone, remove that one
// registration with git worktree remove (unlocking it first when it is
// locked); nothing is created in these cases. Other errors are git or file system failures.
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
		if wc.gitDir, err = registeredCopy(ctx, project, dir); err != nil {
			return WorkingCopy{}, err
		}
		if err := excludeFolder(ctx, project); err != nil {
			return WorkingCopy{}, err
		}
		return wc, nil
	case !errors.Is(err, fs.ErrNotExist):
		return WorkingCopy{}, fmt.Errorf("inspect %s: %w", dir, err)
	}
	listed, err := listCopies(ctx, project)
	if err != nil {
		return WorkingCopy{}, err
	}
	for _, c := range listed {
		if same, err := samePath(c.path, dir); c.branch != "refs/heads/"+Branch || err == nil && same {
			// Another branch, or Surla's own copy, whose stale registration
			// addCopy drops.
			continue
		}
		if _, err := os.Lstat(c.path); !errors.Is(err, fs.ErrNotExist) {
			return WorkingCopy{}, fmt.Errorf("%w: %s has it checked out; switch that folder to another branch, "+
				"then try again", ErrBranchInUse, c.path)
		}
		// The path is someone else's choice and the user will run these
		// commands in a shell, so it is written as one quoted shell word.
		word := shquote.Quote(c.path)
		if c.locked {
			return WorkingCopy{}, fmt.Errorf("%w: git still registers %s, a folder that no longer exists, as holding it, "+
				"and that registration is locked; unlock and remove it with git worktree unlock %s and then "+
				"git worktree remove %s in the project, then try again", ErrBranchInUse, c.path, word, word)
		}
		return WorkingCopy{}, fmt.Errorf("%w: git still registers %s, a folder that no longer exists, as holding it; "+
			"remove that registration with git worktree remove %s in the project, then try again",
			ErrBranchInUse, c.path, word)
	}
	if err := excludeFolder(ctx, project); err != nil {
		return WorkingCopy{}, err
	}
	if err := addCopy(ctx, project, dir, listed); err != nil {
		return WorkingCopy{}, err
	}
	if wc.gitDir, err = registeredCopy(ctx, project, dir); err != nil {
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

// registeredCopy returns the administrative folder the project's repository
// keeps for a linked working copy at dir with Branch checked out, and
// ErrFolderInUse when it keeps none. It reads only the project's repository:
// the folder under its common git folder's worktrees/ whose gitdir file names
// dir/.git, and that folder's HEAD. The copy's own .git is never read.
func registeredCopy(ctx context.Context, project, dir string) (string, error) {
	common, err := git(ctx, project, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	admins := filepath.Join(common, "worktrees")
	entries, err := os.ReadDir(admins)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("list the repository's linked working copies: %w", err)
	}
	var found []string
	for _, e := range entries {
		admin := filepath.Join(admins, e.Name())
		raw, err := os.ReadFile(filepath.Join(admin, "gitdir"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read the repository's linked working copies: %w", err)
		}
		// git records the copy's .git path, relative to the administrative
		// folder when worktree.useRelativePaths is set.
		target := strings.TrimSpace(string(raw))
		if !filepath.IsAbs(target) {
			target = filepath.Join(admin, target)
		}
		if same, err := samePath(filepath.Dir(target), dir); err == nil && same {
			found = append(found, admin)
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%w (%s is not a linked working copy of the project's repository)", ErrFolderInUse, dir)
	case 1:
	default:
		return "", fmt.Errorf("the project's repository registers %s as %d linked working copies: %s",
			dir, len(found), strings.Join(found, ", "))
	}
	head, err := git(ctx, project, nil, "--git-dir="+found[0], "symbolic-ref", "--quiet", "HEAD")
	if err != nil || head != "refs/heads/"+Branch {
		return "", fmt.Errorf("%w (%s does not have branch %s checked out)", ErrFolderInUse, dir, Branch)
	}
	return found[0], nil
}

// listCopies returns the project repository's list of working copies, read
// from the project's side.
func listCopies(ctx context.Context, project string) ([]listedCopy, error) {
	out, err := git(ctx, project, nil, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var listed []listedCopy
	for _, field := range strings.Split(out, "\x00") {
		if path, ok := strings.CutPrefix(field, "worktree "); ok {
			listed = append(listed, listedCopy{path: path})
		} else if branch, ok := strings.CutPrefix(field, "branch "); ok && len(listed) > 0 {
			listed[len(listed)-1].branch = branch
		} else if (field == "locked" || strings.HasPrefix(field, "locked ")) && len(listed) > 0 {
			listed[len(listed)-1].locked = true
		}
	}
	return listed, nil
}

// addCopy creates the linked working copy at dir, which does not exist, given
// the repository's list of working copies. A registration git kept for dir
// after its folder was deleted is removed first; Branch is checked out when
// it exists and otherwise created at the project's current commit.
func addCopy(ctx context.Context, project, dir string, listed []listedCopy) error {
	for _, c := range listed {
		if same, err := samePath(c.path, dir); err == nil && same {
			if _, err := git(ctx, project, nil, "worktree", "remove", dir); err != nil {
				return fmt.Errorf("remove the registration of the deleted working copy: %w", err)
			}
		}
	}
	_, err := git(ctx, project, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+Branch)
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
