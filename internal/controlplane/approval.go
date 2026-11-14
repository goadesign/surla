// This file implements Checkpoint 5 (CP5) promotion approval recording
// (`approve_merge`) and read-time verification of saved approvals against the
// bound PromotionTarget (`currentCard`).

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
	"github.com/goadesign/surla/internal/verifyrun"
)

// ApproveMerge records Checkpoint 5 approval of the caller's confirmed Change
// Card revision. It chooses the tests' commit for the revision through
// testsChoice (returning no_build or tests_not_accepted when written tests are
// not ready), loads the verification report by digest (from the bound
// PromotionTarget when promotion settings are configured, or from the
// verifier's report store otherwise), requires the report to have run that
// tests' commit and verify.Approve to accept the report for the candidate
// commit, revision, tests' commit and (when a PromotionTarget is bound) the
// current binary digests in <release_dir>/<commit>/, and records the report's
// own SuiteID and Environment without comparing them against separate settings.
// When no PromotionTarget is bound, no release folder or staged binaries are
// required and Artifacts is recorded as empty.
// The approval is saved on a copy of the card only if the card still has the
// same request_revision and testsChoice still returns the same tests' commit
// after those calls, so a confirm_intent or a new test acceptance meanwhile is
// never approved with the old report; a revise_change only proposes a revision
// and leaves the card's revision as it was. It returns the approved card with
// its derived TestSource and any proposal waiting beside it; nothing is merged
// or deployed.
func (c *ControlPlane) ApproveMerge(
	ctx context.Context,
	p *gensurla.ApproveMergePayload,
) (*gensurla.ChangeCardResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	target, err := c.resolvePromotion(ctx)
	if err != nil {
		return nil, err
	}

	c.mu.RLock()
	record, exists, err := c.cards.load(p.ChangeID)
	var (
		testsCommit string
		source      verify.TestsSource
		choiceErr   error
	)
	if err == nil && exists && record.owner == principal.ID &&
		record.card.Status == StatusConfirmed && record.card.RequestRevision == p.RequestRevision {
		testsCommit, source, choiceErr = c.testsChoice(principal, p.ChangeID, p.RequestRevision)
	}
	c.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	}
	if record.owner != principal.ID {
		return nil, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	}
	if record.card.Status != StatusConfirmed {
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"change_id %q must be confirmed before approval (status %q)",
			p.ChangeID,
			record.card.Status,
		))
	}
	if record.card.RequestRevision != p.RequestRevision {
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"request_revision mismatch for %q: got %q, want %q",
			p.ChangeID,
			p.RequestRevision,
			record.card.RequestRevision,
		))
	}
	if choiceErr != nil {
		return nil, choiceErr
	}

	// Report and artifact reads may be slow, so they run without holding c.mu.
	// A card is never removed and never changes owner; its request_revision,
	// confirmed status and chosen tests' commit are checked again under c.mu
	// before the approval is written.
	var report verify.Report
	if target != nil {
		report, err = target.LoadReport(ctx, p.ReportDigest)
	} else {
		runs := verifierRuns(c.verifier)
		if runs == nil {
			return nil, gensurla.MakeNotImplemented(errVerificationUnavailable)
		}
		report, err = verifyrun.LoadReport(runs.Plan.ReportDir, p.ReportDigest)
	}
	if err != nil {
		return nil, fmt.Errorf("load verification report %s: %w", p.ReportDigest, err)
	}
	if report.Digest != p.ReportDigest {
		return nil, fmt.Errorf("promotion target returned report %s for digest %s", report.Digest, p.ReportDigest)
	}
	if report.Binding.SuiteCommit != testsCommit {
		if source == verify.TestsEvaluator {
			return nil, gensurla.MakeUnprocessable(fmt.Errorf(
				"the report ran the tests at %s; this project's stored verification settings name the suite at %s: the builder verifies the candidate again; then approve that run",
				report.Binding.SuiteCommit,
				testsCommit,
			))
		}
		return nil, gensurla.MakeUnprocessable(fmt.Errorf(
			"the report ran the tests at %s; the tests accepted for revision %s are at %s: the builder verifies the candidate again; then approve that run",
			report.Binding.SuiteCommit,
			p.RequestRevision,
			testsCommit,
		))
	}
	expectedArtifacts := report.Binding.Artifacts
	recordedArtifacts := map[string]string{}
	if target != nil {
		artifacts, err := target.CurrentArtifacts(ctx, p.CandidateCommit)
		if err != nil {
			return nil, gensurla.MakeUnprocessable(err)
		}
		expectedArtifacts = artifacts
		if cloned := maps.Clone(artifacts); cloned != nil {
			recordedArtifacts = cloned
		}
	}
	current := verify.Binding{
		CandidateCommit: p.CandidateCommit,
		RequestRevision: p.RequestRevision,
		SuiteID:         report.Binding.SuiteID,
		SuiteCommit:     testsCommit,
		Artifacts:       expectedArtifacts,
		Environment:     report.Binding.Environment,
	}
	if err := verify.Approve(current, report); err != nil {
		if errors.Is(err, verify.ErrStaleReport) {
			if target != nil && !maps.Equal(expectedArtifacts, report.Binding.Artifacts) {
				return nil, gensurla.MakeUnprocessable(fmt.Errorf(
					"%w: publish the binaries the run built, whose folder the run's Proof view shows, into the release folder that the promotion settings name, in a folder named after the commit, or remove the promotion settings",
					err,
				))
			}
			return nil, gensurla.MakeUnprocessable(err)
		}
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	record, exists, err = c.cards.load(p.ChangeID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, gensurla.MakeNotFound(fmt.Errorf("change card %q is no longer saved", p.ChangeID))
	}
	if record.card.Status != StatusConfirmed {
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"change_id %q must be confirmed before approval (status %q)",
			p.ChangeID,
			record.card.Status,
		))
	}
	if record.card.RequestRevision != p.RequestRevision {
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"request_revision mismatch for %q: got %q, want %q",
			p.ChangeID,
			p.RequestRevision,
			record.card.RequestRevision,
		))
	}
	nowCommit, nowSource, err := c.testsChoice(principal, p.ChangeID, p.RequestRevision)
	if err != nil || nowCommit != testsCommit {
		return nil, gensurla.MakeConflict(errors.New("the accepted tests changed while approving: approve again"))
	}
	approved := *record.card
	approved.Approval = &gensurla.ApprovalItem{
		ApprovedBy:      principal.ID,
		ReportDigest:    report.Digest,
		CandidateCommit: p.CandidateCommit,
		SuiteID:         report.Binding.SuiteID,
		SuiteCommit:     testsCommit,
		Environment:     report.Binding.Environment,
		Artifacts:       recordedArtifacts,
		ScenarioCount:   report.Counts.Required,
		TestSource:      approvalTestSource(nowSource, testsCommit),
	}
	record.card = &approved
	if err := c.cards.save(record); err != nil {
		return nil, err
	}
	result := approved
	result.ProposedRevision, err = proposedRevisionItem(record)
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// currentCard returns record.card as get_change, an unchanged confirm_intent,
// revise_change and decline_revision report it:
//  1. When card has no saved approval, card is returned as-is (`Approval == nil`,
//     `UnverifiedApproval == nil`) without calling testsChoice or PromotionTarget.
//  2. When card has a saved approval and testsChoice refuses or returns a
//     tests' commit other than the approval's SuiteCommit, an event carrying
//     the change ID and the approval's SuiteCommit is recorded on the
//     `controlplane.check_approval` span and a shallow copy is returned with
//     `Approval == nil` and `UnverifiedApproval == nil` (the saved approval on
//     disk is untouched and is returned again if testsChoice returns its
//     SuiteCommit again).
//  3. When testsChoice returns the approval's SuiteCommit and no
//     PromotionTarget is bound, a shallow copy is returned with `Approval` set
//     (including its derived `TestSource`) and `UnverifiedApproval == nil`.
//  4. When testsChoice returns the approval's SuiteCommit and the bound
//     PromotionTarget reports current binary digests matching the approval's
//     Artifacts, a shallow copy is returned with `Approval` set (including its
//     derived `TestSource`) and `UnverifiedApproval == nil`.
//  5. When testsChoice returns the approval's SuiteCommit and PromotionTarget
//     reports current binary digests that differ, a shallow copy is returned
//     with `Approval == nil` and `UnverifiedApproval == nil`.
//  6. When testsChoice returns the approval's SuiteCommit and resolving the
//     PromotionTarget or calling PromotionTarget.CurrentArtifacts returns an
//     error, the raw error is recorded on the `controlplane.check_approval`
//     OpenTelemetry span and a shallow copy is returned with `Approval == nil`
//     and `UnverifiedApproval` carrying the recorded approval (with derived
//     `TestSource`) and a path-free recovery reason naming the views page's
//     controls.
//
// Callers must not hold c.mu because PromotionTarget.CurrentArtifacts may be slow.
func (c *ControlPlane) currentCard(
	ctx context.Context,
	record storedChangeCard,
) (*gensurla.ChangeCardResult, error) {
	card := record.card
	saved := card.Approval
	if saved == nil {
		return card, nil
	}
	ctx, span := c.tracer("github.com/goadesign/surla/internal/controlplane").Start(
		ctx,
		"controlplane.check_approval",
		trace.WithAttributes(
			attribute.String("surla.change_id", card.ChangeID),
			attribute.String("surla.candidate_commit", saved.CandidateCommit),
		),
	)
	defer span.End()

	owner := Principal{ID: record.owner, CallerID: record.owner, Kind: CallerUser}
	c.mu.RLock()
	testsCommit, source, choiceErr := c.testsChoice(owner, card.ChangeID, card.RequestRevision)
	c.mu.RUnlock()
	if choiceErr != nil || testsCommit != saved.SuiteCommit {
		span.AddEvent(
			"approval.hidden_tests_changed",
			trace.WithAttributes(
				attribute.String("surla.change_id", card.ChangeID),
				attribute.String("surla.suite_commit", saved.SuiteCommit),
			),
		)
		stale := *card
		stale.Approval = nil
		stale.UnverifiedApproval = nil
		return &stale, nil
	}
	a := *saved
	a.TestSource = approvalTestSource(source, saved.SuiteCommit)

	target, err := c.resolvePromotion(ctx)
	if err == nil && target == nil {
		withSource := *card
		withSource.Approval = &a
		return &withSource, nil
	}
	var artifacts map[string]string
	if err == nil {
		artifacts, err = target.CurrentArtifacts(ctx, a.CandidateCommit)
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		unverified := *card
		unverified.Approval = nil
		unverified.UnverifiedApproval = &gensurla.UnverifiedApprovalItem{
			Approval: &a,
			Reason: fmt.Sprintf(
				"Saved approval for candidate commit %s could not be verified because the release binaries could not be read. Publish the binaries the run built into the release folder that the promotion settings name, in a folder named after the commit, and press Check current approval; or remove the promotion settings with surla promotion remove and restart Surla; or press Approve this result on a newly verified run.",
				a.CandidateCommit,
			),
		}
		return &unverified, nil
	}
	if maps.Equal(artifacts, a.Artifacts) {
		withSource := *card
		withSource.Approval = &a
		return &withSource, nil
	}
	stale := *card
	stale.Approval = nil
	stale.UnverifiedApproval = nil
	return &stale, nil
}

// approvalTestSource builds an ApprovalItem's TestSource union from the
// tests' source and commit.
func approvalTestSource(source verify.TestsSource, commit string) gensurla.ApprovalTestSource {
	if source == verify.TestsEvaluator {
		return gensurla.NewApprovalTestSourceEvaluator(&gensurla.OperatorEvaluatorTests{})
	}
	return gensurla.NewApprovalTestSourceWrittenTests(&gensurla.AgentWrittenTests{Commit: commit, Notice: writtenTestsNotice})
}

// resolvePromotion resolves a PromotionTarget dynamically from the currently
// bound workspace directory when ResolvePromotionTarget is set, or returns the
// static PromotionTarget.
func (c *ControlPlane) resolvePromotion(ctx context.Context) (PromotionTarget, error) {
	if c.resolvePromotionTarget != nil {
		return c.resolvePromotionTarget(ctx, c.currentWorkspaceDir())
	}
	if c.promotionTarget != nil {
		return c.promotionTarget, nil
	}
	return nil, nil
}
