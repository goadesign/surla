package verify

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TEMPORARY DIAGNOSTIC test (branch diag/live-start-evidence, never merged).

// TestDiagMarkCountsFailuresItCannotReport checks that a timeline marker
// failure whose MARK-ERROR line also fails is counted and reported on the
// next line, and that the marker never panics or stops the caller.
func TestDiagMarkCountsFailuresItCannotReport(t *testing.T) {
	r := &runner{evidenceDir: filepath.Join(t.TempDir(), "missing")}
	closedR, closedW, err := os.Pipe()
	require.NoError(t, err)
	require.NoError(t, closedR.Close())
	stderr := os.Stderr
	t.Cleanup(func() { os.Stderr = stderr })

	// The timeline cannot be opened and stderr is broken: both lost.
	os.Stderr = closedW
	r.diagMark("start", "go-mod-download-candidate", nil)
	r.diagMark("end", "go-mod-download-candidate", nil)
	require.NoError(t, closedW.Close())
	assert.Equal(t, 2, r.diagFailed)

	// With stderr working again, the next report carries the count.
	readR, readW, err := os.Pipe()
	require.NoError(t, err)
	os.Stderr = readW
	r.diagMark("start", "build-shop", nil)
	require.NoError(t, readW.Close())
	got, err := io.ReadAll(readR)
	require.NoError(t, err)
	assert.Contains(t, string(got), "MARK-ERROR build-shop")
	assert.Contains(t, string(got), "unreported_failures=2")
}
