package verify

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/abspath"
)

// TestWorkspaceBoundaryRootsAndEnclosingWorktreeRoot verifies that
// WorkspaceBoundaryRoots and EnclosingWorktreeRoot return the enclosing Git
// working tree for a nested workspace directory, return only the physical
// workspace path for non-Git and bare repositories, and return an error when
// Git metadata is corrupt or Git fails.
func TestWorkspaceBoundaryRootsAndEnclosingWorktreeRoot(t *testing.T) {
	root, err := abspath.Eval(t.TempDir())
	require.NoError(t, err)

	// 1. Non-Git workspace directory returns only the physical workspace path.
	nonGitWS := filepath.Join(root, "plain-ws")
	require.NoError(t, os.MkdirAll(nonGitWS, 0o755))
	top, err := EnclosingWorktreeRoot(t.Context(), nonGitWS)
	require.NoError(t, err)
	assert.Empty(t, top)
	roots, err := WorkspaceBoundaryRoots(t.Context(), nonGitWS)
	require.NoError(t, err)
	assert.Equal(t, []string{nonGitWS}, roots)

	// 2. Nested workspace directory inside a monorepo returns both the workspace
	// directory and the monorepo working-tree root (including when the nested
	// child directory does not exist on disk yet).
	monorepo := filepath.Join(root, "monorepo")
	nestedWS := filepath.Join(monorepo, "services", "bakery")
	require.NoError(t, os.MkdirAll(nestedWS, 0o755))
	cmd := exec.CommandContext(t.Context(), "git", "-C", monorepo, "init")
	cmd.Env = cleanVerifyGitEnv()
	require.NoError(t, cmd.Run())

	top, err = EnclosingWorktreeRoot(t.Context(), nestedWS)
	require.NoError(t, err)
	assert.Equal(t, monorepo, top)
	roots, err = WorkspaceBoundaryRoots(t.Context(), nestedWS)
	require.NoError(t, err)
	assert.Equal(t, []string{nestedWS, monorepo}, roots)

	missingChild := filepath.Join(nestedWS, "future-child")
	roots, err = WorkspaceBoundaryRoots(t.Context(), missingChild)
	require.NoError(t, err)
	assert.Equal(t, []string{missingChild, monorepo}, roots)

	// Trailing-space enclosing root and nested workspace preserve exact path bytes.
	spaceMonorepo := filepath.Join(root, "monorepo ")
	spaceNestedWS := filepath.Join(spaceMonorepo, "services", "api")
	require.NoError(t, os.MkdirAll(spaceNestedWS, 0o755))
	spaceInitCmd := exec.CommandContext(t.Context(), "git", "-C", spaceMonorepo, "init")
	spaceInitCmd.Env = cleanVerifyGitEnv()
	require.NoError(t, spaceInitCmd.Run())

	top, err = EnclosingWorktreeRoot(t.Context(), spaceNestedWS)
	require.NoError(t, err)
	assert.Equal(t, spaceMonorepo, top)
	roots, err = WorkspaceBoundaryRoots(t.Context(), spaceNestedWS)
	require.NoError(t, err)
	assert.Equal(t, []string{spaceNestedWS, spaceMonorepo}, roots)

	// 3. Bare repository has no working tree; WorkspaceBoundaryRoots keeps the
	// bare directory itself protected without failing.
	bareRepo := filepath.Join(root, "mirror.git")
	bareCmd := exec.CommandContext(t.Context(), "git", "init", "--bare", "--template=", bareRepo)
	bareCmd.Env = cleanVerifyGitEnv()
	require.NoError(t, bareCmd.Run())

	top, err = EnclosingWorktreeRoot(t.Context(), bareRepo)
	require.NoError(t, err)
	assert.Empty(t, top)
	roots, err = WorkspaceBoundaryRoots(t.Context(), bareRepo)
	require.NoError(t, err)
	assert.Equal(t, []string{bareRepo}, roots)

	// 4. Corrupt .git file (pointing to a missing gitdir) fails instead of
	// falling back to a guessed ancestor directory.
	corruptRepo := filepath.Join(root, "corrupt-repo")
	corruptSub := filepath.Join(corruptRepo, "sub")
	require.NoError(t, os.MkdirAll(corruptSub, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(corruptRepo, ".git"), []byte("gitdir: /nonexistent-gitdir\n"), 0o600))
	_, err = WorkspaceBoundaryRoots(t.Context(), corruptSub)
	require.Error(t, err)

	// 5. Git <= 2.44 (rejecting --no-lazy-fetch) still succeeds for metadata-only
	// working-tree root discovery.
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	fakeBin := filepath.Join(root, "fake-git-244")
	require.NoError(t, os.MkdirAll(fakeBin, 0o700))
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = \"--no-lazy-fetch\" ]; then\n    printf 'unknown option: --no-lazy-fetch\\n' >&2\n    exit 129\n  fi\ndone\nexec \"" + realGit + "\" \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(fakeBin, "git"), []byte(script), 0o700))
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	roots, err = WorkspaceBoundaryRoots(t.Context(), nestedWS)
	require.NoError(t, err)
	assert.Equal(t, []string{nestedWS, monorepo}, roots)
}
