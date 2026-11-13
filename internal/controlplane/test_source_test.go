// This file tests where a project's tests come from.
package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectTestSourceHasThreeAnswers binds control planes as the project
// server does, with and without stored verification settings, to workspaces
// with and without the layout `surla init` makes, including layouts an agent
// could fake with links or special files, and checks each answer.
func TestProjectTestSourceHasThreeAnswers(t *testing.T) {
	goMod := "module example.com/shop\n\ngo 1.26\n"
	testGoMod := "module example.com/shop/eval\n\ngo 1.26\n"
	starter := map[string]string{"go.mod": goMod, "eval/go.mod": testGoMod}
	// padded returns goMod followed by a comment that makes it size bytes.
	padded := func(size int) string {
		return goMod + "//" + strings.Repeat("x", size-len(goMod)-3) + "\n"
	}
	// resolve stands in for the resolver the project server binds when the
	// project has no stored verification settings.
	resolve := func(context.Context, string, string) (*VerificationConfig, error) { return nil, nil }
	cases := []struct {
		name      string
		files     map[string]string
		prepare   func(t *testing.T, ws string)
		stored    bool
		resolver  bool
		startup   bool
		unbound   bool
		want      testSource
		wantError string
	}{
		{name: "stored settings", stored: true, unbound: true, want: testSourceStoredEvaluator},
		{name: "stored settings in a starter project", files: starter, stored: true, want: testSourceStoredEvaluator},
		{name: "starter project, no stored settings", files: starter, resolver: true, startup: true, want: testSourceStarterLayout},
		{name: "starter project, nothing configured", files: starter, want: testSourceStarterLayout},
		{name: "no workspace", resolver: true, unbound: true, want: testSourceNone},
		{name: "no go.mod", files: map[string]string{"eval/go.mod": testGoMod}, resolver: true, want: testSourceNone},
		{name: "no test module", files: map[string]string{"go.mod": goMod}, resolver: true, want: testSourceNone},
		{name: "test module of another service", files: map[string]string{
			"go.mod": goMod, "eval/go.mod": "module example.com/orders/eval\n\ngo 1.26\n",
		}, resolver: true, want: testSourceNone},
		{name: "module path without a service name", files: map[string]string{
			"go.mod": "module example.com/Shop\n\ngo 1.26\n", "eval/go.mod": "module example.com/Shop/eval\n\ngo 1.26\n",
		}, resolver: true, want: testSourceNone},
		{name: "go.mod at its size bound", files: map[string]string{"go.mod": padded(maxLayoutFileBytes), "eval/go.mod": testGoMod}, want: testSourceStarterLayout},
		{name: "go.mod over its size bound", files: map[string]string{"go.mod": padded(maxLayoutFileBytes + 1), "eval/go.mod": testGoMod}, want: testSourceNone},
		{name: "link at go.mod", files: map[string]string{"real.mod": goMod, "eval/go.mod": testGoMod}, prepare: func(t *testing.T, ws string) {
			require.NoError(t, os.Symlink("real.mod", filepath.Join(ws, "go.mod")))
		}, want: testSourceNone},
		{name: "link at go.mod leaving the workspace", files: map[string]string{"eval/go.mod": testGoMod}, prepare: func(t *testing.T, ws string) {
			outside := filepath.Join(t.TempDir(), "go.mod")
			require.NoError(t, os.WriteFile(outside, []byte(goMod), 0o600))
			require.NoError(t, os.Symlink(outside, filepath.Join(ws, "go.mod")))
		}, want: testSourceNone},
		{name: "link at eval/go.mod", files: map[string]string{"go.mod": goMod, "eval/real.mod": testGoMod}, prepare: func(t *testing.T, ws string) {
			require.NoError(t, os.Symlink("real.mod", filepath.Join(ws, "eval", "go.mod")))
		}, want: testSourceNone},
		{name: "link at the eval folder", files: map[string]string{"go.mod": goMod, "tests/go.mod": testGoMod}, prepare: func(t *testing.T, ws string) {
			require.NoError(t, os.Symlink("tests", filepath.Join(ws, "eval")))
		}, want: testSourceNone},
		{name: "folder in place of go.mod", files: map[string]string{"eval/go.mod": testGoMod, "go.mod/keep": ""}, want: testSourceNone},
		{name: "FIFO at go.mod", files: map[string]string{"eval/go.mod": testGoMod}, prepare: func(t *testing.T, ws string) {
			require.NoError(t, syscall.Mkfifo(filepath.Join(ws, "go.mod"), 0o600))
		}, want: testSourceNone},
		{name: "FIFO at eval/go.mod", files: map[string]string{"go.mod": goMod, "eval/keep": ""}, prepare: func(t *testing.T, ws string) {
			require.NoError(t, syscall.Mkfifo(filepath.Join(ws, "eval", "go.mod"), 0o600))
		}, want: testSourceNone},
		{name: "unreadable go.mod", files: starter, prepare: func(t *testing.T, ws string) {
			goMod := filepath.Join(ws, "go.mod")
			require.NoError(t, os.Chmod(goMod, 0))
			t.Cleanup(func() { assert.NoError(t, os.Chmod(goMod, 0o600)) })
		}, wantError: "read the test layout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{}
			if !tc.unbound {
				opts.WorkspaceDir = t.TempDir()
				writeWorkspaceFiles(t, opts.WorkspaceDir, tc.files)
			}
			if tc.stored || tc.startup {
				opts.Verification = &VerificationConfig{RunsDir: t.TempDir()}
			}
			if tc.resolver {
				opts.ResolveVerification = resolve
			}
			cp := newControlPlane(t, opts)
			if tc.prepare != nil {
				tc.prepare(t, opts.WorkspaceDir)
			}
			got, err := answerWithin(t, cp, t.Context())
			if tc.wantError != "" {
				assert.ErrorContains(t, err, tc.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want == testSourceStoredEvaluator, cp.storedEvaluator(), "reads of a build use the same first answer")
		})
	}
}

// TestProjectTestSourceFailsRatherThanAnswer checks the two failures that are
// not answers: a workspace replaced after start and a context that ended.
func TestProjectTestSourceFailsRatherThanAnswer(t *testing.T) {
	starter := map[string]string{
		"go.mod":      "module example.com/shop\n\ngo 1.26\n",
		"eval/go.mod": "module example.com/shop/eval\n\ngo 1.26\n",
	}
	t.Run("replaced workspace", func(t *testing.T) {
		ws := t.TempDir()
		writeWorkspaceFiles(t, ws, starter)
		cp := newControlPlane(t, Options{WorkspaceDir: ws})
		require.NoError(t, os.RemoveAll(ws))
		require.NoError(t, os.Mkdir(ws, 0o700))
		writeWorkspaceFiles(t, ws, starter)
		_, err := answerWithin(t, cp, t.Context())
		assert.ErrorContains(t, err, "replaced or removed")
	})
	t.Run("ended context", func(t *testing.T) {
		ws := t.TempDir()
		writeWorkspaceFiles(t, ws, starter)
		cp := newControlPlane(t, Options{WorkspaceDir: ws})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := answerWithin(t, cp, ctx)
		assert.ErrorIs(t, err, context.Canceled)
	})
}

// answerWithin calls projectTestSource and fails the test if it has not
// answered within ten seconds, so a read that blocks (on a FIFO, say) fails
// the test instead of stopping the run.
func answerWithin(t *testing.T, cp *ControlPlane, ctx context.Context) (testSource, error) {
	t.Helper()
	type answer struct {
		source testSource
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		source, err := cp.projectTestSource(ctx)
		done <- answer{source, err}
	}()
	select {
	case got := <-done:
		return got.source, got.err
	case <-time.After(10 * time.Second):
		require.FailNow(t, "projectTestSource did not answer within ten seconds")
		return "", nil
	}
}
