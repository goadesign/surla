// This file checks startup and the one-time copy of saved legacy workspace state.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"goa.design/goa-ai/eval"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
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
	assertOutsideDirectory(t, reopened.cardsDir, reopened.workspaceDir)
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
	assertOutsideDirectory(t, configured.cardsDir, configured.workspaceDir)
}

// TestWorkspaceUpgradeWithExternalVerification checks that an external plan
// cannot bypass import of workspace reports or accepted runs. Import preserves
// the original files, records report paths for check_project, and leaves the
// external plan selected on both the importing start and the next start.
func TestWorkspaceUpgradeWithExternalVerification(t *testing.T) {
	cases := []struct {
		name      string
		reportDir string
		run       bool
		notes     bool
	}{
		{name: "default report", reportDir: "reports"},
		{name: "accepted run with custom report", reportDir: "custom-reports", run: true},
		{name: "no legacy state"},
		{name: "unrelated files", notes: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace, err := filepath.EvalSymlinks(isolateServeWorkspace(t))
			require.NoError(t, err)
			t.Setenv("HOME", t.TempDir())
			ops, err := filepath.EvalSymlinks(t.TempDir())
			require.NoError(t, err)
			file := verificationFile{
				RunsDir:         filepath.Join(ops, "runs"),
				ReportDir:       filepath.Join(ops, "reports"),
				CandidateRepo:   workspace,
				CandidateModule: ".",
				Binaries:        map[string]string{"shop": "./cmd/shop"},
				SuiteRepo:       filepath.Join(ops, "eval-repo"),
				SuiteCommit:     strings.Repeat("d", 40),
				SuiteModule:     ".",
				SuiteCommand:    "./cmd/shop-evals",
				Timeout:         "15m",
			}
			cfg := serveConfig{verificationPath: filepath.Join(ops, "verification.json")}
			raw, err := json.Marshal(file)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(cfg.verificationPath, raw, 0o600))
			wantVerification, err := loadWorkspaceVerificationConfig(cfg.verificationPath, workspace, workspace)
			require.NoError(t, err)

			originals := map[string][]byte{}
			var report verify.Report
			var reportPath string
			if tc.reportDir != "" {
				reportDir := filepath.Join(workspace, tc.reportDir)
				report = saveWorkspaceUpgradeReport(t, reportDir)
				reportPath = filepath.Join(reportDir, report.Digest+".json")
				originals[reportPath], err = os.ReadFile(reportPath)
				require.NoError(t, err)
			}
			runID := strings.Repeat("a", 32)
			if tc.run {
				runDir := filepath.Join(workspace, ".surla", "runs", runID)
				require.NoError(t, os.MkdirAll(runDir, 0o700))
				req := verifyrun.Request{
					RunID:      runID,
					Owner:      "alice@example.com",
					ChangeID:   "chg-workspace-upgrade",
					AcceptedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
					Timeout:    15 * time.Minute,
					Plan:       verify.Plan{ReportDir: filepath.Dir(reportPath)},
				}
				raw, err := json.Marshal(req)
				require.NoError(t, err)
				originals[filepath.Join(runDir, "request.json")] = raw
				originals[filepath.Join(runDir, "lock")] = []byte("legacy lock\n")
			}
			if tc.notes {
				for _, dir := range []string{"reports", ".surla/runs"} {
					note := filepath.Join(workspace, dir, "README.txt")
					require.NoError(t, os.MkdirAll(filepath.Dir(note), 0o700))
					originals[note] = []byte("ordinary project notes\n")
				}
			}
			for path, content := range originals {
				require.NoError(t, os.WriteFile(path, content, 0o600))
			}

			configured, startupErr := configureServeWorkspace(cfg)
			if configured.closeProjectLock != nil {
				require.NoError(t, configured.closeProjectLock())
			}
			if tc.reportDir == "" {
				require.NoError(t, startupErr)
				assert.Empty(t, configured.importedReports)
			} else {
				require.ErrorContains(t, startupErr, "-import-legacy-workspace")
				assert.ErrorContains(t, startupErr, "with writers stopped")
				cfg.importLegacyWorkspace = true
				configured, err = configureServeWorkspace(cfg)
				require.NoError(t, err)
				require.NoError(t, configured.closeProjectLock())
				assert.Equal(t, []string{reportPath}, configured.importedReports)
				importedRoot := filepath.Dir(configured.cardsDir)
				paths, err := verifyrun.LoadImportedReportPaths(filepath.Join(importedRoot, "runs"))
				require.NoError(t, err)
				assert.Equal(t, []string{reportPath}, paths)
				reportDir, err := verifyrun.ResolveImportedReportDir(
					filepath.Join(importedRoot, "reports"), workspace, filepath.Dir(reportPath),
				)
				require.NoError(t, err)
				copied, err := os.ReadFile(filepath.Join(reportDir, report.Digest+".json"))
				require.NoError(t, err)
				assert.Equal(t, originals[reportPath], copied)
				loaded, err := verifyrun.LoadReport(reportDir, report.Digest)
				require.NoError(t, err)
				assert.Equal(t, report, loaded)
				if tc.run {
					req, state, err := verifyrun.Read(filepath.Join(importedRoot, "runs"), runID)
					require.NoError(t, err)
					assert.Equal(t, reportDir, req.Plan.ReportDir)
					assert.Equal(t, verifyrun.StateNotCompleted, state.Kind)
					assert.Equal(t, verifyrun.FailureInterrupted, state.Result.Failure)
				}
			}
			assert.Equal(t, wantVerification, configured.verification)
			assert.Equal(t, file.RunsDir, configured.runsDir)
			assert.Nil(t, configured.resolveVerification)

			cfg.importLegacyWorkspace = false
			restarted, err := configureServeWorkspace(cfg)
			require.NoError(t, err)
			require.NoError(t, restarted.closeProjectLock())
			assert.Equal(t, configured.importedReports, restarted.importedReports)
			assert.Equal(t, wantVerification, restarted.verification)
			assert.Equal(t, file.RunsDir, restarted.runsDir)
			for path, content := range originals {
				got, err := os.ReadFile(path)
				require.NoError(t, err)
				assert.Equal(t, content, got, "original %s must remain unchanged", path)
			}
		})
	}
}

// saveWorkspaceUpgradeReport writes a valid report containing private holdout
// evidence in the legacy layout, using the same assembly and JSON encoding as
// verification. The returned report can be read through verifyrun.LoadReport.
func saveWorkspaceUpgradeReport(t *testing.T, dir string) verify.Report {
	t.Helper()
	binding := verify.Binding{
		CandidateCommit: strings.Repeat("b", 40),
		RequestRevision: strings.Repeat("c", 64),
		SuiteID:         "workspace-upgrade",
		SuiteCommit:     strings.Repeat("d", 40),
		Artifacts:       map[string]string{"shop": strings.Repeat("e", 64)},
		Environment:     "sim",
	}
	suite := verify.Suite{
		ID: binding.SuiteID, Environment: binding.Environment,
		Scenarios: []verify.Scenario{{ID: "private-holdout", Holdout: true}},
	}
	evidence := eval.Report{SuiteID: suite.ID, Scenarios: []eval.ScenarioReport{{
		ID:        "private-holdout",
		StartedAt: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
		Result: &eval.Result{
			Checks:    []eval.Check{{Name: "private-check", Diagnostic: "private diagnostic"}},
			Artifacts: []eval.Artifact{{Name: "candidate:shop", URI: "sha256:" + binding.Artifacts["shop"]}},
		},
	}}}
	report, err := verify.Assemble(binding, suite, evidence, 1)
	require.NoError(t, err)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, report.Digest+".json"), raw, 0o600))
	return report
}

// saveWorkspaceUpgradeCard saves and refines a confirmed card through the public
// control-plane API using the old implicit CWD/.surla/cards store. Leaving
// WorkspaceDir unset reproduces the old layout without encoding owned files.
func saveWorkspaceUpgradeCard(t *testing.T, workspace string) *gensurla.ChangeCardResult {
	t.Helper()
	cp, err := controlplane.New(controlplane.Options{
		CardsDir: filepath.Join(workspace, ".surla", "cards"),
	})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	alice := controlplane.WithPrincipal(context.Background(), controlplane.Principal{
		ID: "alice@example.com",
	})
	proposed, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{
		ChangeID: "chg-workspace-upgrade",
		Prompt:   "Refunds up to $50 are instant.",
		Analysis: confirmedAnalysis(),
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
