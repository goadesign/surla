package genpublish

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemoveLeftoverStagesRemovesWhatAnInterruptedPublishLeft lays out each
// shape of staging directory a publication that died can leave, and each
// entry with a staging name that an author of the module can plant instead.
// A staging directory is removed. A planted entry is never followed: what it
// names outside the module, and the module's other files, are byte-identical
// afterwards.
func TestRemoveLeftoverStagesRemovesWhatAnInterruptedPublishLeft(t *testing.T) {
	const name = ".surla-gen-earlier"
	cases := []struct {
		name string
		// plant lays out the entry called name in the module dir; outside is
		// a folder beside the module that nothing may change.
		plant func(t *testing.T, dir, outside string)
		// wantLeft is whether the entry is still there afterwards.
		wantLeft bool
	}{
		{name: "a stage that died before the exchange", plant: func(t *testing.T, dir, _ string) {
			layout(t, dir, map[string]string{name + "/gen/orders/service.go": "package orders\n", name + "/gen/goa.json": "{}\n"})
		}},
		{name: "a stage that died before its gen was created", plant: func(t *testing.T, dir, _ string) {
			require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o755))
		}},
		{name: "a symbolic link to a folder outside the module", plant: func(t *testing.T, dir, outside string) {
			require.NoError(t, os.Symlink(outside, filepath.Join(dir, name)))
		}, wantLeft: true},
		{name: "a symbolic link to a folder inside the module", plant: func(t *testing.T, dir, _ string) {
			require.NoError(t, os.Symlink("acceptance", filepath.Join(dir, name)))
		}, wantLeft: true},
		{name: "a symbolic link to nothing", plant: func(t *testing.T, dir, _ string) {
			require.NoError(t, os.Symlink("missing", filepath.Join(dir, name)))
		}, wantLeft: true},
		{name: "a regular file", plant: func(t *testing.T, dir, _ string) {
			layout(t, dir, map[string]string{name: "the author's\n"})
		}, wantLeft: true},
		{name: "a folder whose gen is a link to a folder outside", plant: func(t *testing.T, dir, outside string) {
			require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0o755))
			require.NoError(t, os.Symlink(outside, filepath.Join(dir, name, "gen")))
		}, wantLeft: true},
		{name: "links inside the staged tree to a folder and a file outside", plant: func(t *testing.T, dir, outside string) {
			layout(t, dir, map[string]string{name + "/gen/orders/service.go": "package orders\n"})
			require.NoError(t, os.Symlink(outside, filepath.Join(dir, name, "gen", "folder")))
			require.NoError(t, os.Symlink(filepath.Join(outside, "keep.txt"), filepath.Join(dir, name, "gen", "orders", "file")))
			require.NoError(t, os.Symlink("../../../acceptance", filepath.Join(dir, name, "gen", "orders", "inside")))
		}},
		{name: "a hard link inside the staged tree to a file outside", plant: func(t *testing.T, dir, outside string) {
			require.NoError(t, os.MkdirAll(filepath.Join(dir, name, "gen"), 0o755))
			require.NoError(t, os.Link(filepath.Join(outside, "keep.txt"), filepath.Join(dir, name, "gen", "keep.txt")))
		}},
		{name: "a folder holding files no publication stages", plant: func(t *testing.T, dir, _ string) {
			layout(t, dir, map[string]string{name + "/notes.txt": "the author's\n", name + "/gen/orders/service.go": "package orders\n"})
		}, wantLeft: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, root := moduleRoot(t)
			outside := t.TempDir()
			layout(t, outside, map[string]string{"keep.txt": "keep\n", "gen/theirs.go": "theirs\n"})
			layout(t, dir, map[string]string{
				"go.mod": "module example.com/shop/eval\n", "gen/orders/service.go": "current\n",
				"acceptance/acceptance.go": "package acceptance\n", "acceptance/gen/mine.go": "mine\n",
			})
			tc.plant(t, dir, outside)
			planted := files(t, dir, name)
			moduleBefore := files(t, dir, ".")

			require.NoError(t, RemoveLeftoverStages(root))
			assert.Equal(t, map[string]string{"keep.txt": "keep\n", "gen/theirs.go": "theirs\n"}, files(t, outside, "."),
				"nothing outside the module is removed or changed")
			_, statErr := os.Lstat(filepath.Join(dir, name))
			if tc.wantLeft {
				require.NoError(t, statErr, "the entry is the author's and stays")
				assert.Equal(t, moduleBefore, files(t, dir, "."), "the module is untouched")
				return
			}
			assert.ErrorIs(t, statErr, os.ErrNotExist, "the staging directory is removed")
			for p := range planted {
				delete(moduleBefore, p)
			}
			assert.Equal(t, moduleBefore, files(t, dir, "."), "only the staging directory is removed")
		})
	}
}

// TestRemoveLeftoverStagesRemovesEveryStage removes several staging
// directories in one call and leaves a module without any as it is.
func TestRemoveLeftoverStagesRemovesEveryStage(t *testing.T) {
	dir, root := moduleRoot(t)
	layout(t, dir, map[string]string{"gen/orders/service.go": "current\n"})
	require.NoError(t, RemoveLeftoverStages(root))
	layout(t, dir, map[string]string{
		".surla-gen-one/gen/a.go": "a\n", ".surla-gen-two/gen/b/b.go": "b\n", ".surla-genuine/gen/c.go": "not a staging name\n",
	})
	require.NoError(t, RemoveLeftoverStages(root))
	assert.Equal(t, map[string]string{
		"gen/orders/service.go": "current\n", ".surla-genuine/gen/c.go": "not a staging name\n",
	}, files(t, dir, "."))
	assert.Empty(t, leftoverStages(root))
}

// TestALeftoverReplacedWhileItIsOpenedIsLeftAlone replaces a staging
// directory, after it was inspected and before it is opened, with a link to
// a folder that holds a gen: inside the module, which a directory handle
// follows, and outside it, which it refuses. Neither folder loses a file.
func TestALeftoverReplacedWhileItIsOpenedIsLeftAlone(t *testing.T) {
	const name = ".surla-gen-earlier"
	for _, inside := range []bool{true, false} {
		t.Run(map[bool]string{true: "a link inside the module", false: "a link outside the module"}[inside], func(t *testing.T) {
			dir, root := moduleRoot(t)
			outside := t.TempDir()
			layout(t, outside, map[string]string{"gen/theirs.go": "theirs\n"})
			layout(t, dir, map[string]string{"acceptance/gen/mine.go": "mine\n"})
			require.NoError(t, root.Mkdir(name, 0o755))
			inspected, err := root.Lstat(name)
			require.NoError(t, err)
			require.NoError(t, os.Remove(filepath.Join(dir, name)))
			target := outside
			if inside {
				target = "acceptance"
			}
			require.NoError(t, os.Symlink(target, filepath.Join(dir, name)))

			stage, err := openLeftover(root, name, inspected)
			require.Error(t, err)
			assert.Nil(t, stage)
			assert.Equal(t, map[string]string{"gen/theirs.go": "theirs\n"}, files(t, outside, "."))
			assert.Equal(t, map[string]string{"acceptance/gen/mine.go": "mine\n"}, files(t, dir, "acceptance"))
		})
	}
}

// TestALeftoverRenamedDuringItsRemovalTakesNothingElseWithIt opens a staging
// directory for removal, then moves it out of the module and puts something
// else at its name, as a local process could while the staged tree is being
// removed. The removal empties only the directory it opened and never removes
// by name: the replacement, and what a replacing link names, survive.
func TestALeftoverRenamedDuringItsRemovalTakesNothingElseWithIt(t *testing.T) {
	const name = ".surla-gen-earlier"
	cases := []struct {
		name string
		// replace puts something else at the staging name; outside is a
		// folder beside the module.
		replace func(t *testing.T, dir, outside string)
	}{
		{name: "a link to a folder outside the module", replace: func(t *testing.T, dir, outside string) {
			require.NoError(t, os.Symlink(outside, filepath.Join(dir, name)))
		}},
		{name: "another folder", replace: func(t *testing.T, dir, _ string) {
			layout(t, dir, map[string]string{name + "/gen/theirs.go": "theirs\n"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, root := moduleRoot(t)
			outside, moved := t.TempDir(), filepath.Join(t.TempDir(), "moved")
			layout(t, outside, map[string]string{"gen/theirs.go": "theirs\n"})
			layout(t, dir, map[string]string{name + "/gen/orders/service.go": "package orders\n"})
			inspected, err := root.Lstat(name)
			require.NoError(t, err)
			stage, err := openLeftover(root, name, inspected)
			require.NoError(t, err)
			require.NotNil(t, stage)
			require.NoError(t, os.Rename(filepath.Join(dir, name), moved))
			tc.replace(t, dir, outside)

			require.ErrorContains(t, stage.cleanup(root), "no longer names the staging directory")
			assert.Empty(t, files(t, moved, "."), "the staged tree in the opened directory is removed")
			assert.Equal(t, map[string]string{"gen/theirs.go": "theirs\n"}, files(t, outside, "."))
			_, err = os.Lstat(filepath.Join(dir, name))
			require.NoError(t, err, "the replacement stays")
			if tc.name == "another folder" {
				assert.Equal(t, map[string]string{name + "/gen/theirs.go": "theirs\n"}, files(t, dir, name))
			}
		})
	}
}
