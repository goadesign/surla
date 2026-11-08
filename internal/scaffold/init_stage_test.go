// These tests exercise publication and retention using the real scaffold flow
// and a runner that writes the trusted starter fixtures without compiling them.
package scaffold

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInitPublishesTheBuiltTreeFromProtectedStorage proves that publication
// moves the original completed directory, without rebuilding or copying it.
func TestInitPublishesTheBuiltTreeFromProtectedStorage(t *testing.T) {
	workspace, work := t.TempDir(), t.TempDir()
	physicalWork, err := filepath.EvalSymlinks(work)
	require.NoError(t, err)
	tmp := filepath.Join(workspace, "tmp")
	require.NoError(t, os.Mkdir(tmp, 0o700))
	t.Setenv("TMPDIR", tmp)
	var built os.FileInfo
	run := func(_ context.Context, dir, log string, args ...string) error {
		assert.True(t, strings.HasPrefix(dir, physicalWork+string(filepath.Separator)))
		assert.True(t, strings.HasPrefix(log, physicalWork+string(filepath.Separator)))
		if built == nil {
			var err error
			built, err = os.Stat(dir)
			require.NoError(t, err)
		}
		return fakeGo(dir, args)
	}
	target := filepath.Join(workspace, "shop")
	_, err = Init(t.Context(), Options{
		Dir: target, WorkDir: work, Module: "example.com/shop", SurlaVersion: published,
	}, run, &fakeGenerator{pkgs: []string{"shop"}})
	require.NoError(t, err)
	created, err := os.Stat(target)
	require.NoError(t, err)
	assert.True(t, os.SameFile(built, created), "the directory built by the runner is the published tree")
	assertOnly(t, work)
	assertOnly(t, tmp)
}

// TestInitPublishesUnderAWriteSearchOnlyParent preserves the CLI's existing
// destination permissions while moving one completed tree with populated logs.
func TestInitPublishesUnderAWriteSearchOnlyParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("UID 0 bypasses directory read restrictions; this case requires an unprivileged process")
	}
	parent, work := t.TempDir(), t.TempDir()
	require.NoError(t, os.Chmod(parent, 0o300))
	t.Cleanup(func() { require.NoError(t, os.Chmod(parent, 0o700)) })
	var built os.FileInfo
	run := func(_ context.Context, dir, log string, args ...string) error {
		if built == nil {
			var err error
			built, err = os.Stat(dir)
			require.NoError(t, err)
		}
		if err := os.WriteFile(log, []byte("trusted command fixture output\n"), 0o600); err != nil {
			return err
		}
		return fakeGo(dir, args)
	}
	target := filepath.Join(parent, "shop")
	gen := &fakeGenerator{pkgs: []string{"shop"}}
	_, err := Init(t.Context(), Options{
		Dir: target, WorkDir: work, Module: "example.com/shop", SurlaVersion: published,
	}, run, gen)
	require.NoError(t, err)
	created, err := os.Stat(target)
	require.NoError(t, err)
	assert.True(t, os.SameFile(built, created), "publication moves the exact completed tree")
	info, err := os.Stat(parent)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o300), info.Mode().Perm())
	assert.Equal(t, 1, gen.closes)
	assert.FileExists(t, filepath.Join(target, "gen", "shop", "service.go"))
	assertOnly(t, work)
}

// TestInitNeverReplacesAnEmptyDirectoryFileOrLink checks targets planted
// after initial validation; even an empty directory must survive publication.
func TestInitNeverReplacesAnEmptyDirectoryFileOrLink(t *testing.T) {
	for _, kind := range []string{"empty directory", "file", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			parent, work := t.TempDir(), t.TempDir()
			target := filepath.Join(parent, "shop")
			var planted os.FileInfo
			run := func(_ context.Context, dir, _ string, args ...string) error {
				if planted == nil {
					switch kind {
					case "empty directory":
						require.NoError(t, os.Mkdir(target, 0o700))
					case "file":
						require.NoError(t, os.WriteFile(target, []byte("keep"), 0o600))
					case "symlink":
						require.NoError(t, os.Symlink(t.TempDir(), target))
					}
					var err error
					planted, err = os.Lstat(target)
					require.NoError(t, err)
				}
				return fakeGo(dir, args)
			}
			_, err := Init(t.Context(), Options{
				Dir: target, WorkDir: work, Module: "example.com/shop", SurlaVersion: published,
			}, run, &fakeGenerator{pkgs: []string{"shop"}})
			require.ErrorContains(t, err, "move project into")
			current, err := os.Lstat(target)
			require.NoError(t, err)
			assert.True(t, os.SameFile(planted, current), "publication preserved the exact foreign entry")
			assertOnly(t, work)
		})
	}
}

// TestInitRejectsWorkspaceStaging refuses the insecure layout before a runner
// can execute, and preserves the generator's close-on-return contract.
func TestInitRejectsWorkspaceStaging(t *testing.T) {
	parent := t.TempDir()
	gen := &fakeGenerator{}
	_, err := Init(t.Context(), Options{
		Dir: filepath.Join(parent, "shop"), WorkDir: parent, Module: "example.com/shop", SurlaVersion: published,
	}, failRunner(t), gen)
	require.ErrorContains(t, err, "must be outside project parent")
	assert.Equal(t, 1, gen.closes)
	assertOnly(t, parent)
}

// TestRunCommandOverridesBothTemporaryDirectories checks the exact environment
// Go uses for OS temporary files and compiler work, without executing a candidate.
func TestRunCommandOverridesBothTemporaryDirectories(t *testing.T) {
	workspace, work := t.TempDir(), t.TempDir()
	physicalWork, err := filepath.EvalSymlinks(work)
	require.NoError(t, err)
	osTemp, goTemp := filepath.Join(workspace, "os-temp"), filepath.Join(workspace, "go-temp")
	require.NoError(t, os.Mkdir(osTemp, 0o700))
	require.NoError(t, os.Mkdir(goTemp, 0o700))
	t.Setenv("TMPDIR", osTemp)
	t.Setenv("GOTMPDIR", goTemp)
	t.Setenv("GOWORK", filepath.Join(workspace, "go.work"))
	log := filepath.Join(physicalWork, "command.log")
	err = RunCommand(t.Context(), workspace, log, "/bin/sh", "-c", `
printf '%s\n%s\n%s\n' "$TMPDIR" "$GOTMPDIR" "$GOWORK"
printf os-temp > "$TMPDIR/os-file"
printf go-temp > "$GOTMPDIR/go-file"
`)
	require.NoError(t, err)
	output, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, physicalWork+"\n"+physicalWork+"\noff\n", string(output))
	assert.FileExists(t, filepath.Join(physicalWork, "os-file"))
	assert.FileExists(t, filepath.Join(physicalWork, "go-file"))
	assertOnly(t, osTemp)
	assertOnly(t, goTemp)
}
