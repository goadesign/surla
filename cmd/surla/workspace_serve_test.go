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
	"github.com/goadesign/surla/internal/abspath"
	"github.com/goadesign/surla/internal/controlplane"
)

// TestConfigureServeWorkspaceSeparatesOperatorState verifies that
// configureServeWorkspace places operator state outside the workspace.
func TestConfigureServeWorkspaceSeparatesOperatorState(t *testing.T) {
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
	assertOutsideDirectory(t, wsCfg.cardsDir, wsCfg.workspaceDir)
	assertOutsideDirectory(t, wsCfg.runsDir, wsCfg.workspaceDir)
	assertOutsideDirectory(t, wsCfg.initStageDir, wsCfg.workspaceDir)
	assertOutsideDirectory(t, wsCfg.evalWorkDir, wsCfg.workspaceDir)

}

// TestConfigureServeWorkspaceRejectsWorkspacePromotionDirectories constructs the
// server options and refuses promotion inputs or release binaries in the workspace.
func TestConfigureServeWorkspaceRejectsWorkspacePromotionDirectories(t *testing.T) {
	workspace := isolateServeWorkspace(t)
	operatorDir := t.TempDir()
	for _, field := range []string{"report_dir", "release_dir"} {
		t.Run(field, func(t *testing.T) {
			reports := filepath.Join(operatorDir, "reports")
			release := filepath.Join(operatorDir, "releases")
			if field == "report_dir" {
				reports = filepath.Join(workspace, "reports")
			} else {
				release = filepath.Join(workspace, "releases")
			}
			config := filepath.Join(operatorDir, "promotion.json")
			body := `{"report_dir":"` + reports + `","release_dir":"` + release + `","suite_id":"orders","suite_commit":"` + strings.Repeat("a", 40) + `","environment":"sim","binaries":["orders"]}`
			require.NoError(t, os.WriteFile(config, []byte(body), 0o600))
			_, err := configureServeWorkspace(serveConfig{promotionPath: config})
			assert.ErrorContains(t, err, field)
			assert.ErrorContains(t, err, "must be outside the workspace")
		})
	}
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
	assertOutsideDirectory(t, parentCWDCfg.runsDir, wsDir)
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

// TestConfigureServeWorkspaceMonorepoSeparation verifies that
// configureServeWorkspace protects an enclosing
// monorepo working tree R while preserving external operator working-directory
// card stores, separate verification source repositories B, and the starter
// sibling external verification configuration layout.
func TestConfigureServeWorkspaceMonorepoSeparation(t *testing.T) {
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

	// 2. An explicit project inside a monorepo keeps cards outside that monorepo.

	monorepo := filepath.Join(root, "monorepo")
	shopInMono := filepath.Join(monorepo, "services", "shop")
	writeStarter(t, shopInMono, "example.com/shop")
	runTestGit(t, monorepo, "init", "-q")
	gitCommitAll(t, monorepo, "Initial monorepo")

	t.Chdir(monorepo)
	monoCfg, err := configureServeWorkspace(serveConfig{projectDir: "services/shop", projectSet: true})
	require.NoError(t, err)
	assertOutsideDirectory(t, monoCfg.cardsDir, monorepo)
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

	// 4. Separate verification source repository B (e.g. read-only bare mirror) is accepted when
	// workspace is A, while paths inside an enclosing monorepo R2 are rejected.
	monorepo2 := filepath.Join(root, "monorepo-2")
	wsA := filepath.Join(monorepo2, "services", "orders")
	candB := filepath.Join(root, "candidate-b")
	releaseDir := filepath.Join(root, "release-b")
	require.NoError(t, os.MkdirAll(wsA, 0o755))
	require.NoError(t, os.MkdirAll(candB, 0o755))
	require.NoError(t, os.MkdirAll(releaseDir, 0o700))
	runTestGit(t, monorepo2, "init", "-q")

	suiteCommit := strings.Repeat("b", 40)
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

	// Rejects explicit -cards-dir or -promotion-config report_dir inside enclosing monorepo2.
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
// verifies that restarting surla serve with another spelling of the same
// directories, on a case-insensitive volume, selects the same operator
// project, card store and runs, so saved cards are still returned. Each case
// keeps the same way of starting (from the parent with -project, or inside
// the workspace), which by design selects its own card store.
func TestConfigureServeWorkspaceKeepsHistoryAcrossSpellingsOfTheWorkspace(t *testing.T) {
	type start struct {
		chdir   string
		project string
	}
	cases := []struct {
		name   string
		starts []start
	}{
		{name: "-project from the parent", starts: []start{{chdir: ".", project: "WORKSPACE"}, {chdir: ".", project: "workspace"}}},
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
			priorConfigDir := userConfigDir
			userConfigDir = func() (string, error) { return cfgRoot, nil }
			t.Cleanup(func() { userConfigDir = priorConfigDir })
			alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{ID: "alice@example.com"})

			var first workspaceServeConfig
			for i, st := range tc.starts {
				t.Chdir(filepath.Join(root, st.chdir))
				cfg := serveConfig{}
				if st.project != "" {
					cfg.projectDir, cfg.projectSet = st.project, true
				}
				ws, err := configureServeWorkspace(cfg)
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
