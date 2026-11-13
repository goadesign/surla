package testwriter

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// hookNames are the hooks the commands Surla runs could start: checkout,
// commit, reference updates and index writes.
var hookNames = []string{
	"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "post-checkout",
	"reference-transaction", "post-index-change", "post-rewrite",
}

// newProject creates a git repository in a new temporary folder with files
// committed once, the identity "Test User <test@example.com>", signing off
// and automatic maintenance off in its own configuration, and every hook in
// hookNames installed in a tracked .githooks folder that core.hooksPath
// names, as a repository using a hook manager does. A hook that runs writes
// its name into the returned markers folder. It returns the repository's
// resolved top-level folder and markers.
func newProject(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	markers := t.TempDir()
	all := map[string]string{}
	for name, content := range files {
		all[name] = content
	}
	for _, hook := range hookNames {
		all[".githooks/"+hook] = fmt.Sprintf("#!/bin/sh\necho ran > %q\n", filepath.Join(markers, hook))
	}
	writeFiles(t, repo, all)
	for _, hook := range hookNames {
		require.NoError(t, os.Chmod(filepath.Join(repo, ".githooks", hook), 0o755))
	}
	runGit(t, repo, "init", "--quiet")
	runGit(t, repo, "config", "user.name", "Test User")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "commit.gpgsign", "false")
	// Each commit would otherwise start git maintenance in the background,
	// which may repack the repository while a test compares its files.
	runGit(t, repo, "config", "maintenance.auto", "false")
	runGit(t, repo, "add", "--all")
	runGit(t, repo, "commit", "--quiet", "-m", "Start the project")
	runGit(t, repo, "config", "core.hooksPath", ".githooks")
	return repo, markers
}

// runGit runs git in dir for the test itself, with hooks off so only the code
// under test could start one, and returns its trimmed standard output.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	var stderr string
	if exitErr, ok := err.(*exec.ExitError); ok {
		stderr = string(exitErr.Stderr)
	}
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), stderr)
	return strings.TrimSpace(string(out))
}

// writeFiles writes files, keyed by slash path, under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
}

// ranHooks lists the hooks that wrote into markers.
func ranHooks(t *testing.T, markers string) []string {
	t.Helper()
	entries, err := os.ReadDir(markers)
	require.NoError(t, err)
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
