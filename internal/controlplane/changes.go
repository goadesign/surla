// This file lists the owner's saved Change Cards and lets the views page wait
// until that list or the owner's Story evaluation changes.
package controlplane

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/verifyrun"
)

const (
	// defaultChangesWait bounds one wait_for_changes call when nothing changes.
	// It stays below the project server's five-second ordered-shutdown grace,
	// so an open page never makes `surla stop` wait for its request.
	defaultChangesWait = 4 * time.Second
	// defaultChangesPoll is how often a waiting call reads the list again.
	// Verification runs are written by separate processes, so only reading
	// the saved state sees their progress.
	defaultChangesPoll = time.Second
	// waitForChangesSpan names the waiting read's span. Its spans are
	// exported like every operation's but never saved in the diagnostics
	// history (see diagnosticTracer.Start).
	waitForChangesSpan = "surla.controlplane.wait_for_changes"
)

// ListChanges returns the caller's saved Change Cards for this project, most
// recently saved first (tie-broken by change_id), with the state of each
// card's most recently accepted verification run, whether this server can
// record approvals, and a version that changes whenever any listed value
// changes or the owner's Story evaluation starts, finishes or is replaced. A
// card or run record that cannot be read fails the call rather
// than being left out of the list.
func (c *ControlPlane) ListChanges(ctx context.Context) (*gensurla.ChangeList, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	ctx, span := c.tracer("surla/controlplane").Start(ctx, "surla.controlplane.list_changes")
	defer span.End()
	list, err := c.changeList(ctx, span, principal.ID)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.Int("surla.change_count", len(list.Changes)))
	return list, nil
}

// WaitForChanges returns the version of the caller's current change list as
// soon as it differs from p.Since, or after a bounded wait with the unchanged
// version. It reads the saved list again every poll interval and ends early
// when the caller goes away; it never changes anything. The views page keeps
// one such call open, so its span is never saved in the diagnostics history.
func (c *ControlPlane) WaitForChanges(
	ctx context.Context,
	p *gensurla.WaitForChangesPayload,
) (*gensurla.ChangeListVersion, error) {
	principal, ok := PrincipalFromContext(ctx)
	if !ok {
		return nil, errors.New("authenticated principal is required")
	}
	ctx, span := c.tracer("surla/controlplane").Start(ctx, waitForChangesSpan)
	defer span.End()
	wait := cmp.Or(c.changesWait, defaultChangesWait)
	poll := cmp.Or(c.changesPoll, defaultChangesPoll)
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		list, err := c.changeList(ctx, span, principal.ID)
		if err != nil {
			return nil, err
		}
		if list.Version != p.Since {
			span.SetAttributes(attribute.Bool("surla.changed", true))
			return &gensurla.ChangeListVersion{Version: list.Version}, nil
		}
		select {
		case <-ctx.Done():
			// The page closed or paused its request; nobody reads a result.
			return nil, ctx.Err()
		case <-deadline.C:
			span.SetAttributes(attribute.Bool("surla.changed", false))
			return &gensurla.ChangeListVersion{Version: list.Version}, nil
		case <-ticker.C:
		}
	}
}

// changeList reads owner's saved cards and, when verification is configured,
// the newest run of each of owner's changes in one pass over the runs
// directory, then orders the cards by saved time, resolves whether approvals
// can be recorded, and derives the list's version from both and from the
// owner's current Story evaluation, so the version also changes when that
// evaluation starts, finishes or is replaced. Card files are
// read under the control plane's read lock; run records, which verification
// processes write without that lock, are read after it is released. When a
// record cannot be read, the cause is recorded on span and the returned error
// names only which kind of record failed. A promotion target that cannot be
// resolved does not fail the list: it is recorded on span and approvals are
// reported unavailable, as approve_merge refuses them.
func (c *ControlPlane) changeList(ctx context.Context, span trace.Span, owner string) (*gensurla.ChangeList, error) {
	c.mu.RLock()
	records, err := c.cards.all()
	c.mu.RUnlock()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "read change cards")
		return nil, errors.New("list changes: saved change cards could not be read")
	}
	var latest map[string]verifyrun.Run
	if runsDir := c.runsDir; runsDir != "" {
		latest, err = verifyrun.LatestByChange(runsDir, owner)
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "read verification runs")
			return nil, errors.New("list changes: saved verification records could not be read")
		}
	}
	records = slices.DeleteFunc(records, func(r storedChangeCard) bool { return r.owner != owner })
	slices.SortFunc(records, func(a, b storedChangeCard) int {
		if byTime := b.savedAt.Compare(a.savedAt); byTime != 0 {
			return byTime
		}
		return strings.Compare(a.card.ChangeID, b.card.ChangeID)
	})
	items := make([]*gensurla.ChangeListItem, 0, len(records))
	for _, record := range records {
		item := &gensurla.ChangeListItem{
			ChangeID:        record.card.ChangeID,
			Summary:         record.card.Summary,
			Status:          record.card.Status,
			RequestRevision: record.card.RequestRevision,
			SavedAt:         record.savedAt.UTC().Format(time.RFC3339Nano),
		}
		if run, ok := latest[record.card.ChangeID]; ok {
			item.LatestVerification = latestVerification(run)
		}
		if record.proposed != nil {
			item.ProposedRevision = &record.proposed.card.RequestRevision
		}
		items = append(items, item)
	}
	target, err := c.resolvePromotion(ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "resolve promotion target")
	}
	approvalAvailable := err == nil && target != nil
	var team *gensurla.TeamSnapshot
	if c.team != nil && c.team.Owner == owner {
		team, err = c.team.Service.ReadTeam(ctx)
		if err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(struct {
		Changes           []*gensurla.ChangeListItem `json:"changes"`
		ApprovalAvailable bool                       `json:"approval_available"`
		Team              *gensurla.TeamSnapshot     `json:"team"`
		StoryEvaluation   storyEvaluationVersion     `json:"story_evaluation"`
	}{items, approvalAvailable, team, c.storyEvaluation(owner)})
	if err != nil {
		err = fmt.Errorf("encode change list version: %w", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "encode change list")
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &gensurla.ChangeList{
		Changes:           items,
		Version:           hex.EncodeToString(sum[:]),
		ApprovalAvailable: approvalAvailable,
	}, nil
}

// latestVerification reduces a run to the state the change list shows; a
// completed run reports its report's verdict. It relies on verifyrun
// reporting only the three kinds below: no encoder between here and the
// caller checks the state against the design's values.
func latestVerification(run verifyrun.Run) *gensurla.LatestVerificationItem {
	var state string
	switch run.State.Kind {
	case verifyrun.StateRunning:
		state = "running"
	case verifyrun.StateCompleted:
		state = "failed"
		if run.State.Report.Passed {
			state = "passed"
		}
	case verifyrun.StateNotCompleted:
		state = "not_completed"
	}
	return &gensurla.LatestVerificationItem{RunID: run.Request.RunID, State: state}
}
