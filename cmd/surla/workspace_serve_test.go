package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verify"
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

// TestImportLegacyWorkspaceSnapshotsConfiguredPromotionReportsAndIsolatesRoots verifies
// that -import-legacy-workspace prevalidates -promotion-config before publication,
// snapshots standalone CLI reports in a custom workspace report_dir with no CP4 runs,
// preserves valid/corrupt/missing report outcomes and existing approvals, and never
// falls back from a configured report_dir to the default reports directory.
func TestImportLegacyWorkspaceSnapshotsConfiguredPromotionReportsAndIsolatesRoots(t *testing.T) {
	root := t.TempDir()
	cfgRoot := filepath.Join(root, "user-config")
	wsDir := filepath.Join(root, "workspace")
	releaseDir := filepath.Join(root, "release")
	require.NoError(t, os.MkdirAll(cfgRoot, 0o700))
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	require.NoError(t, os.MkdirAll(releaseDir, 0o700))

	priorConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return cfgRoot, nil }
	t.Cleanup(func() { userConfigDir = priorConfigDir })
	t.Chdir(wsDir)

	commit := strings.Repeat("c", 40)
	publish(t, releaseDir, commit, map[string]string{
		"orders":        "api v1",
		"orders-worker": "worker v1",
	})
	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com"})

	legacyCardsDir := filepath.Join(wsDir, ".surla", "cards")
	defaultReportsDir := filepath.Join(wsDir, "reports")
	customReportsDir := filepath.Join(wsDir, "custom-reports")
	require.NoError(t, os.MkdirAll(legacyCardsDir, 0o700))
	require.NoError(t, os.MkdirAll(defaultReportsDir, 0o700))
	require.NoError(t, os.MkdirAll(customReportsDir, 0o700))

	// Create two confirmed cards (chg-existing and chg-new) in a temporary store and copy them into legacyCardsDir.
	seedCardsDir := filepath.Join(root, "seed-cards")
	seedCP, err := controlplane.New(controlplane.Options{
		CardsDir: seedCardsDir,
		Analyzer: confirmedAnalyzer{},
	})
	require.NoError(t, err)
	cardExisting, err := seedCP.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID: "chg-existing",
		Prompt:   "Refunds up to $50 are instant.",
	})
	require.NoError(t, err)
	cardNew, err := seedCP.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID: "chg-new",
		Prompt:   "Refunds up to $50 are instant.",
	})
	require.NoError(t, err)
	require.NoError(t, seedCP.Close())

	// Standalone CLI report in customReportsDir (with NO .surla/runs entry!) for chg-existing and chg-new.
	repExisting := saveReport(t, verify.Binding{
		CandidateCommit: commit,
		RequestRevision: cardExisting.RequestRevision,
		SuiteID:         "orders",
		SuiteCommit:     suiteCommit,
		Environment:     "sim",
		Artifacts: map[string]string{
			"orders":        digestOf("api v1"),
			"orders-worker": digestOf("worker v1"),
		},
	}, true)
	repExistingBytes, err := os.ReadFile(repExisting.path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(customReportsDir, repExisting.digest+".json"), repExistingBytes, 0o600))

	// Record an approval on chg-existing before copying cards to legacyCardsDir.
	seedCP, err = controlplane.New(controlplane.Options{
		CardsDir:        seedCardsDir,
		PromotionTarget: newReleaseTarget(t, releaseDir, customReportsDir),
	})
	require.NoError(t, err)
	approvedExisting, err := seedCP.ApproveMerge(alice, &gensurla.ApproveMergePayload{
		ChangeID:        "chg-existing",
		RequestRevision: cardExisting.RequestRevision,
		CandidateCommit: commit,
		ReportDigest:    repExisting.digest,
	})
	require.NoError(t, err)
	require.NotNil(t, approvedExisting.Approval)
	require.NoError(t, seedCP.Close())

	seedEntries, err := os.ReadDir(seedCardsDir)
	require.NoError(t, err)
	for _, entry := range seedEntries {
		if strings.HasSuffix(entry.Name(), ".json") {
			raw, readErr := os.ReadFile(filepath.Join(seedCardsDir, entry.Name()))
			require.NoError(t, readErr)
			require.NoError(t, os.WriteFile(filepath.Join(legacyCardsDir, entry.Name()), raw, 0o600))
		}
	}

	// Standalone CLI report for chg-new: valid in customReportsDir, corrupt under the SAME digest filename in defaultReportsDir.
	repNew := saveReport(t, verify.Binding{
		CandidateCommit: commit,
		RequestRevision: cardNew.RequestRevision,
		SuiteID:         "orders",
		SuiteCommit:     suiteCommit,
		Environment:     "sim",
		Artifacts: map[string]string{
			"orders":        digestOf("api v1"),
			"orders-worker": digestOf("worker v1"),
		},
	}, true)
	repNewBytes, err := os.ReadFile(repNew.path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(customReportsDir, repNew.digest+".json"), repNewBytes, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(defaultReportsDir, repNew.digest+".json"), []byte(`{"corrupt":true}trailing`), 0o600))

	// Report present ONLY in defaultReportsDir (must NOT be found when customReportsDir is configured).
	defaultOnlyDigest := strings.Repeat("d", 64)
	require.NoError(t, os.WriteFile(filepath.Join(defaultReportsDir, defaultOnlyDigest+".json"), repNewBytes, 0o600))

	// Corrupt report in customReportsDir.
	corruptDigest := strings.Repeat("e", 64)
	require.NoError(t, os.WriteFile(filepath.Join(customReportsDir, corruptDigest+".json"), []byte(`{"bad":true}trailing`), 0o600))

	// 1. Prevalidation failure before publication: a promotion config with release_dir inside wsDir fails without publishing imported/.
	badPromoPath := filepath.Join(root, "bad-promotion.json")
	badPromoContent := `{"report_dir":"` + customReportsDir + `","release_dir":"` + filepath.Join(wsDir, "rel") + `","suite_id":"orders","suite_commit":"` +
		suiteCommit + `","environment":"sim","binaries":["orders","orders-worker"]}`
	require.NoError(t, os.WriteFile(badPromoPath, []byte(badPromoContent), 0o600))
	_, err = configureServeWorkspace(serveConfig{
		importLegacyWorkspace: true,
		promotionPath:         badPromoPath,
	})
	require.ErrorContains(t, err, "release_dir")
	require.ErrorContains(t, err, "must be outside the workspace")

	// 2. Valid promotion config referencing customReportsDir succeeds on retry because the failed attempt did not publish imported/.
	goodPromoPath := filepath.Join(root, "promotion.json")
	goodPromoContent := `{"report_dir":"` + customReportsDir + `","release_dir":"` + releaseDir + `","suite_id":"orders","suite_commit":"` +
		suiteCommit + `","environment":"sim","binaries":["orders","orders-worker"]}`
	require.NoError(t, os.WriteFile(goodPromoPath, []byte(goodPromoContent), 0o600))

	importedCfg, err := configureServeWorkspace(serveConfig{
		importLegacyWorkspace: true,
		promotionPath:         goodPromoPath,
	})
	require.NoError(t, err)
	require.NotNil(t, importedCfg.promotionTarget)
	t.Cleanup(func() {
		if importedCfg.closeProjectLock != nil {
			assert.NoError(t, importedCfg.closeProjectLock())
		}
	})

	// Remove the original workspace custom-reports directory to prove PromotionTarget reads from the imported snapshot.
	require.NoError(t, os.RemoveAll(customReportsDir))
	require.NoError(t, os.RemoveAll(defaultReportsDir))

	// Valid report in customReportsDir is loaded (ignoring the corrupt same-digest file in defaultReportsDir).
	loadedRep, err := importedCfg.promotionTarget.LoadReport(context.Background(), repNew.digest)
	require.NoError(t, err)
	assert.Equal(t, repNew.digest, loadedRep.Digest)

	// Corrupt report in customReportsDir fails with decode error.
	_, err = importedCfg.promotionTarget.LoadReport(context.Background(), corruptDigest)
	require.Error(t, err)

	// Missing report fails with os.ErrNotExist.
	_, err = importedCfg.promotionTarget.LoadReport(context.Background(), strings.Repeat("f", 64))
	require.ErrorIs(t, err, os.ErrNotExist)

	// Report present only in defaultReportsDir is NOT loaded when customReportsDir is configured (no fallback read).
	_, err = importedCfg.promotionTarget.LoadReport(context.Background(), defaultOnlyDigest)
	require.ErrorIs(t, err, os.ErrNotExist)

	// ControlPlane on the imported store preserves existing approval on GetChange/no-op ConfirmIntent and approves chg-new.
	cp, err := controlplane.New(controlplane.Options{
		CardsDir:               importedCfg.cardsDir,
		WorkspaceDir:           importedCfg.workspaceDir,
		PromotionTarget:        importedCfg.promotionTarget,
		ResolvePromotionTarget: importedCfg.resolvePromotionTarget,
		SurlaVersion:           "v0.0.0-20261104215521-580509970fb6",
	})
	require.NoError(t, err)
	defer cp.Close()

	gotExisting, err := cp.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg-existing"})
	require.NoError(t, err)
	assert.Equal(t, approvedExisting.Approval, gotExisting.Approval)
	assert.Nil(t, gotExisting.UnverifiedApproval)

	noopConfirmed, err := cp.ConfirmIntent(alice, &gensurla.ConfirmIntentPayload{
		ChangeID:        "chg-existing",
		RequestRevision: gotExisting.RequestRevision,
		Answers:         map[string]string{},
		Examples:        gotExisting.Examples,
	})
	require.NoError(t, err)
	assert.Equal(t, approvedExisting.Approval, noopConfirmed.Approval)
	assert.Nil(t, noopConfirmed.UnverifiedApproval)

	approvedNew, err := cp.ApproveMerge(alice, &gensurla.ApproveMergePayload{
		ChangeID:        "chg-new",
		RequestRevision: cardNew.RequestRevision,
		CandidateCommit: commit,
		ReportDigest:    repNew.digest,
	})
	require.NoError(t, err)
	require.NotNil(t, approvedNew.Approval)
	assert.Equal(t, repNew.digest, approvedNew.Approval.ReportDigest)
}

// TestDefaultOperatorRootDeterministicAndPermissionValidation verifies D1:
// defaultOperatorRoot uses only the single deterministic root from userConfigDir,
// failing without fallback when userConfigDir fails, overlaps the workspace, or
// has permissive (non-0700) permissions.
func TestDefaultOperatorRootDeterministicAndPermissionValidation(t *testing.T) {
	root := t.TempDir()
	wsDir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	t.Chdir(wsDir)

	priorConfigDir := userConfigDir
	t.Cleanup(func() { userConfigDir = priorConfigDir })

	// 1. When userConfigDir fails, configureServeWorkspace fails without falling back to TMPDIR or sibling dirs.
	userConfigDir = func() (string, error) {
		return "", os.ErrNotExist
	}
	_, err := configureServeWorkspace(serveConfig{})
	require.ErrorIs(t, err, os.ErrNotExist)

	// 2. When userConfigDir points inside the bound workspace, configureServeWorkspace fails without falling back.
	userConfigDir = func() (string, error) {
		return filepath.Join(wsDir, ".config"), nil
	}
	_, err = configureServeWorkspace(serveConfig{})
	require.ErrorContains(t, err, "must be outside the workspace")

	// 3. When <userConfigDir>/surla/operator already exists with permissive 0755 mode, configureServeWorkspace fails.
	cfgRoot := filepath.Join(root, "user-config")
	opRoot := filepath.Join(cfgRoot, "surla", "operator")
	require.NoError(t, os.MkdirAll(opRoot, 0o755))
	require.NoError(t, os.Chmod(opRoot, 0o755))
	userConfigDir = func() (string, error) {
		return cfgRoot, nil
	}
	_, err = configureServeWorkspace(serveConfig{})
	require.ErrorContains(t, err, "has permissions 0755, want 0o700")
}

// isolateServeWorkspace points userConfigDir at a new 0700 temporary directory
// and changes the working directory to a new empty workspace directory,
// returning that workspace directory path.
func isolateServeWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	cfgRoot := filepath.Join(root, "user-config")
	wsDir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(cfgRoot, 0o700))
	require.NoError(t, os.MkdirAll(wsDir, 0o755))
	prior := userConfigDir
	userConfigDir = func() (string, error) { return cfgRoot, nil }
	t.Cleanup(func() { userConfigDir = prior })
	t.Chdir(wsDir)
	return wsDir
}

// closeServeOptions releases the evaluator, analyzer, and project lock owned by
// opts when a test constructs serveOptions without handing opts to
// controlplane.New.
func closeServeOptions(t *testing.T, opts controlplane.Options) {
	t.Helper()
	if opts.Evaluator != nil {
		assert.NoError(t, opts.Evaluator.Close())
	}
	if opts.CloseAnalyzer != nil {
		assert.NoError(t, opts.CloseAnalyzer())
	}
	if opts.CloseProjectLock != nil {
		assert.NoError(t, opts.CloseProjectLock())
	}
}
