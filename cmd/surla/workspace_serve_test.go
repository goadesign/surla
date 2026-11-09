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

	// Before import, normal startup without -import-legacy-workspace fails closed requiring -import-legacy-workspace.
	_, err = configureServeWorkspace(serveConfig{})
	require.ErrorContains(t, err, "legacy workspace state found")
	require.ErrorContains(t, err, "run surla serve -import-legacy-workspace")

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

	// After import, normal startup without -import-legacy-workspace uses the imported stores.
	subsequentCfg, err := configureServeWorkspace(serveConfig{})
	require.NoError(t, err)
	require.NotNil(t, subsequentCfg.closeProjectLock)
	assert.Equal(t, importedCfg.cardsDir, subsequentCfg.cardsDir)
	assert.Equal(t, importedCfg.runsDir, subsequentCfg.runsDir)
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

// TestDefaultOperatorRootDeterministicAndPermissionValidation verifies that
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

	// 4. Starting from a parent directory ( such as $HOME ) that contains userConfigDir
	// while binding a child workspace via -project succeeds because CWD is not a builder grant.
	require.NoError(t, os.Chmod(opRoot, 0o700))
	t.Chdir(root)
	parentCWDCfg, err := configureServeWorkspace(serveConfig{
		projectDir: wsDir,
		projectSet: true,
	})
	require.NoError(t, err)
	assert.False(t, isWithinOrEqualDir(parentCWDCfg.runsDir, wsDir))
	require.NoError(t, parentCWDCfg.closeProjectLock())
}

// TestConfigureServeWorkspacePinsSuiteCommitAndRejectsSymlinkParentEscape verifies
// that configureServeWorkspace resolves symlinks before ".." on -cards-dir,
// -verification-config, and -promotion-config, pins SuiteCommit to the startup
// commit or init_project commit across later builder commits, and refuses
// unpinned post-startup repo creation.
func TestConfigureServeWorkspacePinsSuiteCommitAndRejectsSymlinkParentEscape(t *testing.T) {
	root := t.TempDir()
	cfgRoot := filepath.Join(root, "user-config")
	wsDir := filepath.Join(root, "workspace")
	wsSub := filepath.Join(wsDir, "sub")
	require.NoError(t, os.MkdirAll(cfgRoot, 0o700))
	require.NoError(t, os.MkdirAll(wsSub, 0o755))

	priorConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return cfgRoot, nil }
	t.Cleanup(func() { userConfigDir = priorConfigDir })
	t.Chdir(wsDir)

	alias := filepath.Join(root, "alias")
	require.NoError(t, os.Symlink(wsSub, alias))

	// 1. Symlink before ".." that lands inside wsDir is rejected for -cards-dir and -promotion-config.
	_, err := configureServeWorkspace(serveConfig{
		cardsDir:         alias + "/../cards",
		cardsDirExplicit: true,
	})
	require.ErrorContains(t, err, "must be outside the workspace")

	promoInWSViaAlias := alias + "/../promo.json"
	require.NoError(t, os.WriteFile(filepath.Join(wsDir, "promo.json"), []byte(`{}`), 0o600))
	_, err = configureServeWorkspace(serveConfig{
		promotionPath: promoInWSViaAlias,
	})
	require.ErrorContains(t, err, "must be outside the workspace")
	require.NoError(t, os.Remove(filepath.Join(wsDir, "promo.json")))

	// 2. Uninitialized workspace at startup refuses post-startup manual repo creation without init_project,
	// while accepting a pinned suiteCommit from init_project and keeping SuiteCommit pinned across later builder commits.
	uninitCfg, err := configureServeWorkspace(serveConfig{})
	require.NoError(t, err)
	t.Cleanup(func() {
		if uninitCfg.closeProjectLock != nil {
			assert.NoError(t, uninitCfg.closeProjectLock())
		}
	})
	assert.Nil(t, uninitCfg.verification)

	childRepo, initialCommit := initCommittedStarterRepo(t, filepath.Join(wsDir, "shop"))
	// Without a pinned suiteCommit (as if created manually after startup), resolveVerification fails closed
	// with the startup loader error rather than trusting an unpinned post-startup HEAD.
	_, err = uninitCfg.resolveVerification(context.Background(), childRepo, "")
	require.ErrorContains(t, err, "automatic starter verification was unavailable at server startup")
	require.ErrorContains(t, err, "is not inside a git repository")
	require.ErrorContains(t, err, "restart surla serve so it pins the committed starter HEAD, or call init_project")

	// Simulate builder advancing HEAD with a second commit that edits eval/.
	require.NoError(t, os.WriteFile(filepath.Join(childRepo, "eval", "cmd", "shop-evals", "main.go"), []byte("package main\n// builder edit\n"), 0o644))
	builderCommit := gitCommitAll(t, childRepo, "Builder commit")
	require.NotEqual(t, initialCommit, builderCommit)

	// With the pinned initialCommit from init_project, resolveVerification keeps SuiteCommit == initialCommit.
	resolvedFromInit, err := uninitCfg.resolveVerification(context.Background(), childRepo, initialCommit)
	require.NoError(t, err)
	assert.Equal(t, initialCommit, resolvedFromInit.Plan.SuiteCommit)

	// 3. Existing starter repo committed at server startup keeps startup SuiteCommit across later builder commits
	// while still picking up late promotion.json report_dir updates.
	existingWS, startupCommit := initCommittedStarterRepo(t, filepath.Join(root, "existing-shop"))
	existingCfg, err := configureServeWorkspace(serveConfig{
		projectDir: existingWS,
		projectSet: true,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		if existingCfg.closeProjectLock != nil {
			assert.NoError(t, existingCfg.closeProjectLock())
		}
	})
	require.NotNil(t, existingCfg.verification)
	assert.Equal(t, startupCommit, existingCfg.verification.Plan.SuiteCommit)

	// Builder makes a new commit advancing HEAD in existingWS.
	require.NoError(t, os.WriteFile(filepath.Join(existingWS, "cmd", "shop", "main.go"), []byte("package main\n// candidate v2\n"), 0o644))
	laterCommit := gitCommitAll(t, existingWS, "Candidate v2")
	require.NotEqual(t, startupCommit, laterCommit)

	resolvedExisting, err := existingCfg.resolveVerification(context.Background(), existingCfg.workspaceDir, "")
	require.NoError(t, err)
	assert.Equal(t, startupCommit, resolvedExisting.Plan.SuiteCommit, "existing startup repo must stay pinned to startup HEAD")
}

// TestConfigureServeWorkspaceMonorepoSeparationAndLegacyImport verifies that
// configureServeWorkspace and -import-legacy-workspace protect an enclosing
// monorepo working tree R while preserving external operator working-directory
// card stores, separate verification source repositories B, and the starter
// README's sibling ../bakery-ops/verification.json layout, and importing legacy
// monorepo cards and promotion/run reports.
func TestConfigureServeWorkspaceMonorepoSeparationAndLegacyImport(t *testing.T) {
	isolateGitConfig(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cfgRoot := filepath.Join(root, "user-config")
	require.NoError(t, os.MkdirAll(cfgRoot, 0o700))

	priorConfigDir := userConfigDir
	userConfigDir = func() (string, error) { return cfgRoot, nil }
	t.Cleanup(func() { userConfigDir = priorConfigDir })

	// 1. Starter README sibling layout (../bakery-ops/verification.json) loads cleanly from the starter directory.
	bakeryDir, bakeryCommit := initCommittedStarterRepo(t, filepath.Join(root, "bakery"))
	bakeryOpsDir := filepath.Join(root, "bakery-ops")
	require.NoError(t, os.MkdirAll(bakeryOpsDir, 0o700))
	bakeryVerPath := filepath.Join(bakeryOpsDir, "verification.json")
	bakeryVerJSON := `{"runs_dir":"../bakery-ops/runs","report_dir":"../bakery-ops/reports","candidate_repo":".",` +
		`"candidate_module":".","binaries":{"shop":"./cmd/shop"},"suite_repo":".","suite_commit":"` + bakeryCommit + `",` +
		`"suite_module":"eval","suite_command":"./cmd/shop-evals","timeout":"30m"}`
	require.NoError(t, os.WriteFile(bakeryVerPath, []byte(bakeryVerJSON), 0o600))
	t.Chdir(bakeryDir)
	bakeryCfg, err := configureServeWorkspace(serveConfig{
		projectDir:       ".",
		projectSet:       true,
		verificationPath: "../bakery-ops/verification.json",
	})
	require.NoError(t, err)
	require.NotNil(t, bakeryCfg.verification)
	assert.Equal(t, filepath.Join(bakeryOpsDir, "runs"), bakeryCfg.verification.RunsDir)
	assert.Equal(t, filepath.Join(bakeryOpsDir, "reports"), bakeryCfg.verification.Plan.ReportDir)
	require.NoError(t, bakeryCfg.closeProjectLock())

	// 2. Monorepo R with -project services/shop launched from R places default cardsDir outside R,
	// blocks normal startup when R/.surla/cards or R/services/shop/.surla/cards has unimported cards,
	// and imports both R/.surla/cards and R/services/shop/.surla/cards on -import-legacy-workspace.
	monorepo := filepath.Join(root, "monorepo")
	shopInMono := filepath.Join(monorepo, "services", "shop")
	writeStarter(t, shopInMono, "example.com/shop")
	runTestGit(t, monorepo, "init", "-q")
	gitCommitAll(t, monorepo, "Initial monorepo")

	monoLegacyCards := filepath.Join(monorepo, ".surla", "cards")
	subLegacyCards := filepath.Join(shopInMono, ".surla", "cards")
	require.NoError(t, os.MkdirAll(monoLegacyCards, 0o700))
	require.NoError(t, os.MkdirAll(subLegacyCards, 0o700))
	monoCardDigest := strings.Repeat("1", 64) + ".json"
	subCardDigest := strings.Repeat("2", 64) + ".json"
	require.NoError(t, os.WriteFile(filepath.Join(monoLegacyCards, monoCardDigest), []byte(`{"mono":true}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(subLegacyCards, subCardDigest), []byte(`{"sub":true}`), 0o600))

	t.Chdir(monorepo)
	_, err = configureServeWorkspace(serveConfig{
		projectDir: "services/shop",
		projectSet: true,
	})
	require.ErrorContains(t, err, "legacy workspace state found")
	require.ErrorContains(t, err, "run surla serve -import-legacy-workspace")

	monoImportedCfg, err := configureServeWorkspace(serveConfig{
		projectDir:            "services/shop",
		projectSet:            true,
		importLegacyWorkspace: true,
	})
	require.NoError(t, err)
	assert.False(t, isWithinOrEqualDir(monoImportedCfg.cardsDir, monorepo), "default cardsDir must be outside enclosing monorepo R")
	assert.FileExists(t, filepath.Join(monoImportedCfg.cardsDir, monoCardDigest))
	require.NoError(t, monoImportedCfg.closeProjectLock())

	t.Chdir(shopInMono)
	subAfterImportCfg, err := configureServeWorkspace(serveConfig{})
	require.NoError(t, err)
	assert.FileExists(t, filepath.Join(subAfterImportCfg.cardsDir, subCardDigest))
	require.NoError(t, subAfterImportCfg.closeProjectLock())

	// 3. External operator CWD outside R preserves <opsDir>/.surla/cards when -cards-dir is omitted.
	opsDir := filepath.Join(root, "external-ops")
	require.NoError(t, os.MkdirAll(opsDir, 0o700))
	t.Chdir(opsDir)
	extCWDCfg, err := configureServeWorkspace(serveConfig{
		projectDir: shopInMono,
		projectSet: true,
	})
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(opsDir, ".surla", "cards"), extCWDCfg.cardsDir)
	require.NoError(t, extCWDCfg.closeProjectLock())

	// 4. Separate verification source repository B (e.g. read-only bare mirror) is accepted when
	// workspace is A, while paths inside an enclosing monorepo R2 are rejected until imported.
	monorepo2 := filepath.Join(root, "monorepo-2")
	wsA := filepath.Join(monorepo2, "services", "orders")
	candB := filepath.Join(root, "candidate-b")
	releaseDir := filepath.Join(root, "release-b")
	require.NoError(t, os.MkdirAll(wsA, 0o755))
	require.NoError(t, os.MkdirAll(candB, 0o755))
	require.NoError(t, os.MkdirAll(releaseDir, 0o700))
	runTestGit(t, monorepo2, "init", "-q")

	commitB := strings.Repeat("c", 40)
	publish(t, releaseDir, commitB, map[string]string{
		"orders":        "api v1",
		"orders-worker": "worker v1",
	})

	extRunsDir := filepath.Join(root, "ext-runs-b")
	extReportsDir := filepath.Join(root, "ext-reports-b")
	verInOpsPath := filepath.Join(opsDir, "ver-b.json")
	writeVer := func(runsPath, reportPath string) {
		content := `{"runs_dir":"` + runsPath + `","report_dir":"` + reportPath + `","candidate_repo":"` + candB + `",` +
			`"candidate_module":".","binaries":{"orders":"./cmd/orders"},"suite_repo":"` + candB + `","suite_commit":"` + suiteCommit + `",` +
			`"suite_module":"eval","suite_command":"./cmd/orders-evals","timeout":"15m"}`
		require.NoError(t, os.WriteFile(verInOpsPath, []byte(content), 0o600))
	}

	t.Chdir(wsA)
	// Rejects RunsDir or ReportDir inside enclosing monorepo2 (outside wsA).
	writeVer(filepath.Join(monorepo2, "runs"), extReportsDir)
	_, err = configureServeWorkspace(serveConfig{verificationPath: verInOpsPath})
	require.ErrorContains(t, err, "must be outside the workspace")

	writeVer(extRunsDir, filepath.Join(monorepo2, "reports"))
	_, err = configureServeWorkspace(serveConfig{verificationPath: verInOpsPath})
	require.ErrorContains(t, err, "must be outside the workspace")

	// Rejects explicit -cards-dir or unimported -promotion-config report_dir inside enclosing monorepo2.
	writeVer(extRunsDir, extReportsDir)
	_, err = configureServeWorkspace(serveConfig{
		verificationPath: verInOpsPath,
		cardsDir:         filepath.Join(monorepo2, ".surla", "cards"),
		cardsDirExplicit: true,
	})
	require.ErrorContains(t, err, "must be outside the workspace")

	mono2ReportsDir := filepath.Join(monorepo2, "reports")
	require.NoError(t, os.MkdirAll(mono2ReportsDir, 0o700))
	promoBPath := filepath.Join(opsDir, "promo-b.json")
	promoBContent := `{"report_dir":"` + mono2ReportsDir + `","release_dir":"` + releaseDir + `","suite_id":"orders","suite_commit":"` +
		suiteCommit + `","environment":"sim","binaries":["orders","orders-worker"]}`
	require.NoError(t, os.WriteFile(promoBPath, []byte(promoBContent), 0o600))

	_, err = configureServeWorkspace(serveConfig{
		verificationPath: verInOpsPath,
		promotionPath:    promoBPath,
	})
	require.ErrorContains(t, err, "report_dir")
	require.ErrorContains(t, err, "must be outside the workspace")

	// Seed a confirmed card in wsA/.surla/cards with an existing approval referencing a report in monorepo2/reports.
	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com"})
	seedCardsDir := filepath.Join(root, "seed-cards-b")
	seedCP, err := controlplane.New(controlplane.Options{
		CardsDir: seedCardsDir,
		Analyzer: confirmedAnalyzer{},
	})
	require.NoError(t, err)
	cardB, err := seedCP.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID: "chg-mono-2",
		Prompt:   "Refunds up to $50 are instant.",
	})
	require.NoError(t, err)
	require.NoError(t, seedCP.Close())

	repB := saveReport(t, verify.Binding{
		CandidateCommit: commitB,
		RequestRevision: cardB.RequestRevision,
		SuiteID:         "orders",
		SuiteCommit:     suiteCommit,
		Environment:     "sim",
		Artifacts: map[string]string{
			"orders":        digestOf("api v1"),
			"orders-worker": digestOf("worker v1"),
		},
	}, true)
	repBBytes, err := os.ReadFile(repB.path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(mono2ReportsDir, repB.digest+".json"), repBBytes, 0o600))

	seedCP, err = controlplane.New(controlplane.Options{
		CardsDir:        seedCardsDir,
		PromotionTarget: newReleaseTarget(t, releaseDir, mono2ReportsDir),
	})
	require.NoError(t, err)
	approvedB, err := seedCP.ApproveMerge(alice, &gensurla.ApproveMergePayload{
		ChangeID:        "chg-mono-2",
		RequestRevision: cardB.RequestRevision,
		CandidateCommit: commitB,
		ReportDigest:    repB.digest,
	})
	require.NoError(t, err)
	require.NotNil(t, approvedB.Approval)
	require.NoError(t, seedCP.Close())

	wsALegacyCards := filepath.Join(wsA, ".surla", "cards")
	require.NoError(t, os.MkdirAll(wsALegacyCards, 0o700))
	seedEntries, err := os.ReadDir(seedCardsDir)
	require.NoError(t, err)
	for _, entry := range seedEntries {
		if strings.HasSuffix(entry.Name(), ".json") {
			raw, readErr := os.ReadFile(filepath.Join(seedCardsDir, entry.Name()))
			require.NoError(t, readErr)
			require.NoError(t, os.WriteFile(filepath.Join(wsALegacyCards, entry.Name()), raw, 0o600))
		}
	}

	// Run -import-legacy-workspace with -verification-config (CandidateRepo B) and -promotion-config (report_dir in monorepo2).
	importedBCfg, err := configureServeWorkspace(serveConfig{
		verificationPath:      verInOpsPath,
		promotionPath:         promoBPath,
		importLegacyWorkspace: true,
	})
	require.NoError(t, err)
	require.NotNil(t, importedBCfg.promotionTarget)
	t.Cleanup(func() {
		if importedBCfg.closeProjectLock != nil {
			assert.NoError(t, importedBCfg.closeProjectLock())
		}
	})

	// Remove mono2ReportsDir to prove reports were snapshotted into <imported>/reports/dir-<sha256>.
	require.NoError(t, os.RemoveAll(mono2ReportsDir))

	cpB, err := controlplane.New(controlplane.Options{
		CardsDir:               importedBCfg.cardsDir,
		WorkspaceDir:           importedBCfg.workspaceDir,
		PromotionTarget:        importedBCfg.promotionTarget,
		ResolvePromotionTarget: importedBCfg.resolvePromotionTarget,
		SurlaVersion:           "v0.0.0-20261104215521-580509970fb6",
	})
	require.NoError(t, err)
	defer cpB.Close()

	gotB, err := cpB.GetChange(alice, &gensurla.GetChangePayload{ChangeID: "chg-mono-2"})
	require.NoError(t, err)
	assert.Equal(t, approvedB.Approval, gotB.Approval)
	assert.Nil(t, gotB.UnverifiedApproval)
}

// initCommittedStarterRepo creates a minimal starter repository layout at dir
// and commits it, returning the physical directory path and initial commit SHA.
func initCommittedStarterRepo(t *testing.T, dir string) (string, string) {
	t.Helper()
	writeStarter(t, dir, "example.com/shop")
	runTestGit(t, dir, "init", "-q")
	commit := gitCommitAll(t, dir, "Initial scaffold")
	physDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	return physDir, commit
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
