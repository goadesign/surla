package abspath

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStoredReturnsOneSpellingPerDirectory verifies that every spelling a
// case- and normalization-insensitive volume resolves to one directory
// yields that directory's stored path.
func TestStoredReturnsOneSpellingPerDirectory(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(base, "café", "Repo"), 0o755))
	if _, err := os.Stat(filepath.Join(base, "CAFÉ", "REPO")); err != nil {
		t.Skip("needs a case- and normalization-insensitive volume")
	}
	want := filepath.Join(base, "café", "Repo")
	for _, spelling := range []string{
		want,
		filepath.Join(base, "CAFÉ", "repo"),
		filepath.Join(base, "café", "REPO"),
	} {
		got, err := Stored(spelling)
		require.NoError(t, err)
		assert.Equal(t, want, got, spelling)
	}
	_, err = Stored(filepath.Join(base, "missing"))
	require.ErrorIs(t, err, os.ErrNotExist)
}
