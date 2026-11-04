package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPreviewAndDiffRemoveTheirWorkspaceWhenEvaluationFails checks that
// `surla preview` and `surla diff` remove the design evaluator workspace
// they created, on a return path where the evaluation fails.
func TestPreviewAndDiffRemoveTheirWorkspaceWhenEvaluationFails(t *testing.T) {
	cases := []struct {
		name string
		run  func(dir string) error
	}{
		{name: "preview", run: func(dir string) error { return runPreview([]string{"-dir", dir}, io.Discard) }},
		{name: "diff", run: func(dir string) error { return runDiff([]string{"--old-dir", dir, "--new-dir", dir}) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			module := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(module, "go.mod"), []byte("module example.com/design\n\ngo 1.24\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(module, "go.sum"), nil, 0o600))
			require.NoError(t, os.MkdirAll(filepath.Join(module, "design"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(module, "design", "design.go"), []byte("package design\n"), 0o600))
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			// The go on PATH fails, so the evaluation fails right after the
			// command's evaluator created its workspace.
			bin := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nexit 1\n"), 0o700))
			t.Setenv("PATH", bin+":/usr/bin:/bin")

			err := tc.run(module)
			require.ErrorContains(t, err, "resolve go env")
			left, globErr := filepath.Glob(filepath.Join(tmp, "surla-designeval-*"))
			require.NoError(t, globErr)
			assert.Empty(t, left, "the command removed its evaluator workspace before returning")
		})
	}
}
