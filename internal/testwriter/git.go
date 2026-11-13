// This file runs git for the working copy: the commands that read and write
// it, the private snapshot Check builds from, and the commit Record makes.
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
	if _, err := git(ctx, wc.dir, env, "read-tree", "HEAD"); err != nil {
		return fmt.Errorf("snapshot the working copy: %w", err)
	}
	if _, err := git(ctx, wc.dir, env, "add", "--all"); err != nil {
		return fmt.Errorf("snapshot the working copy: %w", err)
	}
	if _, err := git(ctx, wc.dir, env, "checkout-index", "--all", "--prefix="+dst+string(filepath.Separator)); err != nil {
		return fmt.Errorf("write the working copy's snapshot: %w", err)
	}
	return nil
}

// writeTree records the files under src, a snapshot stageCopy wrote and the
// check then changed, in the private index the same way, and writes that index
// as a tree into the working copy's repository. It returns the tree's ID.
func writeTree(ctx context.Context, wc WorkingCopy, index, src string) (string, error) {
	gitDir, err := git(ctx, wc.dir, nil, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	env := []string{"GIT_INDEX_FILE=" + index}
	repo := []string{"--git-dir=" + gitDir, "--work-tree=" + src}
	if _, err := git(ctx, src, env, append(repo, "add", "--all")...); err != nil {
		return "", fmt.Errorf("record the checked files: %w", err)
	}
	tree, err := git(ctx, src, env, append(repo, "write-tree")...)
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
	head, err := git(ctx, wc.dir, nil, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	headTree, err := git(ctx, wc.dir, nil, "rev-parse", "--verify", head+"^{tree}")
	if err != nil {
		return "", err
	}
	if headTree == tree {
		return head, nil
	}
	commit, err := git(ctx, wc.dir, nil, "commit-tree", tree, "-p", head, "-m", message)
	if err != nil {
		return "", fmt.Errorf("commit the accepted tests: %w", err)
	}
	checkedOut, err := git(ctx, wc.dir, nil, "symbolic-ref", "--quiet", "HEAD")
	if err != nil && exitCode(err) != 1 {
		return "", err
	}
	if checkedOut == ref {
		if _, err := git(ctx, wc.dir, nil, "read-tree", commit); err != nil {
			return "", fmt.Errorf("update the working copy's index: %w", err)
		}
	}
	if _, err := git(ctx, wc.dir, nil, "update-ref", ref, commit, head); err != nil {
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
