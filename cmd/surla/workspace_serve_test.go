package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/controlplane"
)

// TestDefaultOperatorRootDeterministicAndPermissionValidation rejects unavailable or unsafe storage roots.
func TestDefaultOperatorRootDeterministicAndPermissionValidation(t *testing.T) {
	root := t.TempDir()
	wsDir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	t.Chdir(wsDir)

	// 1. When the injected config root fails, configureServeWorkspace fails without falling back to TMPDIR or sibling dirs.
	runtime := testProjectRuntime()
	runtime.configRoot = func() (string, error) { return "", os.ErrNotExist }
	_, err := runtime.operatorRoot(wsDir)
	require.ErrorIs(t, err, os.ErrNotExist)

	// 2. When the injected config root points inside the bound workspace, configureServeWorkspace fails without falling back.
	runtime.configRoot = func() (string, error) { return filepath.Join(wsDir, ".config"), nil }
	_, err = runtime.operatorRoot(wsDir)
	require.ErrorContains(t, err, "must be outside the workspace")

	// 3. When <configRoot>/surla/operator already exists with permissive 0755 mode, configureServeWorkspace fails.
	cfgRoot := filepath.Join(root, "user-config")
	opRoot := filepath.Join(cfgRoot, "surla", "operator")
	require.NoError(t, os.MkdirAll(opRoot, 0o755))
	require.NoError(t, os.Chmod(opRoot, 0o755))
	runtime.configRoot = func() (string, error) { return cfgRoot, nil }
	_, err = runtime.operatorRoot(wsDir)
	require.ErrorContains(t, err, "has permissions 0755, want 0o700")

}

// TestUnusableOperatorFolderNamesStep reports an operator storage root that exists but
// that this account cannot use, as `surla` finds it before reading any project record,
// with its path and the step that repairs it.
func TestUnusableOperatorFolderNamesStep(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("the superuser can open folders of any mode")
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "folder this account cannot open", mode: 0o000},
		{name: "folder this account cannot search", mode: 0o600},
		{name: "folder others can open", mode: 0o755},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgRoot, err := abspath.Eval(t.TempDir())
			require.NoError(t, err)
			opRoot := filepath.Join(cfgRoot, "surla", "operator")
			require.NoError(t, os.MkdirAll(opRoot, 0o700))
			require.NoError(t, os.Chmod(opRoot, tc.mode))
			t.Cleanup(func() { assert.NoError(t, os.Chmod(opRoot, 0o700)) })
			t.Chdir(t.TempDir())

			_, err = testProjectRuntimeAt(cfgRoot).discover()
			require.Error(t, err)
			assert.Contains(t, err.Error(), opRoot)
			assert.Contains(t, err.Error(), "chmod 700 "+opRoot)
			assert.Contains(t, err.Error(), "run surla again")
		})
	}
}

// isolateServeWorkspace supplies a new 0700 test configuration root
// and changes the working directory to a new empty workspace directory,
// returning that workspace directory path.
func isolateServeWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cfgRoot := filepath.Join(root, "user-config")
	wsDir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(cfgRoot, 0o700))
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	t.Setenv("SURLA_TEST_USER_CONFIG_DIR", cfgRoot)
	t.Chdir(wsDir)
	return wsDir
}

// closeServeOptions closes the evaluator owned by opts
// when a test constructs serveOptions without handing opts to
// controlplane.New.
func closeServeOptions(t *testing.T, opts controlplane.Options) {
	t.Helper()
	if opts.Evaluator != nil {
		assert.NoError(t, opts.Evaluator.Close())
	}
}

// assertOutsideDirectory verifies storage exclusion without hiding filesystem errors.
func assertOutsideDirectory(t *testing.T, child, root string) {
	t.Helper()
	inside, err := abspath.Within(root, child)
	require.NoError(t, err)
	assert.False(t, inside, "%s must be outside %s", child, root)
}

// TestConfigureServeWorkspaceKeepsHistoryAcrossSpellingsOfTheWorkspace
// verifies that restarting the project server with another spelling of the same
// directories, on a case-insensitive volume, selects the same project, cards
// and runs, so saved cards are still returned.
func TestConfigureServeWorkspaceKeepsHistoryAcrossSpellingsOfTheWorkspace(t *testing.T) {
	type start struct {
		chdir   string
		project string
	}
	cases := []struct {
		name   string
		starts []start
	}{
		{name: "started inside the workspace", starts: []start{{chdir: "Workspace"}, {chdir: "workspace"}, {chdir: "WORKSPACE"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			cfgRoot := filepath.Join(root, "user-config")
			require.NoError(t, os.MkdirAll(cfgRoot, 0o700))
			require.NoError(t, os.MkdirAll(filepath.Join(root, "workspace"), 0o755))
			if _, err := os.Stat(filepath.Join(root, "WORKSPACE")); err != nil {
				t.Skip("needs a case-insensitive volume")
			}
			t.Setenv("SURLA_TEST_USER_CONFIG_DIR", cfgRoot)
			alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: controlplane.CallerUser})

			var first workspaceServeConfig
			for i, st := range tc.starts {
				t.Chdir(filepath.Join(root, st.chdir))
				if st.project != "" {
					t.Chdir(filepath.Join(root, st.project))
				}
				ws, err := configureServeWorkspace()
				require.NoError(t, err, st)
				cp, err := controlplane.New(controlplane.Options{CardsDir: ws.cardsDir, WorkspaceDir: ws.workspaceDir, RunsDir: ws.runsDir})
				require.NoError(t, err, st)
				if i == 0 {
					first = ws
					_, err = cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: "chg-1", Prompt: "Refunds up to $50 are instant.", Analysis: confirmedAnalysis()})
					require.NoError(t, err)
				} else {
					assert.Equal(t, first.workspaceDir, ws.workspaceDir, st)
					assert.Equal(t, first.cardsDir, ws.cardsDir, st)
					assert.Equal(t, first.runsDir, ws.runsDir, st)
					card, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg-1"})
					require.NoError(t, err, st)
					assert.Equal(t, "chg-1", card.ChangeID)
				}
				require.NoError(t, cp.Close())
			}
			assert.Equal(t, filepath.Join(root, "workspace"), first.workspaceDir, "the stored spelling")
		})
	}
}
