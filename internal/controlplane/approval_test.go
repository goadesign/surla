// This file tests Checkpoint 5 (CP5) promotion approval recording
// (`approve_merge`) and read-time verification of saved approvals against the
// bound PromotionTarget across Go, HTTP, and MCP.

package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
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

// promotionTargetStub is a PromotionTarget serving one report and fixed
// inputs. When loading and release are set, LoadReport signals loading and
// waits for release, so a test can change the card mid-approval.
type promotionTargetStub struct {
	// report is the only report LoadReport returns, whatever the digest.
	report verify.Report
	// inputs is what CurrentInputs returns.
	inputs PromotionInputs
	// loading, when set, is closed when LoadReport starts.
	loading chan struct{}
	// release, when set, is waited on before LoadReport returns.
	release chan struct{}
	// loadErr, when set, is returned by LoadReport.
	loadErr error
	// inputsErr, when set, is returned by CurrentInputs.
	inputsErr error
	// inputsReading, when set, is closed when CurrentInputs starts.
	inputsReading chan struct{}
	// inputsRelease, when set, is waited on before CurrentInputs returns.
	inputsRelease chan struct{}
}

// recordingTracerProvider is an OpenTelemetry TracerProvider that captures
// spans created during a test without importing external SDK packages.
type recordingTracerProvider struct {
	embedded.TracerProvider
	mu    sync.Mutex
	spans []*recordedSpan
}

// recordedSpan captures the name, attributes, recorded errors, and status of
// one OpenTelemetry span.
type recordedSpan struct {
	trace.Span
	mu         sync.Mutex
	name       string
	attrs      []attribute.KeyValue
	errors     []error
	statusCode codes.Code
	statusDesc string
	ended      bool
}

// TestApproveMergeRecordsApprovalOnlyForTheCheckedRevision verifies CP5:
// approve_merge checks the evaluator report against the promotion target's
// current inputs, records the approval on the owner's confirmed card, keeps it
// across a no-op confirmation, and drops it when the revision changes, even
// when the change lands while the report is being loaded.
func TestApproveMergeRecordsApprovalOnlyForTheCheckedRevision(t *testing.T) {
	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	bobCtx := WithPrincipal(context.Background(), Principal{ID: "bob@example.com"})
	const commit = "0123456789abcdef0123456789abcdef01234567"
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
	inputs := PromotionInputs{
		SuiteID: "orders_acceptance", SuiteCommit: "suite-1", Environment: "sim",
		Artifacts: map[string]string{"orders": "d1"},
	}

	// setup proposes a confirmed card and assembles a passing report bound to
	// its revision and to inputs.
	setup := func(t *testing.T) (*ControlPlane, *promotionTargetStub, *gensurla.ChangeCardResult) {
		t.Helper()
		target := &promotionTargetStub{inputs: inputs}
		cp := newControlPlane(t, Options{
			Analyzer:        NewModelCP1Analyzer(newStubModelClient(t, string(analysis))),
			PromotionTarget: target,
		})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant."})
		require.NoError(t, err)
		require.Equal(t, StatusConfirmed, card.Status)
		target.report = passingReport(t, card.RequestRevision, commit, inputs)
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

	t.Run("a saved approval is current only while the target's inputs match", func(t *testing.T) {
		changes := []struct {
			name   string
			change func(*PromotionInputs)
		}{
			{"suite", func(in *PromotionInputs) { in.SuiteID = "other_suite" }},
			{"suite commit", func(in *PromotionInputs) { in.SuiteCommit = "suite-2" }},
			{"environment", func(in *PromotionInputs) { in.Environment = "staging" }},
			{"binary digest", func(in *PromotionInputs) { in.Artifacts = map[string]string{"orders": "d2"} }},
			{"extra binary", func(in *PromotionInputs) { in.Artifacts = map[string]string{"orders": "d1", "worker": "d3"} }},
		}
		for _, tc := range changes {
			t.Run(tc.name, func(t *testing.T) {
				cp, target, card := setup(t)
				approved, err := approve(aliceCtx, cp, card, target.report.Digest)
				require.NoError(t, err)
				saved := *approved.Approval

				// The target changes on its own, without draft_override.
				changed := inputs
				changed.Artifacts = maps.Clone(inputs.Artifacts)
				tc.change(&changed)
				target.inputs = changed

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
				target.inputs = inputs
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
		target.inputsErr = errors.New("target runs prompt overrides no report covers")
		read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
		require.NoError(t, err)
		assert.Nil(t, read.Approval)
		require.NotNil(t, read.UnverifiedApproval)
		assert.Equal(t, approved.Approval, read.UnverifiedApproval.Approval)
		assert.Contains(t, read.UnverifiedApproval.Reason, "could not report its current inputs")

		same, err := reconfirm(cp, card, card.Examples[0].ExpectedOutcome)
		require.NoError(t, err)
		assert.Nil(t, same.Approval)
		require.NotNil(t, same.UnverifiedApproval)
		assert.Equal(t, approved.Approval, same.UnverifiedApproval.Approval)
		assert.Contains(t, same.UnverifiedApproval.Reason, "could not report its current inputs")
	})

	t.Run("a card without an approval never reads the target", func(t *testing.T) {
		cp, target, _ := setup(t)
		target.inputsErr = errors.New("must not be called")
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
		target.inputsReading, target.inputsRelease = reading, release
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
		target.inputsReading, target.inputsRelease = reading, release
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
		target.inputsReading, target.inputsRelease = reading, release
		errc := make(chan error, 1)
		go func() {
			_, err := approve(aliceCtx, cp, card, target.report.Digest)
			errc <- err
		}()
		<-reading
		// approve_merge has measured the old inputs; the target now changes.
		target.inputs = PromotionInputs{
			SuiteID: inputs.SuiteID, SuiteCommit: inputs.SuiteCommit, Environment: inputs.Environment,
			Artifacts: map[string]string{"orders": "d2"},
		}
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
		ambiguous, err := json.Marshal(CP1SemanticAnalysis{
			Summary:     "Refunds up to $50 are instant.",
			Ambiguities: []CP1ModelAmbiguity{{Question: "Is $50.00 itself instant?", Options: []string{"instant", "manager"}}},
			Examples: []CP1ModelExample{{
				Scenario: "Refund at the limit", InputSummary: "Order total $50.00",
				ExpectedOutcome: "Instant store credit", VerificationKind: "deterministic",
			}},
			PreservedBehaviors: []string{},
		})
		require.NoError(t, err)
		cp := newControlPlane(t, Options{
			Analyzer:        NewModelCP1Analyzer(newStubModelClient(t, string(ambiguous))),
			PromotionTarget: &promotionTargetStub{inputs: inputs},
		})
		card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant."})
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
		{"no promotion target is bound", aliceCtx, "alice-token", func(cp *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			cp.promotionTarget = nil
			return target.report.Digest
		}, "no promotion target", false, http.StatusNotImplemented},
		{"caller does not own the card", bobCtx, "bob-token", func(_ *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			return target.report.Digest
		}, "is not the owner", false, http.StatusForbidden},
		{"target returns a different report", aliceCtx, "alice-token", func(_ *ControlPlane, _ *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			return strings.Repeat("0", 64)
		}, "returned report", false, http.StatusInternalServerError},
		{"measured binary differs", aliceCtx, "alice-token", func(_ *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			target.inputs.Artifacts = map[string]string{"orders": "rebuilt"}
			return target.report.Digest
		}, "candidate binaries", true, http.StatusUnprocessableEntity},
		{"report cannot be loaded", aliceCtx, "alice-token", func(_ *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			target.loadErr = errors.New("no report under that digest")
			return target.report.Digest
		}, "no report under that digest", false, http.StatusInternalServerError},
		{"current inputs cannot be measured", aliceCtx, "alice-token", func(_ *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			target.inputsErr = errors.New("commit not built")
			return target.report.Digest
		}, "commit not built", false, http.StatusInternalServerError},
		{"required environment differs", aliceCtx, "alice-token", func(_ *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			target.inputs.Environment = "staging"
			return target.report.Digest
		}, "environment", true, http.StatusUnprocessableEntity},
		{"required suite commit differs", aliceCtx, "alice-token", func(_ *ControlPlane, target *promotionTargetStub, _ *gensurla.ChangeCardResult) string {
			target.inputs.SuiteCommit = "suite-2"
			return target.report.Digest
		}, "evaluator suite", true, http.StatusUnprocessableEntity},
		{"report did not pass", aliceCtx, "alice-token", func(_ *ControlPlane, target *promotionTargetStub, card *gensurla.ChangeCardResult) string {
			target.report = failingReport(t, card.RequestRevision, commit, inputs)
			return target.report.Digest
		}, "did not pass", true, http.StatusUnprocessableEntity},
	}
	for _, c := range rejections {
		t.Run(c.name, func(t *testing.T) {
			cp, target, card := setup(t)
			digest := c.change(cp, target, card)
			_, err := approve(c.ctx, cp, card, digest)
			require.ErrorContains(t, err, c.wantErr)
			assert.Equal(t, c.isStale, errors.Is(err, verify.ErrStaleReport))

			handler := NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{
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

			read, err := cp.GetChange(aliceCtx, &gensurla.GetChangePayload{ChangeID: "chg"})
			require.NoError(t, err)
			assert.Nil(t, read.Approval)
		})
	}
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

	aliceCtx := WithPrincipal(context.Background(), Principal{ID: "alice@example.com"})
	const commit = "0123456789abcdef0123456789abcdef01234567"
	const secretHostPath = "/var/lib/surla/secret-releases/0123456789abcdef0123456789abcdef01234567/orders"

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
	inputs := PromotionInputs{
		SuiteID: "orders_acceptance", SuiteCommit: "suite-1", Environment: "sim",
		Artifacts: map[string]string{"orders": "d1"},
	}

	cardsDir := t.TempDir()
	target := &promotionTargetStub{inputs: inputs}
	cp := newControlPlane(t, Options{
		CardsDir:        cardsDir,
		Analyzer:        NewModelCP1Analyzer(newStubModelClient(t, string(analysis))),
		PromotionTarget: target,
	})
	srv := httptest.NewServer(NewHTTPHandler(cp, NewTokenAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
	t.Cleanup(srv.Close)
	adapter := genmcpadapter.NewMCPAdapter(gensurla.NewEndpoints(cp), nil)

	type wireApproval struct {
		ApprovedBy      string            `json:"approved_by"`
		ReportDigest    string            `json:"report_digest"`
		CandidateCommit string            `json:"candidate_commit"`
		SuiteID         string            `json:"suite_id"`
		SuiteCommit     string            `json:"suite_commit"`
		Environment     string            `json:"environment"`
		Artifacts       map[string]string `json:"artifacts"`
		ScenarioCount   int               `json:"scenario_count"`
	}
	type wireUnverifiedApproval struct {
		Approval *wireApproval `json:"approval"`
		Reason   string        `json:"reason"`
	}
	type wireChangeCard struct {
		ChangeID           string                  `json:"change_id"`
		RequestRevision    string                  `json:"request_revision"`
		Approval           *wireApproval           `json:"approval"`
		UnverifiedApproval *wireUnverifiedApproval `json:"unverified_approval"`
	}
	toApprovalItem := func(w *wireApproval) *gensurla.ApprovalItem {
		if w == nil {
			return nil
		}
		return &gensurla.ApprovalItem{
			ApprovedBy:      w.ApprovedBy,
			ReportDigest:    w.ReportDigest,
			CandidateCommit: w.CandidateCommit,
			SuiteID:         w.SuiteID,
			SuiteCommit:     w.SuiteCommit,
			Environment:     w.Environment,
			Artifacts:       w.Artifacts,
			ScenarioCount:   w.ScenarioCount,
		}
	}
	decodeWireCard := func(t *testing.T, raw []byte) *gensurla.ChangeCardResult {
		t.Helper()
		var w wireChangeCard
		require.NoError(t, json.Unmarshal(raw, &w))
		out := &gensurla.ChangeCardResult{
			ChangeID:        w.ChangeID,
			RequestRevision: w.RequestRevision,
			Approval:        toApprovalItem(w.Approval),
		}
		if w.UnverifiedApproval != nil {
			out.UnverifiedApproval = &gensurla.UnverifiedApprovalItem{
				Approval: toApprovalItem(w.UnverifiedApproval.Approval),
				Reason:   w.UnverifiedApproval.Reason,
			}
		}
		return out
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

	// State 1: no record.
	card, err := cp.ProposeChange(aliceCtx, &gensurla.ProposeChangePayload{ChangeID: "chg", Prompt: "Refunds up to $50 are instant."})
	require.NoError(t, err)
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
	target.report = passingReport(t, card.RequestRevision, commit, inputs)
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
	target.inputs = PromotionInputs{
		SuiteID: "orders_acceptance", SuiteCommit: "suite-1", Environment: "sim",
		Artifacts: map[string]string{"orders": "d2"},
	}
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

	// State 4: unavailable target (CurrentInputs fails with a host path error).
	recorder.reset()
	target.inputs = inputs
	target.inputsErr = errors.New("open " + secretHostPath + ": no such file or directory")
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
		assert.Contains(t, got.UnverifiedApproval.Reason, "could not report its current inputs")
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

	// State 5: absent target (server restarted without PromotionTarget).
	require.NoError(t, cp.Close())
	cpNoTarget := newControlPlane(t, Options{
		CardsDir: cardsDir,
		Analyzer: NewModelCP1Analyzer(newStubModelClient(t, string(analysis))),
	})
	srvNoTarget := httptest.NewServer(NewHTTPHandler(cpNoTarget, NewTokenAuthenticator(map[string]Principal{"alice-token": {ID: "alice@example.com"}})))
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
		assert.Nil(t, got.Approval)
		require.NotNil(t, got.UnverifiedApproval)
		assert.Equal(t, approved.Approval, got.UnverifiedApproval.Approval)
		assert.Contains(t, got.UnverifiedApproval.Reason, "-promotion-config")
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

// CurrentInputs returns s.inputs, or s.inputsErr when set. When
// inputsReading is set, the first call closes it and waits on inputsRelease.
// The snapshot is taken when the call starts, so a test that changes
// s.inputs while the call is held models a target that changed after it was
// measured.
func (s *promotionTargetStub) CurrentInputs(context.Context, string) (PromotionInputs, error) {
	inputs, err := s.inputs, s.inputsErr
	if s.inputsReading != nil {
		reading, release := s.inputsReading, s.inputsRelease
		s.inputsReading, s.inputsRelease = nil, nil
		close(reading)
		<-release
	}
	if err != nil {
		return PromotionInputs{}, err
	}
	return inputs, nil
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

// passingReport assembles a report in which both declared scenarios passed
// against the binaries in inputs.
func passingReport(t *testing.T, revision, commit string, inputs PromotionInputs) verify.Report {
	t.Helper()
	return assembleReport(t, revision, commit, inputs, true)
}

// failingReport assembles a report in which the second scenario failed.
func failingReport(t *testing.T, revision, commit string, inputs PromotionInputs) verify.Report {
	t.Helper()
	return assembleReport(t, revision, commit, inputs, false)
}

// assembleReport builds a verify.Report for a two-scenario suite (one
// holdout) whose second scenario has outcome secondPassed.
func assembleReport(t *testing.T, revision, commit string, inputs PromotionInputs, secondPassed bool) verify.Report {
	t.Helper()
	binding := verify.Binding{
		CandidateCommit: commit, RequestRevision: revision, SuiteID: inputs.SuiteID,
		SuiteCommit: inputs.SuiteCommit, Artifacts: inputs.Artifacts, Environment: inputs.Environment,
	}
	suite := verify.Suite{ID: inputs.SuiteID, Environment: inputs.Environment, Scenarios: []verify.Scenario{{ID: "a"}, {ID: "h", Holdout: true}}}
	var artifacts []eval.Artifact
	for name, digest := range inputs.Artifacts {
		artifacts = append(artifacts, eval.Artifact{Name: "candidate:" + name, URI: "sha256:" + digest})
	}
	ran := func(id string, passed bool) eval.ScenarioReport {
		return eval.ScenarioReport{ID: id, StartedAt: time.Now(), Passed: passed,
			Result: &eval.Result{Checks: []eval.Check{{Name: "check", Passed: passed}}, Artifacts: artifacts}}
	}
	r, err := verify.Assemble(binding, suite, eval.Report{SuiteID: inputs.SuiteID, Scenarios: []eval.ScenarioReport{
		ran("a", true), ran("h", secondPassed),
	}}, 0)
	require.NoError(t, err)
	return r
}
