// This file schedules and drains the team's injected readers without owning outside services.
package project

import (
	"context"
	"sync"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
)

type (
	// watcherObservation transfers only the narrowed current watcher facts.
	watcherObservation struct {
		view TeamWatcherView
		err  error
	}
	// teamReaders owns cancellation and joining for one immutable registered selection.
	teamReaders struct {
		cancel  context.CancelFunc
		workers sync.WaitGroup
	}
)

// run is the one owner loop. Assignment reads and all state transitions serialize
// here; stream and long-poll readers merely deliver typed observations.
func (o *teamSupervisor) run(ctx context.Context, assignments controlplane.AssignmentReader) error {
	events := make(chan TeamEvent, 32)
	watchers := make(chan watcherObservation, 1)
	var readers *teamReaders
	defer func() {
		if readers != nil {
			readers.close()
		}
		// A stopped owner leaves no apparently healthy snapshot behind.
		for _, role := range o.roles {
			role.ready, role.watchKnown = false, false
			role.assignmentKnown = false
		}
		o.publish(context.WithoutCancel(ctx), o.server.now(), false)
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	observations := time.NewTicker(teamObservationInterval)
	defer observations.Stop()
	for {
		if o.syncSelection(o.server.teamSelection()) {
			if readers != nil {
				readers.close()
				readers = nil
			}
			// Drop queued observations from the previous selection after joining its producers.
			drainTeamObservations(events, watchers)
			// One owner read scans all cards and their build files; do it when
			// selection changes, never on the one-second display/registry tick.
			o.assignments(ctx, assignments)
			if o.selection.Space != "" && len(o.selection.Roles) > 0 && o.server.team.dependencies.Observer != nil {
				readers = o.startReaders(ctx, events, watchers)
			}
		}
		now := o.server.now()
		if o.publish(ctx, now, false) {
			o.restartReaders = false
			// A stalled handle makes every role unknown before draining readers.
			// Restart all subscriptions after the same bounded failure delay;
			// heartbeats cannot restore ready before their fresh snapshots.
			if readers != nil {
				readers.close()
				readers = nil
			}
			drainTeamObservations(events, watchers)
			if !waitTeamReader(ctx) {
				return nil
			}
			if o.selection.Space != "" && len(o.selection.Roles) > 0 && o.server.team.dependencies.Observer != nil {
				readers = o.startReaders(ctx, events, watchers)
			}
			continue
		}
		o.remind(ctx, now)
		select {
		case <-ctx.Done():
			return nil
		case event := <-events:
			o.observeEvent(ctx, event, o.server.now(), assignments)
		case observation := <-watchers:
			o.watchers(observation.view, observation.err, o.server.now())
		case <-ticker.C:
		case <-observations.C:
			o.publish(ctx, o.server.now(), true)
		}
	}
}

// observeEvent refreshes assignment facts only with a new terminal observation.
// Each callback scans all owner cards/build files; watcher updates, periodic spans
// and the one-second waiting tick must not repeat that filesystem walk.
func (o *teamSupervisor) observeEvent(
	ctx context.Context,
	event TeamEvent,
	now time.Time,
	assignments controlplane.AssignmentReader,
) {
	o.event(ctx, event, now)
	if event.Kind == "snapshot" || event.Kind == "heartbeat" ||
		event.Kind == "event" && (event.Type == "status" || event.Type == "turn") {
		o.assignments(ctx, assignments)
	}
}

// startReaders captures the selection once; changing registry/space cancels and joins it.
func (o *teamSupervisor) startReaders(ctx context.Context, events chan<- TeamEvent, watchers chan<- watcherObservation) *teamReaders {
	ctx, cancel := context.WithCancel(ctx)
	readers := &teamReaders{cancel: cancel}
	selection := o.selection
	observer := o.server.team.dependencies.Observer
	readers.workers.Go(func() { followTeam(ctx, observer, selection, events) })
	readers.workers.Go(func() { followWatchers(ctx, observer, selection, watchers) })
	return readers
}

// close drains all readers before another selection or the store owner can proceed.
func (r *teamReaders) close() { r.cancel(); r.workers.Wait() }

// followTeam reconnects after failures with a fresh snapshot and a bounded delay.
func followTeam(ctx context.Context, observer TeamObserver, selection TeamSelection, events chan<- TeamEvent) {
	for ctx.Err() == nil {
		emit := func(event TeamEvent) {
			select {
			case events <- event:
			case <-ctx.Done():
			}
		}
		emit(TeamEvent{Kind: "recovering"})
		err := observer.Follow(ctx, selection, emit)
		if ctx.Err() != nil {
			return
		}
		emit(TeamEvent{Kind: "failed"})
		if err != nil {
			teamReaderFailure(ctx)
		}
		if !waitTeamReader(ctx) {
			return
		}
	}
}

// followWatchers uses conditional reads while retaining only handle/watcher facts.
// A 304 is a fresh confirmation of the previous narrowed snapshot, including after a failure.
func followWatchers(ctx context.Context, observer TeamObserver, selection TeamSelection, updates chan<- watcherObservation) {
	var current TeamWatcherView
	for ctx.Err() == nil {
		view, err := observer.ReadWatchers(ctx, selection.Team, current.Version, selection.Roles)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if !view.Unchanged {
				current = view
			}
			view = current
		}
		select {
		case updates <- watcherObservation{view: view, err: err}:
		case <-ctx.Done():
			return
		}
		// Conditional reads can return immediately on busy teams; a bounded pause
		// prevents unrelated message churn from becoming a CPU or request loop.
		if !waitTeamReader(ctx) {
			return
		}
	}
}

// waitTeamReader ends promptly on cancellation and never starts an outside dependency.
func waitTeamReader(ctx context.Context) bool {
	timer := time.NewTimer(teamReaderRetry)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// drainTeamObservations discards only queued values whose reader has already joined.
func drainTeamObservations(events <-chan TeamEvent, watchers <-chan watcherObservation) {
	for {
		select {
		case <-events:
		default:
			goto watchers
		}
	}
watchers:
	for {
		select {
		case <-watchers:
		default:
			return
		}
	}
}
