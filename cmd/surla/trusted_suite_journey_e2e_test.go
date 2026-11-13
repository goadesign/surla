//go:build e2e

// This file supplies the real verification and check helper subprocess
// entrypoints and the fixtures for the starter's authenticated MCP journey,
// without a second scaffold build.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genmcpsurla "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/scaffold"
	"github.com/goadesign/surla/internal/verifyrun"
)

// journeyOwnerWait bounds each client wait on a server-owned operation in
// these journeys: the owner's own limit, defaultProjectVerifyTimeout, which
// serveOptions gives a live start and writeStarterVerification stores as a
// verification run's timeout, plus the owner's stop path (stop grace 45s,
// group stop 30s, executor wait 5s). The owner therefore reports its own
// timeout or terminal state first when it can; the wait does not guarantee
// the owner finished if its I/O fails.
const journeyOwnerWait = defaultProjectVerifyTimeout + 2*time.Minute

// trustedSuiteCommit commits only the harmless evaluator comment and returns
// the full candidate commit ID; Git identity is local to the parent test.
func trustedSuiteCommit(t *testing.T, dir string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	for _, args := range [][]string{
		{"add", "eval/design/design.go"},
		{"commit", "-qm", "Add evaluator design comment"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
	}
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "HEAD^{commit}")
	cmd.Dir = dir
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

// trustedSuiteCall returns a successful real MCP tool result. Tool failures
// remain builder-safe; private evaluator logs are never printed by this helper.
func trustedSuiteCall(t *testing.T, ctx context.Context, client *genmcpsurla.Client,
	name string, args []byte,
) json.RawMessage {
	t.Helper()
	result, err := client.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: name, Arguments: args})
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok, "%s must complete in this call", name)
	require.False(t, complete.IsError != nil && *complete.IsError, "%s: %s", name, toolErrorText(complete.Content))
	return complete.StructuredContent
}

// toolErrorText joins the text items of an MCP tool result, the
// builder-safe message a failing tool returns, so a test failure shows it.
func toolErrorText(content []*genmcpsurla.ContentItem) string {
	var texts []string
	for _, item := range content {
		if item != nil && item.Text != nil {
			texts = append(texts, *item.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// writeStarterVerification stores, through CLI management before the next
// server start, the operator's verification settings for the starter at dir:
// its service and test modules, with the tests at suiteCommit run by the
// command the starter's layout names. With them, the project verifies every
// change with that evaluator and needs no build.
func writeStarterVerification(t *testing.T, workspace, dir, modulePath, suiteCommit string) {
	t.Helper()
	t.Chdir(workspace)
	layout, err := scaffold.LayoutForModule(modulePath)
	require.NoError(t, err)
	settings, err := json.Marshal(map[string]any{
		"candidate_repo":   dir,
		"candidate_module": ".",
		"binaries":         map[string]string{layout.Service: layout.CandidatePackage},
		"suite_repo":       dir,
		"suite_commit":     suiteCommit,
		"suite_module":     scaffold.SuiteDir,
		"suite_command":    layout.SuiteCommand,
		"timeout":          defaultProjectVerifyTimeout.String(),
	})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "verification.json")
	require.NoError(t, os.WriteFile(path, settings, 0o600))
	require.NoError(t, runProjectSettings("verification", []string{"set", path}, io.Discard))
}

// assertVerifyRefusedWithoutBuild proposes and confirms changeID through MCP
// on a server without stored verification settings, and checks that
// verify_change of candidateCommit is refused, naming the build the user
// starts, and that runsDir holds no run of changeID for owner.
func assertVerifyRefusedWithoutBuild(t *testing.T, ctx context.Context, client *genmcpsurla.Client, runsDir, owner, changeID, candidateCommit string) {
	t.Helper()
	args, err := gensurla.EncodeProposeChangePayload(&gensurla.ProposeChangePayload{
		ChangeID: changeID, Prompt: "Keep answering price questions.",
		Analysis: &gensurla.CP1Analysis{
			Summary:     "The helper keeps answering price questions through the price API.",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
			Examples: []*gensurla.CP1AnalysisExample{{
				Scenario: "Price question", InputSummary: "Ask for an item's price.",
				ExpectedOutcome: "Return its price.", VerificationKind: "deterministic",
			}},
			PreservedBehaviors: []string{"The price API remains available."},
		},
	})
	require.NoError(t, err)
	proposed, err := gensurla.DecodeChangeCardResult(trustedSuiteCall(t, ctx, client, "propose_change", args))
	require.NoError(t, err)
	args, err = gensurla.EncodeConfirmIntentPayload(&gensurla.ConfirmIntentPayload{
		ChangeID: changeID, RequestRevision: proposed.RequestRevision, Answers: map[string]string{}, Examples: proposed.Examples,
	})
	require.NoError(t, err)
	card, err := gensurla.DecodeChangeCardResult(trustedSuiteCall(t, ctx, client, "confirm_intent", args))
	require.NoError(t, err)
	args, err = gensurla.EncodeVerifyChangePayload(&gensurla.VerifyChangePayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, CandidateCommit: candidateCommit,
	})
	require.NoError(t, err)
	result, err := client.ToolsCall(ctx, &genmcpsurla.ToolsCallPayload{Name: "verify_change", Arguments: args})
	require.NoError(t, err)
	complete, ok := result.Outcome.AsComplete()
	require.True(t, ok)
	require.True(t, complete.IsError != nil && *complete.IsError, "verify_change without a build is refused")
	assert.Contains(t, toolErrorText(complete.Content), "the user has not started a build of change \""+changeID+"\"")
	runs, err := verifyrun.List(runsDir, owner, changeID)
	require.NoError(t, err)
	assert.Empty(t, runs, "no run is accepted")
}

// writeStarterPromotion stores promotion settings through CLI management before the
// next server start. Reports use project-owned storage and staged releases remain
// outside the workspace. Suite identity matches the starter evaluator contract.
func writeStarterPromotion(t *testing.T, workspace, service, suiteCommit string) promotionFile {
	t.Helper()
	t.Chdir(workspace)
	state, err := fixtureProjectState(workspace)
	require.NoError(t, err)
	promotion := promotionFile{
		ReportDir:   filepath.Join(state.Dir, "reports"),
		ReleaseDir:  filepath.Join(t.TempDir(), "releases"),
		SuiteID:     service + "_acceptance",
		SuiteCommit: suiteCommit,
		Environment: "scripted-model",
		Binaries:    []string{service},
	}
	require.NoError(t, os.Mkdir(promotion.ReleaseDir, 0o700))
	raw, err := json.Marshal(promotion)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "promotion.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	require.NoError(t, runProjectSettings("promotion", []string{"set", path}, io.Discard))
	return promotion
}

// assertTrustedSuiteJourney starts and stops a real live candidate, then walks
// the Change Card journey a coding agent follows through MCP on a server with
// no model provider key: propose_change with the agent's own analysis (one
// open question), confirm_intent with the answer, preview_architecture of the
// workspace design, verify_change of the candidate, and approve_merge of the
// passed report against the operator's promotion configuration. It checks the
// saved plan and full report retain the operator's suite commit and the
// confirmed revision. The operator stages the binaries that verification
// built and measured for the candidate commit, copied from the run's retained
// work directory into a complete <release_dir>/<commit>/ published by one
// rename; nothing is rebuilt, and no suite payload is replaced or executed
// outside the existing candidate sandbox.
func assertTrustedSuiteJourney(t *testing.T, client *genmcpsurla.Client,
	cardsDir, runsDir, candidateCommit, suiteCommit, changeID string, promotion promotionFile,
) {
	t.Helper()
	liveCtx, cancelLive := context.WithTimeout(t.Context(), journeyOwnerWait)
	defer cancelLive()
	args, err := gensurla.EncodeStartLiveTargetPayload(&gensurla.StartLiveTargetPayload{CandidateCommit: candidateCommit})
	require.NoError(t, err)
	started, err := gensurla.DecodeLiveTargetResult(trustedSuiteCall(t, liveCtx, client, "start_live_target", args))
	require.NoError(t, err)
	assert.Equal(t, candidateCommit, started.CandidateCommit)
	assert.NotEmpty(t, started.URL)
	liveDirs, err := verifyrun.LeftoverLiveDirs(filepath.Join(cardsDir, "live"))
	require.NoError(t, err)
	require.Len(t, liveDirs, 1)
	planJSON, err := os.ReadFile(filepath.Join(liveDirs[0], "live.json"))
	require.NoError(t, err)
	var live verifyrun.LiveRequest
	require.NoError(t, json.Unmarshal(planJSON, &live))
	assert.Equal(t, candidateCommit, live.Plan.CandidateCommit)
	assert.Equal(t, suiteCommit, live.Plan.SuiteCommit)
	stopCtx, cancelStop := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancelStop()
	stopped, err := gensurla.DecodeStopLiveTargetResult(trustedSuiteCall(t, stopCtx, client, "stop_live_target", []byte(`{}`)))
	require.NoError(t, err)
	assert.True(t, stopped.Stopped)
	liveDirs, err = verifyrun.LeftoverLiveDirs(filepath.Join(cardsDir, "live"))
	require.NoError(t, err)
	assert.Empty(t, liveDirs, "stopping the live target removes its processes and directory")

	phase, cancel := context.WithTimeout(t.Context(), journeyOwnerWait)
	defer cancel()
	args, err = gensurla.EncodeProposeChangePayload(&gensurla.ProposeChangePayload{
		ChangeID: changeID, Prompt: "Keep answering price questions.",
		Analysis: &gensurla.CP1Analysis{
			Summary: "The helper keeps answering price questions through the price API.",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{{
				Question: "Should the helper quote prices for items that are out of stock?",
				Options:  []string{"Quote the price", "Say the item is unavailable"},
			}},
			Examples: []*gensurla.CP1AnalysisExample{{
				Scenario: "Price question", InputSummary: "Ask for an item's price.",
				ExpectedOutcome: "Return its price.", VerificationKind: "deterministic",
			}},
			PreservedBehaviors: []string{"The price API remains available."},
		},
	})
	require.NoError(t, err)
	proposed, err := gensurla.DecodeChangeCardResult(trustedSuiteCall(t, phase, client, "propose_change", args))
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusNeedsConfirmation, proposed.Status)
	require.Len(t, proposed.Ambiguities, 1)
	require.Len(t, proposed.Examples, 1)
	assert.Equal(t, "check_01", proposed.Examples[0].ID, "Surla assigns the example ID")
	args, err = gensurla.EncodeConfirmIntentPayload(&gensurla.ConfirmIntentPayload{
		ChangeID: changeID, RequestRevision: proposed.RequestRevision,
		Answers:  map[string]string{proposed.Ambiguities[0].ID: "Quote the price"},
		Examples: proposed.Examples,
	})
	require.NoError(t, err)
	card, err := gensurla.DecodeChangeCardResult(trustedSuiteCall(t, phase, client, "confirm_intent", args))
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusConfirmed, card.Status)
	args, err = gensurla.EncodePreviewArchitecturePayload(&gensurla.PreviewArchitecturePayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
	})
	require.NoError(t, err)
	preview, err := gensurla.DecodeArchitecturePreviewResult(trustedSuiteCall(t, phase, client, "preview_architecture", args))
	require.NoError(t, err)
	assert.Equal(t, card.RequestRevision, preview.RequestRevision)
	assert.NotEmpty(t, preview.Containers, "the starter's design declares its service")
	args, err = gensurla.EncodeVerifyChangePayload(&gensurla.VerifyChangePayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, CandidateCommit: candidateCommit,
	})
	require.NoError(t, err)
	run, err := gensurla.DecodeVerificationStatusResult(trustedSuiteCall(t, phase, client, "verify_change", args))
	require.NoError(t, err)
	require.NotEmpty(t, run.RunID)
	runID := run.RunID
	// A failed assertion must not remove t.TempDir while a detached run is
	// still writing; the run's own saved timeout stops it.
	defer drainTrustedSuiteRun(runsDir, runID, cardsDir, runsDir)
	args, err = gensurla.EncodeGetVerificationPayload(&gensurla.GetVerificationPayload{RunID: runID})
	require.NoError(t, err)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var done *gensurla.VerificationCompleted
	for {
		require.Equal(t, runID, run.RunID)
		assert.Equal(t, card.ChangeID, run.ChangeID)
		assert.Equal(t, card.RequestRevision, run.RequestRevision)
		assert.Equal(t, candidateCommit, run.CandidateCommit)
		assert.Equal(t, suiteCommit, run.SuiteCommit)
		assert.Equal(t, gensurla.NewTestSourceEvaluator(&gensurla.OperatorEvaluatorTests{}), run.TestSource, "the operator's evaluator runs")
		if completed, ok := run.State.AsCompleted(); ok {
			done = completed
			break
		}
		if failed, ok := run.State.AsNotCompleted(); ok {
			t.Fatalf("verification ended without a report: %+v", failed)
		}
		_, running := run.State.AsRunning()
		require.True(t, running, "unexpected verification state")
		select {
		case <-phase.Done():
			t.Fatalf("verification phase did not finish: %v", phase.Err())
		case <-ticker.C:
		}
		run, err = gensurla.DecodeVerificationStatusResult(trustedSuiteCall(t, phase, client, "get_verification", args))
		require.NoError(t, err)
	}
	require.NotNil(t, done.Report)
	require.True(t, done.Report.Passed, "starter's scripted evaluator must pass")
	req, saved, err := verifyrun.Read(runsDir, run.RunID)
	require.NoError(t, err)
	require.Equal(t, verifyrun.StateCompleted, saved.Kind)
	assert.Equal(t, "operator@example.com", req.Owner)
	assert.Equal(t, card.ChangeID, req.ChangeID)
	assert.Equal(t, candidateCommit, req.Plan.CandidateCommit)
	assert.Equal(t, suiteCommit, req.Plan.SuiteCommit)
	assert.Equal(t, card.RequestRevision, req.Plan.RequestRevision)
	assert.Equal(t, promotion.ReportDir, req.Plan.ReportDir, "verification writes reports where the promotion target reads them")
	assert.Equal(t, candidateCommit, saved.Report.Binding.CandidateCommit)
	assert.Equal(t, suiteCommit, saved.Report.Binding.SuiteCommit)
	assert.Equal(t, card.RequestRevision, saved.Report.Binding.RequestRevision)
	assert.Equal(t, saved.Report.Digest, done.Report.ReportDigest)

	// The operator stages the binaries this verification built and measured
	// for the candidate commit, from the run's retained work directory, as one
	// complete <release_dir>/<commit>/ directory.
	staged := make(map[string]string, len(promotion.Binaries))
	for _, name := range promotion.Binaries {
		built, err := os.ReadFile(filepath.Join(req.Plan.WorkDir, "candidate-bin", name))
		require.NoError(t, err)
		require.Equal(t, saved.Report.Binding.Artifacts[name], digestOf(string(built)),
			"the staged %s is the binary the report measured", name)
		staged[name] = string(built)
	}
	publish(t, promotion.ReleaseDir, candidateCommit, staged)

	args, err = gensurla.EncodeApproveMergePayload(&gensurla.ApproveMergePayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision,
		CandidateCommit: candidateCommit, ReportDigest: done.Report.ReportDigest,
	})
	require.NoError(t, err)
	approved, err := gensurla.DecodeChangeCardResult(trustedSuiteCall(t, phase, client, "approve_merge", args))
	require.NoError(t, err)
	assert.Equal(t, card.RequestRevision, approved.RequestRevision, "approval does not change the card's revision")
	want := &gensurla.ApprovalItem{
		ApprovedBy:      "operator@example.com",
		ReportDigest:    done.Report.ReportDigest,
		CandidateCommit: candidateCommit,
		SuiteID:         promotion.SuiteID,
		SuiteCommit:     suiteCommit,
		Environment:     promotion.Environment,
		Artifacts:       saved.Report.Binding.Artifacts,
		ScenarioCount:   saved.Report.Counts.Required,
	}
	assert.Equal(t, want, approved.Approval)
	assert.Nil(t, approved.UnverifiedApproval)
	assert.Equal(t, promotion.SuiteID, saved.Report.Binding.SuiteID)
	assert.Equal(t, promotion.Environment, saved.Report.Binding.Environment)

	args, err = gensurla.EncodeGetChangePayload(&gensurla.GetChangePayload{ChangeID: card.ChangeID})
	require.NoError(t, err)
	read, err := gensurla.DecodeChangeCardResult(trustedSuiteCall(t, phase, client, "get_change", args))
	require.NoError(t, err)
	assert.Equal(t, card.RequestRevision, read.RequestRevision)
	assert.Equal(t, want, read.Approval, "the saved approval still matches the staged release")
	assert.Nil(t, read.UnverifiedApproval)
}

// closeRetaining closes the server, which stops its live target and removes
// its check containers. If Close fails or does not return within 45 seconds,
// a process may still use dirs, so it keeps them by exiting the test binary
// before t.TempDir cleanup.
func closeRetaining(cp *controlplane.ControlPlane, dirs ...string) {
	done := make(chan error, 1)
	go func() { done <- cp.Close() }()
	select {
	case err := <-done:
		if err != nil {
			retainAndExit(fmt.Errorf("close the server: %w", err), dirs...)
		}
	case <-time.After(45 * time.Second):
		retainAndExit(errors.New("the server did not close within 45s"), dirs...)
	}
}

// retainAndExit prints the kept directories and the reason, then exits the
// test binary so t.TempDir cleanup cannot delete what a still-running process
// may use.
func retainAndExit(reason error, dirs ...string) {
	fmt.Fprintf(os.Stderr, "e2e: kept %s: %v\n", strings.Join(dirs, ", "), reason)
	os.Exit(2)
}

// drainTrustedSuiteRun waits, even after an assertion failed, until the run
// has a saved report, the only terminal state that shows its supervisor
// finished and cleaned up, so t.TempDir cleanup never removes directories a
// detached run still uses. A run without a report may also be a supervisor
// that exited before writing its result, so its cleanup is uncertain; that,
// an unreadable or unexpected state, or no report within journeyOwnerWait
// keeps dirs by exiting the test binary before that cleanup.
func drainTrustedSuiteRun(runsDir, runID string, dirs ...string) {
	deadline := time.NewTimer(journeyOwnerWait)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, state, err := verifyrun.Read(runsDir, runID)
		if err != nil {
			retainAndExit(fmt.Errorf("read verification run %s: %w", runID, err), dirs...)
		}
		switch state.Kind {
		case verifyrun.StateCompleted:
			return
		case verifyrun.StateNotCompleted:
			retainAndExit(fmt.Errorf("verification run %s ended without a report, so its process cleanup is unconfirmed: %s", runID, state.Result.Summary), dirs...)
		case verifyrun.StateRunning:
		default:
			retainAndExit(fmt.Errorf("verification run %s is in unexpected state %s", runID, state.Kind), dirs...)
		}
		select {
		case <-deadline.C:
			retainAndExit(fmt.Errorf("verification run %s did not finish within %s", runID, journeyOwnerWait), dirs...)
		case <-ticker.C:
		}
	}
}
