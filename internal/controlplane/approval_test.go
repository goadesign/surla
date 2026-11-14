// This file tests Checkpoint 5 (CP5) promotion approval recording
// (`approve_merge`) and read-time verification of saved approvals against the
// chosen tests' commit and the bound PromotionTarget across Go, HTTP, and MCP.

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
	"go.opentelemetry.io/otel/trace/noop"

	genmcpadapter "github.com/goadesign/surla/gen/mcp_surla"
	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
	"goa.design/goa-ai/eval"
)

type (
	// reportSpec holds the suite and binary fields assembleReport binds into a
	// fixture verification report.
	reportSpec struct {
		SuiteID     string
		SuiteCommit string
		Environment string
		Artifacts   map[string]string
	}

	// promotionTargetStub is a PromotionTarget serving one report and fixed
	// binary digests. When loading and release are set, LoadReport signals
	// loading and waits for release, so a test can change the card or its
	// accepted tests mid-approval.
	promotionTargetStub struct {
		// report is the only report LoadReport returns, whatever the digest.
		report verify.Report
		// artifacts is what CurrentArtifacts returns.
		artifacts map[string]string
		// loading, when set, is closed when LoadReport starts.
		loading chan struct{}
		// release, when set, is waited on before LoadReport returns.
		release chan struct{}
		// loadErr, when set, is returned by LoadReport.
		loadErr error
		// artifactsErr, when set, is returned by CurrentArtifacts.
		artifactsErr error
		// artifactsReading, when set, is closed when CurrentArtifacts starts.
		artifactsReading chan struct{}
		// artifactsRelease, when set, is waited on before CurrentArtifacts returns.
		artifactsRelease chan struct{}
	}

	// recordingTracerProvider is an OpenTelemetry TracerProvider that captures
	// spans created during a test without importing external SDK packages.
	recordingTracerProvider struct {
		embedded.TracerProvider
		mu    sync.Mutex
		spans []*recordedSpan
	}

	// recordedEvent captures the name and attributes of one span event.
	recordedEvent struct {
		name  string
		attrs []attribute.KeyValue
	}

	// recordedSpan captures the name, attributes, events, recorded errors, and
	// status of one OpenTelemetry span.
	recordedSpan struct {
		trace.Span
		mu         sync.Mutex
		name       string
		attrs      []attribute.KeyValue
		events     []recordedEvent
		errors     []error
		statusCode codes.Code
		statusDesc string
		ended      bool
	}
)

// TestApproveMergeRecordsApprovalOnlyForTheCheckedRevision verifies CP5:
// approve_merge checks the verification report against the chosen tests'
// commit and the promotion target's current binary digests, records the
// approval on the owner's confirmed card, keeps it across a no-op
// confirmation, and drops it when the revision changes, even when the change
// lands while the report is being loaded.
func TestApproveMergeRecordsApprovalOnlyForTheCheckedRevision(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	bobCtx := WithPrincipal(context.Background(), Principal{ID: "bob@example.com", CallerID: "bob@example.com", Kind: CallerUser})
	const commit = "0123456789abcdef0123456789abcdef01234567"
	analysis := &gensurla.CP1Analysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
		Examples: []*gensurla.CP1AnalysisExample{{
			Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
			ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{},
	}
	spec := reportSpec{
		SuiteID: "orders_acceptance", SuiteCommit: "suite-1", Environment: "sim",
		Artifacts: map[string]string{"orders": "d1"},
	}

	// setup proposes a confirmed card and assembles a passing report bound to
	// its revision and to spec.
	setup := func(t *testing.T) (*ControlPlane, *promotionTargetStub, *gensurla.ChangeCardResult) {
		t.Helper()
		cfg := verificationConfig(t)
		cfg.Plan.SuiteCommit = spec.SuiteCommit
		target := &promotionTargetStub{artifacts: maps.Clone(spec.Artifacts)}
		cp := newControlPlane(t, Options{
			Verification:    cfg,
			PromotionTarget: target,
		})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: analysis})
		require.NoError(t, err)
		card = mustConfirmNoop(t, cp, aliceCtx, card)
		require.Equal(t, StatusConfirmed, card.Status)
		target.report = passingReport(t, card.RequestRevision, commit, spec)
		return cp, target, card
	}
	approveAt := func(ctx context.Context, cp *ControlPlane, revision, candidate, digest string) (*gensurla.ChangeCardResult, error) {
		return cp.ApproveMerge(ctx, &gensurla.ApproveMergePayload{
			ChangeID: "chg", RequestRevision: revision, CandidateCommit: candidate, ReportDigest: digest,
		})
	}
	approve := func(ctx context.Context, cp *ControlPlane, card *gensurla.ChangeCardResult, digest string) (*gensurla.ChangeCardResult, error) {
		return approveAt(ctx, cp, card.RequestRevision, commit, digest)
	}
	reconfirm := func(cp *ControlPlane, card *gensurla.ChangeCardResult, outcome string) (*gensurla.ChangeCardResult, error) {
		ex := *card.Examples[0]
		ex.ExpectedOutcome = outcome
		return cp.ConfirmIntent(aliceCtx, &gensurla.ConfirmIntentPayload{
			ChangeID: "chg", RequestRevision: card.RequestRevision, Answers: map[string]string{}, Examples: []*gensurla.ExampleItem{&ex},
		})
	}

	t.Run("approval is recorded and survives a no-op confirmation", func(t *testing.T) {
		cp, target, card := setup(t)
		approved, err := approve(aliceCtx, cp, card, target.report.Digest)
		require.NoError(t, err)
		assert.Equal(t, &gensurla.ApprovalItem{
			ApprovedBy: "alice@example.com", ReportDigest: target.report.Digest, CandidateCommit: commit,
			SuiteID: "orders_acceptance", SuiteCommit: "suite-1", Environment: "sim",
			Artifacts: map[string]string{"orders": "d1"}, ScenarioCount: 2,
			TestSource: gensurla.NewApprovalTestSourceEvaluator(&gensurla.OperatorEvaluatorTests{}),
		}, approved.Approval)
		assert.Nil(t, approved.UnverifiedApproval)
		assert.Equal(t, card.RequestRevision, approved.RequestRevision, "approval is not part of the revision")
		assert.Nil(t, card.Approval, "the card returned before approval is not changed")

		read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
		require.NoError(t, err)
		assert.Equal(t, approved.Approval, read.Approval)
		assert.Nil(t, read.UnverifiedApproval)

		same, err := reconfirm(cp, card, card.Examples[0].ExpectedOutcome)
		require.NoError(t, err)
		assert.Equal(t, approved.Approval, same.Approval)
		assert.Nil(t, same.UnverifiedApproval)

		changed, err := reconfirm(cp, card, "Manager approval")
		require.NoError(t, err)
		assert.NotEqual(t, card.RequestRevision, changed.RequestRevision)
		assert.Nil(t, changed.Approval, "a new revision drops the approval")
		assert.Nil(t, changed.UnverifiedApproval)
	})

	t.Run("approval without promotion settings loads the report from the verifier and records empty artifacts", func(t *testing.T) {
		cfg := verificationConfig(t)
		cfg.Plan.SuiteCommit = spec.SuiteCommit
		cp := newControlPlane(t, Options{Verification: cfg})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: analysis})
		require.NoError(t, err)
		card = mustConfirmNoop(t, cp, aliceCtx, card)
		report := passingReport(t, card.RequestRevision, commit, spec)
		writeReportFile(t, cfg.Plan.ReportDir, report)

		approved, err := approve(aliceCtx, cp, card, report.Digest)
		require.NoError(t, err)
		assert.Equal(t, &gensurla.ApprovalItem{
			ApprovedBy: "alice@example.com", ReportDigest: report.Digest, CandidateCommit: commit,
			SuiteID: "orders_acceptance", SuiteCommit: "suite-1", Environment: "sim",
			Artifacts: map[string]string{}, ScenarioCount: 2,
			TestSource: gensurla.NewApprovalTestSourceEvaluator(&gensurla.OperatorEvaluatorTests{}),
		}, approved.Approval)
		assert.Nil(t, approved.UnverifiedApproval)

		read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
		require.NoError(t, err)
		assert.Equal(t, approved.Approval, read.Approval)
		assert.Nil(t, read.UnverifiedApproval)
	})

	t.Run("a saved approval is current only while the chosen tests and target artifacts match", func(t *testing.T) {
		changes := []struct {
			name    string
			change  func(*ControlPlane, *promotionTargetStub)
			restore func(*ControlPlane, *promotionTargetStub)
		}{
			{
				name: "suite commit",
				change: func(cp *ControlPlane, _ *promotionTargetStub) {
					cp.verifier.(storedVerification).cfg.Plan.SuiteCommit = "suite-2"
				},
				restore: func(cp *ControlPlane, _ *promotionTargetStub) {
					cp.verifier.(storedVerification).cfg.Plan.SuiteCommit = spec.SuiteCommit
				},
			},
			{
				name: "binary digest",
				change: func(_ *ControlPlane, target *promotionTargetStub) {
					target.artifacts = map[string]string{"orders": "d2"}
				},
				restore: func(_ *ControlPlane, target *promotionTargetStub) {
					target.artifacts = maps.Clone(spec.Artifacts)
				},
			},
			{
				name: "extra binary",
				change: func(_ *ControlPlane, target *promotionTargetStub) {
					target.artifacts = map[string]string{"orders": "d1", "worker": "d3"}
				},
				restore: func(_ *ControlPlane, target *promotionTargetStub) {
					target.artifacts = maps.Clone(spec.Artifacts)
				},
			},
		}
		for _, tc := range changes {
			t.Run(tc.name, func(t *testing.T) {
				cp, target, card := setup(t)
				approved, err := approve(aliceCtx, cp, card, target.report.Digest)
				require.NoError(t, err)
				saved := *approved.Approval

				tc.change(cp, target)

				read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
				require.NoError(t, err)
				assert.Nil(t, read.Approval, "get_change does not present a stale approval")
				assert.Nil(t, read.UnverifiedApproval, "changed inputs are stale, not unverified")
				assert.Equal(t, card.RequestRevision, read.RequestRevision)
				same, err := reconfirm(cp, card, card.Examples[0].ExpectedOutcome)
				require.NoError(t, err)
				assert.Nil(t, same.Approval, "an unchanged confirm_intent does not either")
				assert.Nil(t, same.UnverifiedApproval)
				assert.Equal(t, card.RequestRevision, same.RequestRevision)
				assert.Equal(t, saved, *approved.Approval, "the returned approval record is not changed")

				// Restoring the approved inputs presents the saved approval again.
				tc.restore(cp, target)
				read, err = cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
				require.NoError(t, err)
				assert.Equal(t, &saved, read.Approval)
				assert.Nil(t, read.UnverifiedApproval)
			})
		}
	})

	t.Run("an approval is returned as unverified when current inputs cannot be read", func(t *testing.T) {
		cp, target, card := setup(t)
		approved, err := approve(aliceCtx, cp, card, target.report.Digest)
		require.NoError(t, err)
		target.artifactsErr = errors.New("target runs prompt overrides no report covers")
		read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
		require.NoError(t, err)
		assert.Nil(t, read.Approval)
		require.NotNil(t, read.UnverifiedApproval)
		assert.Equal(t, approved.Approval, read.UnverifiedApproval.Approval)
		assert.Contains(t, read.UnverifiedApproval.Reason, "because the release binaries could not be read")
		assert.Contains(t, read.UnverifiedApproval.Reason, "in a folder named after the commit")
		assert.Contains(t, read.UnverifiedApproval.Reason, "Check current approval")
		assert.Contains(t, read.UnverifiedApproval.Reason, "Approve this result")
		assert.Contains(t, read.UnverifiedApproval.Reason, "surla promotion remove")
		assert.NotContains(t, read.UnverifiedApproval.Reason, "/")
		assert.NotContains(t, read.UnverifiedApproval.Reason, "get_change")
		assert.NotContains(t, read.UnverifiedApproval.Reason, "approve_merge")

		same, err := reconfirm(cp, card, card.Examples[0].ExpectedOutcome)
		require.NoError(t, err)
		assert.Nil(t, same.Approval)
		require.NotNil(t, same.UnverifiedApproval)
		assert.Equal(t, approved.Approval, same.UnverifiedApproval.Approval)
		assert.Contains(t, same.UnverifiedApproval.Reason, "because the release binaries could not be read")
	})

	t.Run("a card without an approval never reads the target", func(t *testing.T) {
		cp, target, _ := setup(t)
		target.artifactsErr = errors.New("must not be called")
		read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
		require.NoError(t, err)
		assert.Nil(t, read.Approval)
		assert.Nil(t, read.UnverifiedApproval)
	})

	t.Run("checking an approval does not hold the card lock", func(t *testing.T) {
		cp, target, card := setup(t)
		_, err := approve(aliceCtx, cp, card, target.report.Digest)
		require.NoError(t, err)
		reading, release := make(chan struct{}), make(chan struct{})
		target.artifactsReading, target.artifactsRelease = reading, release
		got := make(chan *gensurla.ChangeCardResult, 1)
		go func() {
			read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
			assert.NoError(t, err)
			got <- read
		}()
		<-reading
		// While get_change waits on the target, a confirmation that takes
		// the lock completes.
		changed, err := reconfirm(cp, card, "Manager approval")
		require.NoError(t, err)
		assert.Nil(t, changed.Approval)
		close(release)
		read := <-got
		assert.Equal(t, card.RequestRevision, read.RequestRevision, "get_change answers for the card it read")
	})

	t.Run("an unchanged confirmation checking the approval does not hold the card lock", func(t *testing.T) {
		cp, target, card := setup(t)
		_, err := approve(aliceCtx, cp, card, target.report.Digest)
		require.NoError(t, err)
		reading, release := make(chan struct{}), make(chan struct{})
		target.artifactsReading, target.artifactsRelease = reading, release
		got := make(chan *gensurla.ChangeCardResult, 1)
		go func() {
			same, err := reconfirm(cp, card, card.Examples[0].ExpectedOutcome)
			assert.NoError(t, err)
			got <- same
		}()
		<-reading
		changed, err := reconfirm(cp, card, "Manager approval")
		require.NoError(t, err, "a second confirmation takes the lock while the first waits on the target")
		assert.NotEqual(t, card.RequestRevision, changed.RequestRevision)
		close(release)
		same := <-got
		assert.Equal(t, card.RequestRevision, same.RequestRevision)
	})

	t.Run("an approval recorded from inputs measured before the target changed is not presented", func(t *testing.T) {
		cp, target, card := setup(t)
		reading, release := make(chan struct{}), make(chan struct{})
		target.artifactsReading, target.artifactsRelease = reading, release
		errc := make(chan error, 1)
		go func() {
			_, err := approve(aliceCtx, cp, card, target.report.Digest)
			errc <- err
		}()
		<-reading
		// approve_merge has measured the old artifacts; the target now changes.
		target.artifacts = map[string]string{"orders": "d2"}
		close(release)
		require.NoError(t, <-errc, "the approval matched what was measured")

		read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
		require.NoError(t, err)
		assert.Nil(t, read.Approval, "a fresh read checks the changed target")
	})

	t.Run("a revision minted while the report loads is not approved", func(t *testing.T) {
		cp, target, card := setup(t)
		target.loading = make(chan struct{})
		target.release = make(chan struct{})
		errc := make(chan error, 1)
		go func() {
			_, err := approve(aliceCtx, cp, card, target.report.Digest)
			errc <- err
		}()
		<-target.loading
		_, err := reconfirm(cp, card, "Manager approval")
		require.NoError(t, err)
		close(target.release)
		assert.ErrorContains(t, <-errc, "request_revision mismatch")

		read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
		require.NoError(t, err)
		assert.Nil(t, read.Approval)
	})

	t.Run("candidate commit other than the report's is stale", func(t *testing.T) {
		cp, target, card := setup(t)
		_, err := approveAt(aliceCtx, cp, card.RequestRevision, strings.Repeat("f", 40), target.report.Digest)
		require.ErrorIs(t, err, verify.ErrStaleReport)
		assert.ErrorContains(t, err, "candidate commit")
	})

	t.Run("revision already superseded at entry", func(t *testing.T) {
		cp, target, card := setup(t)
		_, err := reconfirm(cp, card, "Manager approval")
		require.NoError(t, err)
		_, err = approve(aliceCtx, cp, card, target.report.Digest)
		assert.ErrorContains(t, err, "request_revision mismatch")
	})

	t.Run("card still needs confirmation", func(t *testing.T) {
		ambiguous := &gensurla.CP1Analysis{
			Summary:     "Refunds up to $50 are instant.",
			Ambiguities: []*gensurla.CP1AnalysisAmbiguity{{Question: "Is $50.00 itself instant?", Options: []string{"instant", "manager"}}},
			Examples: []*gensurla.CP1AnalysisExample{{
				Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
				ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
			}},
			PreservedBehaviors: []string{},
		}
		cfg := verificationConfig(t)
		cfg.Plan.SuiteCommit = spec.SuiteCommit
		cp := newControlPlane(t, Options{
			Verification:    cfg,
			PromotionTarget: &promotionTargetStub{artifacts: maps.Clone(spec.Artifacts)},
		})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: ambiguous})
		require.NoError(t, err)
		_, err = approve(aliceCtx, cp, card, strings.Repeat("0", 64))
		assert.ErrorContains(t, err, "must be confirmed")
	})

	rejections := []struct {
		name           string
		ctx            context.Context
		token          string
		change         func(cp *ControlPlane, target *promotionTargetStub, card *gensurla.ChangeCardResult) string
		wantErr        string
		isStale        bool
		wantHTTPStatus int
	}{
		{"verification is not configured", aliceCtx, "alice-token", func(cp *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			cp.promotionTarget = nil
			cp.verifier = noVerification{}
			return target.report.Digest
		}, "verification is not configured", false, http.StatusNotImplemented},
		{"caller does not own the card", bobCtx, "bob-token", func(_ *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			return target.report.Digest
		}, "is not the owner", false, http.StatusForbidden},
		{"target returns a different report", aliceCtx, "alice-token", func(_ *ControlPlane, _ *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			return strings.Repeat("0", 64)
		}, "returned report", false, http.StatusInternalServerError},
		{"measured binary differs", aliceCtx, "alice-token", func(_ *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			target.artifacts = map[string]string{"orders": "rebuilt"}
			return target.report.Digest
		}, "publish the binaries the run built, whose folder the run's Proof view shows, into the release folder that the promotion settings name, in a folder named after the commit, or remove the promotion settings", true, http.StatusUnprocessableEntity},
		{"report cannot be loaded", aliceCtx, "alice-token", func(_ *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			target.loadErr = errors.New("no report under that digest")
			return target.report.Digest
		}, "no report under that digest", false, http.StatusInternalServerError},
		{"current artifacts cannot be measured", aliceCtx, "alice-token", func(_ *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			target.artifactsErr = errors.New("open release directory for commit " + commit + ": no such file or directory: publish the binaries the run built, whose folder the run's Proof view shows, into the release folder that the promotion settings name, in a folder named after the commit, or remove the promotion settings")
			return target.report.Digest
		}, "publish the binaries the run built, whose folder the run's Proof view shows, into the release folder that the promotion settings name, in a folder named after the commit, or remove the promotion settings", false, http.StatusUnprocessableEntity},
		{"chosen suite commit differs from report", aliceCtx, "alice-token", func(cp *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			cp.verifier.(storedVerification).cfg.Plan.SuiteCommit = "suite-2"
			return target.report.Digest
		}, "the report ran the tests at suite-1; this project's stored verification settings name the suite at suite-2: the builder verifies the candidate again; then approve that run", false, http.StatusUnprocessableEntity},
		{"report did not pass", aliceCtx, "alice-token", func(_ *ControlPlane, target *promotionTargetStub, card *gensurla.ChangeCardResult) string {
			target.report = failingReport(t, card.RequestRevision, commit, spec)
			return target.report.Digest
		}, "did not pass", true, http.StatusUnprocessableEntity},
	}
	for _, c := range rejections {
		t.Run(c.name, func(t *testing.T) {
			cp, target, card := setup(t)
			digest := c.change(cp, target, card)
			_, err := approve(c.ctx, cp, card, digest)
			require.ErrorContains(t, err, c.wantErr)
			assert.NotContains(t, err.Error(), "/var/lib/surla/secret")
			assert.Equal(t, c.isStale, errors.Is(err, verify.ErrStaleReport))

			handler := NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{
				"alice-token": {ID: "alice@example.com"},
				"bob-token":   {ID: "bob@example.com"},
			}))
			body, err := json.Marshal(map[string]any{
				"change_id":        card.ChangeID,
				"request_revision": card.RequestRevision,
				"candidate_commit": commit,
				"report_digest":    digest,
			})
			require.NoError(t, err)
			req := httptest.NewRequest(http.MethodPost, "/changes/approve", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+c.token)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			assert.Equal(t, c.wantHTTPStatus, rec.Code)
			assert.NotContains(t, rec.Body.String(), "/var/lib/surla/secret")

			read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
			require.NoError(t, err)
			assert.Nil(t, read.Approval)
		})
	}
}

// TestApproveMergeChecksTheChosenTestsCommit verifies Proof clause 1 across
// HTTP (POST /changes/approve) and the MCP tool (approve_merge) as the user:
// only a report that ran the chooser's tests commit is approved; a report at
// another commit is refused as unprocessable with the recovery step; and a
// written-tests change with no build or whose tests are not accepted is
// refused with no_build or tests_not_accepted.
func TestApproveMergeChecksTheChosenTestsCommit(t *testing.T) {
	const (
		candidate = "0123456789abcdef0123456789abcdef01234567"
		commitT   = "1111111111111111111111111111111111111111"
		commitU   = "2222222222222222222222222222222222222222"
	)
	artifacts := map[string]string{"shop": strings.Repeat("a", 64)}

	cases := []struct {
		name           string
		stored         bool
		startBuild     bool
		tw             *testWriterAssignment
		reportCommit   string
		wantRefusal    string
		wantHTTPStatus int
		wantStep       func(revision string) string
		wantSource     gensurla.ApprovalTestSource
	}{
		{
			name:         "written tests at accepted commit approves",
			startBuild:   true,
			tw:           &testWriterAssignment{State: testsAccepted, Read: true, Findings: []string{}, Commit: commitT, CheckedAt: time.Now()},
			reportCommit: commitT,
			wantSource:   gensurla.NewApprovalTestSourceWrittenTests(&gensurla.AgentWrittenTests{Commit: commitT, Notice: writtenTestsNotice}),
		},
		{
			name:           "written tests report at another commit is refused",
			startBuild:     true,
			tw:             &testWriterAssignment{State: testsAccepted, Read: true, Findings: []string{}, Commit: commitT, CheckedAt: time.Now()},
			reportCommit:   commitU,
			wantRefusal:    "unprocessable",
			wantHTTPStatus: http.StatusUnprocessableEntity,
			wantStep: func(revision string) string {
				return "the report ran the tests at " + commitU + "; the tests accepted for revision " + revision + " are at " + commitT + ": the builder verifies the candidate again; then approve that run"
			},
		},
		{
			name:           "written tests without a build is refused",
			reportCommit:   commitT,
			wantRefusal:    "no_build",
			wantHTTPStatus: http.StatusConflict,
			wantStep: func(revision string) string {
				return `change "chg" has no build with a test writer, so no tests are accepted for revision ` + revision + `: the user starts a build on the views page`
			},
		},
		{
			name:           "written tests assigned is refused",
			startBuild:     true,
			reportCommit:   commitT,
			wantRefusal:    "tests_not_accepted",
			wantHTTPStatus: http.StatusConflict,
			wantStep: func(string) string {
				return "the tests of this build are not accepted; their state is assigned: the test writer writes the required tests and reports them finished; the builder verifies again once Surla accepts them"
			},
		},
		{
			name:           "written tests reported is refused",
			startBuild:     true,
			tw:             &testWriterAssignment{State: testsReported, Findings: []string{}},
			reportCommit:   commitT,
			wantRefusal:    "tests_not_accepted",
			wantHTTPStatus: http.StatusConflict,
			wantStep: func(string) string {
				return "the tests of this build are not accepted; their state is reported: Surla is checking the test writer's report; the builder verifies again after its verdict"
			},
		},
		{
			name:           "written tests needs changes is refused",
			startBuild:     true,
			tw:             &testWriterAssignment{State: testsNeedChanges, Findings: []string{"refund_over_limit is missing"}, CheckedAt: time.Now()},
			reportCommit:   commitT,
			wantRefusal:    "tests_not_accepted",
			wantHTTPStatus: http.StatusConflict,
			wantStep: func(string) string {
				return "the tests of this build are not accepted; their state is needs changes: the test writer fixes Surla's findings and reports again; the builder verifies again once Surla accepts them"
			},
		},
		{
			name:       "written tests could not run is refused",
			startBuild: true,
			tw: &testWriterAssignment{
				State: testsCouldNotRun, Findings: []string{}, Reason: "git could not write the tests branch",
				Step: "Free disk space, then report again.", CheckedAt: time.Now(),
			},
			reportCommit:   commitT,
			wantRefusal:    "tests_not_accepted",
			wantHTTPStatus: http.StatusConflict,
			wantStep: func(string) string {
				return "the tests of this build are not accepted; their state is could not run: Surla's check could not run (git could not write the tests branch); the user's step: Free disk space, then report again."
			},
		},
		{
			name:         "stored evaluator at configured commit approves",
			stored:       true,
			reportCommit: commitT,
			wantSource:   gensurla.NewApprovalTestSourceEvaluator(&gensurla.OperatorEvaluatorTests{}),
		},
		{
			name:           "stored evaluator report at another commit is refused",
			stored:         true,
			reportCommit:   commitU,
			wantRefusal:    "unprocessable",
			wantHTTPStatus: http.StatusUnprocessableEntity,
			wantStep: func(string) string {
				return "the report ran the tests at " + commitU + "; this project's stored verification settings name the suite at " + commitT + ": the builder verifies the candidate again; then approve that run"
			},
		},
	}

	for _, tc := range cases {
		for _, transport := range []string{"http", "mcp"} {
			t.Run(tc.name+"/"+transport, func(t *testing.T) {
				target := &promotionTargetStub{artifacts: maps.Clone(artifacts)}
				opts := Options{
					WorkspaceDir:    t.TempDir(),
					RunsDir:         t.TempDir(),
					SurlaVersion:    testSurlaVersion,
					PromotionTarget: target,
				}
				if tc.stored {
					cfg := verificationConfig(t)
					cfg.Plan.SuiteCommit = commitT
					opts.Verification = cfg
				} else {
					opts.WrittenTestRuns = &WrittenTestRuns{
						ReportDir: t.TempDir(),
						Command:   []string{"/bin/sh", "-c", "exit 0", "sh"},
						Timeout:   time.Hour,
					}
				}
				cp := newBuildControlPlane(t, opts)
				card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
				user := asCaller("alice@example.com", CallerUser)
				if tc.startBuild {
					_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
					require.NoError(t, err)
				}
				if tc.tw != nil {
					saveTestWriter(t, cp, "chg", *tc.tw)
				}
				spec := reportSpec{
					SuiteID: "shop_acceptance", SuiteCommit: tc.reportCommit,
					Environment: "written-env", Artifacts: maps.Clone(artifacts),
				}
				target.report = passingReport(t, card.RequestRevision, candidate, spec)
				srv := roleServer(t, cp)
				reqBody := `{"change_id":"chg","request_revision":"` + card.RequestRevision + `","candidate_commit":"` + candidate + `","report_digest":"` + target.report.Digest + `"}`

				if tc.wantRefusal != "" {
					wantMsg := tc.wantStep(card.RequestRevision)
					if transport == "http" {
						status, raw := httpCall(t, srv.URL, "user", http.MethodPost, "/changes/approve", reqBody)
						assert.Equal(t, tc.wantHTTPStatus, status, raw)
						var refused struct {
							Name    string `json:"name"`
							Message string `json:"message"`
						}
						require.NoError(t, json.Unmarshal([]byte(raw), &refused), raw)
						assert.Equal(t, tc.wantRefusal, refused.Name)
						assert.Equal(t, wantMsg, refused.Message)
					} else {
						failure, _ := mcpToolCall(t, srv.URL, "user", "approve_merge", reqBody)
						assert.Contains(t, failure, wantMsg)
					}
					saved, ok, err := cp.cards.load("chg")
					require.NoError(t, err)
					require.True(t, ok)
					assert.Nil(t, saved.card.Approval)
					return
				}

				if transport == "http" {
					status, raw := httpCall(t, srv.URL, "user", http.MethodPost, "/changes/approve", reqBody)
					require.Equal(t, http.StatusOK, status, raw)
					decoded, err := gensurla.DecodeChangeCardResult([]byte(raw))
					require.NoError(t, err)
					assert.Equal(t, &gensurla.ApprovalItem{
						ApprovedBy: "alice@example.com", ReportDigest: target.report.Digest,
						CandidateCommit: candidate, SuiteID: "shop_acceptance",
						SuiteCommit: commitT, Environment: "written-env",
						Artifacts: maps.Clone(artifacts), ScenarioCount: 2,
						TestSource: tc.wantSource,
					}, decoded.Approval)
				} else {
					failure, structured := mcpToolCall(t, srv.URL, "user", "approve_merge", reqBody)
					require.Empty(t, failure)
					decoded, err := gensurla.DecodeChangeCardResult(structured)
					require.NoError(t, err)
					assert.Equal(t, &gensurla.ApprovalItem{
						ApprovedBy: "alice@example.com", ReportDigest: target.report.Digest,
						CandidateCommit: candidate, SuiteID: "shop_acceptance",
						SuiteCommit: commitT, Environment: "written-env",
						Artifacts: maps.Clone(artifacts), ScenarioCount: 2,
						TestSource: tc.wantSource,
					}, decoded.Approval)
				}
			})
		}
	}
}

// TestSavedApprovalStaysValidOnlyWhileChosenTestsMatch verifies Proof clause 3:
// an approval recorded for written tests at T is returned while T is accepted,
// hidden when the test writer reports again, hidden when Surla accepts U != T,
// and returned again after a rebuild whose tests are accepted at T again.
func TestSavedApprovalStaysValidOnlyWhileChosenTestsMatch(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	recorder := &recordingTracerProvider{}
	otel.SetTracerProvider(recorder)
	t.Cleanup(func() { otel.SetTracerProvider(prevProvider) })

	const (
		candidate = "0123456789abcdef0123456789abcdef01234567"
		commitT   = "1111111111111111111111111111111111111111"
		commitU   = "2222222222222222222222222222222222222222"
	)
	artifacts := map[string]string{"shop": strings.Repeat("a", 64)}
	target := &promotionTargetStub{artifacts: maps.Clone(artifacts)}
	cp := newBuildControlPlane(t, Options{
		WorkspaceDir:    t.TempDir(),
		RunsDir:         t.TempDir(),
		SurlaVersion:    testSurlaVersion,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Command: []string{"/bin/sh", "-c", "exit 0", "sh"}, Timeout: time.Hour},
		PromotionTarget: target,
	})
	card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
	user := asCaller("alice@example.com", CallerUser)
	_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	acceptTests(t, cp, "chg", commitT)

	spec := reportSpec{SuiteID: "shop_acceptance", SuiteCommit: commitT, Environment: "sim", Artifacts: maps.Clone(artifacts)}
	target.report = passingReport(t, card.RequestRevision, candidate, spec)
	approved, err := cp.ApproveMerge(user, &gensurla.ApproveMergePayload{
		ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: candidate, ReportDigest: target.report.Digest,
	})
	require.NoError(t, err)
	require.NotNil(t, approved.Approval)
	assert.Equal(t, gensurla.NewApprovalTestSourceWrittenTests(&gensurla.AgentWrittenTests{Commit: commitT, Notice: writtenTestsNotice}), approved.Approval.TestSource)

	// 1. Report again -> hidden.
	recorder.reset()
	saveTestWriter(t, cp, "chg", testWriterAssignment{State: testsReported, Read: true, Findings: []string{}})
	read, err := cp.GetChange(user, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Nil(t, read.Approval, "reporting again hides the approval until judged")
	assert.Nil(t, read.UnverifiedApproval)
	spans := recorder.matching("controlplane.check_approval")
	require.Len(t, spans, 1)
	require.Len(t, spans[0].events, 1)
	assert.Equal(t, "approval.hidden_tests_changed", spans[0].events[0].name)
	assert.Contains(t, spans[0].events[0].attrs, attribute.String("surla.change_id", "chg"))
	assert.Contains(t, spans[0].events[0].attrs, attribute.String("surla.suite_commit", commitT))

	// 2. Accept U -> hidden.
	acceptTests(t, cp, "chg", commitU)
	read, err = cp.GetChange(user, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Nil(t, read.Approval, "accepting another commit hides the approval")
	assert.Nil(t, read.UnverifiedApproval)

	// 3. Rebuild and accept T again -> returned.
	_, err = cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	read, err = cp.GetChange(user, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Nil(t, read.Approval, "hidden while the rebuilt tests are not yet accepted")
	acceptTests(t, cp, "chg", commitT)
	read, err = cp.GetChange(user, &gensurla.GetChangePayload{ChangeID: "chg"})
	require.NoError(t, err)
	assert.Equal(t, approved.Approval, read.Approval, "accepting the same commit again restores the approval")
}

// TestApproveMergeRejectsAcceptanceChangedDuringReportLoad verifies Proof
// clause 6: when a new tests commit is accepted while LoadReport is in flight,
// ApproveMerge refuses with conflict ("the accepted tests changed while
// approving: approve again") and saves nothing.
func TestApproveMergeRejectsAcceptanceChangedDuringReportLoad(t *testing.T) {
	const (
		candidate = "0123456789abcdef0123456789abcdef01234567"
		commitT   = "1111111111111111111111111111111111111111"
		commitU   = "2222222222222222222222222222222222222222"
	)
	artifacts := map[string]string{"shop": strings.Repeat("a", 64)}
	target := &promotionTargetStub{
		artifacts: maps.Clone(artifacts),
		loading:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	cp := newBuildControlPlane(t, Options{
		WorkspaceDir:    t.TempDir(),
		RunsDir:         t.TempDir(),
		SurlaVersion:    testSurlaVersion,
		WrittenTestRuns: &WrittenTestRuns{ReportDir: t.TempDir(), Command: []string{"/bin/sh", "-c", "exit 0", "sh"}, Timeout: time.Hour},
		PromotionTarget: target,
	})
	card := saveStoryCard(t, cp, "chg", StatusConfirmed, refundStory("Refund over the limit waits"))
	user := asCaller("alice@example.com", CallerUser)
	_, err := cp.StartBuild(user, &gensurla.StartBuildPayload{ChangeID: "chg", RequestRevision: card.RequestRevision})
	require.NoError(t, err)
	acceptTests(t, cp, "chg", commitT)
	target.report = passingReport(t, card.RequestRevision, candidate, reportSpec{
		SuiteID: "shop_acceptance", SuiteCommit: commitT, Environment: "sim", Artifacts: maps.Clone(artifacts),
	})

	errc := make(chan error, 1)
	go func() {
		_, err := cp.ApproveMerge(user, &gensurla.ApproveMergePayload{
			ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: candidate, ReportDigest: target.report.Digest,
		})
		errc <- err
	}()
	<-target.loading
	acceptTests(t, cp, "chg", commitU)
	close(target.release)

	err = <-errc
	assertRefusalSays(t, err, "conflict", "the accepted tests changed while approving: approve again")
	saved, ok, err := cp.cards.load("chg")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Nil(t, saved.card.Approval, "nothing is saved when the accepted tests changed during approval")
}

// TestCurrentCardAllFiveApprovalStatesAndSpanRecording verifies all 5 approval
// states (`no record`, `current`, `changed inputs`, `unavailable target`, and
// `absent target`) across direct Go (`GetChange` and unchanged `ConfirmIntent`),
// HTTP (`GET /changes/{change_id}` and `POST /changes/confirm`), and MCP
// (`get_change` and `confirm_intent`), and verifies that `unavailable target`
// records the raw error on the `controlplane.check_approval` OpenTelemetry span
// while returning a path-free `UnverifiedApproval.Reason`.
func TestCurrentCardAllFiveApprovalStatesAndSpanRecording(t *testing.T) {
	prevProvider := otel.GetTracerProvider()
	recorder := &recordingTracerProvider{}
	otel.SetTracerProvider(recorder)
	t.Cleanup(func() { otel.SetTracerProvider(prevProvider) })

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com", CallerID: "alice@example.com", Kind: CallerUser})
	const commit = "0123456789abcdef0123456789abcdef01234567"
	const secretHostPath = "/var/lib/surla/secret-releases/0123456789abcdef0123456789abcdef01234567/orders"

	analysis := &gensurla.CP1Analysis{
		Summary:     "Refunds up to $50 are instant.",
		Ambiguities: []*gensurla.CP1AnalysisAmbiguity{},
		Examples: []*gensurla.CP1AnalysisExample{{
			Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
			ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
		}},
		PreservedBehaviors: []string{},
	}
	spec := reportSpec{
		SuiteID: "orders_acceptance", SuiteCommit: "suite-1", Environment: "sim",
		Artifacts: map[string]string{"orders": "d1"},
	}

	cardsDir := t.TempDir()
	cfg := verificationConfig(t)
	cfg.Plan.SuiteCommit = spec.SuiteCommit
	target := &promotionTargetStub{artifacts: maps.Clone(spec.Artifacts)}
	cp := newControlPlane(t, Options{
		CardsDir:        cardsDir,
		Verification:    cfg,
		PromotionTarget: target,
	})
	srv := httptest.NewServer(NewHTTPHandler(cp, workflowAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
	t.Cleanup(srv.Close)
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)

	decodeWireCard := func(t *testing.T, raw []byte) *gensurla.ChangeCardResult {
		t.Helper()
		card, err := gensurla.DecodeChangeCardResult(raw)
		require.NoError(t, err)
		return &gensurla.ChangeCardResult{
			ChangeID:           card.ChangeID,
			RequestRevision:    card.RequestRevision,
			Approval:           card.Approval,
			UnverifiedApproval: card.UnverifiedApproval,
		}
	}
	wireExamples := func(card *gensurla.ChangeCardResult) []map[string]any {
		out := make([]map[string]any, 0, len(card.Examples))
		for _, ex := range card.Examples {
			out = append(out, map[string]any{
				"id":                ex.ID,
				"scenario":          ex.Scenario,
				"input_summary":     ex.InputSummary,
				"expected_outcome":  ex.ExpectedOutcome,
				"verification_kind": ex.VerificationKind,
			})
		}
		return out
	}
	httpGetChange := func(t *testing.T, url string) *gensurla.ChangeCardResult {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url+"/changes/chg", nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer alice-token")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var buf bytes.Buffer
		_, err = buf.ReadFrom(resp.Body)
		require.NoError(t, err)
		return decodeWireCard(t, buf.Bytes())
	}
	httpConfirmNoop := func(t *testing.T, url string, card *gensurla.ChangeCardResult) *gensurla.ChangeCardResult {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"change_id":        "chg",
			"request_revision": card.RequestRevision,
			"answers":          map[string]string{},
			"examples":         wireExamples(card),
		})
		require.NoError(t, err)
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url+"/changes/confirm", bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer alice-token")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var buf bytes.Buffer
		_, err = buf.ReadFrom(resp.Body)
		require.NoError(t, err)
		return decodeWireCard(t, buf.Bytes())
	}
	mcpCallCard := func(t *testing.T, a *genmcpadapter.MCPAdapter, name string, args map[string]any) *gensurla.ChangeCardResult {
		t.Helper()
		rawArgs, err := json.Marshal(args)
		require.NoError(t, err)
		res, err := a.ToolsCall(aliceCtx, &genmcpadapter.ToolsCallPayload{Name: name, Arguments: rawArgs})
		require.NoError(t, err)
		complete, ok := res.Outcome.AsComplete()
		require.True(t, ok)
		require.False(t, complete.IsError != nil && *complete.IsError)
		require.NotEmpty(t, complete.StructuredContent)
		return decodeWireCard(t, complete.StructuredContent)
	}

	// State 1: no approval record.
	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant.", Analysis: analysis})
	require.NoError(t, err)
	card = mustConfirmNoop(t, cp, aliceCtx, card)
	for _, got := range []*gensurla.ChangeCardResult{
		mustGetChange(t, cp, aliceCtx, "chg"),
		mustConfirmNoop(t, cp, aliceCtx, card),
		httpGetChange(t, srv.URL),
		httpConfirmNoop(t, srv.URL, card),
		mcpCallCard(t, adapter, "get_change", map[string]any{"change_id": "chg"}),
		mcpCallCard(t, adapter, "confirm_intent", map[string]any{"change_id": "chg", "request_revision": card.RequestRevision, "answers": map[string]string{}, "examples": wireExamples(card)}),
	} {
		assert.Nil(t, got.Approval)
		assert.Nil(t, got.UnverifiedApproval)
	}

	// State 2: current approval.
	target.report = passingReport(t, card.RequestRevision, commit, spec)
	approved, err := cp.ApproveMerge(aliceCtx, &gensurla.ApproveMergePayload{
		ChangeID: "chg", RequestRevision: card.RequestRevision, CandidateCommit: commit, ReportDigest: target.report.Digest,
	})
	require.NoError(t, err)
	require.NotNil(t, approved.Approval)
	for _, got := range []*gensurla.ChangeCardResult{
		mustGetChange(t, cp, aliceCtx, "chg"),
		mustConfirmNoop(t, cp, aliceCtx, card),
		httpGetChange(t, srv.URL),
		httpConfirmNoop(t, srv.URL, card),
		mcpCallCard(t, adapter, "get_change", map[string]any{"change_id": "chg"}),
		mcpCallCard(t, adapter, "confirm_intent", map[string]any{"change_id": "chg", "request_revision": card.RequestRevision, "answers": map[string]string{}, "examples": wireExamples(card)}),
	} {
		assert.Equal(t, approved.Approval, got.Approval)
		assert.Nil(t, got.UnverifiedApproval)
	}

	// State 3: changed inputs.
	target.artifacts = map[string]string{"orders": "d2"}
	for _, got := range []*gensurla.ChangeCardResult{
		mustGetChange(t, cp, aliceCtx, "chg"),
		mustConfirmNoop(t, cp, aliceCtx, card),
		httpGetChange(t, srv.URL),
		httpConfirmNoop(t, srv.URL, card),
		mcpCallCard(t, adapter, "get_change", map[string]any{"change_id": "chg"}),
		mcpCallCard(t, adapter, "confirm_intent", map[string]any{"change_id": "chg", "request_revision": card.RequestRevision, "answers": map[string]string{}, "examples": wireExamples(card)}),
	} {
		assert.Nil(t, got.Approval)
		assert.Nil(t, got.UnverifiedApproval)
	}

	// State 4: unavailable target (CurrentArtifacts fails with a host path error).
	recorder.reset()
	target.artifacts = maps.Clone(spec.Artifacts)
	target.artifactsErr = errors.New("open " + secretHostPath + ": no such file or directory")
	for _, got := range []*gensurla.ChangeCardResult{
		mustGetChange(t, cp, aliceCtx, "chg"),
		mustConfirmNoop(t, cp, aliceCtx, card),
		httpGetChange(t, srv.URL),
		httpConfirmNoop(t, srv.URL, card),
		mcpCallCard(t, adapter, "get_change", map[string]any{"change_id": "chg"}),
		mcpCallCard(t, adapter, "confirm_intent", map[string]any{"change_id": "chg", "request_revision": card.RequestRevision, "answers": map[string]string{}, "examples": wireExamples(card)}),
	} {
		assert.Nil(t, got.Approval)
		require.NotNil(t, got.UnverifiedApproval)
		assert.Equal(t, approved.Approval, got.UnverifiedApproval.Approval)
		assert.Contains(t, got.UnverifiedApproval.Reason, "because the release binaries could not be read")
		assert.Contains(t, got.UnverifiedApproval.Reason, "in a folder named after the commit")
		assert.Contains(t, got.UnverifiedApproval.Reason, "Check current approval")
		assert.Contains(t, got.UnverifiedApproval.Reason, "Approve this result")
		assert.Contains(t, got.UnverifiedApproval.Reason, "surla promotion remove")
		assert.NotContains(t, got.UnverifiedApproval.Reason, "get_change")
		assert.NotContains(t, got.UnverifiedApproval.Reason, "approve_merge")
		assert.NotContains(t, got.UnverifiedApproval.Reason, "/")
		assert.NotContains(t, got.UnverifiedApproval.Reason, secretHostPath, "transport response must not leak host filesystem paths")
		assert.NotContains(t, got.UnverifiedApproval.Reason, "/var/lib/surla")
	}
	spans := recorder.matching("controlplane.check_approval")
	require.Len(t, spans, 6, "each of the 6 reads must emit a controlplane.check_approval span")
	for _, sp := range spans {
		assert.True(t, sp.ended)
		assert.Equal(t, codes.Error, sp.statusCode)
		assert.Contains(t, sp.statusDesc, secretHostPath)
		require.Len(t, sp.errors, 1)
		assert.Contains(t, sp.errors[0].Error(), secretHostPath)
		assert.Contains(t, sp.attrs, attribute.String("surla.change_id", "chg"))
		assert.Contains(t, sp.attrs, attribute.String("surla.candidate_commit", commit))
	}

	// State 5: absent target (server restarted after `surla promotion remove`):
	// without promotion settings, the saved approval remains valid while the
	// chosen tests' commit still matches.
	require.NoError(t, cp.Close())
	cpNoTarget := newControlPlane(t, Options{
		CardsDir:     cardsDir,
		Verification: cfg,
	})
	srvNoTarget := httptest.NewServer(NewHTTPHandler(cpNoTarget, workflowAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
	t.Cleanup(srvNoTarget.Close)
	adapterNoTarget := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cpNoTarget), nil)
	for _, got := range []*gensurla.ChangeCardResult{
		mustGetChange(t, cpNoTarget, aliceCtx, "chg"),
		mustConfirmNoop(t, cpNoTarget, aliceCtx, card),
		httpGetChange(t, srvNoTarget.URL),
		httpConfirmNoop(t, srvNoTarget.URL, card),
		mcpCallCard(t, adapterNoTarget, "get_change", map[string]any{"change_id": "chg"}),
		mcpCallCard(t, adapterNoTarget, "confirm_intent", map[string]any{"change_id": "chg", "request_revision": card.RequestRevision, "answers": map[string]string{}, "examples": wireExamples(card)}),
	} {
		assert.Equal(t, approved.Approval, got.Approval)
		assert.Nil(t, got.UnverifiedApproval)
	}
}

// LoadReport returns s.report, or s.loadErr when set, after the optional
// loading/release handshake.
func (s *promotionTargetStub) LoadReport(context.Context, string) (verify.Report, error) {
	if s.loading != nil {
		close(s.loading)
		<-s.release
	}
	if s.loadErr != nil {
		return verify.Report{}, s.loadErr
	}
	return s.report, nil
}

// CurrentArtifacts returns s.artifacts, or s.artifactsErr when set. When
// artifactsReading is set, the first call closes it and waits on
// artifactsRelease. The snapshot is taken when the call starts, so a test that
// changes s.artifacts while the call is held models a target that changed
// after it was measured.
func (s *promotionTargetStub) CurrentArtifacts(context.Context, string) (map[string]string, error) {
	artifacts, err := s.artifacts, s.artifactsErr
	if s.artifactsReading != nil {
		reading, release := s.artifactsReading, s.artifactsRelease
		s.artifactsReading, s.artifactsRelease = nil, nil
		close(reading)
		<-release
	}
	if err != nil {
		return nil, err
	}
	return artifacts, nil
}

// Tracer returns a trace.Tracer that records started spans into p.
func (p *recordingTracerProvider) Tracer(name string, _ ...trace.TracerOption) trace.Tracer {
	return &recordingTracer{provider: p, name: name}
}

func (p *recordingTracerProvider) reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spans = nil
}

func (p *recordingTracerProvider) matching(name string) []*recordedSpan {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*recordedSpan
	for _, s := range p.spans {
		if s.name == name {
			out = append(out, s)
		}
	}
	return out
}

type recordingTracer struct {
	embedded.Tracer
	provider *recordingTracerProvider
	name     string
}

func (t *recordingTracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	cfg := trace.NewSpanStartConfig(opts...)
	_, noopSpan := noop.NewTracerProvider().Tracer(t.name).Start(ctx, name, opts...)
	sp := &recordedSpan{
		Span:  noopSpan,
		name:  name,
		attrs: append([]attribute.KeyValue(nil), cfg.Attributes()...),
	}
	t.provider.mu.Lock()
	t.provider.spans = append(t.provider.spans, sp)
	t.provider.mu.Unlock()
	return trace.ContextWithSpan(ctx, sp), sp
}

func (s *recordedSpan) AddEvent(name string, opts ...trace.EventOption) {
	cfg := trace.NewEventConfig(opts...)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, recordedEvent{
		name:  name,
		attrs: append([]attribute.KeyValue(nil), cfg.Attributes()...),
	})
}

func (s *recordedSpan) RecordError(err error, _ ...trace.EventOption) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, err)
}

func (s *recordedSpan) SetStatus(code codes.Code, desc string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCode = code
	s.statusDesc = desc
}

func (s *recordedSpan) End(_ ...trace.SpanEndOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ended = true
}

func mustGetChange(t *testing.T, cp *ControlPlane, ctx context.Context, changeID string) *gensurla.ChangeCardResult {
	t.Helper()
	got, err := cp.GetChange(ctx, &gensurla.GetChangePayload{ChangeID: changeID})
	require.NoError(t, err)
	return got
}

func mustConfirmNoop(t *testing.T, cp *ControlPlane, ctx context.Context, card *gensurla.ChangeCardResult) *gensurla.ChangeCardResult {
	t.Helper()
	got, err := cp.ConfirmIntent(ctx, &gensurla.ConfirmIntentPayload{
		ChangeID:        card.ChangeID,
		RequestRevision: card.RequestRevision,
		Answers:         map[string]string{},
		Examples:        card.Examples,
	})
	require.NoError(t, err)
	return got
}

// writeReportFile marshals report and writes it under reportDir/<digest>.json.
func writeReportFile(t *testing.T, reportDir string, report verify.Report) {
	t.Helper()
	require.NoError(t, os.MkdirAll(reportDir, 0o700))
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(reportDir, report.Digest+".json"), raw, 0o600))
}

// passingReport assembles a report in which both declared scenarios passed
// against the binaries in spec.
func passingReport(t *testing.T, revision, commit string, spec reportSpec) verify.Report {
	t.Helper()
	return assembleReport(t, revision, commit, spec, true)
}

// failingReport assembles a report in which the second scenario failed.
func failingReport(t *testing.T, revision, commit string, spec reportSpec) verify.Report {
	t.Helper()
	return assembleReport(t, revision, commit, spec, false)
}

// assembleReport builds a verify.Report for a two-scenario suite (one
// holdout) whose second scenario has outcome secondPassed.
func assembleReport(t *testing.T, revision, commit string, spec reportSpec, secondPassed bool) verify.Report {
	t.Helper()
	binding := verify.Binding{
		CandidateCommit: commit, RequestRevision: revision, SuiteID: spec.SuiteID,
		SuiteCommit: spec.SuiteCommit, Artifacts: spec.Artifacts, Environment: spec.Environment,
	}
	suite := verify.Suite{ID: spec.SuiteID, Environment: spec.Environment, Scenarios: []verify.Scenario{{ID: "a"}, {ID: "h", Holdout: true}}}
	var artifacts []eval.Artifact
	for name, digest := range spec.Artifacts {
		artifacts = append(artifacts, eval.Artifact{Name: "candidate:" + name, URI: "sha256:" + digest})
	}
	ran := func(id string, passed bool) eval.ScenarioReport {
		return eval.ScenarioReport{ID: id, StartedAt: time.Now(), Passed: passed,
			Result: &eval.Result{Checks: []eval.Check{{Name: "check", Passed: passed}}, Artifacts: artifacts}}
	}
	r, err := verify.Assemble(binding, suite, eval.Report{SuiteID: spec.SuiteID, Scenarios: []eval.ScenarioReport{
		ran("a", true), ran("h", secondPassed),
	}}, 0)
	require.NoError(t, err)
	return r
}
