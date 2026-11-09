package designeval

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/goadesign/surla/internal/testmodule"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// minimalDesign is a valid Goa design used by the boundary tests.
const minimalDesign = `package design

import . "goa.design/goa/v3/dsl"

var _ = Service("orders", func() {
	Method("get_order", func() {
		Payload(String)
		Result(String)
	})
})
`

func TestValidatePathRejectsPathsOutsideTheModuleContract(t *testing.T) {
	for _, name := range []string{
		"", "/etc/passwd", "../x.go", "design/../../x.go", "./design/a.go", "design//a.go", `design\a.go`,
		"surla_designeval/main.go", "surla_designeval", "sub/go.mod", "sub/go.sum", "design/notes.txt", "design/",
	} {
		assert.Error(t, validatePath(name), "%q must be rejected", name)
	}
	for _, name := range []string{"go.mod", "go.sum", "design/design.go", "internal/types/types.go", "design/a_test.go"} {
		assert.NoError(t, validatePath(name), "%q is a valid snapshot path", name)
	}
}

func TestValidateSnapshotRequiresACompleteModule(t *testing.T) {
	valid := map[string]string{
		"go.mod":           "module example.com/design\n\ngo 1.24\n",
		"go.sum":           "",
		"design/design.go": minimalDesign,
	}
	_, err := validateSnapshot(valid)
	require.NoError(t, err)

	cases := map[string]map[string]string{
		"no go.mod":                  testmodule.WithFile(without(valid, "go.mod"), "design/x.go", "package design"),
		"no go.sum":                  without(valid, "go.sum"),
		"no design package":          testmodule.WithFile(without(valid, "design/design.go"), "other/a.go", "package other"),
		"only design tests":          testmodule.WithFile(without(valid, "design/design.go"), "design/a_test.go", "package design"),
		"no module path":             testmodule.WithFile(valid, "go.mod", "go 1.24\n"),
		"relative directory replace": testmodule.WithFile(valid, "go.mod", "module example.com/design\n\nreplace goa.design/goa/v3 => ../goa\n"),
		"absolute directory replace": testmodule.WithFile(valid, "go.mod", "module example.com/design\n\nreplace goa.design/goa/v3 => /src/goa\n"),
		"escaping path":              testmodule.WithFile(valid, "../outside.go", "package x"),
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := validateSnapshot(files)
			assert.Error(t, err)
		})
	}

	_, err = validateSnapshot(without(valid, "go.sum"))
	assert.ErrorContains(t, err, "run `go mod tidy` in the module", "a missing go.sum tells the user how to create it")

	versioned := testmodule.WithFile(valid, "go.mod", "module example.com/design\n\nreplace goa.design/goa/v3 => goa.design/goa/v3 v3.20.0\n")
	_, err = validateSnapshot(versioned)
	assert.NoError(t, err, "a versioned replacement is resolved like any requirement")
}

func TestStageSnapshotWritesExactBytesWithoutReadingHostPaths(t *testing.T) {
	dir := t.TempDir()
	// A design file whose content names a real host path is only content.
	files := map[string]string{
		"go.mod":           "module example.com/design\n",
		"design/design.go": "/etc/passwd",
	}
	require.NoError(t, stageSnapshot(dir, files))
	for name, want := range files {
		got, err := readFile(dir, name)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	}
	assert.Error(t, stageSnapshot(dir, files), "staging never overwrites or follows an existing file")
}

func TestSnapshotDigestDistinguishesEveryFile(t *testing.T) {
	a := map[string]string{"a.go": "x", "b.go": "y"}
	assert.Equal(t, SnapshotDigest(a), SnapshotDigest(map[string]string{"b.go": "y", "a.go": "x"}))
	assert.NotEqual(t, SnapshotDigest(a), SnapshotDigest(map[string]string{"a.go": "xb.go", "": "y"}),
		"length prefixes keep path and content boundaries apart")
	assert.NotEqual(t, SnapshotDigest(a), SnapshotDigest(map[string]string{"a.go": "x", "b.go": "z"}))
}

func TestOpenSnapshotFileAndReadDesignFilesFromRootConfineToOpenedRoot(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "design"), 0o750))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "internal", "x"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/design\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "design", "design.go"), []byte("package design\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "design", "design_test.go"), []byte("package design\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "internal", "x", "x.go"), []byte("package x\n"), 0o600))

	outsideFile := filepath.Join(t.TempDir(), "secret.go")
	require.NoError(t, os.WriteFile(outsideFile, []byte("package secret\n"), 0o600))

	cleanDir, root, err := OpenModuleRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, root.Close()) })

	designOnly, err := ReadDesignFilesFromRoot(cleanDir, root)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"design/design.go": "package design\n",
	}, designOnly)

	linkPath := filepath.Join(dir, "design", "linked.go")
	require.NoError(t, os.Symlink(outsideFile, linkPath))
	f, err := openSnapshotFile(root, "design/linked.go")
	require.Nil(t, f)
	require.Error(t, err)
	_, err = ReadDesignFilesFromRoot(cleanDir, root)
	require.ErrorContains(t, err, "symbolic link")
	require.NoError(t, os.Remove(linkPath))

	fifoPath := filepath.Join(dir, "design", "fifo.go")
	require.NoError(t, syscall.Mkfifo(fifoPath, 0o600))
	f, err = openSnapshotFile(root, "design/fifo.go")
	require.Nil(t, f)
	require.ErrorContains(t, err, "is not a regular file")
}

// without returns a copy of files without name.
func without(files map[string]string, name string) map[string]string {
	out := map[string]string{}
	for k, v := range files {
		if k != name {
			out[k] = v
		}
	}
	return out
}

// readFile reads a staged snapshot file.
func readFile(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
	return string(b), err
}
