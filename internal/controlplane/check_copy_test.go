package controlplane

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/goadesign/surla/internal/abspath"
)

// realTempDir returns a symlink-free temporary directory.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

// writeTree creates files (path -> content) under root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

// copyGrantPath copies the grant at the path grant, holding it open as
// startCheckOperation does, with the workspace at workspaceDir inside it.
func copyGrantPath(t *testing.T, ctx context.Context, grant, workspaceDir, dst string) (grantCopy, error) {
	t.Helper()
	rel, err := filepath.Rel(grant, workspaceDir)
	require.NoError(t, err)
	fd, err := syscall.Open(grant, openDirFlags, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, syscall.Close(fd)) }()
	return copyGrant(ctx, fd, filepath.ToSlash(rel), identityOf(t, workspaceDir), dst)
}

// identityOf returns the device and inode of the directory at p.
func identityOf(t *testing.T, p string) fileID {
	t.Helper()
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(p, &st))
	return fileID{dev: uint64(st.Dev), ino: st.Ino}
}

// copiedFiles lists the slash paths of every non-directory entry under root.
func copiedFiles(t *testing.T, root string) []string {
	t.Helper()
	var got []string
	require.NoError(t, filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		require.NoError(t, err)
		if !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	}))
	sort.Strings(got)
	return got
}

// caseInsensitive reports whether dir's volume resolves case aliases.
func caseInsensitive(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "case-probe")
	require.NoError(t, os.WriteFile(probe, nil, 0o600))
	_, err := os.Stat(filepath.Join(dir, "CASE-PROBE"))
	require.NoError(t, os.Remove(probe))
	return err == nil
}

func TestCopyGrantCopiesLiveTree(t *testing.T) {
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{
		"go.mod":                      "module example.com/shop\n",
		"go.sum":                      "sum\n",
		"main_test.go":                "package main\n",
		"bin/ignored.out":             "generated\n",
		".gitignore":                  "bin/\n",
		"testdata/fixture.json":       "{}\n",
		"testdata/mod/go.mod":         "module example.com/fixture\n",
		"eval/go.mod":                 "module example.com/shop/eval\n",
		"eval/acceptance/a_test.go":   "package acceptance\n",
		"_tools/go.mod":               "module example.com/tools\n",
		".git/HEAD":                   "ref: refs/heads/main\n",
		".surla/cards/card.json":      "{}\n",
		".surla-gen-123/gen.go":       "package gen\n",
		"reports/public-fixture.json": "{}\n",
	})
	require.NoError(t, os.Symlink("/etc/hosts", filepath.Join(grant, "hosts-link")))
	dst := realTempDir(t)

	got, err := copyGrantPath(t, context.Background(), grant, grant, dst)
	require.NoError(t, err)

	assert.Equal(t, []string{".", "eval"}, got.modules)
	assert.Equal(t, []string{
		".gitignore", "_tools/go.mod", "bin/ignored.out", "eval/acceptance/a_test.go", "eval/go.mod",
		"go.mod", "go.sum", "hosts-link", "main_test.go", "reports/public-fixture.json",
		"testdata/fixture.json", "testdata/mod/go.mod",
	}, copiedFiles(t, dst))
	link, err := os.Readlink(filepath.Join(dst, "hosts-link"))
	require.NoError(t, err)
	assert.Equal(t, "/etc/hosts", link, "symlinks keep their raw target")
	assert.Equal(t, "sum\n", string(got.manifests["go.sum"]))
	assert.Equal(t, "module example.com/shop/eval\n", string(got.manifests["eval/go.mod"]))
}

func TestCopyGrantWorkspaceInsideMonorepo(t *testing.T) {
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{
		"lib/go.mod":           "module example.com/lib\n",
		"services/shop/go.mod": "module example.com/shop\n",
		"services/shop/go.sum": "\n",
	})
	dst := realTempDir(t)
	got, err := copyGrantPath(t, context.Background(), grant, filepath.Join(grant, "services", "shop"), dst)
	require.NoError(t, err)
	assert.Equal(t, []string{"."}, got.modules, "only modules under the workspace are tested")
	assert.Contains(t, copiedFiles(t, dst), "lib/go.mod", "siblings in the grant are copied for replace directives")
}

func TestCopyEntryReportsVanishedEntry(t *testing.T) {
	grant := realTempDir(t)
	fd, err := syscall.Open(grant, openDirFlags, 0)
	require.NoError(t, err)
	defer syscall.Close(fd)
	c := &grantCopier{ctx: context.Background(), workspaceRel: ".", out: grantCopy{manifests: map[string][]byte{}}}
	err = c.copyEntry(fd, "gone.txt", "gone.txt", realTempDir(t))
	var changed *errSourceChanged
	require.True(t, errors.As(err, &changed))
	assert.Contains(t, err.Error(), "the workspace changed while check_project was copying gone.txt")
}

func TestCopyGrantExcludesProtectedDirectoryAliases(t *testing.T) {
	cases := []struct {
		name        string
		insensitive bool
		files       map[string]string
		wantAbsent  []string
		wantPresent []string
	}{
		{
			name:        "upper-case .SURLA is the private store on a case-insensitive volume",
			insensitive: true,
			files: map[string]string{
				".SURLA/runs/r1/work/suite/main.go": "package main\n",
				".SURLA/runs/r1/log.txt":            "suite log\n",
				"surla-notes/fixture.txt":           "adjacent fixture\n",
			},
			wantAbsent:  []string{".SURLA/runs/r1/work/suite/main.go", ".SURLA/runs/r1/log.txt"},
			wantPresent: []string{"surla-notes/fixture.txt"},
		},
		{
			name:        "mixed-case .Git is the repository on a case-insensitive volume",
			insensitive: true,
			files: map[string]string{
				".Git/config":       "[core]\n",
				"gitdocs/notes.txt": "adjacent fixture\n",
			},
			wantAbsent:  []string{".Git/config"},
			wantPresent: []string{"gitdocs/notes.txt"},
		},
		{
			name: "distinct .SURLA beside .surla is a fixture on a case-sensitive volume",
			files: map[string]string{
				".surla/cards/card.json": "{}\n",
				".SURLA/fixture.txt":     "fixture\n",
			},
			wantAbsent:  []string{".surla/cards/card.json"},
			wantPresent: []string{".SURLA/fixture.txt"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grant := realTempDir(t)
			if caseInsensitive(t, grant) != tc.insensitive {
				t.Skip("needs a case-insensitive volume to equal ", tc.insensitive)
			}
			writeTree(t, grant, map[string]string{"go.mod": "module example.com/shop\n"})
			writeTree(t, grant, tc.files)
			dst := realTempDir(t)
			_, err := copyGrantPath(t, context.Background(), grant, grant, dst)
			require.NoError(t, err)
			got := copiedFiles(t, dst)
			for _, p := range tc.wantAbsent {
				assert.NotContains(t, got, p)
			}
			for _, p := range tc.wantPresent {
				assert.Contains(t, got, p)
			}
		})
	}
}

func TestCopyGrantStopsWhenTheOperationEnds(t *testing.T) {
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{"go.mod": "module example.com/shop\n", "big.bin": strings.Repeat("x", 1<<20)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := copyGrantPath(t, ctx, grant, grant, realTempDir(t))
	require.ErrorIs(t, err, context.Canceled)

	// A file that keeps growing stops at the deadline mid-read.
	ctx, cancel = context.WithCancel(context.Background())
	r := ctxReader{ctx: ctx, r: &endlessReader{cancelAfter: 3, cancel: cancel}}
	_, err = io.Copy(io.Discard, r)
	require.ErrorIs(t, err, context.Canceled)
}

// endlessReader never reaches EOF, like a log appended faster than it is
// read, and cancels its operation after cancelAfter reads.
type endlessReader struct {
	cancelAfter int
	cancel      context.CancelFunc
}

func (r *endlessReader) Read(p []byte) (int, error) {
	if r.cancelAfter--; r.cancelAfter <= 0 {
		r.cancel()
	}
	return len(p), nil
}

func TestCopyEntryDecidesProtectedAliasWhenItOpensTheEntry(t *testing.T) {
	// The directory was listed with .SURLA in it; .SURLA is then replaced by
	// a new directory before its entry is decided. The decision uses the
	// current kernel resolution, so the replacement is still excluded.
	grant := realTempDir(t)
	if !caseInsensitive(t, grant) {
		t.Skip("needs a case-insensitive volume")
	}
	writeTree(t, grant, map[string]string{".SURLA/runs/old.txt": "old\n"})
	fd, err := syscall.Open(grant, openDirFlags, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, syscall.Close(fd)) }()
	names, err := readDirNames(fd)
	require.NoError(t, err)
	require.Contains(t, names, ".SURLA")
	require.NoError(t, os.RemoveAll(filepath.Join(grant, ".SURLA")))
	writeTree(t, grant, map[string]string{".SURLA/runs/work/suite/main.go": "package main\n"})

	dst := realTempDir(t)
	c := &grantCopier{ctx: context.Background(), workspaceRel: ".", out: grantCopy{manifests: map[string][]byte{}}}
	require.NoError(t, c.copyEntry(fd, ".SURLA", ".SURLA", dst))
	assert.Empty(t, copiedFiles(t, dst))
}

func TestCopyGrantRefusesAReplacedNestedWorkspace(t *testing.T) {
	// grant=/repo, workspace=/repo/app: app is replaced after the server
	// verified it, so the copy refuses instead of reading the replacement.
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{"app/go.mod": "module example.com/app\n"})
	verified := identityOf(t, filepath.Join(grant, "app"))
	require.NoError(t, os.Rename(filepath.Join(grant, "app"), filepath.Join(grant, "app-old")))
	writeTree(t, grant, map[string]string{"app/go.mod": "module example.com/replacement\n"})

	fd, err := syscall.Open(grant, openDirFlags, 0)
	require.NoError(t, err)
	defer func() { require.NoError(t, syscall.Close(fd)) }()
	_, err = copyGrant(context.Background(), fd, "app", verified, realTempDir(t))
	var changed *errSourceChanged
	require.ErrorAs(t, err, &changed)
	assert.Contains(t, err.Error(), "the workspace directory was replaced")
}

// TestCopyGrantTreatsSymlinkedGoModAsAModule verifies that a nested
// directory whose copied go.mod is a symlink, resolvable or not, is scheduled
// for its own go commands, and that the link is copied raw and never
// recorded as a publishable manifest.
func TestCopyGrantTreatsSymlinkedGoModAsAModule(t *testing.T) {
	// A directory with a go.mod symlink gets its own go commands; the link is
	// copied raw, and Go in the container resolves it and decides which
	// module owns the directory's packages.
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{
		"go.mod":             "module example.com/shop\n",
		"manifests/eval.mod": "module example.com/shop/eval\n",
		"eval/eval_test.go":  "package eval\n",
		"other/x_test.go":    "package other\n",
	})
	require.NoError(t, os.Symlink("../manifests/eval.mod", filepath.Join(grant, "eval", "go.mod")))
	require.NoError(t, os.Symlink("../missing.mod", filepath.Join(grant, "other", "go.mod")))
	dst := realTempDir(t)

	got, err := copyGrantPath(t, context.Background(), grant, grant, dst)
	require.NoError(t, err)
	assert.Equal(t, []string{".", "eval", "other"}, got.modules, "every copied go.mod entry is scheduled; Go in the container resolves the link")
	link, err := os.Readlink(filepath.Join(dst, "eval", "go.mod"))
	require.NoError(t, err)
	assert.Equal(t, "../manifests/eval.mod", link, "the link is copied raw")
	assert.NotContains(t, got.manifests, "eval/go.mod", "a linked manifest is never published")
}

// TestOpenEntryHandlesRegularFileDirectorySymlinkAndFIFO verifies that
// openEntry opens regular files and directories for reading, opens symbolic
// links without following them, and opens FIFOs without blocking.
func TestOpenEntryHandlesRegularFileDirectorySymlinkAndFIFO(t *testing.T) {
	dir := realTempDir(t)
	writeTree(t, dir, map[string]string{
		"go.mod":        "module example.com/shop\n",
		"sub/hello.txt": "hello\n",
	})
	require.NoError(t, os.Symlink("sub/hello.txt", filepath.Join(dir, "hello-link")))
	require.NoError(t, os.Symlink("missing-target", filepath.Join(dir, "dangling-link")))
	require.NoError(t, unix.Mkfifo(filepath.Join(dir, "pipe.fifo"), 0o600))

	dirFD, err := unix.Open(dir, openDirFlags, 0)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, unix.Close(dirFD)) })

	// 1. Regular file is readable and reports its stored path.
	regFD, err := openEntry(dirFD, "go.mod")
	require.NoError(t, err)
	regFile := os.NewFile(uintptr(regFD), "go.mod")
	regPath, err := abspath.FDPath(regFD)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "go.mod"), regPath)
	regBytes, err := io.ReadAll(regFile)
	require.NoError(t, err)
	assert.Equal(t, "module example.com/shop\n", string(regBytes))
	require.NoError(t, regFile.Close())

	// 2. Directory supports readDirNames and reports its stored path.
	subFD, err := openEntry(dirFD, "sub")
	require.NoError(t, err)
	subPath, err := abspath.FDPath(subFD)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "sub"), subPath)
	names, err := readDirNames(subFD)
	require.NoError(t, err)
	assert.Equal(t, []string{"hello.txt"}, names)
	require.NoError(t, unix.Close(subFD))

	// 3. Both valid and dangling symlinks open as symlinks without following targets.
	for _, linkName := range []string{"hello-link", "dangling-link"} {
		linkFD, err := openEntry(dirFD, linkName)
		require.NoError(t, err)
		linkFile := os.NewFile(uintptr(linkFD), linkName)
		info, err := linkFile.Stat()
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&fs.ModeSymlink, linkName)
		gotLinkPath, err := abspath.FDPath(linkFD)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(dir, linkName), gotLinkPath)
		require.NoError(t, linkFile.Close())
	}

	// 4. FIFO opens without blocking and copyGrant skips it cleanly.
	fifoFD, err := openEntry(dirFD, "pipe.fifo")
	require.NoError(t, err)
	fifoFile := os.NewFile(uintptr(fifoFD), "pipe.fifo")
	fifoInfo, err := fifoFile.Stat()
	require.NoError(t, err)
	assert.NotZero(t, fifoInfo.Mode()&fs.ModeNamedPipe)
	require.NoError(t, fifoFile.Close())

	dst := realTempDir(t)
	got, err := copyGrantPath(t, context.Background(), dir, dir, dst)
	require.NoError(t, err)
	assert.Equal(t, []string{"pipe.fifo"}, got.skipped)
}
