package genpublish

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// TestPublishReportsWrittenFilesAndChanges checks the facts returned to CLI
// and MCP callers, including unchanged generated files and the root guide.
func TestPublishReportsWrittenFilesAndChanges(t *testing.T) {
	dir, root := moduleRoot(t)
	layout(t, dir, map[string]string{
		"go.mod": "module example.com/shop\n", "go.sum": "untouched\n",
		"gen/stale/old.go": "old\n", "gen/orders/service.go": "old\n",
		"gen/NOTES.md": "mine\n", "gen/goa.json": "{\"goa_version\": \"test\"}\n",
		quickstartGuide: "old guide\n",
	})
	generated := maps.Clone(generatedFiles)
	generated[quickstartGuide] = []byte("new guide\n")
	wantInput := maps.Clone(generated)
	result, err := Publish(t.Context(), root, generated)
	require.NoError(t, err)
	assert.Equal(t, []string{quickstartGuide, "gen/goa.json", "gen/orders/service.go"}, result.WrittenFiles)
	assert.Empty(t, result.Added)
	assert.Equal(t, []string{"gen/orders/service.go"}, result.Changed)
	assert.Equal(t, []string{"gen/stale/old.go"}, result.Removed)
	assert.Empty(t, result.Warnings)
	assert.Equal(t, wantInput, generated, "the caller retains the complete generated output")
	assert.Equal(t, map[string]string{
		"go.mod": "module example.com/shop\n", "go.sum": "untouched\n",
		"gen/orders/service.go": "package orders\n", "gen/NOTES.md": "mine\n",
		"gen/goa.json": "{\"goa_version\": \"test\"}\n", quickstartGuide: "new guide\n",
	}, files(t, dir, "."))
}

// TestPublishRejectsInvalidOutputBeforeWriting keeps every existing file when
// one generated path is outside the permitted output or cannot be staged.
func TestPublishRejectsInvalidOutputBeforeWriting(t *testing.T) {
	for _, name := range []string{
		"go.mod", "go.sum", "outside.go", "../escape.go", "/gen/a.go", "gen",
		"gen/", "gen/../a.go", "gen/a/../b.go", "gen//a.go", "gen/a\x00.go",
	} {
		t.Run(name, func(t *testing.T) {
			dir, root := moduleRoot(t)
			before := map[string]string{
				"gen/orders/service.go": "old\n", quickstartGuide: "old guide\n",
				"go.mod": "module example.com/shop\n", "go.sum": "untouched\n",
			}
			layout(t, dir, before)
			generated := maps.Clone(generatedFiles)
			generated[quickstartGuide] = []byte("new guide\n")
			generated[name] = []byte("invalid\n")
			result, err := Publish(t.Context(), root, generated)
			require.Error(t, err)
			assert.Empty(t, result.WrittenFiles)
			assert.Equal(t, before, files(t, dir, "."))
			assert.Empty(t, leftoverStages(root))
		})
	}
}

// TestPublishCancellationLeavesGenUntouched checks that cancellation prevents
// the exchange even after all generated files can be staged successfully.
func TestPublishCancellationLeavesGenUntouched(t *testing.T) {
	dir, root := moduleRoot(t)
	layout(t, dir, map[string]string{"gen/orders/service.go": "old\n"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := Publish(ctx, root, generatedFiles)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, result.WrittenFiles)
	assert.Equal(t, map[string]string{"gen/orders/service.go": "old\n"}, files(t, dir, "."))
	assert.Empty(t, leftoverStages(root))
}

// TestPublishExchangeFailureLeavesGenUntouched checks that the public call
// removes its unpublished stage and reports the exchange error to its caller.
func TestPublishExchangeFailureLeavesGenUntouched(t *testing.T) {
	dir, root := moduleRoot(t)
	layout(t, dir, map[string]string{"gen/orders/service.go": "old\n"})
	exchangeNames = func(int, string, int, string) error { return syscall.EIO }
	t.Cleanup(func() { exchangeNames = defaultExchangeNames })
	result, err := Publish(t.Context(), root, generatedFiles)
	require.ErrorIs(t, err, syscall.EIO)
	assert.Empty(t, result.WrittenFiles)
	assert.Equal(t, map[string]string{"gen/orders/service.go": "old\n"}, files(t, dir, "."))
}

// TestPublishGuideFailureIsAWarning checks that gen/ publication remains
// successful when a directory at the guide's name prevents replacing it.
func TestPublishGuideFailureIsAWarning(t *testing.T) {
	dir, root := moduleRoot(t)
	require.NoError(t, root.Mkdir(quickstartGuide, 0o755))
	generated := maps.Clone(generatedFiles)
	generated[quickstartGuide] = []byte("guide\n")
	result, err := Publish(t.Context(), root, generated)
	require.NoError(t, err)
	assert.Equal(t, []string{"gen/goa.json", "gen/orders/service.go"}, result.WrittenFiles)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "gen/ was updated; AGENTS_QUICKSTART.md was not:")
	assert.Equal(t, "package orders\n", files(t, dir, "gen")["gen/orders/service.go"])
	stages, err := filepath.Glob(filepath.Join(dir, ".*surla-*"))
	require.NoError(t, err)
	assert.Empty(t, stages, "failed guide writing removes its temporary file")
}

// TestPublishCleanupFailureIsAWarning checks success after another process
// renames the held stage and replaces its old name immediately after exchange.
func TestPublishCleanupFailureIsAWarning(t *testing.T) {
	dir, root := moduleRoot(t)
	layout(t, dir, map[string]string{"gen/orders/service.go": "old\n"})
	exchangeNames = func(fromfd int, from string, tofd int, to string) error {
		if err := defaultExchangeNames(fromfd, from, tofd, to); err != nil {
			return err
		}
		stages := leftoverStages(root)
		require.Len(t, stages, 1)
		require.NoError(t, root.Rename(stages[0], ".moved-stage"))
		layout(t, dir, map[string]string{stages[0] + "/keep.txt": "theirs\n"})
		return nil
	}
	t.Cleanup(func() { exchangeNames = defaultExchangeNames })
	result, err := Publish(t.Context(), root, generatedFiles)
	require.NoError(t, err)
	assert.Equal(t, []string{"gen/goa.json", "gen/orders/service.go"}, result.WrittenFiles)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "no longer names the staging directory")
	assert.Equal(t, "package orders\n", files(t, dir, "gen")["gen/orders/service.go"])
	assert.Empty(t, files(t, dir, ".moved-stage"), "the held directory's old gen/ was removed")
	stages := leftoverStages(root)
	require.Len(t, stages, 1)
	keep, err := root.ReadFile(stages[0] + "/keep.txt")
	require.NoError(t, err)
	assert.Equal(t, "theirs\n", string(keep))
}

// TestPublishExchangeHandleCloseFailureIsAWarning checks that a descriptor
// close error after the exchange cannot make a successful publication fail.
func TestPublishExchangeHandleCloseFailureIsAWarning(t *testing.T) {
	dir, root := moduleRoot(t)
	noReplace = func(fromfd int, from string, tofd int, to string) error {
		if err := defaultNoReplace(fromfd, from, tofd, to); err != nil {
			return err
		}
		// Close the exchange's temporary handle before exchange closes it.
		// Its held stage handle remains open for reporting and cleanup.
		require.NoError(t, unix.Close(fromfd))
		return nil
	}
	t.Cleanup(func() { noReplace = defaultNoReplace })
	result, err := Publish(t.Context(), root, generatedFiles)
	require.NoError(t, err)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], "gen/ was updated; closing exchange directory handles failed:")
	assert.Equal(t, "package orders\n", files(t, dir, "gen")["gen/orders/service.go"])
	assert.Empty(t, leftoverStages(root))
}

// TestPublishLeavesEarlierStagesAlone warns about earlier interrupted work
// without reading or deleting its files.
func TestPublishLeavesEarlierStagesAlone(t *testing.T) {
	dir, root := moduleRoot(t)
	layout(t, dir, map[string]string{".surla-gen-earlier/gen/keep.txt": "keep\n"})
	result, err := Publish(t.Context(), root, generatedFiles)
	require.NoError(t, err)
	require.Len(t, result.Warnings, 1)
	assert.Contains(t, result.Warnings[0], ".surla-gen-earlier")
	got, err := os.ReadFile(filepath.Join(dir, ".surla-gen-earlier/gen/keep.txt"))
	require.NoError(t, err)
	assert.Equal(t, "keep\n", string(got))
}
