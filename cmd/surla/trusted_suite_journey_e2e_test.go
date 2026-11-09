//go:build e2e

// This file supplies the real verification subprocess entrypoint and fixtures
// for the starter's authenticated MCP journey, without a second scaffold build.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	genmcpsurla "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/goadesign/surla/internal/verifyrun"
)

type (
	// trustedSuiteAnalyzer supplies one confirmed public example. This test
	// checks evaluator ownership, not semantic analysis or provider quality.
	trustedSuiteAnalyzer struct{}
)

// TestMain lets selfCommand launch the test executable's real verify-run
// implementation. Other invocations retain the ordinary Go testing entrypoint.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "verify-run" {
		if len(os.Args) == 4 && os.Args[2] == "supervise" {
			// Bound detached test runs while preserving the supervisor's actual
			// SIGTERM handling, which stops and reaps its executor process group.
			watchdog := time.AfterFunc(4*time.Minute, func() {
				if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			})
			err := runVerifyRun(os.Args[2:])
			watchdog.Stop()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			os.Exit(0)
		}
		if err := runVerifyRun(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

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

// assertTrustedSuiteJourney starts and stops a real live candidate, then
// verifies that candidate through MCP and checks the saved plan and full report
// retain the operator's suite commit. No suite payload is replaced or executed
// outside the existing candidate sandbox.
func assertTrustedSuiteJourney(t *testing.T, client *genmcpsurla.Client,
	cardsDir, runsDir, candidateCommit, suiteCommit, changeID string,
) {
	t.Helper()
	liveCtx, cancelLive := context.WithTimeout(t.Context(), 4*time.Minute)
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

	phase, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	args, err = gensurla.EncodeProposeChangePayload(&gensurla.ProposeChangePayload{
		ChangeID: changeID, Prompt: "Keep answering price questions.",
	})
	require.NoError(t, err)
	card, err := gensurla.DecodeChangeCardResult(trustedSuiteCall(t, phase, client, "propose_change", args))
	require.NoError(t, err)
	require.Equal(t, controlplane.StatusConfirmed, card.Status)
	args, err = gensurla.EncodeVerifyChangePayload(&gensurla.VerifyChangePayload{
		ChangeID: card.ChangeID, RequestRevision: card.RequestRevision, CandidateCommit: candidateCommit,
	})
	require.NoError(t, err)
	run, err := gensurla.DecodeVerificationStatusResult(trustedSuiteCall(t, phase, client, "verify_change", args))
	require.NoError(t, err)
	require.NotEmpty(t, run.RunID)
	runID := run.RunID
	// A failed assertion must not remove t.TempDir while a detached run is
	// still writing. The child watchdog drives real supervisor cleanup.
	defer drainTrustedSuiteRun(t, runsDir, runID)
	args, err = gensurla.EncodeGetVerificationPayload(&gensurla.GetVerificationPayload{RunID: runID})
	require.NoError(t, err)
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		require.Equal(t, runID, run.RunID)
		assert.Equal(t, card.ChangeID, run.ChangeID)
		assert.Equal(t, card.RequestRevision, run.RequestRevision)
		assert.Equal(t, candidateCommit, run.CandidateCommit)
		assert.Equal(t, suiteCommit, run.SuiteCommit)
		if done, ok := run.State.AsCompleted(); ok {
			require.NotNil(t, done.Report)
			assert.True(t, done.Report.Passed, "starter's scripted evaluator must pass")
			req, saved, err := verifyrun.Read(runsDir, run.RunID)
			require.NoError(t, err)
			require.Equal(t, verifyrun.StateCompleted, saved.Kind)
			assert.Equal(t, "operator@example.com", req.Owner)
			assert.Equal(t, card.ChangeID, req.ChangeID)
			assert.Equal(t, candidateCommit, req.Plan.CandidateCommit)
			assert.Equal(t, suiteCommit, req.Plan.SuiteCommit)
			assert.Equal(t, card.RequestRevision, req.Plan.RequestRevision)
			assert.Equal(t, candidateCommit, saved.Report.Binding.CandidateCommit)
			assert.Equal(t, suiteCommit, saved.Report.Binding.SuiteCommit)
			assert.Equal(t, card.RequestRevision, saved.Report.Binding.RequestRevision)
			assert.Equal(t, saved.Report.Digest, done.Report.ReportDigest)
			return
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
}

// Analyze returns a fixed public example with no open questions, so the real
// CP1 persistence path saves a confirmed card without contacting a provider.
func (trustedSuiteAnalyzer) Analyze(context.Context, string, string) (controlplane.CP1SemanticAnalysis, error) {
	return controlplane.CP1SemanticAnalysis{
		Summary: "Keep answering price questions.",
		Examples: []controlplane.CP1ModelExample{{
			Scenario: "Price question", InputSummary: "Ask for an item's price.",
			ExpectedOutcome: "Return its price.", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{"The price API remains available."},
	}, nil
}

// Revise is unused because this test proves the existing evaluator pin, not
// semantic revision behavior.
func (trustedSuiteAnalyzer) Revise(context.Context, controlplane.CP1PriorCard, string, string) (controlplane.CP1RevisionSemanticAnalysis, error) {
	return controlplane.CP1RevisionSemanticAnalysis{}, errors.New("revision is outside this test's journey")
}

// closeTrustedSuiteControlPlane waits for the real live-process owner to stop
// within its documented cleanup interval before test directories are removed.
func closeTrustedSuiteControlPlane(t *testing.T, cp *controlplane.ControlPlane) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cp.Close() }()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(45 * time.Second):
		t.Error("control plane did not finish live process cleanup within 45s")
	}
}

// drainTrustedSuiteRun waits for the owner's terminal state even after an
// assertion fails. It stops immediately on an invalid or unreadable state.
func drainTrustedSuiteRun(t *testing.T, runsDir, runID string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, state, err := verifyrun.Read(runsDir, runID)
		if !assert.NoError(t, err, "read accepted verification during cleanup") {
			return
		}
		switch state.Kind {
		case verifyrun.StateCompleted, verifyrun.StateNotCompleted:
			return
		case verifyrun.StateRunning:
		default:
			t.Errorf("unexpected verification cleanup state: %s", state.Kind)
			return
		}
		select {
		case <-deadline.C:
			t.Error("accepted verification did not finish process cleanup within 5m")
			return
		case <-ticker.C:
		}
	}
}
