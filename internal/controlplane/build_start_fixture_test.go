// This file supplies deterministic working-copy stand-ins for build contracts.
package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goadesign/surla/internal/designeval"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/testwriter"
	"github.com/stretchr/testify/require"
	"golang.org/x/mod/modfile"
)

// buildCopyFixture performs no Git or candidate execution. Hooks model the
// owner's refresh/preparation boundary, including concurrent request effects.
type buildCopyFixture struct {
	folder  string
	refresh func(context.Context) (testwriter.RefreshResult, error)
	prepare func(context.Context, []designeval.StorySummary) error
}

// Project is the test writer's independently assigned project folder.
func (f *buildCopyFixture) Project() string { return f.folder }

// Refresh returns the declared postcondition or runs a race/refusal seam.
func (f *buildCopyFixture) Refresh(ctx context.Context) (testwriter.RefreshResult, error) {
	if f.refresh != nil {
		return f.refresh(ctx)
	}
	return testwriter.RefreshResult{State: testwriter.RefreshAtProjectCommit, Commit: strings.Repeat("c", 40)}, nil
}

// Prepare applies a generation outcome to the Story actually saved by the start.
func (f *buildCopyFixture) Prepare(ctx context.Context, stories []designeval.StorySummary) error {
	if f.prepare != nil {
		return f.prepare(ctx, stories)
	}
	return nil
}

// newBuildControlPlane binds the explicitly simulated team, platform checks and
// working copy used by tests of cards/assignments rather than Git or generation.
// The real source decision still reads a starter layout from temporary files.
func newBuildControlPlane(t *testing.T, opts Options) *ControlPlane {
	t.Helper()
	if opts.Team == nil {
		opts.Team = &TeamBinding{Owner: "alice@example.com", Service: &teamServiceFixture{}}
	}
	if opts.WorkspaceDir == "" {
		opts.WorkspaceDir = t.TempDir()
	}
	goMod := filepath.Join(opts.WorkspaceDir, "go.mod")
	data, err := os.ReadFile(goMod)
	if os.IsNotExist(err) {
		data = []byte("module example.com/shop\n\ngo 1.27.3\n")
		require.NoError(t, os.WriteFile(goMod, data, 0o600))
	} else {
		require.NoError(t, err)
	}
	evalMod := filepath.Join(opts.WorkspaceDir, scaffold.SuiteDir, "go.mod")
	if _, err := os.Stat(evalMod); os.IsNotExist(err) {
		require.NoError(t, os.MkdirAll(filepath.Dir(evalMod), 0o700))
		require.NoError(t, os.WriteFile(evalMod, []byte("module "+modfile.ModulePath(data)+"/eval\n\ngo 1.27.3\n"), 0o600))
	} else {
		require.NoError(t, err)
	}
	cp := newControlPlane(t, opts)
	cp.buildIdentityCheck = func(context.Context, string) error { return nil }
	cp.buildSandboxCheck = func() error { return nil }
	copy := &buildCopyFixture{folder: t.TempDir()}
	cp.buildCopyFactory = func(context.Context, string, scaffold.Layout) (buildCopy, error) {
		return copy, nil
	}
	return cp
}
