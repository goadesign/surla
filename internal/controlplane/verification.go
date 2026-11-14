package controlplane

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	goa "goa.design/goa/v3/pkg"

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

// writtenTestsNotice is the fixed sentence every result of a run with written
// tests carries.
const writtenTestsNotice = "These tests were written by a separate agent on this machine and are not isolated from the builder."

// errVerificationUnavailable is returned by the CP4 verification methods of a
// control plane without a VerificationConfig.
var errVerificationUnavailable = errors.New("verification is not configured on this control plane")

// VerifyChange starts Checkpoint 4 verification of a candidate commit for the
// caller's confirmed Change Card revision and returns at once with the run in
// the running state. The caller must own the card, the card must be
// confirmed, and request_revision must be its current revision; then
// verificationPlan chooses what runs and refuses what cannot (no build, tests
// not accepted, no test layout). The run is accepted into its own directory
// under the runs folder and carried out by processes that outlive this call
// and this server (see verifyrun).
func (c *ControlPlane) VerifyChange(ctx context.Context, p *gensurla.VerifyChangePayload) (*gensurla.VerificationStatusResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if _, none := c.verifier.(noVerification); none {
		return nil, gensurla.MakeNotImplemented(errVerificationUnavailable)
	}
	if err := c.verifiableCard(principal, p.ChangeID, p.RequestRevision); err != nil {
		return nil, err
	}
	verCfg, err := c.verificationPlan(ctx, principal, p.ChangeID, p.RequestRevision, p.CandidateCommit)
	if err != nil {
		return nil, err
	}
	runID := verifyrun.NewRunID()
	plan := verCfg.Plan
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
	// Observe the selected inputs before accepting work. A later edit can
	// invalidate this observation, but no failed read can hide an accepted run.
	_, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.start_verification",
		trace.WithAttributes(attribute.String("surla.change_id", p.ChangeID)))
	defer span.End()
	c.mu.RLock()
	inputsCurrent, err := c.currentVerificationInputs(principal, p.ChangeID, plan)
	c.mu.RUnlock()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "read verification inputs")
		return nil, fmt.Errorf("start verification for %q: saved verification inputs could not be read", p.ChangeID)
	}
	if _, err := verifyrun.Start(verCfg.RunsDir, req, verCfg.Command); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "start verification run")
		return nil, fmt.Errorf("start verification: %w", err)
	}
	return statusResult(req, verifyrun.State{Kind: verifyrun.StateRunning}, inputsCurrent), nil
}

// verifiableCard checks, under the read lock, that principal owns the card
// changeID, that it is confirmed and that revision is its current revision.
func (c *ControlPlane) verifiableCard(principal Principal, changeID, revision string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	record, exists, err := c.cards.load(changeID)
	switch {
	case err != nil:
		return err
	case !exists:
		return gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", changeID))
	case record.owner != principal.ID:
		return gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, changeID))
	case record.card.Status != StatusConfirmed:
		return gensurla.MakeConflict(fmt.Errorf("change_id %q must be confirmed before verification (status %q)", changeID, record.card.Status))
	case record.card.RequestRevision != revision:
		return gensurla.MakeConflict(fmt.Errorf("request_revision mismatch for %q: got %q, want %q", changeID, revision, record.card.RequestRevision))
	}
	return nil
}

// GetVerification returns the state of a verification run to the principal
// that started it. The run is read from its directory, not from the Change
// Card, so it keeps the revision it was started for and stays readable after
// the card changes or this server restarts. A completed run returns only the
// builder view of its report. A test writer reads only the runs of a change it
// holds an assignment for (testWriterHolds); any other is forbidden.
func (c *ControlPlane) GetVerification(ctx context.Context, p *gensurla.GetVerificationPayload) (*gensurla.VerificationStatusResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	runsDir := c.runsDir
	if runsDir == "" {
		return nil, gensurla.MakeNotImplemented(errVerificationUnavailable)
	}
	_, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.get_verification",
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
	c.mu.RLock()
	defer c.mu.RUnlock()
	if principal.Kind == CallerTestWriter {
		holds, err := c.testWriterHolds(principal, req.ChangeID)
		if err != nil {
			var refusal *goa.ServiceError
			if errors.As(err, &refusal) {
				return nil, err
			}
			span.RecordError(err)
			span.SetStatus(codes.Error, "read verification assignment")
			return nil, fmt.Errorf("read verification %q: saved verification inputs could not be read", p.RunID)
		}
		if !holds {
			return nil, gensurla.MakeForbidden(fmt.Errorf(
				"the test writer reads only the runs of a change it has an assignment for, and change %q has none", req.ChangeID))
		}
	}
	inputsCurrent, err := c.currentVerificationInputs(principal, req.ChangeID, req.Plan)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "read verification inputs")
		return nil, fmt.Errorf("read verification %q: saved verification inputs could not be read", p.RunID)
	}
	return statusResult(req, state, inputsCurrent), nil
}

// ListVerifications returns all Checkpoint 4 verification runs started by the
// caller for the named Change Card, ordered newest first by accepted_at
// (tie-broken by run_id). The caller must own the card; listing works whether
// the card is currently confirmed or in needs_confirmation after a later
// revision. Each completed run returns only the builder view of its report. A
// test writer lists only the runs of a change it holds an assignment for
// (testWriterHolds); any other is forbidden.
func (c *ControlPlane) ListVerifications(ctx context.Context, p *gensurla.ListVerificationsPayload) (*gensurla.VerificationListResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	runsDir := c.runsDir
	if runsDir == "" {
		return nil, gensurla.MakeNotImplemented(errVerificationUnavailable)
	}
	_, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.list_verifications",
		trace.WithAttributes(attribute.String("surla.change_id", p.ChangeID)))
	defer span.End()

	c.mu.RLock()
	record, exists, err := c.cards.load(p.ChangeID)
	if err == nil && exists && record.owner == principal.ID {
		if principal.Kind == CallerTestWriter {
			holds, holdErr := c.testWriterHolds(principal, p.ChangeID)
			if holdErr != nil || !holds {
				c.mu.RUnlock()
				if holdErr != nil {
					var refusal *goa.ServiceError
					if errors.As(holdErr, &refusal) {
						return nil, holdErr
					}
					span.RecordError(holdErr)
					span.SetStatus(codes.Error, "read verification assignment")
					return nil, fmt.Errorf("list verifications for %q: saved verification inputs could not be read", p.ChangeID)
				}
				return nil, gensurla.MakeForbidden(fmt.Errorf(
					"the test writer reads only the runs of a change it has an assignment for, and change %q has none", p.ChangeID))
			}
		}
		build, buildErr := c.changeBuild(record)
		if buildErr != nil {
			c.mu.RUnlock()
			span.RecordError(buildErr)
			span.SetStatus(codes.Error, "read verification inputs")
			return nil, fmt.Errorf("list verifications for %q: saved verification inputs could not be read", p.ChangeID)
		}
		runs, listErr := verifyrun.List(runsDir, principal.ID, p.ChangeID)
		c.mu.RUnlock()
		if listErr != nil {
			span.RecordError(listErr)
			span.SetStatus(codes.Error, "list verification runs")
			return nil, fmt.Errorf("list verifications for %q: saved verification records could not be read", p.ChangeID)
		}
		out := make([]*gensurla.VerificationStatusResult, 0, len(runs))
		for _, r := range runs {
			out = append(out, statusResult(r.Request, r.State,
				c.verificationInputsCurrent(record, build, r.Request.Plan)))
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

// currentVerificationInputs observes the current owned card and its canonical
// build once. Historical runs remain readable without an owned card; malformed
// current records remain errors. Callers hold c.mu after authorizing access.
func (c *ControlPlane) currentVerificationInputs(
	principal Principal,
	changeID string,
	plan verify.Plan,
) (bool, error) {
	record, exists, err := c.cards.load(changeID)
	if err != nil {
		return false, err
	}
	if !exists || record.owner != principal.ID {
		return false, nil
	}
	build, err := c.changeBuild(record)
	if err != nil {
		return false, err
	}
	return c.verificationInputsCurrent(record, build, plan), nil
}

// runTestSource renders where a run's tests came from, from its stored plan,
// which verifyrun read strictly: the operator's evaluator, or the tests a
// separate agent on this machine wrote, at the plan's suite commit, with the
// fixed notice.
func runTestSource(plan verify.Plan) gensurla.TestSource {
	if plan.Tests == verify.TestsEvaluator {
		return gensurla.NewTestSourceEvaluator(&gensurla.OperatorEvaluatorTests{})
	}
	return gensurla.NewTestSourceWrittenTests(&gensurla.AgentWrittenTests{Commit: plan.SuiteCommit, Notice: writtenTestsNotice})
}

// testWriterHolds reports whether the test writer principal holds an
// assignment for changeID: the project verifies without a stored evaluator,
// which leaves no test writer, and ownedBuild finds a build of the change,
// which without stored settings is one started with a test writer. Callers
// hold c.mu.
func (c *ControlPlane) testWriterHolds(principal Principal, changeID string) (bool, error) {
	if c.storedEvaluator() {
		return false, nil
	}
	_, err := c.ownedBuild(principal, changeID)
	var missing *goa.ServiceError
	if errors.As(err, &missing) && missing.Name == "not_found" {
		return false, nil
	}
	return err == nil, err
}

// statusResult returns the wire form of a run: its request and its state,
// with a completed run's report reduced to the builder view and the caller's
// separately observed current-input match.
func statusResult(
	req verifyrun.Request,
	state verifyrun.State,
	inputsCurrent bool,
) *gensurla.VerificationStatusResult {
	acceptedAt := req.AcceptedAt.UTC().Format(time.RFC3339Nano)
	res := &gensurla.VerificationStatusResult{
		RunID:           req.RunID,
		ChangeID:        req.ChangeID,
		RequestRevision: req.Plan.RequestRevision,
		CandidateCommit: req.Plan.CandidateCommit,
		SuiteCommit:     req.Plan.SuiteCommit,
		AcceptedAt:      acceptedAt,
		InputsCurrent:   inputsCurrent,
		TestSource:      runTestSource(req.Plan),
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
