// This file tests reading one agent-writable file from a root.
package designeval

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReadRegularFileReadsOnlyARegularFileWithinItsLimit reads each kind of
// path an agent could leave and checks the bytes or the error's kind.
func TestReadRegularFileReadsOnlyARegularFileWithinItsLimit(t *testing.T) {
	const limit = 16
	cases := []struct {
		name    string
		rel     string
		prepare func(t *testing.T, dir string)
		want    string
		wantErr error
	}{
		{name: "regular file", rel: "dir/file", want: "content", prepare: writeStep("dir/file", "content")},
		{name: "at the limit", rel: "file", want: strings.Repeat("x", limit), prepare: writeStep("file", strings.Repeat("x", limit))},
		{name: "over the limit", rel: "file", wantErr: ErrFileTooLarge, prepare: writeStep("file", strings.Repeat("x", limit+1))},
		{name: "missing", rel: "dir/file", wantErr: fs.ErrNotExist, prepare: writeStep("dir/other", "")},
		{name: "missing directory", rel: "dir/file", wantErr: fs.ErrNotExist, prepare: writeStep("other", "")},
		{name: "link at the file", rel: "file", wantErr: ErrNotRegularFile, prepare: func(t *testing.T, dir string) {
			writeStep("real", "content")(t, dir)
			require.NoError(t, os.Symlink("real", filepath.Join(dir, "file")))
		}},
		{name: "link out of the root", rel: "file", wantErr: ErrNotRegularFile, prepare: func(t *testing.T, dir string) {
			outside := filepath.Join(t.TempDir(), "outside")
			require.NoError(t, os.WriteFile(outside, []byte("content"), 0o600))
			require.NoError(t, os.Symlink(outside, filepath.Join(dir, "file")))
		}},
		{name: "link on the way", rel: "dir/file", wantErr: ErrNotRegularFile, prepare: func(t *testing.T, dir string) {
			writeStep("real/file", "content")(t, dir)
			require.NoError(t, os.Symlink("real", filepath.Join(dir, "dir")))
		}},
		{name: "file on the way", rel: "dir/file", wantErr: ErrNotRegularFile, prepare: writeStep("dir", "content")},
		{name: "directory", rel: "file", wantErr: ErrNotRegularFile, prepare: writeStep("file/keep", "")},
		{name: "FIFO", rel: "file", wantErr: ErrNotRegularFile, prepare: func(t *testing.T, dir string) {
			require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "file"), 0o600))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.prepare(t, dir)
			root, err := os.OpenRoot(dir)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, root.Close()) })
			got, err := ReadRegularFile(root, tc.rel, limit)
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, string(got))
		})
	}
}

// TestReadRegularFileRefusesAFileSwappedAfterItsCheck opens a path whose file
// is not the one that was checked, as when an agent replaces it between the
// check and the open, and checks that nothing is read.
func TestReadRegularFileRefusesAFileSwappedAfterItsCheck(t *testing.T) {
	dir := t.TempDir()
	writeStep("file", "checked")(t, dir)
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, root.Close()) })
	checked, err := root.Lstat("file")
	require.NoError(t, err)
	require.NoError(t, os.Rename(filepath.Join(dir, "file"), filepath.Join(dir, "old")))
	writeStep("file", "swapped")(t, dir)

	got, err := readCheckedFile(root, "file", checked, 16)
	assert.ErrorIs(t, err, ErrNotRegularFile)
	assert.ErrorContains(t, err, "changed while it was opened")
	assert.Nil(t, got, "nothing of the swapped file is returned")
	same, err := root.Lstat("file")
	require.NoError(t, err)
	unchanged, err := readCheckedFile(root, "file", same, 16)
	require.NoError(t, err)
	assert.Equal(t, "swapped", string(unchanged), "the file that was checked is read")
}

// writeStep returns a step that writes content at slash-separated rel in dir.
func writeStep(rel, content string) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o700))
		require.NoError(t, os.WriteFile(full, []byte(content), 0o600))
	}
}
