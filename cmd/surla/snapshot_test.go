package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadModuleSnapshotReadsOnlyTheModulesOwnRegularFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(root, filepath.FromSlash(name))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	}
	write("go.mod", "module example.com/design\r\n")
	write("go.sum", "example.com/dep v1.0.0 h1:abc=\n")
	write("design/design.go", "package design // €150\r\n")
	write("design/design_test.go", "package design\n")
	write("design/notes.md", "notes")
	write("internal/x/x.go", "package x\n// raw bytes \xff\xfe preserved for local preview/diff\n")
	write("nested/go.mod", "module example.com/nested\n")
	write("nested/n.go", "package nested\n")
	write(".git/config", "")
	write("vendor/v/v.go", "package v\n")
	write("testdata/t/t.go", "package t\n")

	files, err := readModuleSnapshot(root)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"go.mod":           "module example.com/design\r\n",
		"go.sum":           "example.com/dep v1.0.0 h1:abc=\n",
		"design/design.go": "package design // €150\r\n",
		"internal/x/x.go":  "package x\n// raw bytes \xff\xfe preserved for local preview/diff\n",
	}, files)
}

func TestReadModuleSnapshotRejectsSymbolicLinks(t *testing.T) {
	t.Parallel()
	outside := filepath.Join(t.TempDir(), "secret.go")
	require.NoError(t, os.WriteFile(outside, []byte("package secret\n"), 0o600))
	for name, link := range map[string]func(root string) (string, error){
		"file link": func(root string) (string, error) {
			return root, os.Symlink(outside, filepath.Join(root, "design", "linked.go"))
		},
		"directory link": func(root string) (string, error) {
			return root, os.Symlink(filepath.Dir(outside), filepath.Join(root, "linked"))
		},
		"root directory link": func(root string) (string, error) {
			linkRoot := filepath.Join(t.TempDir(), "root-link")
			return linkRoot, os.Symlink(root, linkRoot)
		},
		"root directory link with trailing slash": func(root string) (string, error) {
			linkRoot := filepath.Join(t.TempDir(), "root-link-slash")
			return linkRoot + string(filepath.Separator), os.Symlink(root, linkRoot)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, "design"), 0o750))
			require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/design\n"), 0o600))
			target, err := link(root)
			require.NoError(t, err)
			_, err = readModuleSnapshot(target)
			assert.ErrorContains(t, err, "symbolic link")
		})
	}
}

func TestReadModuleSnapshotConfinesReadsToOpenedRoot(t *testing.T) {
	t.Parallel()

	t.Run("replaced parent directory symlink is rejected by openSnapshotFile", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "design"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/design\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "design", "design.go"), []byte("package design\n"), 0o600))

		outsideDir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(outsideDir, "design.go"), []byte("package secret\n"), 0o600))

		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, root.Close()) })

		// Swap design/ with a symlink to outsideDir after opening root.
		require.NoError(t, os.RemoveAll(filepath.Join(dir, "design")))
		require.NoError(t, os.Symlink(outsideDir, filepath.Join(dir, "design")))

		f, err := openSnapshotFile(root, "design/design.go")
		require.Nil(t, f)
		require.Error(t, err)

		_, err = readModuleSnapshotFromRoot(dir, root)
		require.ErrorContains(t, err, "symbolic link")
	})

	t.Run("replaced file symlink and FIFO are rejected by openSnapshotFile", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "design"), 0o750))
		outsideFile := filepath.Join(t.TempDir(), "secret.go")
		require.NoError(t, os.WriteFile(outsideFile, []byte("package secret\n"), 0o600))

		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, root.Close()) })

		linkPath := filepath.Join(dir, "design", "linked.go")
		require.NoError(t, os.Symlink(outsideFile, linkPath))
		f, err := openSnapshotFile(root, "design/linked.go")
		require.Nil(t, f)
		require.Error(t, err)

		fifoPath := filepath.Join(dir, "design", "fifo.go")
		require.NoError(t, syscall.Mkfifo(fifoPath, 0o600))
		f, err = openSnapshotFile(root, "design/fifo.go")
		require.Nil(t, f)
		require.ErrorContains(t, err, "is not a regular file")
	})

	t.Run("renamed root directory continues reading original root handle", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		dir := filepath.Join(parent, "module")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "design"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/original\n"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "design", "design.go"), []byte("package design\n"), 0o600))

		root, err := os.OpenRoot(dir)
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, root.Close()) })

		moved := filepath.Join(parent, "moved")
		require.NoError(t, os.Rename(dir, moved))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "design"), 0o750))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/replaced\n"), 0o600))

		files, err := readModuleSnapshotFromRoot(dir, root)
		require.NoError(t, err)
		assert.Equal(t, map[string]string{
			"go.mod":           "module example.com/original\n",
			"design/design.go": "package design\n",
		}, files)
	})
}
