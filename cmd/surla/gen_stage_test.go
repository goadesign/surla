package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// generatedFiles is a small generated set: one service package file and the
// version file.
var generatedFiles = map[string][]byte{
	"gen/orders/service.go": []byte("package orders\n"),
	"gen/goa.json":          []byte("{\"goa_version\": \"test\"}\n"),
}

func TestStageReplacesGenWithExactGoaGenParity(t *testing.T) {
	cases := []struct {
		name string
		// before lays out the module's gen/ before publishing.
		before map[string]string
		// want maps every file under gen/ afterwards to its content.
		want map[string]string
	}{
		{
			name: "no gen yet",
			want: map[string]string{"gen/orders/service.go": "package orders\n", "gen/goa.json": "{\"goa_version\": \"test\"}\n"},
		},
		{
			name: "stale subdirectory removed, top-level user file kept, goa.json rewritten",
			before: map[string]string{
				"gen/stale/old.go": "package stale\n",
				"gen/NOTES.md":     "mine\n",
				"gen/goa.json":     "old\n",
			},
			want: map[string]string{
				"gen/orders/service.go": "package orders\n",
				"gen/goa.json":          "{\"goa_version\": \"test\"}\n",
				"gen/NOTES.md":          "mine\n",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, root := moduleRoot(t)
			layout(t, dir, tc.before)
			stage, err := newStage(root)
			require.NoError(t, err)
			require.NoError(t, stage.write(root, generatedFiles))
			require.NoError(t, stage.exchange(root))
			_, err = stage.report(root)
			require.NoError(t, err)
			require.NoError(t, stage.cleanup(root))
			assert.Equal(t, tc.want, files(t, dir, "gen"))
			assert.Empty(t, leftoverStages(root), "the staging directory is removed")
		})
	}
}

func TestStageFailureBeforeTheExchangeLeavesGenUntouched(t *testing.T) {
	dir, root := moduleRoot(t)
	layout(t, dir, map[string]string{"gen/orders/service.go": "old\n"})
	stage, err := newStage(root)
	require.NoError(t, err)
	require.NoError(t, stage.write(root, generatedFiles))
	renameatx = func(int, string, int, string, uint32) error { return syscall.EIO }
	t.Cleanup(func() { renameatx = defaultRenameatx })

	require.ErrorIs(t, stage.exchange(root), syscall.EIO)
	require.NoError(t, stage.cleanup(root))
	assert.Equal(t, map[string]string{"gen/orders/service.go": "old\n"}, files(t, dir, "gen"))
	assert.Empty(t, leftoverStages(root))
}

func TestStageExclusiveExchangeLosesARaceWithoutTouchingGen(t *testing.T) {
	dir, root := moduleRoot(t)
	stage, err := newStage(root)
	require.NoError(t, err)
	require.NoError(t, stage.write(root, generatedFiles))
	// gen/ appears after staging decided there was none.
	renameatx = func(fromfd int, from string, tofd int, to string, _ uint32) error {
		layout(t, dir, map[string]string{"gen/theirs.go": "theirs\n"})
		return defaultRenameatx(fromfd, from, tofd, to, renameExcl)
	}
	t.Cleanup(func() { renameatx = defaultRenameatx })

	require.Error(t, stage.exchange(root))
	require.NoError(t, stage.cleanup(root))
	assert.Equal(t, map[string]string{"gen/theirs.go": "theirs\n"}, files(t, dir, "gen"))
}

func TestGenThatIsNotADirectoryIsRefused(t *testing.T) {
	dir, root := moduleRoot(t)
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(dir, "gen")))
	stage, err := newStage(root)
	require.NoError(t, err)
	require.ErrorContains(t, stage.write(root, generatedFiles), "gen is not a directory")
	require.NoError(t, stage.cleanup(root))
}

func TestCleanupNeverDeletesAReplacementByName(t *testing.T) {
	cases := []struct {
		name string
		// replace puts something else at the staging directory's name after
		// the exchange.
		replace func(t *testing.T, dir, name string)
	}{
		{name: "another directory", replace: func(t *testing.T, dir, name string) {
			layout(t, dir, map[string]string{name + "/keep.txt": "keep\n"})
		}},
		{name: "a symbolic link", replace: func(t *testing.T, dir, name string) {
			outside := t.TempDir()
			layout(t, outside, map[string]string{"keep.txt": "keep\n"})
			require.NoError(t, os.Symlink(outside, filepath.Join(dir, name)))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, root := moduleRoot(t)
			layout(t, dir, map[string]string{"gen/orders/old.go": "old\n"})
			stage, err := newStage(root)
			require.NoError(t, err)
			require.NoError(t, stage.write(root, generatedFiles))
			require.NoError(t, stage.exchange(root))
			// A local process renames the staging directory A and puts
			// something else at its old name.
			require.NoError(t, os.Rename(filepath.Join(dir, stage.name), filepath.Join(dir, ".moved-A")))
			tc.replace(t, dir, stage.name)

			err = stage.cleanup(root)
			require.ErrorContains(t, err, "no longer names the staging directory")
			assert.Empty(t, files(t, dir, ".moved-A"), "the old tree inside the held directory is removed")
			keep, err := os.ReadFile(filepath.Join(dir, stage.name, "keep.txt"))
			require.NoError(t, err)
			assert.Equal(t, "keep\n", string(keep), "the replacement and its file survive")
			assert.Equal(t, "package orders\n", files(t, dir, "gen")["gen/orders/service.go"])
		})
	}
}

func TestWriteGuideReplacesTheGuide(t *testing.T) {
	dir, root := moduleRoot(t)
	layout(t, dir, map[string]string{quickstartGuide: "old\n"})
	require.NoError(t, writeGuide(root, []byte("new\n")))
	got, err := os.ReadFile(filepath.Join(dir, quickstartGuide))
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(got))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no temporary file is left")
}

func TestMissingRequirementsNamesImportsGoModLacks(t *testing.T) {
	files := map[string]string{"go.mod": "module example.com/shop\n\ngo 1.24\n\nrequire goa.design/goa/v3 v3.0.0\n"}
	generated := map[string][]byte{
		"gen/a/a.go":   []byte("package a\nimport (\n\t\"context\"\n\t\"goa.design/goa/v3/pkg\"\n\t\"example.com/shop/gen/b\"\n\t\"goa.design/goa-ai/runtime\"\n)\n"),
		"gen/goa.json": []byte("{}"),
	}
	assert.Equal(t, []string{"goa.design/goa-ai/runtime"}, missingRequirements(files, generated))
}

// renameExcl is RENAME_EXCL, for the race test.
const renameExcl = unix.RENAME_EXCL

// defaultRenameatx is the production exchange the tests restore.
var defaultRenameatx = renameatx

// moduleRoot returns a temporary module directory and an os.Root on it.
func moduleRoot(t *testing.T) (string, *os.Root) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, root.Close()) })
	return dir, root
}

// layout writes files, keyed by slash-separated path, under dir.
func layout(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
	}
}

// files returns every regular file under dir/sub, keyed by its path relative
// to dir, with its content. A missing sub is empty.
func files(t *testing.T, dir, sub string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(filepath.Join(dir, sub), func(p string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return filepath.SkipDir
		}
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			content, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return err
			}
			out[filepath.ToSlash(rel)] = string(content)
		}
		return nil
	})
	require.NoError(t, err)
	return out
}
