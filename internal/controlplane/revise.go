// This file implements Checkpoint 1 (CP1) Change Card revision (`revise_change`).

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	gensurla "github.com/goadesign/surla/gen/surla"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// ReviseChange saves the next revision of an existing Checkpoint 1 Change
// Card from the caller's revised feature request and analysis, bound to the
// card's current request_revision. Only the card's owner may revise it. The
// generated HTTP and MCP decoders have already checked the analysis's
// structure; ReviseChange checks the rules that span fields: every current
// example is kept (examples[].prior_id) or dropped (removed_example_ids)
// exactly once, and each ambiguity's options are distinct. Kept examples keep
// their IDs, new examples get the lowest unused check_NN ID, and the revision
// is saved in needs_confirmation status with no answers, confirmed_by or
// approval. The design source is design_source when given; otherwise a
// workspace-backed card records design/*.go from the bound workspace again
// and any other card keeps its saved design source. The card is saved only if
// it still has the caller's request_revision; a broken rule or an unreadable
// workspace design is unprocessable and leaves the card unchanged.
func (c *ControlPlane) ReviseChange(
	ctx context.Context,
	p *gensurla.ReviseChangePayload,
) (*gensurla.ChangeCardResult, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	if p == nil ||
		strings.TrimSpace(p.ChangeID) == "" ||
		strings.TrimSpace(p.RequestRevision) == "" ||
		strings.TrimSpace(p.Prompt) == "" ||
		p.Analysis == nil {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id, request_revision, prompt, and analysis are required"))
	}
	if !utf8.ValidString(p.ChangeID) {
		return nil, gensurla.MakeUnprocessable(errors.New("change_id must be valid UTF-8"))
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
	nextOrigin := record.sourceOrigin
	switch {
	case p.DesignSource != nil:
		designSrc = *p.DesignSource
		nextOrigin = sourceOriginSupplied
	case nextOrigin == sourceOriginWorkspace:
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
		sourceOrigin: nextOrigin,
	}); err != nil {
		span.SetAttributes(attribute.String("surla.public_summary", cp1SafeFaultSummary(true, cp1FaultStorageSave)))
		span.RecordError(err)
		span.SetStatus(codes.Error, "save revised cp1 change card")
		return nil, err
	}
	span.SetAttributes(attribute.String("surla.public_summary", "Revised Change Card (needs_confirmation)."))
	return revised, nil
}
