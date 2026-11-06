// This file implements Checkpoint 5 (CP5) promotion approval recording
// (`approve_merge`) and read-time verification of saved approvals against the
// bound PromotionTarget (`currentCard`).

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verify"
)

// ApproveMerge records Checkpoint 5 approval of the caller's confirmed Change
// Card revision. It loads the verification report by digest and the current
// suite, environment and binary digests from the bound PromotionTarget, and
// requires verify.Approve to accept the report for exactly that change. The
// approval is saved on a copy of the card only if the card still has the same
// request_revision after those calls, so a confirm_intent or revise_change
// that changed the card meanwhile is never approved with the old report. It
// returns the approved card; nothing is merged or deployed.
func (c *ControlPlane) ApproveMerge(
	ctx context.Context,
	p *gensurla.ApproveMergePayload,
) (*gensurla.ChangeCardResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if c.promotionTarget == nil {
		return nil, gensurla.MakeNotImplemented(errors.New("no promotion target is bound to this control plane"))
	}

	c.mu.RLock()
	record, exists, err := c.cards.load(p.ChangeID)
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

	// The target calls may be slow, so they run without holding c.mu. A card
	// is never removed and never changes owner; its request_revision and
	// confirmed status are checked again under c.mu before the approval is
	// written.
	report, err := c.promotionTarget.LoadReport(ctx, p.ReportDigest)
	if err != nil {
		return nil, fmt.Errorf("load verification report %s: %w", p.ReportDigest, err)
	}
	if report.Digest != p.ReportDigest {
		return nil, fmt.Errorf("promotion target returned report %s for digest %s", report.Digest, p.ReportDigest)
	}
	inputs, err := c.promotionTarget.CurrentInputs(ctx, p.CandidateCommit)
	if err != nil {
		return nil, fmt.Errorf("load promotion inputs for %s: %w", p.CandidateCommit, err)
	}
	current := verify.Binding{
		CandidateCommit: p.CandidateCommit,
		RequestRevision: p.RequestRevision,
		SuiteID:         inputs.SuiteID,
		SuiteCommit:     inputs.SuiteCommit,
		Artifacts:       inputs.Artifacts,
		Environment:     inputs.Environment,
	}
	if err := verify.Approve(current, report); err != nil {
		if errors.Is(err, verify.ErrStaleReport) {
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
	if record.card.RequestRevision != p.RequestRevision {
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"request_revision mismatch for %q: got %q, want %q",
			p.ChangeID,
			p.RequestRevision,
			record.card.RequestRevision,
		))
	}
	approved := *record.card
	approved.Approval = &gensurla.ApprovalItem{
		ApprovedBy:      principal.ID,
		ReportDigest:    report.Digest,
		CandidateCommit: p.CandidateCommit,
		SuiteID:         inputs.SuiteID,
		SuiteCommit:     inputs.SuiteCommit,
		Environment:     inputs.Environment,
		Artifacts:       maps.Clone(inputs.Artifacts),
		ScenarioCount:   report.Counts.Required,
	}
	record.card = &approved
	if err := c.cards.save(record); err != nil {
		return nil, err
	}
	return &approved, nil
}

// currentCard returns card as get_change and an unchanged confirm_intent
// report it:
//  1. When card has no saved approval, card is returned as-is (`Approval == nil`,
//     `UnverifiedApproval == nil`) without calling PromotionTarget.
//  2. When card has a saved approval and the bound PromotionTarget reports
//     current inputs matching the approval's suite, environment, and binary
//     digests, card is returned with `Approval` set and `UnverifiedApproval == nil`.
//  3. When card has a saved approval and PromotionTarget reports current inputs
//     that differ, a shallow copy is returned with `Approval == nil` and
//     `UnverifiedApproval == nil` (the saved approval on disk is untouched and
//     is returned in `Approval` again if target inputs match again).
//  4. When card has a saved approval and PromotionTarget.CurrentInputs returns
//     an error (for example because the candidate commit's staged release
//     directory or binary is missing or the target runs runtime prompt
//     overrides), the raw error is recorded on the `controlplane.check_approval`
//     OpenTelemetry span and a shallow copy is returned with `Approval == nil`
//     and `UnverifiedApproval` carrying the recorded approval and a path-free
//     recovery reason.
//  5. When card has a saved approval and no PromotionTarget is bound, a shallow
//     copy is returned with `Approval == nil` and `UnverifiedApproval` carrying
//     the recorded approval and a recovery reason naming `-promotion-config`.
//
// Callers must not hold c.mu because PromotionTarget.CurrentInputs may be slow.
func (c *ControlPlane) currentCard(
	ctx context.Context,
	card *gensurla.ChangeCardResult,
) (*gensurla.ChangeCardResult, error) {
	a := card.Approval
	if a == nil {
		return card, nil
	}
	if c.promotionTarget == nil {
		unverified := *card
		unverified.Approval = nil
		unverified.UnverifiedApproval = &gensurla.UnverifiedApprovalItem{
			Approval: a,
			Reason: fmt.Sprintf(
				"Saved approval for candidate commit %s could not be verified because no promotion target is configured (-promotion-config). Configure a promotion target and reload the card with get_change.",
				a.CandidateCommit,
			),
		}
		return &unverified, nil
	}
	ctx, span := otel.Tracer("github.com/goadesign/surla/internal/controlplane").Start(
		ctx,
		"controlplane.check_approval",
		trace.WithAttributes(
			attribute.String("surla.change_id", card.ChangeID),
			attribute.String("surla.candidate_commit", a.CandidateCommit),
		),
	)
	defer span.End()

	inputs, err := c.promotionTarget.CurrentInputs(ctx, a.CandidateCommit)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		unverified := *card
		unverified.Approval = nil
		unverified.UnverifiedApproval = &gensurla.UnverifiedApprovalItem{
			Approval: a,
			Reason: fmt.Sprintf(
				"Saved approval for candidate commit %s could not be verified because the promotion target could not report its current inputs. Check the promotion target and reload the card with get_change, or run approve_merge for a newly verified commit.",
				a.CandidateCommit,
			),
		}
		return &unverified, nil
	}
	if inputs.SuiteID == a.SuiteID &&
		inputs.SuiteCommit == a.SuiteCommit &&
		inputs.Environment == a.Environment &&
		maps.Equal(inputs.Artifacts, a.Artifacts) {
		return card, nil
	}
	stale := *card
	stale.Approval = nil
	stale.UnverifiedApproval = nil
	return &stale, nil
}
