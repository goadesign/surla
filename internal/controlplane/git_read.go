// This file reads committed files from the project's git repository without
// running anything the repository could configure: the read-only queries the
// verification plan uses to learn a commit's test layout.
package controlplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
	"time"

	"github.com/goadesign/surla/internal/abspath"
)

type (
	// cappedBuffer collects one git query's output stream, failing the query
	// once the stream exceeds limit bytes.
	cappedBuffer struct {
		// buf holds the bytes written so far, never more than limit.
		buf bytes.Buffer
		// limit is the most bytes the stream may carry.
		limit int
		// overflow reports that the stream exceeded limit.
		overflow bool
	}

	// changedGitFile is a committed path under the test module that a
	// candidate commit added, changed or deleted since its base commit.
	changedGitFile struct {
		// path is the slash-separated path from the repository root.
		path string
		// added reports that the base commit does not have path.
		added bool
	}

	// gitEntryKind is what a committed path is.
	gitEntryKind int
)

const (
	// layoutReadTimeout bounds all the git queries one verification plan or
	// live start makes together, so an unresponsive repository is refused
	// with an error instead of holding the call.
	layoutReadTimeout = 30 * time.Second
	// gitReadMaxOutput bounds the stdout of each git query. The largest is a
	// committed go.mod file; a larger one fails the query.
	gitReadMaxOutput = 1 << 20
	// gitReadMaxStderr bounds the stderr of each git query, which a failed
	// query's error message quotes.
	gitReadMaxStderr = 4 << 10

	// gitMissing is a path the commit does not have.
	gitMissing gitEntryKind = iota
	// gitRegularFile is a committed regular file (mode 100644 or 100755).
	gitRegularFile
	// gitTree is a committed directory.
	gitTree
	// gitOther is anything else at the path: a link, a submodule.
	gitOther
)

// errNoRepository is resolveStarterRepo's error for a folder outside any git
// repository.
var errNoRepository = errors.New("not inside a git repository")

// Write appends p, or records the overflow and fails when p would take the
// stream past limit, so the git command stops instead of producing output
// nobody will read.
func (c *cappedBuffer) Write(p []byte) (int, error) {
	if c.buf.Len()+len(p) > c.limit {
		c.overflow = true
		return 0, errors.New("output limit exceeded")
	}
	return c.buf.Write(p)
}

// resolveStarterRepo resolves dir to its physical path and asks git for its
// repository root and its slash-separated prefix within that repository, such
// as "services/shop/" or "" at the root. A dir outside any git repository is
// errNoRepository.
func resolveStarterRepo(ctx context.Context, dir string) (repoRoot, prefix string, err error) {
	info, err := os.Stat(dir)
	if err != nil {
		return "", "", fmt.Errorf("read project %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", "", fmt.Errorf("project %s is not a directory", dir)
	}
	// Resolve from the working directory without cleaning parent components
	// before following symlinks: alias/.. must name the linked directory's parent.
	projectRoot, err := abspath.Eval(dir)
	if err != nil {
		return "", "", fmt.Errorf("resolve project %s: %w", dir, err)
	}
	topOut, err := runGitRead(ctx, projectRoot, "rev-parse", "--show-toplevel", "--show-prefix")
	if err != nil {
		if strings.Contains(err.Error(), "not a git repository") {
			return "", "", fmt.Errorf("project %s: %w", dir, errNoRepository)
		}
		return "", "", fmt.Errorf("project %s: find its git repository: %w", dir, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(topOut), "\n"), "\n")
	if len(lines) != 2 {
		return "", "", fmt.Errorf("project %s: git rev-parse printed %q, want the repository root and prefix", dir, topOut)
	}
	return lines[0], lines[1], nil
}

// commitExists reports whether repoRoot holds commit. git rev-parse --verify
// --quiet exits 1, printing nothing, only when the commit is missing; any
// other failure is an error.
func commitExists(ctx context.Context, repoRoot, commit string) (bool, error) {
	_, err := runGitRead(ctx, repoRoot, "rev-parse", "--verify", "--quiet", commit+"^{commit}")
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

// mergeBaseCommit returns a best common ancestor of a and b in repoRoot.
// git merge-base exits 1, printing nothing, only when the commits share no
// common ancestor; any other failure is an error.
func mergeBaseCommit(ctx context.Context, repoRoot, a, b string) (base string, found bool, err error) {
	out, err := runGitRead(ctx, repoRoot, "merge-base", a, b)
	if err == nil {
		base = strings.TrimSpace(string(out))
		return base, base != "", nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return "", false, nil
	}
	return "", false, err
}

// changedTestModuleFiles returns the hand-written paths under testModule
// (such as "eval" or "services/shop/eval") that differ between baseCommit and
// candidateCommit in repoRoot, in git diff-tree's lexical order. Generated
// files under <testModule>/gen/ and <testModule>/AGENTS_QUICKSTART.md are
// excluded because generate_code(target: "eval") rewrites them from the design.
func changedTestModuleFiles(
	ctx context.Context, repoRoot, baseCommit, candidateCommit, testModule string,
) ([]changedGitFile, error) {
	out, err := runGitRead(
		ctx, repoRoot,
		"diff-tree", "-r", "-z", "--name-status", "--no-renames",
		baseCommit, candidateCommit, "--", testModule,
	)
	if err != nil {
		return nil, fmt.Errorf("compare %s between %s and %s: %w", testModule, baseCommit, candidateCommit, err)
	}
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return nil, nil
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("git diff-tree printed an odd number of fields (%d)", len(fields))
	}
	var changed []changedGitFile
	for i := 0; i < len(fields); i += 2 {
		status, p := fields[i], fields[i+1]
		if isGeneratedTestModuleFile(testModule, p) {
			continue
		}
		changed = append(changed, changedGitFile{path: p, added: status == "A"})
	}
	return changed, nil
}

// isGeneratedTestModuleFile reports whether p is a file under testModule that
// generate_code(target: "eval") owns (<testModule>/AGENTS_QUICKSTART.md or
// anything under <testModule>/gen/).
func isGeneratedTestModuleFile(testModule, p string) bool {
	return p == path.Join(testModule, "AGENTS_QUICKSTART.md") ||
		strings.HasPrefix(p, testModule+"/gen/")
}

// readGitBlob reads the committed regular file at <commit>:<treePath> in
// repoRoot. It returns found false when the commit has no regular file there
// (no such path, a link, a directory or a submodule).
func readGitBlob(ctx context.Context, repoRoot, commit, treePath string) (content []byte, found bool, err error) {
	kind, err := gitEntry(ctx, repoRoot, commit, treePath)
	if err != nil || kind != gitRegularFile {
		return nil, false, err
	}
	content, err = runGitRead(ctx, repoRoot, "cat-file", "blob", commit+":"+treePath)
	if err != nil {
		return nil, false, fmt.Errorf("read committed %s at %s: %w (Surla never downloads objects; if this is a partial clone,"+
			" check out that commit there first so git fetches them)", treePath, commit, err)
	}
	return content, true, nil
}

// gitEntry returns what <commit>:<treePath> is in repoRoot. An error means
// git itself failed.
func gitEntry(ctx context.Context, repoRoot, commit, treePath string) (gitEntryKind, error) {
	out, err := runGitRead(ctx, repoRoot, "ls-tree", commit, "--", treePath)
	if err != nil {
		return gitMissing, fmt.Errorf("list committed %s at %s: %w", treePath, commit, err)
	}
	fields := strings.Fields(string(out))
	switch {
	case len(fields) < 2:
		return gitMissing, nil
	case fields[1] == "tree":
		return gitTree, nil
	case fields[1] == "blob" && (fields[0] == "100644" || fields[0] == "100755"):
		return gitRegularFile, nil
	default:
		return gitOther, nil
	}
}

// runGitRead runs one read-only git query in dir and returns its stdout. It
// removes every inherited GIT_* variable, so dir alone selects the repository;
// ignores replace refs, so a commit ID names its real content; disables a
// configured file-system monitor and hooks, and never fetches objects missing
// from a partial clone, so no repository program or network access runs;
// takes no optional locks; and stops when ctx ends. Output past
// gitReadMaxOutput on stdout or gitReadMaxStderr on stderr fails the query.
func runGitRead(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmdArgs := append([]string{"-C", dir, "--no-optional-locks", "--no-replace-objects", "--no-lazy-fetch", "--literal-pathspecs",
		"-c", "core.fsmonitor=false", "-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Env = cleanGitEnv()
	cmd.WaitDelay = time.Second
	stdout := &cappedBuffer{limit: gitReadMaxOutput}
	stderr := &cappedBuffer{limit: gitReadMaxStderr}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if strings.Contains(stderr.buf.String(), "unknown option: --no-lazy-fetch") {
		return nil, errors.New("Surla needs Git 2.45 or newer (for --no-lazy-fetch, so its reads never download objects)")
	}
	if stdout.overflow || stderr.overflow {
		return nil, fmt.Errorf("git %s printed more output than a layout read allows", args[0])
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.buf.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w", msg, err)
		}
		return nil, err
	}
	return stdout.buf.Bytes(), nil
}

// cleanGitEnv returns os.Environ without any GIT_* variable, such as GIT_DIR,
// GIT_OBJECT_DIRECTORY or GIT_CONFIG_PARAMETERS, that could make git read a
// different repository, objects or configuration than the project
// directory's own, plus settings that disable terminal prompts and the
// system-wide configuration file and keep git's messages in English, which
// resolveStarterRepo matches.
func cleanGitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			env = append(env, kv)
		}
	}
	return append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "LC_ALL=C")
}
