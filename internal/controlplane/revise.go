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
// card's current request_revision. Only the card's owner may revise it. It
// first checks p against the design's rules with the generated
// gensurla.ValidateReviseChangePayloadValue, so a Go caller gets the same
// structural checks the HTTP and MCP decoders apply. It then checks the rules
// that span fields: every current example is kept (examples[].prior_id) or
// dropped (removed_example_ids) exactly once, and each ambiguity's options
// are distinct. Kept examples keep their IDs, new examples get the lowest
// unused check_NN ID, and the revision is saved in needs_confirmation status
// with no answers, confirmed_by or approval. When story_refresh_op_id is
// given, the revision adopts the evaluated Stories
// and design/*.go snapshot from that succeeded refresh_story operation; when
// story_refresh_op_id is omitted on a card that already has Stories, the
// saved Stories and saved design source carry over together; otherwise the
// design source is read from the bound workspace, or kept when no workspace
// is bound.
// The card is saved only if it still has the caller's request_revision; a
// payload that breaks a rule, a blank change_id, request_revision or prompt,
// an invalid or stale story_refresh_op_id, or an unreadable workspace design
// is unprocessable and leaves the card unchanged.
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

	// Phase 1: read the current card without holding the write lock, because
	// recording a workspace design reads the bound workspace, which must not
	// happen under c.mu.
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
	if record.card.RequestRevision != p.RequestRevision {
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"request_revision mismatch for %q: got %q, want %q",
			p.ChangeID,
			p.RequestRevision,
			record.card.RequestRevision,
		))
	}

	designSrc := record.designSource
	var nextStories []designeval.StorySummary
	switch {
	case p.StoryRefreshOpID != nil:
		designSrc, nextStories, err = c.adoptStoryRefresh(principal, *p.StoryRefreshOpID)
		if err != nil {
			return nil, err
		}
	case len(record.stories) > 0:
		nextStories = cloneStorySummaries(record.stories)
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
	// caller's request_revision names; Phase 3 saves only while the card still
	// has that revision, so they are still the card's examples then.
	ambiguities, examples, preserved, err := revisedCardContent(p.ChangeID, record.card.Examples, p.Analysis)
	if err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1RejectedAnalysisSummary(true)))
		return nil, gensurla.MakeUnprocessable(fmt.Errorf("invalid analysis: %w", err))
	}
	// Every revised card enters needs_confirmation so the operator explicitly
	// reviews and confirms the revised summary, decisions, and examples before
	// advancing to downstream checkpoints.
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
	// card since Phase 1.
	c.mu.Lock()
	defer c.mu.Unlock()
	current, exists, err := c.cards.load(p.ChangeID)
	if err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(true, cp1FaultStorageRead)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "load cp1 change card")
		return nil, err
	}
	if !exists {
		span.SetAttributes(attribute.String("surla.public_summary", "Change Card was not found; check change_id or call propose_change."))
		return nil, gensurla.MakeNotFound(fmt.Errorf("unknown change_id %q", p.ChangeID))
	}
	if current.owner != principal.ID {
		span.SetAttributes(attribute.String("surla.public_summary", "Caller does not own this Change Card (this request did not modify the Change Card)."))
		return nil, gensurla.MakeForbidden(fmt.Errorf("principal %q is not the owner of change_id %q", principal.ID, p.ChangeID))
	}
	if current.card.RequestRevision != p.RequestRevision {
		span.SetAttributes(attribute.String("surla.public_summary", "Change Card request_revision changed concurrently (this request did not modify the Change Card); call get_change and retry revise_change."))
		return nil, gensurla.MakeConflict(fmt.Errorf(
			"request_revision mismatch for %q: got %q, want %q",
			p.ChangeID,
			p.RequestRevision,
			current.card.RequestRevision,
		))
	}
	if err := c.cards.save(storedChangeCard{
		card:         revised,
		owner:        principal.ID,
		designSource: designSrc,
		stories:      nextStories,
	}); err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(true, cp1FaultStorageSave)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "save revised cp1 change card")
		return nil, err
	}
	span.SetAttributes(attribute.String("surla.public_summary", "Revised Change Card (needs_confirmation)."))
	return revised, nil
}
