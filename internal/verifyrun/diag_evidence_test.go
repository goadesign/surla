package verifyrun

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TEMPORARY DIAGNOSTIC tests (branch diag/live-start-evidence, never merged).

func TestDiagLiveEvidenceNamesEveryFileAndEarnsComplete(t *testing.T) {
	cases := []struct {
		name string
		// prepare lays out the live directory.
		prepare func(t *testing.T, dir string)
		// out is the writer, or nil for a buffer.
		out io.Writer
		// want are fragments the output must hold.
		want []string
		// complete is the expected END value.
		complete bool
	}{
		{
			name: "during download: build logs missing, still complete",
			prepare: func(t *testing.T, dir string) {
				write(t, dir, "executor.log", "MARK start prepare\n")
				write(t, dir, "work/evidence/timeline.log", "MARK start go-mod-download-candidate\n")
			},
			want: []string{
				"ENTRY path=work/evidence/build-shop.log missing",
				"ENTRY path=work/evidence/build-suite.log missing",
				"ENTRY path=work/evidence/timeline.log size=",
			},
			complete: true,
		},
		{
			name: "unreadable file is an error",
			prepare: func(t *testing.T, dir string) {
				write(t, dir, "executor.log", "x")
				require.NoError(t, os.Chmod(filepath.Join(dir, "executor.log"), 0))
				t.Cleanup(func() { require.NoError(t, os.Chmod(filepath.Join(dir, "executor.log"), 0o600)) })
			},
			want:     []string{"ENTRY path=executor.log read-error="},
			complete: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.prepare(t, dir)
			var buf bytes.Buffer
			diagLiveEvidence(&buf, dir, []string{"shop"})
			out := buf.String()
			for _, w := range tc.want {
				assert.Contains(t, out, w)
			}
			if tc.complete {
				assert.Contains(t, out, "LIVE EVIDENCE END complete=true errors=0")
			} else {
				assert.Contains(t, out, "LIVE EVIDENCE END complete=false")
			}
		})
	}
}

func TestDiagLiveEvidenceCountsFailedWrites(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "executor.log", "x")
	w := &failingWriter{failAfter: 1}
	diagLiveEvidence(w, dir, nil)
	assert.Contains(t, w.String(), "LIVE EVIDENCE BEGIN")
	assert.NotContains(t, w.String(), "complete=true", "a failed line write never reads as complete")
	assert.NotContains(t, w.String(), "LIVE EVIDENCE END", "here the END line itself also failed, so its absence shows the capture is incomplete")
}

func TestDiagTailFromKeepsOnlyBytesRead(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		size      int64
		wantTail  string
		wantShort bool
	}{
		{name: "whole small file", content: "abc", size: 3, wantTail: "abc"},
		{name: "file shrank after its size was taken", content: "ab", size: 5, wantTail: "ab", wantShort: true},
		{name: "longer than the bound keeps the end", content: strings.Repeat("a", diagTail) + "END", size: int64(diagTail + 3), wantTail: strings.Repeat("a", diagTail-3) + "END"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tail, short, err := diagTailFrom(strings.NewReader(tc.content), tc.size)
			require.NoError(t, err)
			assert.Equal(t, tc.wantTail, string(tail), "no padding")
			assert.Equal(t, tc.wantShort, short)
		})
	}
}

// write creates name, a slash-separated path under dir, with content.
func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o700))
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
}

// failingWriter accepts failAfter writes and fails every later one. It has
// only a Write method, so io.WriteString goes through it.
type failingWriter struct {
	// got holds what was written.
	got bytes.Buffer
	// failAfter is how many writes succeed.
	failAfter int
}

// Write fails once failAfter writes have succeeded.
func (w *failingWriter) Write(p []byte) (int, error) {
	if w.failAfter <= 0 {
		return 0, errors.New("write failed")
	}
	w.failAfter--
	return w.got.Write(p)
}

// String returns what was written.
func (w *failingWriter) String() string {
	return w.got.String()
}
