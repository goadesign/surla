package controlplane

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

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// TestLiveAndVerificationUseTheWorkspacePinnedSuite verifies that the
// evaluator commit init_project pins reaches what both consumers save: the
// live target's launch plan (live.json) and the verification run's request
// (request.json), on the server that initialized the workspace and on a
// server reopened from the same card store, while the candidate is a later
// builder commit. Both tools resolve their plan through the same owner. The
// live target is this test binary and the verification supervisor is a shell
// that exits at once, so no Go build runs.
func TestLiveAndVerificationUseTheWorkspacePinnedSuite(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	root := t.TempDir()
	wsDir := filepath.Join(root, "ws")
	runsDir := filepath.Join(root, "runs")
	reportsDir := filepath.Join(root, "reports")
	for _, d := range []string{wsDir, runsDir, reportsDir} {
		require.NoError(t, os.MkdirAll(d, 0o700))
	}
	analysis, err := json.Marshal(CP1SemanticAnalysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []CP1ModelAmbiguity{},
		Examples: []CP1ModelExample{{
			Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
			ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{},
	})
	require.NoError(t, err)
	options := func() Options {
		return Options{
			CardsDir:     filepath.Join(root, "cards"),
			WorkspaceDir: wsDir,
			InitStageDir: filepath.Join(root, "stage"),
			RunsDir:      runsDir,
			SurlaVersion: testSurlaVersion,
			InitRunner:   fakeInitRunner,
			NewGenerator: func() scaffold.Generator { return &fakeInitGenerator{pkg: "shop"} },
			Analyzer:     NewModelCP1Analyzer(newStubModelClient(t, string(analysis))),
			ResolveVerification: func(_ context.Context, workspaceDir, suiteCommit string) (*VerificationConfig, error) {
				return &VerificationConfig{
					RunsDir: runsDir,
					Plan: verify.Plan{
						CandidateRepo: workspaceDir, CandidateModule: ".", Binaries: map[string]string{"shop": "./cmd/shop"},
						SuiteRepo: workspaceDir, SuiteCommit: suiteCommit, SuiteModule: "eval",
						SuiteCommand: "./cmd/shop-eval", ReportDir: reportsDir,
					},
					// The supervisor exits at once; only the saved request is
					// under test.
					Command: []string{"/bin/sh", "-c", "exit 0", "sh"},
					Timeout: time.Hour,
				}, nil
			},
			Live: &LiveConfig{
				Command:      []string{self},
				Env:          append(os.Environ(), liveTargetModeEnv+"=serve"),
				StartTimeout: time.Minute,
			},
		}
	}
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})

	initServer, err := New(options())
	require.NoError(t, err)
	// Close is idempotent: this also covers a failure before the explicit
	// Close below.
	t.Cleanup(func() { assert.NoError(t, initServer.Close()) })
	_, err = initServer.InitProject(alice, &gensurla.InitProjectPayload{ModulePath: "example.com/shop"})
	require.NoError(t, err)
	t.Run("server that initialized the workspace", func(t *testing.T) {
		assertPinnedSuite(t, initServer, filepath.Join(root, "cards"), runsDir, "chg-init")
	})
	require.NoError(t, initServer.Close())

	reopened, err := New(options())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, reopened.Close()) })
	t.Run("server reopened from the same card store", func(t *testing.T) {
		assertPinnedSuite(t, reopened, filepath.Join(root, "cards"), runsDir, "chg-reopened")
	})
}

// assertPinnedSuite starts and stops a live target and starts a verification
// run for a later candidate commit on cp, and checks that the saved live.json
// and request.json both carry the evaluator commit init_project pinned.
func assertPinnedSuite(t *testing.T, cp *ControlPlane, cardsDir, runsDir, changeID string) {
	t.Helper()
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	candidate := strings.Repeat("b", 40)

	started, err := cp.StartLiveTarget(alice, &gensurla.StartLiveTargetPayload{CandidateCommit: candidate})
	require.NoError(t, err)
	assert.Equal(t, candidate, started.CandidateCommit)
	liveDirs, err := verifyrun.LeftoverLiveDirs(LiveRoot(cardsDir))
	require.NoError(t, err)
	require.Len(t, liveDirs, 1)
	var live verifyrun.LiveRequest
	readJSONFile(t, filepath.Join(liveDirs[0], "live.json"), &live)
	assert.Equal(t, fakeInitialSuiteCommit, live.Plan.SuiteCommit, "the live target uses the pinned evaluator")
	assert.Equal(t, candidate, live.Plan.CandidateCommit)
	stopped, err := cp.StopLiveTarget(alice)
	require.NoError(t, err)
	assert.True(t, stopped.Stopped)

	card, err := cp.ProposeChange(alice, &gensurla.ProposeChangePayload{ChangeID: changeID, Prompt: "Refunds up to $50 are instant."})
	require.NoError(t, err)
	require.Equal(t, StatusConfirmed, card.Status)
	run, err := cp.VerifyChange(alice, &gensurla.VerifyChangePayload{ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, CandidateCommit: candidate})
	require.NoError(t, err)
	assert.Equal(t, fakeInitialSuiteCommit, run.SuiteCommit)
	var req verifyrun.Request
	readJSONFile(t, filepath.Join(runsDir, run.RunID, "request.json"), &req)
	assert.Equal(t, fakeInitialSuiteCommit, req.Plan.SuiteCommit, "the verification run uses the pinned evaluator")
	assert.Equal(t, candidate, req.Plan.CandidateCommit)
}

// readJSONFile decodes the JSON file at p into v.
func readJSONFile(t *testing.T, p string, v any) {
	t.Helper()
	raw, err := os.ReadFile(p)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, v))
}
