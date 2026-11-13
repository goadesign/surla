// This file implements Checkpoint 1 (CP1) Change Card revision (`revise_change`).

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/designeval"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ReviseChange saves the next revision of an existing Checkpoint 1 Change
// Card from the caller's revised feature request and analysis, bound to the
// latest revision: the proposed revision when one waits beside a confirmed
// card, otherwise the card itself. Only the card's owner may revise it. It
// first checks p against the design's rules with the generated
// gensurla.ValidateReviseChangePayloadValue, so a Go caller gets the same
// structural checks the HTTP and MCP decoders apply. It then checks the rules
// that span fields: every example of the revised revision is kept
// (examples[].prior_id) or dropped (removed_example_ids) exactly once, and
// each ambiguity's options are distinct. Kept examples keep their IDs, new
// examples get the lowest unused check_NN ID, and the revision is built in
// needs_confirmation status with no answers, confirmed_by or approval. When
// story_refresh_op_id is given, the revision adopts the evaluated Stories and
// design/*.go snapshot from that succeeded refresh_story operation; when it
// is omitted on a revision that has Stories, those Stories and their design
// source carry over together; otherwise the design source is read from the
// bound workspace, or kept when no workspace is bound.
//
// A card that was never confirmed is replaced by the new revision. Beside a
// confirmed card the new revision is saved as the proposal, replacing any
// earlier one, and the confirmed revision stays in force with its answers,
// confirmation, approval, Stories and design source, until the user adopts
// the proposal (ConfirmIntent) or declines it (DeclineRevision). The change
// is saved only if its latest revision is still the caller's
// request_revision; a payload that breaks a rule, a blank change_id,
// request_revision or prompt, an invalid or stale story_refresh_op_id, or an
// unreadable workspace design is unprocessable and leaves the change as it
// was. The result is the change as GetChange returns it.
func (c *ControlPlane) ReviseChange(
	ctx context.Context,
	p *gensurla.ReviseChangePayload,
) (*gensurla.ChangeCardResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if err := gensurla.ValidateReviseChangePayloadValue(p); err != nil {
		return nil, gensurla.MakeUnprocessable(err)
	}
	if strings.TrimSpace(p.ChangeID) == "" ||
		strings.TrimSpace(p.RequestRevision) == "" ||
		strings.TrimSpace(p.Prompt) == "" {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id, request_revision, and prompt are required"))
	}

	// Phase 1: read the current change without holding the write lock,
	// because recording a workspace design reads the bound workspace, which
	// must not happen under c.mu.
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
	latest := record.latest()
	if err := checkLatestRevision(record, p.RequestRevision); err != nil {
		return nil, err
	}

	designSrc := latest.designSource
	var nextStories []designeval.StorySummary
	switch {
	case p.StoryRefreshOpID != nil:
		designSrc, nextStories, err = c.adoptStoryRefresh(principal, *p.StoryRefreshOpID)
		if err != nil {
			return nil, err
		}
	case len(latest.stories) > 0:
		nextStories = cloneStorySummaries(latest.stories)
	default:
		if wsDir, wsDev, wsIno := c.currentWorkspaceTarget(); wsDir != "" {
			designSrc, err = readWorkspaceDesignSource(wsDir, wsDev, wsIno)
			if err != nil {
				return nil, err
			}
		}
	}

	_, span := c.tracer("surla/controlplane").Start(
		ctx,
		"surla.controlplane.revise_change",
		trace.WithAttributes(attribute.String("surla.change_id", p.ChangeID)),
	)
	defer span.End()

	// Phase 2: build the revision. The examples checked here are the ones the
	// caller's request_revision names; Phase 3 saves only while that is still
	// the latest revision, so they are still its examples then.
	ambiguities, examples, preserved, err := revisedCardContent(p.ChangeID, latest.card.Examples, p.Analysis)
	if err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1RejectedAnalysisSummary(true)))
		return nil, gensurla.MakeUnprocessable(fmt.Errorf("invalid analysis: %w", err))
	}
	// Every revision enters needs_confirmation so the user explicitly reviews
	// and confirms the revised summary, decisions, and examples before it
	// reaches downstream checkpoints.
	revised := &gensurla.ChangeCardResult{
		ChangeID:           p.ChangeID,
		Status:             StatusNeedsConfirmation,
		Prompt:             p.Prompt,
		Summary:            p.Analysis.Summary,
		Ambiguities:        ambiguities,
		Examples:           examples,
		PreservedBehaviors: preserved,
		Stories:            storySummaryItemsOrNil(nextStories),
	}
	rev, err := computeCanonicalRevision(revised, principal.ID, designSrc)
	if err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(true, cp1FaultRevision)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "compute revised cp1 card revision")
		return nil, err
	}
	revised.RequestRevision = rev
	if p.Analysis.UIComposition != nil {
		revised.UIComposition = &gensurla.UICompositionItem{
			Program:             *p.Analysis.UIComposition,
			ComposedForRevision: rev,
		}
	}

	// Phase 3: save under the write lock only if no other request changed the
	// change since Phase 1.
	saved, err := c.saveRevision(span, principal, p, storedRevision{card: revised, designSource: designSrc, stories: nextStories})
	if err != nil {
		return nil, err
	}
	return c.changeResult(ctx, saved)
}

// DeclineRevision removes the revision the guide proposed beside a confirmed
// card, which p.RequestRevision must name. The confirmed revision in force
// stays exactly as it was. Only the card's owner reaches this; the operation
// table lets only the user's session call it. The result is the change as
// GetChange returns it.
func (c *ControlPlane) DeclineRevision(
	ctx context.Context,
	p *gensurla.DeclineRevisionPayload,
) (*gensurla.ChangeCardResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if strings.TrimSpace(p.ChangeID) == "" || strings.TrimSpace(p.RequestRevision) == "" {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id and request_revision are required"))
	}
	record, err := c.declineRevision(principal, p)
	if err != nil {
		return nil, err
	}
	return c.changeResult(ctx, record)
}

// latest returns the revision a new revision is made from: the proposal
// when one waits, otherwise the revision in force.
func (r storedChangeCard) latest() storedRevision {
	if r.proposed != nil {
		return *r.proposed
	}
	return storedRevision{card: r.card, designSource: r.designSource, stories: r.stories}
}

// checkLatestRevision refuses a revision request that does not name record's
// latest revision. Naming the revision in force while a proposal waits gets a
// message saying the proposal must be revised instead.
func checkLatestRevision(record storedChangeCard, requestRevision string) error {
	latest := record.latest().card.RequestRevision
	switch {
	case requestRevision == latest:
		return nil
	case record.proposed != nil && requestRevision == record.card.RequestRevision:
		return gensurla.MakeConflict(fmt.Errorf(
			"change_id %q has a proposed revision %q waiting beside the confirmed revision; read it with get_change and revise it by naming its request_revision",
			record.card.ChangeID,
			latest,
		))
	default:
		return gensurla.MakeConflict(fmt.Errorf(
			"request_revision mismatch for %q: got %q, want %q",
			record.card.ChangeID,
			requestRevision,
			latest,
		))
	}
}

// saveRevision saves next under c.mu as the latest revision of p.ChangeID,
// provided that is still the revision p names: it replaces a card that was
// never confirmed, or becomes the proposal beside a confirmed card. It
// returns the saved change.
func (c *ControlPlane) saveRevision(
	span trace.Span,
	principal Principal,
	p *gensurla.ReviseChangePayload,
	next storedRevision,
) (storedChangeCard, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	current, exists, err := c.cards.load(p.ChangeID)
	if err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(true, cp1FaultStorageRead)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "load cp1 change card")
		return storedChangeCard{}, err
	}
	if !exists {
		span.SetAttributes(attribute.String("surla.public_summary", "Change Card was not found; check change_id or call propose_change."))
		return storedChangeCard{}, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	}
	if current.owner != principal.ID {
		span.SetAttributes(attribute.String("surla.public_summary", "Caller does not own this Change Card (this request did not modify the Change Card)."))
		return storedChangeCard{}, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	}
	if err := checkLatestRevision(current, p.RequestRevision); err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", "Change Card request_revision changed concurrently (this request did not modify the Change Card); call get_change and retry revise_change."))
		return storedChangeCard{}, err
	}
	saved := storedChangeCard{card: next.card, owner: principal.ID, designSource: next.designSource, stories: next.stories}
	summary := "Revised Change Card (needs_confirmation)."
	if current.card.Status == StatusConfirmed {
		saved = current
		saved.proposed = &next
		summary = "Proposed a revision beside the confirmed Change Card (needs the user's adoption)."
	}
	if err := c.cards.save(saved); err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(true, cp1FaultStorageSave)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "save revised cp1 change card")
		return storedChangeCard{}, err
	}
	span.SetAttributes(attribute.String("surla.public_summary", summary))
	return saved, nil
}

// declineRevision removes the proposal p names from its change under c.mu
// and returns the saved change.
func (c *ControlPlane) declineRevision(principal Principal, p *gensurla.DeclineRevisionPayload) (storedChangeCard, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, exists, err := c.cards.load(p.ChangeID)
	if err != nil {
		return storedChangeCard{}, err
	}
	if !exists {
		return storedChangeCard{}, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	}
	if record.owner != principal.ID {
		return storedChangeCard{}, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	}
	if record.proposed == nil {
		return storedChangeCard{}, gensurla.MakeConflict(fmt.Errorf("change_id %q has no proposed revision to decline", p.ChangeID))
	}
	if record.proposed.card.RequestRevision != p.RequestRevision {
		return storedChangeCard{}, gensurla.MakeConflict(fmt.Errorf(
			"request_revision %q is not the proposed revision of %q; its proposed revision is %q",
			p.RequestRevision,
			p.ChangeID,
			record.proposed.card.RequestRevision,
		))
	}
	record.proposed = nil
	if err := c.cards.save(record); err != nil {
		return storedChangeCard{}, err
	}
	return record, nil
}

// changeResult returns record as GetChange reports it: the revision in force
// as currentCard reports it, with the proposed revision beside it when one
// waits. Callers must not hold c.mu, because currentCard may ask a slow
// promotion target.
func (c *ControlPlane) changeResult(ctx context.Context, record storedChangeCard) (*gensurla.ChangeCardResult, error) {
	card, err := c.currentCard(ctx, record.card)
	if err != nil {
		return nil, err
	}
	if record.proposed == nil {
		return card, nil
	}
	out := *card
	out.ProposedRevision = proposedRevisionItem(record.proposed)
	return &out, nil
}

// proposedRevisionItem is proposed as a change result reports it, or nil
// when no proposal waits.
func proposedRevisionItem(proposed *storedRevision) *gensurla.ProposedRevisionItem {
	if proposed == nil {
		return nil
	}
	card := proposed.card
	return &gensurla.ProposedRevisionItem{
		Prompt:             card.Prompt,
		Summary:            card.Summary,
		Ambiguities:        card.Ambiguities,
		Examples:           card.Examples,
		PreservedBehaviors: card.PreservedBehaviors,
		Stories:            card.Stories,
		RequestRevision:    card.RequestRevision,
		UIComposition:      card.UIComposition,
	}
}
