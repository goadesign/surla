// This file proves how the project server opens the state folder its launcher names.
package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOpenServesOnlyTheNamedProjectState opens an initialized state folder exactly as
// Discover found it, and refuses every folder that is not that project's private state.
func TestOpenServesOnlyTheNamedProjectState(t *testing.T) {
	state := testState(t)
	root := filepath.Dir(state.Dir)
	record, err := os.ReadFile(filepath.Join(state.Dir, "project.json"))
	require.NoError(t, err)

	opened, err := Open(state.Dir)
	require.NoError(t, err)
	assert.Equal(t, state.Dir, opened.Dir)
	assert.Equal(t, state.Record, opened.Record)

	uninitialized, err := Discover(root, t.TempDir())
	require.NoError(t, err)
	lock, err := uninitialized.StartLock(context.Background())
	require.NoError(t, err)
	require.NoError(t, lock.Close())

	// A copied record in a folder named for another project folder belongs to no project.
	foreign := projectDirectory(root, t.TempDir())
	require.NoError(t, os.Mkdir(foreign, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(foreign, "project.json"), record, 0o600))

	shared := filepath.Join(root, "shared-state")
	require.NoError(t, os.Mkdir(shared, 0o700))
	require.NoError(t, os.Chmod(shared, 0o755))
	linked := filepath.Join(root, "linked-state")
	require.NoError(t, os.Symlink(state.Dir, linked))

	corrupt := projectDirectory(root, t.TempDir())
	require.NoError(t, os.Mkdir(corrupt, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(corrupt, "project.json"), []byte(`{"id":"x"}`), 0o600))

	relative, err := filepath.Rel(root, state.Dir)
	require.NoError(t, err)

	for _, tc := range []struct {
		name, dir, want string
	}{
		{name: "relative path", dir: relative, want: "is not an absolute path; start the project with surla"},
		{name: "missing folder", dir: filepath.Join(root, "absent"), want: "no such file or directory"},
		{name: "folder other accounts can read", dir: shared, want: "must be private and owned by this account"},
		{name: "symbolic link to the state", dir: linked, want: "must be private and owned by this account"},
		{name: "folder never initialized", dir: uninitialized.Dir, want: "holds no project; start the project with surla"},
		{name: "record of another project", dir: foreign, want: "does not belong to the project folder " + state.Record.Directory},
		{name: "invalid record", dir: corrupt, want: "invalid project record"},
		{name: "not the exact folder path", dir: state.Dir + string(filepath.Separator), want: "does not belong to the project folder"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(tc.dir)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}
