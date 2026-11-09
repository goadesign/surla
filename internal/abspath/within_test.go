// These tests verify storage admission using filesystem identity, including
// names that resolve to the same directory without being symbolic links.
package abspath

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWithin checks existing and missing targets without changing path bytes
// or applying parent components before symbolic links have been followed.
func TestWithin(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "nested"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(base, "workspace "), 0o700))
	require.NoError(t, os.Symlink(filepath.Join(root, "nested"), filepath.Join(base, "alias")))
	require.NoError(t, os.Symlink(filepath.Join(root, "missing"), filepath.Join(base, "dangling")))
	require.NoError(t, os.WriteFile(filepath.Join(root, "file"), []byte("fixture"), 0o600))
	for _, tc := range []struct {
		// name identifies the filesystem relation under test.
		name string
		// path is the target supplied to the storage admission check.
		path string
		// inside states whether the target belongs to the workspace.
		inside bool
	}{
		{name: "equal", path: root, inside: true},
		{name: "existing", path: filepath.Join(root, "nested"), inside: true},
		{name: "file", path: filepath.Join(root, "file"), inside: true},
		{name: "missing_suffix", path: filepath.Join(root, "new", "cards"), inside: true},
		{name: "symlink_parent", path: base + "/alias/../new", inside: true},
		{name: "dangling_parent", path: base + "/dangling/../new", inside: true},
		{name: "outside", path: filepath.Join(base, "new", "cards")},
		{name: "ancestor", path: base},
		{name: "significant_space", path: filepath.Join(base, "workspace ", "cards")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inside, err := Within(root, tc.path)
			require.NoError(t, err)
			assert.Equal(t, tc.inside, inside)
		})
	}
	_, err := Within(filepath.Join(base, "missing-root"), root)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = Within(filepath.Join(root, "file"), root)
	require.ErrorContains(t, err, "not a directory")
	_, err = Within(root, filepath.Join(root, "file", "child"))
	require.Error(t, err, "non-directory ancestors must not become an outside verdict")
	require.NoError(t, os.Symlink("loop", filepath.Join(base, "loop")))
	_, err = Within(root, filepath.Join(base, "loop"))
	require.Error(t, err, "resolution failures must not become an outside verdict")
}

// TestWithinDirectoryCaseIdentity uses the filesystem's own answer about case
// aliases, preserving distinct directories on filesystems where case matters.
func TestWithinDirectoryCaseIdentity(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "workspace")
	alias := filepath.Join(base, "WORKSPACE")
	require.NoError(t, os.Mkdir(root, 0o700))
	a, err := os.Stat(root)
	require.NoError(t, err)
	b, err := os.Stat(alias)
	if os.IsNotExist(err) {
		require.NoError(t, os.Mkdir(alias, 0o700))
		inside, err := Within(root, filepath.Join(alias, "cards"))
		require.NoError(t, err)
		assert.False(t, inside, "distinct case-sensitive directories stay outside")
		return
	}
	require.NoError(t, err)
	require.True(t, os.SameFile(a, b), "fixture requires a real directory alias")
	for _, paths := range [][2]string{{root, alias}, {alias, root}} {
		for _, suffix := range []string{"", "existing", "missing/deep"} {
			if suffix == "existing" {
				require.NoError(t, os.MkdirAll(filepath.Join(root, suffix), 0o700))
			}
			inside, err := Within(paths[0], filepath.Join(paths[1], suffix))
			require.NoError(t, err)
			assert.True(t, inside, "root=%s target=%s", paths[0], paths[1])
		}
	}
}
