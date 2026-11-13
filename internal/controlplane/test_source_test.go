// This file tests where a project's tests come from.
package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectTestSourceHasThreeAnswers binds control planes as the project
// server does, with and without stored verification settings, to workspaces
// with and without the layout `surla init` makes, and checks each answer.
func TestProjectTestSourceHasThreeAnswers(t *testing.T) {
	starter := map[string]string{
		"go.mod":      "module example.com/shop\n\ngo 1.26\n",
		"eval/go.mod": "module example.com/shop/eval\n\ngo 1.26\n",
	}
	// resolve stands in for the resolver the project server binds when the
	// project has no stored verification settings.
	resolve := func(context.Context, string, string) (*VerificationConfig, error) { return nil, nil }
	cases := []struct {
		name      string
		files     map[string]string
		stored    bool
		resolver  bool
		startup   bool
		unbound   bool
		want      testSource
		wantError bool
	}{
		{name: "stored settings", stored: true, unbound: true, want: testSourceStoredEvaluator},
		{name: "stored settings in a starter project", files: starter, stored: true, want: testSourceStoredEvaluator},
		{name: "starter project, no stored settings", files: starter, resolver: true, startup: true, want: testSourceStarterLayout},
		{name: "starter project, nothing configured", files: starter, want: testSourceStarterLayout},
		{name: "no workspace", resolver: true, unbound: true, want: testSourceNone},
		{name: "no go.mod", files: map[string]string{"eval/go.mod": starter["eval/go.mod"]}, resolver: true, want: testSourceNone},
		{name: "no test module", files: map[string]string{"go.mod": starter["go.mod"]}, resolver: true, want: testSourceNone},
		{name: "test module of another service", files: map[string]string{
			"go.mod": starter["go.mod"], "eval/go.mod": "module example.com/orders/eval\n\ngo 1.26\n",
		}, resolver: true, want: testSourceNone},
		{name: "module path without a service name", files: map[string]string{
			"go.mod": "module example.com/Shop\n\ngo 1.26\n", "eval/go.mod": "module example.com/Shop/eval\n\ngo 1.26\n",
		}, resolver: true, want: testSourceNone},
		{name: "unreadable go.mod", files: starter, resolver: true, wantError: true},
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
			if tc.wantError {
				goMod := filepath.Join(opts.WorkspaceDir, "go.mod")
				require.NoError(t, os.Chmod(goMod, 0))
				t.Cleanup(func() { assert.NoError(t, os.Chmod(goMod, 0o600)) })
			}
			got, err := cp.projectTestSource()
			if tc.wantError {
				assert.ErrorContains(t, err, "read the test layout")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want == testSourceStoredEvaluator, cp.storedEvaluator(), "reads of a build use the same first answer")
		})
	}
}

// TestProjectTestSourceRefusesAReplacedWorkspace replaces the bound workspace
// after start and checks that the answer is an error, not "neither".
func TestProjectTestSourceRefusesAReplacedWorkspace(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFiles(t, ws, map[string]string{
		"go.mod":      "module example.com/shop\n\ngo 1.26\n",
		"eval/go.mod": "module example.com/shop/eval\n\ngo 1.26\n",
	})
	cp := newControlPlane(t, Options{WorkspaceDir: ws})
	require.NoError(t, os.RemoveAll(ws))
	require.NoError(t, os.Mkdir(ws, 0o700))
	_, err := cp.projectTestSource()
	assert.ErrorContains(t, err, "replaced or removed")
}
