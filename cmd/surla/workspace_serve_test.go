package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goadesign/surla/internal/controlplane"
)

// TestConfigureServeWorkspaceSeparatesOperatorStateAndImportsLegacyWorkspace verifies that
// configureServeWorkspace places operator state outside the workspace and imports legacy
// in-workspace .surla/ and reports/ directories once when -import-legacy-workspace is set.
func TestConfigureServeWorkspaceSeparatesOperatorStateAndImportsLegacyWorkspace(t *testing.T) {
	root := t.TempDir()
	cfgRoot := filepath.Join(root, "user-config")
	wsDir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(cfgRoot, 0o700))
	require.NoError(t, os.MkdirAll(wsDir, 0o755))

	priorConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return cfgRoot, nil }
	t.Cleanup(func() { userConfigDir = priorConfigDir })

	t.Chdir(wsDir)

	// 1. Rejects explicit -cards-dir inside the bound workspace.
	_, err := configureServeWorkspace(serveConfig{
		cardsDir:         filepath.Join(wsDir, ".surla", "cards"),
		cardsDirExplicit: true,
	})
	require.ErrorContains(t, err, "must be outside the workspace")

	// 2. Default zero-flag startup in an empty workspace selects protected operator storage outside wsDir.
	wsCfg, err := configureServeWorkspace(serveConfig{})
	require.NoError(t, err)
	require.NotNil(t, wsCfg.closeProjectLock)
	assert.False(t, isWithinOrEqualDir(wsCfg.cardsDir, wsCfg.workspaceDir))
	assert.False(t, isWithinOrEqualDir(wsCfg.runsDir, wsCfg.workspaceDir))
	assert.False(t, isWithinOrEqualDir(wsCfg.initStageDir, wsCfg.workspaceDir))
	assert.False(t, isWithinOrEqualDir(wsCfg.evalWorkDir, wsCfg.workspaceDir))
	require.NoError(t, wsCfg.closeProjectLock())

	// 3. Seed legacy workspace .surla/cards, .surla/runs, and reports/ and verify -import-legacy-workspace.
	legacyCardsDir := filepath.Join(wsDir, ".surla", "cards")
	legacyRunsDir := filepath.Join(wsDir, ".surla", "runs")
	legacyReportsDir := filepath.Join(wsDir, "reports")
	require.NoError(t, os.MkdirAll(legacyCardsDir, 0o700))
	require.NoError(t, os.MkdirAll(legacyRunsDir, 0o700))
	require.NoError(t, os.MkdirAll(legacyReportsDir, 0o700))

	cardDigest := strings.Repeat("a", 64) + ".json"
	cardRaw := []byte(`{"card":{"ChangeID":"CHG-1"}}`)
	require.NoError(t, os.WriteFile(filepath.Join(legacyCardsDir, cardDigest), cardRaw, 0o600))

	reportDigest := strings.Repeat("b", 64) + ".json"
	reportRaw := []byte(`{"passed":true}`)
	require.NoError(t, os.WriteFile(filepath.Join(legacyReportsDir, reportDigest), reportRaw, 0o600))

	// Before import, hasUnimportedLegacyWorkspaceNotice reports true.
	assert.True(t, hasUnimportedLegacyWorkspaceNotice(controlplane.Options{
		WorkspaceDir: wsCfg.workspaceDir,
		CardsDir:     wsCfg.cardsDir,
		RunsDir:      wsCfg.runsDir,
	}))

	// Run one-time -import-legacy-workspace cutover.
	importedCfg, err := configureServeWorkspace(serveConfig{
		importLegacyWorkspace: true,
	})
	require.NoError(t, err)
	require.NotNil(t, importedCfg.closeProjectLock)
	assert.Contains(t, filepath.ToSlash(importedCfg.cardsDir), "/imported/cards-")
	assert.Contains(t, filepath.ToSlash(importedCfg.runsDir), "/imported/runs")
	require.NoError(t, importedCfg.closeProjectLock())

	// Copied card and report exist in the imported store, and legacy originals are untouched.
	copiedCard, err := os.ReadFile(filepath.Join(importedCfg.cardsDir, cardDigest))
	require.NoError(t, err)
	assert.Equal(t, cardRaw, copiedCard)
	origCard, err := os.ReadFile(filepath.Join(legacyCardsDir, cardDigest))
	require.NoError(t, err)
	assert.Equal(t, cardRaw, origCard)

	// After import, normal startup without -import-legacy-workspace uses the imported stores and suppresses the notice.
	subsequentCfg, err := configureServeWorkspace(serveConfig{})
	require.NoError(t, err)
	require.NotNil(t, subsequentCfg.closeProjectLock)
	assert.Equal(t, importedCfg.cardsDir, subsequentCfg.cardsDir)
	assert.Equal(t, importedCfg.runsDir, subsequentCfg.runsDir)
	assert.False(t, hasUnimportedLegacyWorkspaceNotice(controlplane.Options{
		WorkspaceDir: subsequentCfg.workspaceDir,
		CardsDir:     subsequentCfg.cardsDir,
		RunsDir:      subsequentCfg.runsDir,
	}))
	require.NoError(t, subsequentCfg.closeProjectLock())

	// Running -import-legacy-workspace a second time fails cleanly.
	_, err = configureServeWorkspace(serveConfig{
		importLegacyWorkspace: true,
	})
	require.ErrorContains(t, err, "legacy workspace import has already been completed")
}
