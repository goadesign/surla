// This file runs git for the working copy: the commands that read and write
// it, the private snapshot Check builds from, and the commit Record makes.
// Every command on the copy goes through WorkingCopy.git, which names the
// administrative folder the project's repository keeps for the copy.
package testwriter

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// capBuffer keeps the first limit bytes written to it and counts the rest,
// never failing a write, so a command's output is drained to the end.
type capBuffer struct {
	// buf holds the kept bytes.
	buf bytes.Buffer
	// limit is the most bytes buf keeps.
	limit int
	// dropped counts the bytes written past limit.
	dropped int
}

const (
	// maxGitStdout bounds the standard output git may print for one command;
	// every command here prints at most a few paths or object IDs.
	maxGitStdout = 1 << 20
	// maxGitStderr bounds the standard error kept for an error message.
	maxGitStderr = 4 << 10
)

// gitFlags turn off what git would otherwise run on the repository's or the
// working copy's say: hooks (a repository's core.hooksPath may name a folder
// of the working copy, which its author writes) and the file system monitor.
var gitFlags = []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

// gitRedirects are environment variables that would make git act on another
// repository, work tree, index or object store than the one named on its
// command line. gitEnv drops them; the user's identity, signing and
// configuration-file settings stay.
var gitRedirects = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
	"GIT_REPLACE_REF_BASE", "GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
	"GIT_PREFIX", "GIT_SHALLOW_FILE", "GIT_QUARANTINE_PATH", "GIT_TERMINAL_PROMPT",
}

// Write keeps what fits in the limit and counts the rest.
func (c *capBuffer) Write(p []byte) (int, error) {
	keep := min(len(p), max(c.limit-c.buf.Len(), 0))
	c.buf.Write(p[:keep])
	c.dropped += len(p) - keep
	return len(p), nil
}

// git runs one git command from dir with gitFlags and gitEnv plus env, with no
// standard input, and returns its standard output without the final newline.
// A failed command's error wraps its *exec.ExitError and quotes the start of
// its standard error.
func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	full := append(append([]string{"-C", dir}, gitFlags...), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(gitEnv(), env...)
	stdout := &capBuffer{limit: maxGitStdout}
	stderr := &capBuffer{limit: maxGitStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.buf.String()))
	}
	if stdout.dropped > 0 {
		return "", fmt.Errorf("git %s printed more than %d bytes", strings.Join(args, " "), maxGitStdout)
	}
	return strings.TrimSuffix(stdout.buf.String(), "\n"), nil
}

// stageCopy records the working copy's files, as `git add --all` would commit
// them on top of its HEAD, in the private index file index, and writes those
// files under dst, which must not exist. The copy's own index and files are
// only read.
func stageCopy(ctx context.Context, wc WorkingCopy, index, dst string) error {
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := wc.git(ctx, wc.dir, env, "read-tree", "HEAD"); err != nil {
		return fmt.Errorf("snapshot the working copy: %w", err)
	}
	if _, err := wc.git(ctx, wc.dir, env, "add", "--all"); err != nil {
		return fmt.Errorf("snapshot the working copy: %w", err)
	}
	prefix := "--prefix=" + dst + string(filepath.Separator)
	if _, err := wc.git(ctx, wc.dir, env, "checkout-index", "--all", prefix); err != nil {
		return fmt.Errorf("write the working copy's snapshot: %w", err)
	}
	return nil
}

// copyBase returns the commit a copy's changes are measured from: the merge
// base of Branch and the project's HEAD. The tests branch is read through the
// administrative folder the project's repository keeps for the copy and the
// HEAD through the project folder, never through the copy's own .git. Before
// the user merges the tests branch it is the commit the branch started from;
// after the merge it is the branch's latest commit, which the project
// contains. When the two share no history, for example after the project's
// history was rewritten from its first commit, it fails with
// ErrNoCommonBase.
func copyBase(ctx context.Context, wc WorkingCopy) (string, error) {
	head, err := git(ctx, filepath.Dir(wc.dir), nil, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("read the project's commit: %w", err)
	}
	base, err := wc.git(ctx, wc.dir, nil, "merge-base", "refs/heads/"+Branch, head)
	if exitCode(err) == 1 {
		return "", fmt.Errorf("%w: %s and the project's commit %s", ErrNoCommonBase, Branch, head)
	}
	if err != nil {
		return "", fmt.Errorf("find the working copy's base: %w", err)
	}
	return base, nil
}

// changedSince returns the paths, relative to the copy's root and in git's
// order, that the snapshot recorded in the private index file index adds,
// changes or deletes compared with the commit base.
func changedSince(ctx context.Context, wc WorkingCopy, index, base string) ([]string, error) {
	tree, err := wc.git(ctx, wc.dir, []string{"GIT_INDEX_FILE=" + index}, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("record the working copy's files: %w", err)
	}
	out, err := wc.git(ctx, wc.dir, nil, "diff-tree", "-r", "-z", "--name-only", "--no-renames", base, tree)
	if err != nil {
		return nil, fmt.Errorf("compare the working copy with its base: %w", err)
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// writeTree records every file under src, the snapshot stageCopy wrote and
// the check then changed and compiled, in the private index, and writes that
// index as a tree into the working copy's repository. It returns the tree's
// ID. Ignore rules do not apply here: a file the author's ignore file names,
// such as the regenerated gen/ folder, was compiled, so it is recorded too.
// (Files those rules kept out of the copy's snapshot never reached src.)
func writeTree(ctx context.Context, wc WorkingCopy, index, src string) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err := wc.git(ctx, src, env, "add", "--all", "--force"); err != nil {
		return "", fmt.Errorf("record the checked files: %w", err)
	}
	tree, err := wc.git(ctx, src, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("record the checked files: %w", err)
	}
	return tree, nil
}

// commitTree makes tree the content of the working copy's branch. When the
// branch's commit already holds tree it returns that commit. Otherwise it
// commits tree with message on top of the branch with the user's own git
// identity, points the copy's index at the commit when the copy has the
// branch checked out, so the copy's status shows only edits made after the
// check, and then moves the branch, failing if it moved meanwhile.
func commitTree(ctx context.Context, wc WorkingCopy, tree, message string) (string, error) {
	ref := "refs/heads/" + Branch
	head, err := wc.git(ctx, wc.dir, nil, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	headTree, err := wc.git(ctx, wc.dir, nil, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return "", err
	}
	if headTree == tree {
		return head, nil
	}
	commit, err := wc.git(ctx, wc.dir, nil, "commit-tree", tree, "-p", head, "-m", message)
	if err != nil {
		return "", fmt.Errorf("commit the accepted tests: %w", err)
	}
	checkedOut, err := wc.git(ctx, wc.dir, nil, "symbolic-ref", "--quiet", "HEAD")
	if err != nil && exitCode(err) != 1 {
		return "", err
	}
	if checkedOut == ref {
		if _, err := wc.git(ctx, wc.dir, nil, "read-tree", commit); err != nil {
			return "", fmt.Errorf("update the working copy's index: %w", err)
		}
	}
	if _, err := wc.git(ctx, wc.dir, nil, "update-ref", ref, commit, head); err != nil {
		return "", fmt.Errorf("move %s to the accepted tests: %w", Branch, err)
	}
	return commit, nil
}

// gitEnv is the server's environment without gitRedirects, with terminal
// prompts off.
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(gitRedirects, key) {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0")
}

// git runs one git command on the working copy's repository, with worktree
// (the copy, or a snapshot of it) as the work tree: --git-dir names the
// administrative folder found from the project's side, so git never reads the
// copy's own .git, whose configuration the copy's author could write.
func (w WorkingCopy) git(ctx context.Context, worktree string, env []string, args ...string) (string, error) {
	repo := []string{"--git-dir=" + w.gitDir, "--work-tree=" + worktree}
	return git(ctx, worktree, env, append(repo, args...)...)
}
