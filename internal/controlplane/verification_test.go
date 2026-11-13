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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

func TestVerifyChangeAcceptsOnlyTheOwnersCurrentConfirmedRevision(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	bob := WithPrincipal(context.Background(), Principal{ID: "bob@example.com", CallerID: "bob@example.com", Kind: CallerUser})
	commit := strings.Repeat("a", 40)
	cp, card, cfg := verificationSetup(t)

	start := func(ctx context.Context, revision string) (*gensurla.VerificationStatusResult, error) {
		return cp.VerifyChange(ctx, &gensurla.VerifyChangePayload{ChangeID: "chg", RequestRevision: revision, CandidateCommit: commit})
	}
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		rev     string
		wantErr string
	}{
		{"another principal", bob, card.RequestRevision, "is not the owner"},
		{"stale revision", alice, "rev-old", "request_revision mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := start(tc.ctx, tc.rev)
			assert.ErrorContains(t, err, tc.wantErr)
		})
	}
	t.Run("unknown change", func(t *testing.T) {
		_, err := cp.VerifyChange(alice, &gensurla.VerifyChangePayload{ChangeID: "nope", RequestRevision: "r", CandidateCommit: commit})
		assert.ErrorContains(t, err, `unknown change_id "nope"`)
	})
	t.Run("no verification configured", func(t *testing.T) {
		unconfigured := newControlPlane(t, Options{})
		_, err := unconfigured.VerifyChange(alice, &gensurla.VerifyChangePayload{ChangeID: "chg", RequestRevision: "r", CandidateCommit: commit})
		assert.ErrorIs(t, err, errVerificationUnavailable)
		_, err = unconfigured.GetVerification(alice, &gensurla.GetVerificationPayload{RunID: verifyrun.NewRunID()})
		assert.ErrorIs(t, err, errVerificationUnavailable)
	})

	t.Run("accepted run carries the operator's suite and the caller's commit and revision", func(t *testing.T) {
		started, err := start(alice, card.RequestRevision)
		require.NoError(t, err)
		assert.Equal(t, "chg", started.ChangeID)
		assert.Equal(t, card.RequestRevision, started.RequestRevision)
		assert.Equal(t, commit, started.CandidateCommit)
		assert.Equal(t, cfg.Plan.SuiteCommit, started.SuiteCommit)
		assert.Equal(t, gensurla.StateKindRunning, started.State.Kind())

		saved, err := os.ReadFile(filepath.Join(cfg.RunsDir, started.RunID, "request.json"))
		require.NoError(t, err)
		var req verifyrun.Request
		require.NoError(t, json.Unmarshal(saved, &req))
		assert.Equal(t, "alice@example.com", req.Owner)
		assert.Equal(t, cfg.Plan.SuiteRepo, req.Plan.SuiteRepo, "the suite comes from the operator, never the caller")
		assert.Equal(t, cfg.Plan.Binaries, req.Plan.Binaries)

		_, err = cp.GetVerification(bob, &gensurla.GetVerificationPayload{RunID: started.RunID})
		assert.ErrorContains(t, err, "is not the owner of run_id")
	})
}

func TestGetVerificationReadsTheRunFromDiskInEveryState(t *testing.T) {
	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cp, card, cfg := verificationSetup(t)
	commit := strings.Repeat("a", 40)
	inputs := PromotionInputs{SuiteID: "orders_acceptance", SuiteCommit: cfg.Plan.SuiteCommit, Environment: "sim",
		Artifacts: map[string]string{"orders": "d1"}}

	// writeRun saves a finished run as its supervisor would.
	writeRun := func(t *testing.T, result string) string {
		t.Helper()
		runID := verifyrun.NewRunID()
		dir := filepath.Join(cfg.RunsDir, runID)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		plan := cfg.Plan
		plan.CandidateCommit, plan.RequestRevision, plan.Tests = commit, card.RequestRevision, verify.TestsEvaluator
		req, err := json.Marshal(verifyrun.Request{RunID: runID, Owner: "alice@example.com", ChangeID: "chg",
			AcceptedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), Plan: plan})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "request.json"), req, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "lock"), nil, 0o600))
		if result != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "result.json"), []byte(result), 0o600))
		}
		return runID
	}
	get := func(t *testing.T, runID string) *gensurla.VerificationStatusResult {
		t.Helper()
		res, err := cp.GetVerification(alice, &gensurla.GetVerificationPayload{RunID: runID})
		require.NoError(t, err)
		assert.Equal(t, card.RequestRevision, res.RequestRevision)
		assert.Equal(t, commit, res.CandidateCommit)
		return res
	}

	t.Run("completed returns only the builder view", func(t *testing.T) {
		report := failingReport(t, card.RequestRevision, commit, inputs)
		b, err := json.Marshal(report)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(cfg.Plan.ReportDir, report.Digest+".json"), b, 0o600))
		res := get(t, writeRun(t, `{"kind":"report","report_digest":"`+report.Digest+`"}`))
		completed, ok := res.State.AsCompleted()
		require.True(t, ok)
		assert.Equal(t, "orders_acceptance", completed.SuiteID)
		assert.Equal(t, "sim", completed.Environment)
		assert.False(t, completed.Report.Passed)
		assert.Equal(t, report.Digest, completed.Report.ReportDigest)
		assert.Equal(t, &gensurla.VerificationCounts{Required: 2, Passed: 1, Failed: 1}, completed.Report.Counts)
		require.Len(t, completed.Report.Scenarios, 2)
		assert.Equal(t, "a", completed.Report.Scenarios[0].ID)
		assert.Equal(t, "holdout-1", completed.Report.Scenarios[1].ID, "holdout names never reach builders")
		assert.Empty(t, completed.Report.Scenarios[1].Checks)
		wire, err := json.Marshal(res)
		require.NoError(t, err)
		assert.NotContains(t, string(wire), `"h"`)
	})

	t.Run("not completed returns where it stopped", func(t *testing.T) {
		res := get(t, writeRun(t, `{"kind":"no_report","failure":"execution_failed","stage":"build","summary":"The candidate did not build."}`))
		failed, ok := res.State.AsNotCompleted()
		require.True(t, ok)
		assert.Equal(t, &gensurla.VerificationNotCompleted{Kind: "execution_failed", Stage: "build", Summary: "The candidate did not build."}, failed)
	})

	t.Run("a run whose processes ended without a result is interrupted", func(t *testing.T) {
		res := get(t, writeRun(t, ""))
		failed, ok := res.State.AsNotCompleted()
		require.True(t, ok)
		assert.Equal(t, "interrupted", failed.Kind)
	})

	t.Run("a new control plane reads runs started by an earlier one", func(t *testing.T) {
		runID := writeRun(t, `{"kind":"no_report","failure":"interrupted","stage":"supervise","summary":"The verification was canceled."}`)
		restarted := newControlPlane(t, Options{Verification: cfg})
		res, err := restarted.GetVerification(alice, &gensurla.GetVerificationPayload{RunID: runID})
		require.NoError(t, err, "the run does not depend on the saved Change Card")
		assert.Equal(t, "chg", res.ChangeID)
	})

	t.Run("unknown run", func(t *testing.T) {
		_, err := cp.GetVerification(alice, &gensurla.GetVerificationPayload{RunID: verifyrun.NewRunID()})
		assert.ErrorContains(t, err, "unknown run_id")
	})
}

func TestGetVerificationSafeErrors(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	recorder := &recordingTracerProvider{}
	otel.SetTracerProvider(recorder)
	t.Cleanup(func() { otel.SetTracerProvider(prevProvider) })

	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	cp, card, cfg := verificationSetup(t)
	commit := strings.Repeat("a", 40)

	t.Run("foreign run with malformed accepted_at still reports non-owner without leaking validation error", func(t *testing.T) {
		recorder.reset()
		runID := verifyrun.NewRunID()
		dir := filepath.Join(cfg.RunsDir, runID)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		plan := cfg.Plan
		plan.CandidateCommit, plan.RequestRevision, plan.Tests = commit, card.RequestRevision, verify.TestsEvaluator
		raw, err := json.Marshal(verifyrun.Request{
			RunID:    runID,
			Owner:    "bob@example.com",
			ChangeID: "chg",
			Plan:     plan,
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "request.json"), raw, 0o600))

		_, err = cp.GetVerification(alice, &gensurla.GetVerificationPayload{RunID: runID})
		require.Error(t, err)
		assert.ErrorContains(t, err, "is not the owner of run_id")
		assert.NotContains(t, err.Error(), "accepted_at")
		assert.NotContains(t, err.Error(), cfg.RunsDir)
	})

	t.Run("empty object request.json records raw error on span and returns safe public error", func(t *testing.T) {
		recorder.reset()
		runID := verifyrun.NewRunID()
		dir := filepath.Join(cfg.RunsDir, runID)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "request.json"), []byte(`{}`), 0o600))

		_, err := cp.GetVerification(alice, &gensurla.GetVerificationPayload{RunID: runID})
		require.Error(t, err)
		assert.Equal(t, `read verification "`+runID+`": saved verification record could not be read`, err.Error())
		assert.NotContains(t, err.Error(), cfg.RunsDir)

		spans := recorder.matching("surla.controlplane.get_verification")
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Error, spans[0].statusCode)
		require.Len(t, spans[0].errors, 1)
		assert.ErrorContains(t, spans[0].errors[0], "does not match directory")
	})

	t.Run("missing report file records evaluator path on span and returns safe public error", func(t *testing.T) {
		recorder.reset()
		runID := verifyrun.NewRunID()
		dir := filepath.Join(cfg.RunsDir, runID)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		plan := cfg.Plan
		plan.CandidateCommit, plan.RequestRevision, plan.Tests = commit, card.RequestRevision, verify.TestsEvaluator
		raw, err := json.Marshal(verifyrun.Request{
			RunID:      runID,
			Owner:      "alice@example.com",
			ChangeID:   "chg",
			AcceptedAt: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC),
			Plan:       plan,
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "request.json"), raw, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "lock"), nil, 0o600))
		missingDigest := strings.Repeat("f", 64)
		require.NoError(t, os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"kind":"report","report_digest":"`+missingDigest+`"}`), 0o600))

		_, err = cp.GetVerification(alice, &gensurla.GetVerificationPayload{RunID: runID})
		require.Error(t, err)
		assert.Equal(t, `read verification "`+runID+`": saved verification record could not be read`, err.Error())
		assert.NotContains(t, err.Error(), cfg.Plan.ReportDir)

		spans := recorder.matching("surla.controlplane.get_verification")
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Error, spans[0].statusCode)
		require.Len(t, spans[0].errors, 1)
		assert.ErrorContains(t, spans[0].errors[0], missingDigest)
	})
}

func TestListVerifications(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	recorder := &recordingTracerProvider{}
	otel.SetTracerProvider(recorder)
	t.Cleanup(func() { otel.SetTracerProvider(prevProvider) })

	alice := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	bob := WithPrincipal(context.Background(), Principal{ID: "bob@example.com", CallerID: "bob@example.com", Kind: CallerUser})
	cp, card, cfg := verificationSetup(t)
	commit := strings.Repeat("a", 40)
	inputs := PromotionInputs{
		SuiteID: "orders_acceptance", SuiteCommit: cfg.Plan.SuiteCommit, Environment: "sim",
		Artifacts: map[string]string{"orders": "d1"},
	}

	t.Run("authorization and configuration guards", func(t *testing.T) {
		_, err := cp.ListVerifications(context.Background(), &gensurla.ListVerificationsPayload{ChangeID: "chg"})
		assert.ErrorContains(t, err, "authenticated principal is required")

		unconfigured := newControlPlane(t, Options{})
		_, err = unconfigured.ListVerifications(alice, &gensurla.ListVerificationsPayload{ChangeID: "chg"})
		assert.ErrorIs(t, err, errVerificationUnavailable)

		_, err = cp.ListVerifications(alice, &gensurla.ListVerificationsPayload{ChangeID: "unknown-card"})
		assert.ErrorContains(t, err, `unknown change_id "unknown-card"`)

		_, err = cp.ListVerifications(bob, &gensurla.ListVerificationsPayload{ChangeID: "chg"})
		assert.ErrorContains(t, err, `principal "bob@example.com" is not the owner of change_id "chg"`)
	})

	t.Run("empty list when no runs exist yet", func(t *testing.T) {
		res, err := cp.ListVerifications(alice, &gensurla.ListVerificationsPayload{ChangeID: "chg"})
		require.NoError(t, err)
		assert.Equal(t, "chg", res.ChangeID)
		assert.NotNil(t, res.Runs)
		assert.Empty(t, res.Runs)
	})

	t.Run("lists owner runs newest-first with sub-second accepted_at, builder projection, and a waiting proposal", func(t *testing.T) {
		report := failingReport(t, card.RequestRevision, commit, inputs)
		b, err := json.Marshal(report)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(cfg.Plan.ReportDir, report.Digest+".json"), b, 0o600))

		writeCompletedRun := func(runID string, acceptedAt time.Time) {
			dir := filepath.Join(cfg.RunsDir, runID)
			require.NoError(t, os.MkdirAll(dir, 0o700))
			plan := cfg.Plan
			plan.CandidateCommit, plan.RequestRevision, plan.Tests = commit, card.RequestRevision, verify.TestsEvaluator
			raw, err := json.Marshal(verifyrun.Request{
				RunID:      runID,
				Owner:      "alice@example.com",
				ChangeID:   "chg",
				AcceptedAt: acceptedAt,
				Plan:       plan,
			})
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(dir, "request.json"), raw, 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "lock"), nil, 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"kind":"report","report_digest":"`+report.Digest+`"}`), 0o600))
		}

		runEarly := "10000000000000000000000000000001"
		runLate := "20000000000000000000000000000002"
		writeCompletedRun(runEarly, time.Date(2026, 10, 31, 12, 0, 0, 100_000_000, time.UTC))
		writeCompletedRun(runLate, time.Date(2026, 10, 31, 12, 0, 0, 900_000_000, time.UTC))

		// Revise the card into needs_confirmation; prior R1 runs must remain discoverable.
		priorID := "check_01"
		revised, err := cp.ReviseChange(alice, &gensurla.ReviseChangePayload{
			ChangeID:        "chg",
			RequestRevision: card.RequestRevision,
			Prompt:          "Require a refund reason.",
			Analysis: &gensurla.CP1RevisionAnalysis{
				Summary: "Refunds up to $50 require a reason.",
				Ambiguities: []*gensurla.CP1AnalysisAmbiguity{{
					Question: "Which reasons are allowed?",
					Options:  []string{"damaged_item", "any_reason"},
				}},
				Examples: []*gensurla.CP1RevisionAnalysisExample{{
					PriorID:          &priorID,
					Scenario:         "Refund at the limit",
					InputSummary:     "Order total $50.00",
					ExpectedOutcome:  "Instant store credit",
					VerificationKind: "deterministic",
				}},
				RemovedExampleIds:  []string{},
				PreservedBehaviors: []string{},
			},
		})
		require.NoError(t, err)
		require.Equal(t, StatusConfirmed, revised.Status, "the agreed revision stays in force")
		require.NotNil(t, revised.ProposedRevision, "the revision waits as a proposal")

		res, err := cp.ListVerifications(alice, &gensurla.ListVerificationsPayload{ChangeID: "chg"})
		require.NoError(t, err)
		require.Len(t, res.Runs, 2)
		assert.Equal(t, runLate, res.Runs[0].RunID)
		assert.Equal(t, "2026-10-31T12:00:00.9Z", res.Runs[0].AcceptedAt)
		assert.Equal(t, card.RequestRevision, res.Runs[0].RequestRevision)
		assert.Equal(t, runEarly, res.Runs[1].RunID)
		assert.Equal(t, "2026-10-31T12:00:00.1Z", res.Runs[1].AcceptedAt)

		completed, ok := res.Runs[0].State.AsCompleted()
		require.True(t, ok)
		require.Len(t, completed.Report.Scenarios, 2)
		assert.Equal(t, "holdout-1", completed.Report.Scenarios[1].ID)
		assert.Empty(t, completed.Report.Scenarios[1].Checks)
	})

	t.Run("corrupt published request.json records raw error on span and returns safe public error", func(t *testing.T) {
		recorder.reset()
		corruptID := "30000000000000000000000000000003"
		corruptDir := filepath.Join(cfg.RunsDir, corruptID)
		require.NoError(t, os.MkdirAll(corruptDir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(corruptDir, "request.json"), []byte(`{"secret_host_path":"/var/lib/secret`), 0o600))

		_, err := cp.ListVerifications(alice, &gensurla.ListVerificationsPayload{ChangeID: "chg"})
		require.Error(t, err)
		assert.Equal(t, `list verifications for "chg": saved verification records could not be read`, err.Error())
		assert.NotContains(t, err.Error(), cfg.RunsDir)
		assert.NotContains(t, err.Error(), "secret_host_path")

		spans := recorder.matching("surla.controlplane.list_verifications")
		require.Len(t, spans, 1)
		assert.Equal(t, codes.Error, spans[0].statusCode)
		require.Len(t, spans[0].errors, 1)
		assert.ErrorContains(t, spans[0].errors[0], "decode run request")
	})
}

// verificationSetup returns a control plane with a verification
// configuration whose runs are supervised by a command that exits at once,
// and a confirmed card owned by alice.
func verificationSetup(t *testing.T) (*ControlPlane, *gensurla.ChangeCardResult, *VerificationConfig) {
	t.Helper()
	analysis := &gensurla.CP1Analysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
		Examples: []*gensurla.CP1AnalysisExample{{
			Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
			ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{},
	}
	cfg := verificationConfig(t)
	cp := newControlPlane(t, Options{
		Verification: cfg,
	})
	card, err := cp.ProposeChange(WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser}),
		&gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: analysis})
	require.NoError(t, err)
	card = mustConfirmNoop(t, cp, WithPrincipal(t.Context(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser}), card)
	require.Equal(t, StatusConfirmed, card.Status)
	return cp, card, cfg
}

// verificationConfig returns a verification configuration whose runs are
// supervised by a command that exits at once.
func verificationConfig(t *testing.T) *VerificationConfig {
	t.Helper()
	return &VerificationConfig{
		RunsDir: t.TempDir(),
		Plan: verify.Plan{
			CandidateRepo: "/repos/candidate", CandidateModule: ".", Binaries: map[string]string{"orders": "./cmd/orders"},
			SuiteRepo: "/repos/suite", SuiteCommit: strings.Repeat("b", 40), SuiteModule: "eval",
			SuiteCommand: "./cmd/orders-evals", ReportDir: t.TempDir(),
		},
		// The supervisor exits at once; these tests cover acceptance and
		// reading, and verifyrun's own tests run the real lifecycle.
		Command: []string{"/bin/sh", "-c", "exit 0", "sh"},
		Timeout: time.Hour,
	}
}
