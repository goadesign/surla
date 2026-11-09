// This file checks startup and the one-time copy of saved legacy Change Cards.
package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
)

// TestWorkspaceUpgradeRequiresImportBeforeAcceptingNewWork checks that an actual
// saved card stops default startup, then remains readable by the same owner with
// the same ID and revised content after the explicit legacy copy and a restart.
func TestWorkspaceUpgradeRequiresImportBeforeAcceptingNewWork(t *testing.T) {
	workspace := isolateServeWorkspace(t)
	t.Setenv("HOME", t.TempDir())
	saved := saveWorkspaceUpgradeCard(t, workspace)
	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{
		ID: "alice@example.com",
	})
	bob := controlplane.WithPrincipal(context.Background(), controlplane.Principal{
		ID: "bob@example.com",
	})

	// Release an unexpectedly successful startup so the recovery path can still
	// be exercised and cannot fail merely because this test kept a lock open.
	unexpected, startupErr := configureServeWorkspace(serveConfig{})
	if unexpected.closeProjectLock != nil {
		require.NoError(t, unexpected.closeProjectLock())
	}
	assert.ErrorContains(t, startupErr, "-import-legacy-workspace",
		"default startup must stop before accepting work into a new, empty card store")

	imported, err := configureServeWorkspace(serveConfig{importLegacyWorkspace: true})
	require.NoError(t, err)
	require.NotNil(t, imported.closeProjectLock)
	require.NoError(t, imported.closeProjectLock())

	// Startup must select the completed copy without needing the import flag
	// again, even though the original card still exists in the workspace.
	reopened, err := configureServeWorkspace(serveConfig{})
	require.NoError(t, err)
	assert.Equal(t, imported.cardsDir, reopened.cardsDir)
	assert.False(t, isWithinOrEqualDir(reopened.cardsDir, reopened.workspaceDir))
	cp, err := controlplane.New(controlplane.Options{
		CardsDir:         reopened.cardsDir,
		WorkspaceDir:     reopened.workspaceDir,
		CloseProjectLock: reopened.closeProjectLock,
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })

	got, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: saved.ChangeID})
	require.NoError(t, err)
	assert.Equal(t, saved, got, "the copy must preserve the complete saved revision")
	_, err = cp.GetChange(bob, &gensurla.GetChangePayload{ChangeID: saved.ChangeID})
	assert.ErrorContains(t, err, "is not the owner")
}

// TestWorkspaceUpgradeKeepsExplicitIndependentCards checks that choosing an
// outside cards directory does not require copying the unselected legacy cards.
func TestWorkspaceUpgradeKeepsExplicitIndependentCards(t *testing.T) {
	workspace := isolateServeWorkspace(t)
	t.Setenv("HOME", t.TempDir())
	saveWorkspaceUpgradeCard(t, workspace)
	independent := t.TempDir()
	want, err := filepath.EvalSymlinks(independent)
	require.NoError(t, err)

	configured, err := configureServeWorkspace(serveConfig{
		cardsDir:         independent,
		cardsDirExplicit: true,
	})
	require.NoError(t, err)
	require.NotNil(t, configured.closeProjectLock)
	t.Cleanup(func() { assert.NoError(t, configured.closeProjectLock()) })
	assert.Equal(t, want, configured.cardsDir)
}

// TestWorkspaceUpgradeIgnoresUnrelatedFiles checks that directory names alone
// do not require a legacy copy when they contain no saved durable records.
func TestWorkspaceUpgradeIgnoresUnrelatedFiles(t *testing.T) {
	workspace := isolateServeWorkspace(t)
	t.Setenv("HOME", t.TempDir())
	for _, dir := range []string{
		filepath.Join(workspace, ".surla", "cards"),
		filepath.Join(workspace, ".surla", "runs"),
		filepath.Join(workspace, "reports"),
	} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "README.txt"), []byte("notes\n"), 0o600))
	}

	configured, err := configureServeWorkspace(serveConfig{})
	require.NoError(t, err)
	require.NotNil(t, configured.closeProjectLock)
	t.Cleanup(func() { assert.NoError(t, configured.closeProjectLock()) })
	assert.False(t, isWithinOrEqualDir(configured.cardsDir, configured.workspaceDir))
}

// saveWorkspaceUpgradeCard saves and refines a confirmed card through the public
// control-plane API using the old implicit CWD/.surla/cards store. Leaving
// WorkspaceDir unset reproduces the old layout without encoding owned files.
func saveWorkspaceUpgradeCard(t *testing.T, workspace string) *gensurla.ChangeCardResult {
	t.Helper()
	cp, err := controlplane.New(controlplane.Options{
		CardsDir: filepath.Join(workspace, ".surla", "cards"),
		Analyzer: confirmedAnalyzer{},
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{
		ID: "alice@example.com",
	})
	proposed, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID: "chg-workspace-upgrade",
		Prompt:   "Refunds up to $50 are instant.",
	})
	require.NoError(t, err)
	require.NotEmpty(t, proposed.Examples)
	refinedExample := *proposed.Examples[0]
	refinedExample.ExpectedOutcome = "Store credit within one day"
	saved, err := cp.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{
		ChangeID:        proposed.ChangeID,
		RequestRevision: proposed.RequestRevision,
		Answers:         map[string]string{},
		Examples:        []*gensurla.ExampleItem{&refinedExample},
	})
	require.NoError(t, err)
	require.NotEqual(t, proposed.RequestRevision, saved.RequestRevision)
	require.NoError(t, cp.Close())
	return saved
}
