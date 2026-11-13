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

// TestNewProjectAvoidsPortsOtherProjectsSaved keeps a new project off every port another
// project of this account saved, whether or not that project's server is running, so two
// projects never share an address. A free port the kernel offers is only a snapshot.
func TestNewProjectAvoidsPortsOtherProjectsSaved(t *testing.T) {
	root := physicalTempDir(t)
	first := initializedState(t, root, t.TempDir())
	other := first.Record.Port + 1
	if other > 65535 {
		other = first.Record.Port - 1
	}
	second, err := Discover(root, t.TempDir())
	require.NoError(t, err)
	lock, err := second.StartLock(context.Background())
	require.NoError(t, err)
	offered := []int{first.Record.Port, other}
	calls := 0
	created, err := second.initialize("uid:test", func() (int, error) {
		port := offered[min(calls, len(offered)-1)]
		calls++
		return port, nil
	})
	require.NoError(t, err)
	require.NoError(t, lock.Close())
	assert.True(t, created)
	assert.Equal(t, other, second.Record.Port, "the port the first project saved is not taken again")
	assert.Equal(t, 2, calls)
}

// TestBrokenStateNamesPathAndStep reports a state folder or record that discovery or the
// start lock cannot use with its path and a step the user can take. Discovery walks up
// through parent folders, so the path also says which project's state is broken. A
// folder this account cannot search hides the record inside it, so the folder, not the
// record, is named with its repair.
func TestBrokenStateNamesPathAndStep(t *testing.T) {
	record := func(state *State) string { return filepath.Join(state.Dir, "project.json") }
	for _, tc := range []struct {
		name   string
		damage func(*testing.T, *State)
		want   func(*State) []string
		absent []string
	}{
		{
			name:   "record others can read",
			damage: func(t *testing.T, state *State) { require.NoError(t, os.Chmod(record(state), 0o644)) },
			want: func(state *State) []string {
				return []string{"Surla project in " + state.Record.Directory, "chmod 600 " + record(state)}
			},
		},
		{
			name: "record that is not JSON",
			damage: func(t *testing.T, state *State) {
				require.NoError(t, os.WriteFile(record(state), []byte("{"), 0o600))
			},
			want: func(state *State) []string {
				return []string{"Surla project in " + state.Record.Directory, record(state), "restore it from a backup", "move the folder " + state.Dir}
			},
		},
		{
			name: "record with trailing data",
			damage: func(t *testing.T, state *State) {
				raw, err := os.ReadFile(record(state))
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(record(state), append(raw, []byte("{}")...), 0o600))
			},
			want: func(state *State) []string {
				return []string{"Surla project in " + state.Record.Directory, record(state), "restore it from a backup"}
			},
		},
		{
			name: "invalid record",
			damage: func(t *testing.T, state *State) {
				require.NoError(t, os.WriteFile(record(state), []byte(`{"id":"short","directory":"/","owner":"","port":0,"agents":{}}`), 0o600))
			},
			want: func(state *State) []string {
				return []string{"Surla project in " + state.Record.Directory, record(state), "restore it from a backup"}
			},
		},
		{
			name:   "folder others can open",
			damage: func(t *testing.T, state *State) { require.NoError(t, os.Chmod(state.Dir, 0o755)) },
			want:   func(state *State) []string { return []string{"chmod 700 " + state.Dir} },
		},
		{
			name:   "folder this account cannot open",
			damage: func(t *testing.T, state *State) { closeFolder(t, state.Dir, 0o000) },
			want:   func(state *State) []string { return []string{state.Dir, "chmod 700 " + state.Dir} },
			absent: []string{"chmod 600", "record of the Surla project"},
		},
		{
			name:   "folder this account cannot search",
			damage: func(t *testing.T, state *State) { closeFolder(t, state.Dir, 0o600) },
			want:   func(state *State) []string { return []string{state.Dir, "chmod 700 " + state.Dir} },
			absent: []string{"chmod 600", "record of the Surla project"},
		},
		{
			name:   "operator folder this account cannot open",
			damage: func(t *testing.T, state *State) { closeFolder(t, filepath.Dir(state.Dir), 0o000) },
			want: func(state *State) []string {
				return []string{"chmod 700 " + filepath.Dir(state.Dir)}
			},
			absent: []string{"chmod 600", "Surla project in"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := physicalTempDir(t)
			project := physicalTempDir(t)
			state := initializedState(t, root, project)
			tc.damage(t, state)
			// Discovery from a folder inside the project reads the same record.
			inside := filepath.Join(project, "inside")
			require.NoError(t, os.Mkdir(inside, 0o755))
			found, err := Discover(root, inside)
			if err == nil {
				_, err = found.StartLock(context.Background())
			}
			require.Error(t, err)
			for _, want := range tc.want(state) {
				assert.Contains(t, err.Error(), want)
			}
			for _, absent := range tc.absent {
				assert.NotContains(t, err.Error(), absent)
			}
		})
	}
}

// TestInitializeNamesClosedStateFolder reports a state folder that this account cannot
// search when a new project is created in it, with the folder's repair: the folder
// holds no record yet, so a step about the record would name a file that does not exist.
func TestInitializeNamesClosedStateFolder(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "folder this account cannot open", mode: 0o000},
		{name: "folder this account cannot search", mode: 0o600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, err := Discover(physicalTempDir(t), physicalTempDir(t))
			require.NoError(t, err)
			require.NoError(t, os.Mkdir(state.Dir, 0o700))
			closeFolder(t, state.Dir, tc.mode)
			_, err = state.Initialize("uid:test")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "chmod 700 "+state.Dir)
			assert.NotContains(t, err.Error(), "chmod 600")
			assert.NotContains(t, err.Error(), "record of the Surla project")
		})
	}
}

// closeFolder gives dir mode for the rest of the test and restores 0700 when the test
// ends, so the test's temporary folders can still be removed. The superuser can open
// any folder, so the test is skipped when it runs as that account.
func closeFolder(t *testing.T, dir string, mode os.FileMode) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("the superuser can open folders of any mode")
	}
	require.NoError(t, os.Chmod(dir, mode))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(dir, 0o700)) })
}
