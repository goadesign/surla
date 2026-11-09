package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goa "goa.design/goa/v3/pkg"
)

// newPublishOp returns an operation whose live grant is grant and whose
// copy is src, with before holding the manifest bytes read during the copy.
func newPublishOp(t *testing.T, grant string, before map[string][]byte) *checkOperation {
	t.Helper()
	dir := realTempDir(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "src"), 0o700))
	return &checkOperation{
		dir:          dir,
		grantRoot:    grant,
		workspaceDir: grant,
		copy:         grantCopy{modules: []string{"."}, manifests: before},
	}
}

func TestPublishManifest(t *testing.T) {
	cases := []struct {
		name        string
		live        map[string]string
		before      map[string][]byte
		staged      map[string]string
		mutate      func(t *testing.T, grant, src string)
		wantChanged bool
		wantErr     string
		wantLive    string
	}{
		{
			name:        "changed go.sum is written",
			live:        map[string]string{"go.sum": "old\n"},
			before:      map[string][]byte{"go.sum": []byte("old\n")},
			staged:      map[string]string{"go.sum": "new\n"},
			wantChanged: true,
			wantLive:    "new\n",
		},
		{
			name:     "unchanged go.sum writes nothing",
			live:     map[string]string{"go.sum": "same\n"},
			before:   map[string][]byte{"go.sum": []byte("same\n")},
			staged:   map[string]string{"go.sum": "same\n"},
			wantLive: "same\n",
		},
		{
			name:        "absent go.sum is created exclusively",
			before:      map[string][]byte{},
			staged:      map[string]string{"go.sum": "created\n"},
			wantChanged: true,
			wantLive:    "created\n",
		},
		{
			name:     "go.sum created concurrently is not overwritten",
			live:     map[string]string{"go.sum": "someone else\n"},
			before:   map[string][]byte{},
			staged:   map[string]string{"go.sum": "ours\n"},
			wantErr:  "was created during update_dependencies",
			wantLive: "someone else\n",
		},
		{
			name:     "go.sum edited during the call is refused",
			live:     map[string]string{"go.sum": "edited\n"},
			before:   map[string][]byte{"go.sum": []byte("old\n")},
			staged:   map[string]string{"go.sum": "new\n"},
			wantErr:  "changed during update_dependencies",
			wantLive: "edited\n",
		},
		{
			name:   "staged go.sum symlink is refused without a host read",
			before: map[string][]byte{"go.sum": []byte("old\n")},
			mutate: func(t *testing.T, _, src string) {
				require.NoError(t, os.Symlink("/etc/hosts", filepath.Join(src, "go.sum")))
			},
			live:     map[string]string{"go.sum": "old\n"},
			wantErr:  "is a symbolic link",
			wantLive: "old\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			grant := realTempDir(t)
			writeTree(t, grant, tc.live)
			op := newPublishOp(t, grant, tc.before)
			src := op.path("src")
			writeTree(t, src, tc.staged)
			if tc.mutate != nil {
				tc.mutate(t, grant, src)
			}
			changed, err := op.publishManifest("go.sum")
			if tc.wantErr != "" {
				require.ErrorIs(t, err, errManifestConflict)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantChanged, changed)
			got, err := os.ReadFile(filepath.Join(grant, "go.sum"))
			require.NoError(t, err)
			assert.Equal(t, tc.wantLive, string(got))
		})
	}
}

func TestPublishManifestRefusesLiveSymlinkAndKeepsItsTarget(t *testing.T) {
	// The live go.sum points at an operator file outside the workspace, the
	// case agy's #340 probe showed host go mod tidy would truncate.
	grant := realTempDir(t)
	outside := filepath.Join(realTempDir(t), "operator_file.txt")
	require.NoError(t, os.WriteFile(outside, []byte(`{"operator_secret":"do-not-clobber"}`), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(grant, "go.sum")))
	op := newPublishOp(t, grant, map[string][]byte{})
	writeTree(t, op.path("src"), map[string]string{"go.sum": "tidy output\n"})

	changed, err := op.publishManifest("go.sum")
	require.ErrorIs(t, err, errManifestConflict)
	assert.Contains(t, err.Error(), "is a symbolic link")
	assert.False(t, changed)
	got, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, `{"operator_secret":"do-not-clobber"}`, string(got))
	link, err := os.Readlink(filepath.Join(grant, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, outside, link)
}

func TestCapWriterRecordsGap(t *testing.T) {
	dir := realTempDir(t)
	w, err := newCapWriter(filepath.Join(dir, outputFile))
	require.NoError(t, err)
	w.limit = 10
	n, err := w.Write([]byte("0123456789abcdef"))
	require.NoError(t, err)
	assert.Equal(t, 16, n)
	require.NoError(t, w.close())
	got, err := os.ReadFile(filepath.Join(dir, outputFile))
	require.NoError(t, err)
	assert.Equal(t, "0123456789", string(got))
	raw, err := os.ReadFile(filepath.Join(dir, outputMetaFile))
	require.NoError(t, err)
	var meta outputMeta
	require.NoError(t, json.Unmarshal(raw, &meta))
	assert.Equal(t, outputMeta{Captured: 10, Dropped: 6}, meta)
}

func TestReadCheckOutputPages(t *testing.T) {
	workDir := realTempDir(t)
	id := strings.Repeat("ab", 16)
	opDir := filepath.Join(workDir, id)
	require.NoError(t, os.Mkdir(opDir, 0o700))
	text := strings.Repeat("x", maxCheckMessageBytes) + "tail"
	require.NoError(t, os.WriteFile(filepath.Join(opDir, outputFile), []byte(text), 0o600))
	raw, _ := json.Marshal(outputMeta{Captured: int64(len(text)), Dropped: 3})
	require.NoError(t, os.WriteFile(filepath.Join(opDir, outputMetaFile), raw, 0o600))
	c := &ControlPlane{checkWorkDir: workDir}

	first, err := c.ReadCheckOutput(context.Background(), &gensurla.ReadCheckOutputPayload{OpID: id})
	require.NoError(t, err)
	assert.Len(t, first.Text, maxCheckMessageBytes)
	assert.Equal(t, int64(3), first.DroppedBytes)
	require.NotNil(t, first.NextCursor)

	second, err := c.ReadCheckOutput(context.Background(), &gensurla.ReadCheckOutputPayload{OpID: id, Cursor: first.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, "tail", second.Text)
	assert.Nil(t, second.NextCursor)

	otherID := strings.Repeat("cd", 16)
	foreign := encodeCursor(otherID, 0)
	_, err = c.ReadCheckOutput(context.Background(), &gensurla.ReadCheckOutputPayload{OpID: id, Cursor: &foreign})
	assertServiceError(t, err, "unprocessable")

	bad := "not-base64!"
	_, err = c.ReadCheckOutput(context.Background(), &gensurla.ReadCheckOutputPayload{OpID: id, Cursor: &bad})
	assertServiceError(t, err, "unprocessable")

	_, err = c.ReadCheckOutput(context.Background(), &gensurla.ReadCheckOutputPayload{OpID: otherID})
	assertServiceError(t, err, "not_found")

	_, err = c.ReadCheckOutput(context.Background(), &gensurla.ReadCheckOutputPayload{OpID: "../../etc"})
	assertServiceError(t, err, "not_found")
}

// assertServiceError requires err to be a goa ServiceError named name.
func assertServiceError(t *testing.T, err error, name string) {
	t.Helper()
	var se *goa.ServiceError
	require.True(t, errors.As(err, &se), "want ServiceError %s, got %v", name, err)
	assert.Equal(t, name, se.Name)
}
