package testwriter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPublishMarksAFailureToWriteTheGeneratedFolder publishes into a module
// whose gen is a symbolic link, which is never followed or replaced. The
// error wraps ErrWriteGenerated, so a caller can say that writing failed and
// not generation, and what the link names is untouched.
func TestPublishMarksAFailureToWriteTheGeneratedFolder(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	writeFiles(t, outside, map[string]string{"keep.txt": "keep\n"})
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "gen")))
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, root.Close()) })

	err = publish(t.Context(), root, map[string][]byte{"gen/evals/suite.go": []byte("new\n")})
	require.ErrorIs(t, err, ErrWriteGenerated)
	assert.NotErrorIs(t, err, ErrGeneration)
	assert.ErrorContains(t, err, "write the generated folder: gen is not a directory")
	assert.Equal(t, map[string]string{"keep.txt": "keep\n"}, projectFiles(t, outside))
}
