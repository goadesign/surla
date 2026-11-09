package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	goa "goa.design/goa/v3/pkg"
)

// newPublishOp returns an operation that holds the live grant open and
// whose copy is src, with before holding the manifest bytes read during the
// copy.
func newPublishOp(t *testing.T, grant string, before map[string][]byte) *checkOperation {
	t.Helper()
	dir := realTempDir(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "src"), 0o700))
	fd, err := syscall.Open(grant, openDirFlags, 0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, syscall.Close(fd)) })
	return &checkOperation{
		dir:          dir,
		grantRoot:    grant,
		grantFD:      fd,
		workspaceRel: ".",
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

func TestPublishManifestWritesThroughTheHeldGrant(t *testing.T) {
	// The workspace is renamed away and a new directory takes its path while
	// go mod tidy runs: the write lands in the verified tree, not the
	// replacement.
	parent := realTempDir(t)
	grant := filepath.Join(parent, "shop")
	writeTree(t, grant, map[string]string{"go.sum": "old\n"})
	op := newPublishOp(t, grant, map[string][]byte{"go.sum": []byte("old\n")})
	writeTree(t, op.path("src"), map[string]string{"go.sum": "new\n"})
	moved := filepath.Join(parent, "shop-moved")
	require.NoError(t, os.Rename(grant, moved))
	writeTree(t, grant, map[string]string{"go.sum": "old\n"})

	changed, err := op.publishManifest("go.sum")
	require.NoError(t, err)
	assert.True(t, changed)
	got, err := os.ReadFile(filepath.Join(moved, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(got))
	got, err = os.ReadFile(filepath.Join(grant, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, "old\n", string(got), "the replacement directory is untouched")
}

func TestPublishManifestsReportsAWrittenFileWhoseSyncFailed(t *testing.T) {
	grant := realTempDir(t)
	writeTree(t, grant, map[string]string{"go.mod": "module example.com/shop\n", "go.sum": "old\n"})
	op := newPublishOp(t, grant, map[string][]byte{"go.mod": []byte("module example.com/shop\n"), "go.sum": []byte("old\n")})
	writeTree(t, op.path("src"), map[string]string{"go.mod": "module example.com/shop\n", "go.sum": "new\n"})
	prior := fsyncDir
	fsyncDir = func(int) error { return syscall.EIO }
	t.Cleanup(func() { fsyncDir = prior })

	written, notes := op.publishManifests(context.Background(), nil)
	assert.Equal(t, []string{"go.sum"}, written, "a renamed file is reported written")
	require.Len(t, notes, 1)
	assert.Contains(t, notes[0], "go.sum was written, but making the write durable failed")
	got, err := os.ReadFile(filepath.Join(grant, "go.sum"))
	require.NoError(t, err)
	assert.Equal(t, "new\n", string(got))
}

func TestCapWriterReportsWriteFailure(t *testing.T) {
	dir := realTempDir(t)
	w, err := newCapWriter(filepath.Join(dir, outputFile))
	require.NoError(t, err)
	_, err = w.Write([]byte("0123"))
	require.NoError(t, err)
	// The disk fails: the stream keeps draining, every later byte counts
	// as dropped, and close reports the failure.
	require.NoError(t, w.f.Close())
	n, err := w.Write([]byte("456"))
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	n, err = w.Write([]byte("78"))
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	err = w.close()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "capture check output after byte 4 (5 bytes not captured)")
	raw, err := os.ReadFile(filepath.Join(dir, outputMetaFile))
	require.NoError(t, err)
	var meta outputMeta
	require.NoError(t, json.Unmarshal(raw, &meta))
	assert.Equal(t, outputMeta{Captured: 4, Dropped: 5}, meta)
}

func TestResultMessagesAreBoundedAsAWhole(t *testing.T) {
	newOp := func(t *testing.T, output string, dropped int64) *checkOperation {
		t.Helper()
		dir := realTempDir(t)
		w, err := newCapWriter(filepath.Join(dir, outputFile))
		require.NoError(t, err)
		_, err = w.Write([]byte(output))
		require.NoError(t, err)
		w.dropped = dropped
		require.NoError(t, w.close())
		return &checkOperation{dir: dir, out: w}
	}
	manyModules := make([]string, 3000)
	for i := range manyModules {
		manyModules[i] = fmt.Sprintf("services/module-%04d", i)
	}
	output := strings.Repeat("x", 40<<10) + "--- FAIL: TestLast"

	t.Run("check passes with many modules", func(t *testing.T) {
		op := newOp(t, "", 0)
		op.copy.modules = manyModules
		msg := op.message(nil)
		assert.LessOrEqual(t, len(msg), maxCheckMessageBytes)
		assert.True(t, strings.HasPrefix(msg, "tested 3000 module(s): services/module-0000"))
		assert.Contains(t, msg, "read_check_output(op_id) has the full list")
		assert.True(t, strings.HasSuffix(msg, ". All passed."))
	})
	t.Run("check fails with many modules and long output", func(t *testing.T) {
		op := newOp(t, output, 7)
		op.copy.modules = manyModules
		f := moduleOutcome{dir: "services/module-2999", step: "test", exitCode: 1, start: 0, end: int64(len(output))}
		msg := op.message([]moduleOutcome{f, f})
		assert.LessOrEqual(t, len(msg), maxCheckMessageBytes)
		assert.Contains(t, msg, "services/module-2999 failed: go test exited 1. 1 other module step(s) also failed.")
		assert.Contains(t, msg, "--- FAIL: TestLast")
		assert.True(t, strings.HasSuffix(msg, "Output after byte "+fmt.Sprint(len(output))+" was not captured: 7 bytes dropped."))
	})
	t.Run("update with many refused files", func(t *testing.T) {
		op := newOp(t, "", 0)
		written := []string{"go.mod", "go.sum"}
		var notes []string
		for _, m := range manyModules {
			notes = append(notes, m+"/go.sum was not written: manifest conflict: go.sum changed during update_dependencies; call again")
		}
		msg := op.updateMessage(written, notes, " The files were written, but finishing the operation failed: boom.", nil)
		assert.LessOrEqual(t, len(msg), maxCheckMessageBytes)
		assert.True(t, strings.HasPrefix(msg, "Wrote 2 file(s): go.mod, go.sum."))
		assert.Contains(t, msg, "read_check_output(op_id) has the full list")
		assert.True(t, strings.HasSuffix(msg, "finishing the operation failed: boom."))
	})
}

func TestCheckToolsRequireAnAuthenticatedPrincipal(t *testing.T) {
	workDir := realTempDir(t)
	c := &ControlPlane{checkWorkDir: workDir}
	_, err := c.CheckProject(context.Background())
	require.EqualError(t, err, "authenticated principal is required")
	_, err = c.UpdateDependencies(context.Background())
	require.EqualError(t, err, "authenticated principal is required")
	id := strings.Repeat("ab", 16)
	writeTree(t, filepath.Join(workDir, id), map[string]string{outputFile: "secret test output", outputMetaFile: `{"captured":18,"dropped":0}`})
	page, err := c.ReadCheckOutput(context.Background(), &gensurla.ReadCheckOutputPayload{OpID: id})
	require.EqualError(t, err, "authenticated principal is required")
	assert.Nil(t, page)
	entries, err := os.ReadDir(workDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no operation directory was created")
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
	ctx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	first, err := c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: id})
	require.NoError(t, err)
	assert.Len(t, first.Text, maxCheckMessageBytes)
	assert.Equal(t, int64(3), first.DroppedBytes)
	require.NotNil(t, first.NextCursor)

	second, err := c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: id, Cursor: first.NextCursor})
	require.NoError(t, err)
	assert.Equal(t, "tail", second.Text)
	assert.Nil(t, second.NextCursor)

	otherID := strings.Repeat("cd", 16)
	foreign := encodeCursor(otherID, 0)
	_, err = c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: id, Cursor: &foreign})
	assertServiceError(t, err, "unprocessable")

	bad := "not-base64!"
	_, err = c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: id, Cursor: &bad})
	assertServiceError(t, err, "unprocessable")

	_, err = c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: otherID})
	assertServiceError(t, err, "not_found")

	_, err = c.ReadCheckOutput(ctx, &gensurla.ReadCheckOutputPayload{OpID: "../../etc"})
	assertServiceError(t, err, "not_found")
}

// assertServiceError requires err to be a goa ServiceError named name.
func assertServiceError(t *testing.T, err error, name string) {
	t.Helper()
	var se *goa.ServiceError
	require.True(t, errors.As(err, &se), "want ServiceError %s, got %v", name, err)
	assert.Equal(t, name, se.Name)
}
