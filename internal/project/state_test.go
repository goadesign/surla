// This file proves how the project server opens the state folder its launcher names.
package project

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/abspath"
)

// TestOpenServesOnlyTheNamedProjectState opens an initialized state folder exactly as
// Discover found it under a physical root, as the `surla` command's operator root is,
// and refuses every folder that is not that project's private state.
func TestOpenServesOnlyTheNamedProjectState(t *testing.T) {
	root := physicalTempDir(t)
	state := initializedState(t, root, t.TempDir())
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
	writeStateCopy(t, foreign, record)

	// A correctly named copy under a root inside the project folder is the project's own file tree.
	insideRoot := filepath.Join(state.Record.Directory, "operator")
	require.NoError(t, os.Mkdir(insideRoot, 0o700))
	inside := projectDirectory(insideRoot, state.Record.Directory)
	writeStateCopy(t, inside, record)

	// The same folder reached through a linked root is not the physical path surla passes.
	alias := filepath.Join(physicalTempDir(t), "alias")
	require.NoError(t, os.Symlink(root, alias))

	shared := filepath.Join(root, "shared-state")
	require.NoError(t, os.Mkdir(shared, 0o700))
	require.NoError(t, os.Chmod(shared, 0o755))
	linked := filepath.Join(root, "linked-state")
	require.NoError(t, os.Symlink(state.Dir, linked))

	corrupt := projectDirectory(root, t.TempDir())
	writeStateCopy(t, corrupt, []byte(`{"id":"x"}`))

	relative, err := filepath.Rel(root, state.Dir)
	require.NoError(t, err)

	// A record must name its project folder by the path the volume stores for it. Each
	// copy below sits under the folder name derived from the spelling its record uses,
	// so only the comparison with the independently resolved path can refuse it.
	withDirectory := func(directory string) string {
		var fields map[string]any
		require.NoError(t, json.Unmarshal(record, &fields))
		fields["directory"] = directory
		raw, err := json.Marshal(fields)
		require.NoError(t, err)
		copyDir := projectDirectory(root, directory)
		writeStateCopy(t, copyDir, raw)
		return copyDir
	}
	projectAlias := filepath.Join(physicalTempDir(t), "project-alias")
	require.NoError(t, os.Symlink(state.Record.Directory, projectAlias))
	throughLink := withDirectory(projectAlias)
	relativeProject := withDirectory("relative/project")
	goneProject := withDirectory(filepath.Join(physicalTempDir(t), "gone"))

	for _, tc := range []struct {
		name, dir, want string
	}{
		{name: "relative path", dir: relative, want: "is not an absolute path; start the project with surla"},
		{name: "missing folder", dir: filepath.Join(root, "absent"), want: "no such file or directory"},
		{name: "folder other accounts can read", dir: shared, want: "must be private and owned by this account"},
		{name: "symbolic link to the state", dir: linked, want: "must be private and owned by this account"},
		{name: "folder reached through a linked root", dir: filepath.Join(alias, filepath.Base(state.Dir)), want: "is not its physical path " + state.Dir},
		{name: "folder never initialized", dir: uninitialized.Dir, want: "holds no project; start the project with surla"},
		{name: "record of another project", dir: foreign, want: "does not belong to the project folder " + state.Record.Directory},
		{name: "invalid record", dir: corrupt, want: "invalid project record"},
		{name: "not the exact folder path", dir: state.Dir + string(filepath.Separator), want: "is not its physical path " + state.Dir},
		{name: "folder inside the project folder", dir: inside, want: "must lie outside the project folder " + state.Record.Directory},
		{name: "record names its project folder through a link", dir: throughLink, want: "which is not that folder's stored path " + state.Record.Directory},
		{name: "record names a relative project folder", dir: relativeProject, want: "invalid project record"},
		{name: "record names a project folder that is gone", dir: goneProject, want: "no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Open(tc.dir)
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

// physicalTempDir returns a test-owned folder by its symlink-free path, because a
// temporary folder can be reached through a linked parent such as macOS's /var.
func physicalTempDir(t *testing.T) string {
	t.Helper()
	dir, err := abspath.Eval(t.TempDir())
	require.NoError(t, err)
	return dir
}

// initializedState creates the project for directory under root as `surla` does.
func initializedState(t *testing.T, root, directory string) *State {
	t.Helper()
	state, err := Discover(root, directory)
	require.NoError(t, err)
	lock, err := state.StartLock(context.Background())
	require.NoError(t, err)
	created, err := state.Initialize("uid:test")
	require.NoError(t, err)
	require.True(t, created)
	require.NoError(t, lock.Close())
	return state
}

// writeStateCopy creates a private state folder at dir holding record as its project file.
func writeStateCopy(t *testing.T, dir string, record []byte) {
	t.Helper()
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "project.json"), record, 0o600))
}
