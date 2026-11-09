package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// VerificationConfig is the operator's Checkpoint 4 configuration: what is
// built, which evaluator suite runs, and where runs and reports live. A caller
// of verify_change names only the Change Card revision and the candidate
// commit; everything here comes from the operator, so a builder cannot choose
// how its own work is graded.
type VerificationConfig struct {
	// RunsDir holds one directory per verification run (see verifyrun). It
	// must be private to the evaluator.
	RunsDir string
	// Plan is the operator's part of every run's verify.Plan: CandidateRepo,
	// CandidateModule, Binaries, SuiteRepo, SuiteCommit, SuiteModule,
	// SuiteCommand and ReportDir. CandidateCommit, RequestRevision and
	// WorkDir are set per run and must be empty here. RunsDir grows by one
	// directory per run, kept as evidence; the operator prunes it.
	Plan verify.Plan
	// Command is the argument list that runs this surla executable; runs
	// start `<Command> verify-run supervise <dir>`.
	Command []string
	// Timeout is how long a run may take before it is stopped and recorded
	// as interrupted.
	Timeout time.Duration
}

// errVerificationUnavailable is returned by the CP4 verification methods of a
// control plane without a VerificationConfig.
var errVerificationUnavailable = errors.New("verification is not configured on this control plane")

// VerifyChange starts Checkpoint 4 verification of a candidate commit for the
// caller's confirmed Change Card revision and returns at once with the run in
// the running state. The run is accepted into its own directory under the
// configured RunsDir and carried out by processes that outlive this call and
// this server (see verifyrun). The caller must own the card, the card must be
// confirmed, and request_revision must be its current revision.
func (c *ControlPlane) VerifyChange(ctx context.Context, p *gensurla.VerifyChangePayload) (*gensurla.VerificationStatusResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	verCfg, err := c.resolveVerificationConfig(ctx)
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	record, exists, err := c.cards.load(p.ChangeID)
	switch {
	case err != nil:
		return nil, err
	case !exists:
		return nil, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	case record.owner != principal.ID:
		return nil, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	case record.card.Status != StatusConfirmed:
		return nil, gensurla.MakeConflict(fmt.Errorf("change_id %q must be confirmed before verification (status %q)", p.ChangeID, record.card.Status))
	case record.card.RequestRevision != p.RequestRevision:
		return nil, gensurla.MakeConflict(fmt.Errorf("request_revision mismatch for %q: got %q, want %q", p.ChangeID, p.RequestRevision, record.card.RequestRevision))
	}
	runID := verifyrun.NewRunID()
	plan := verCfg.Plan
	plan.CandidateCommit = p.CandidateCommit
	plan.RequestRevision = p.RequestRevision
	// The work directory lives inside the run's own directory, which only
	// the evaluator can read; it is kept as the run's evidence.
	plan.WorkDir = filepath.Join(verCfg.RunsDir, runID, "work")
	req := verifyrun.Request{
		RunID:      runID,
		Owner:      principal.ID,
		ChangeID:   p.ChangeID,
		AcceptedAt: time.Now().UTC(),
		Timeout:    verCfg.Timeout,
		Plan:       plan,
	}
	if _, err := verifyrun.Start(verCfg.RunsDir, req, verCfg.Command); err != nil {
		return nil, fmt.Errorf("start verification: %w", err)
	}
	return statusResult(req, verifyrun.State{Kind: verifyrun.StateRunning}), nil
}

// GetVerification returns the state of a verification run to the principal
// that started it. The run is read from its directory, not from the Change
// Card, so it keeps the revision it was started for and stays readable after
// the card changes or this server restarts. A completed run returns only the
// builder view of its report.
func (c *ControlPlane) GetVerification(ctx context.Context, p *gensurla.GetVerificationPayload) (*gensurla.VerificationStatusResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	runsDir := c.effectiveRunsDir()
	if runsDir == "" {
		return nil, gensurla.MakeNotImplemented(errVerificationUnavailable)
	}
	_, span := otel.Tracer("surla/controlplane").Start(ctx, "surla.controlplane.get_verification",
		trace.WithAttributes(attribute.String("surla.run_id", p.RunID)))
	defer span.End()

	c.mu.RLock()
	req, state, err := verifyrun.Read(runsDir, p.RunID)
	c.mu.RUnlock()
	switch {
	case req.Owner == "" && errors.Is(err, fs.ErrNotExist):
		return nil, gensurla.MakeNotFound(fmt.Errorf("unknown run_id %q", p.RunID))
	case req.Owner != principal.ID && req.Owner != "":
		// Checked before any later read error, whose text may name
		// evaluator paths.
		return nil, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of run_id %q", principal.ID, p.RunID))
	case err != nil:
		span.RecordError(err)
		span.SetStatus(codes.Error, "read verification run")
		return nil, fmt.Errorf("read verification %q: saved verification record could not be read", p.RunID)
	}
	return statusResult(req, state), nil
}

// ListVerifications returns all Checkpoint 4 verification runs started by the
// caller for the named Change Card, ordered newest first by accepted_at
// (tie-broken by run_id). The caller must own the card; listing works whether
// the card is currently confirmed or in needs_confirmation after a later
// revision. Each completed run returns only the builder view of its report.
func (c *ControlPlane) ListVerifications(ctx context.Context, p *gensurla.ListVerificationsPayload) (*gensurla.VerificationListResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	runsDir := c.effectiveRunsDir()
	if runsDir == "" {
		return nil, gensurla.MakeNotImplemented(errVerificationUnavailable)
	}
	_, span := otel.Tracer("surla/controlplane").Start(ctx, "surla.controlplane.list_verifications",
		trace.WithAttributes(attribute.String("surla.change_id", p.ChangeID)))
	defer span.End()

	c.mu.RLock()
	record, exists, err := c.cards.load(p.ChangeID)
	if err == nil && exists && record.owner == principal.ID {
		runs, listErr := verifyrun.List(runsDir, principal.ID, p.ChangeID)
		c.mu.RUnlock()
		if listErr != nil {
			span.RecordError(listErr)
			span.SetStatus(codes.Error, "list verification runs")
			return nil, fmt.Errorf("list verifications for %q: saved verification records could not be read", p.ChangeID)
		}
		out := make([]*gensurla.VerificationStatusResult, 0, len(runs))
		for _, r := range runs {
			out = append(out, statusResult(r.Request, r.State))
		}
		return &gensurla.VerificationListResult{
			ChangeID: p.ChangeID,
			Runs:     out,
		}, nil
	}
	c.mu.RUnlock()
	switch {
	case err != nil:
		span.RecordError(err)
		span.SetStatus(codes.Error, "load change card")
		return nil, fmt.Errorf("list verifications for %q: saved change card could not be read", p.ChangeID)
	case !exists:
		return nil, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	default:
		return nil, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	}
}

// effectiveRunsDir returns the verification runs directory configured via
// Options.RunsDir or directly on Verification.
func (c *ControlPlane) effectiveRunsDir() string {
	if c.runsDir != "" {
		return c.runsDir
	}
	if c.verification != nil && c.verification.RunsDir != "" {
		return c.verification.RunsDir
	}
	return ""
}

// resolveVerificationConfig resolves a VerificationConfig dynamically from the
// currently bound workspace directory and suite commit when ResolveVerification
// is set, or returns the static VerificationConfig, validating that RunsDir and
// ReportDir lie outside both initialWorkspaceRoot and activeWorkspaceDir.
func (c *ControlPlane) resolveVerificationConfig(ctx context.Context) (*VerificationConfig, error) {
	cfg, err := c.resolveActiveVerification(ctx)
	if err != nil {
		return nil, gensurla.MakeNotImplemented(err)
	}
	if cfg == nil {
		return nil, gensurla.MakeNotImplemented(errVerificationUnavailable)
	}
	return cfg, nil
}

// statusResult returns the wire form of a run: its request and its state,
// with a completed run's report reduced to the builder view.
func statusResult(req verifyrun.Request, state verifyrun.State) *gensurla.VerificationStatusResult {
	acceptedAt := req.AcceptedAt.UTC().Format(time.RFC3339Nano)
	res := &gensurla.VerificationStatusResult{
		RunID:           req.RunID,
		ChangeID:        req.ChangeID,
		RequestRevision: req.Plan.RequestRevision,
		CandidateCommit: req.Plan.CandidateCommit,
		SuiteCommit:     req.Plan.SuiteCommit,
		AcceptedAt:      acceptedAt,
	}
	switch state.Kind {
	case verifyrun.StateRunning:
		res.State = gensurla.NewStateRunning(&gensurla.VerificationRunning{StartedAt: acceptedAt})
	case verifyrun.StateCompleted:
		res.State = gensurla.NewStateCompleted(&gensurla.VerificationCompleted{
			SuiteID:     state.Report.Binding.SuiteID,
			Environment: state.Report.Binding.Environment,
			Report:      builderReport(state.Report.ForBuilders()),
		})
	case verifyrun.StateNotCompleted:
		res.State = gensurla.NewStateNotCompleted(&gensurla.VerificationNotCompleted{
			Kind:    string(state.Result.Failure),
			Stage:   state.Result.Stage,
			Summary: state.Result.Summary,
		})
	}
	return res
}

// builderReport converts the builder view of a report to its wire form.
func builderReport(b verify.BuilderReport) *gensurla.BuilderVerificationReport {
	out := &gensurla.BuilderVerificationReport{
		ReportDigest: b.ReportDigest,
		Counts: &gensurla.VerificationCounts{
			Required: b.Counts.Required, Passed: b.Counts.Passed, Failed: b.Counts.Failed,
			NotRun: b.Counts.NotRun, HarnessError: b.Counts.HarnessError,
		},
		Passed:      b.Passed,
		SuiteFailed: b.SuiteFailed,
		Scenarios:   make([]*gensurla.VerificationScenario, 0, len(b.Scenarios)),
	}
	for _, s := range b.Scenarios {
		scenario := &gensurla.VerificationScenario{ID: s.ID, Outcome: string(s.Outcome)}
		if s.Error != "" {
			scenario.Error = &s.Error
		}
		for _, check := range s.Checks {
			wire := &gensurla.VerificationCheck{Name: check.Name, Passed: check.Passed}
			if check.Diagnostic != "" {
				wire.Diagnostic = &check.Diagnostic
			}
			scenario.Checks = append(scenario.Checks, wire)
		}
		out.Scenarios = append(out.Scenarios, scenario)
	}
	return out
}
