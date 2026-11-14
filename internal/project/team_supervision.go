// This file owns the team's in-memory observation and reminder state machine.
package project

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type (
	// TeamRole selects one registered handle, never a guessed terminal or role label.
	TeamRole struct{ Role, Handle string }
	// TeamSelection is the intersection required before observing any terminal.
	TeamSelection struct {
		Root, Space, Team string
		Roles             []TeamRole
	}
	// TeamStatus excludes messages, titles, application names and other free text.
	TeamStatus struct {
		State string `json:"state"`
		Kind  string `json:"kind"`
	}
	// TeamEvent is a narrowed Herdr observation. Snapshots are scoped to Role;
	// events are ordered by Sequence within that handle's subscription.
	TeamEvent struct {
		Kind, Role, Type, Terminal string
		Sequence                   uint64
		Terminals                  []TeamTerminal
		Labels                     map[string]string
	}
	// TeamWatcherView retains only registered agents' watcher facts, never messages.
	// Held has a key exactly for each registered handle the Chorus view listed.
	TeamWatcherView struct {
		Version   string
		Unchanged bool
		Held      map[string]bool
	}
	// TeamObserver owns the injected read-only external contracts. Follow ends on
	// cancellation, emits snapshots before ordered events, and never starts services.
	TeamObserver interface {
		FindTerminals(context.Context, TeamSelection) ([]TeamTerminal, error)
		Follow(context.Context, TeamSelection, func(TeamEvent)) error
		ReadTerminal(context.Context, string) (TeamTerminal, error)
		ReadWatchers(context.Context, string, string, []TeamRole) (TeamWatcherView, error)
	}
	// TeamReminder is one fixed Chorus direct message chosen by code. Team is the
	// saved team (the Chorus swarm), From is Surla's own handle in it, To is the
	// registered role handle without its leading "@", and Text asks only for a read.
	TeamReminder struct{ Team, From, To, Text string }
	// TeamSender hands one reminder to the local Chorus server. It returns nil when
	// Chorus accepted the message, or a TeamSendFailure naming the post that failed
	// and how. It never keeps or returns any byte of what Chorus answered.
	TeamSender interface {
		Send(context.Context, TeamReminder) error
	}
	// TeamSendFailure is a sender's only error. Post is "who" or "send"; Kind is
	// connection, timeout, http_status, rpc_error, tool_error or response. Neither
	// carries response text, which agents can write.
	TeamSendFailure struct{ Post, Kind string }
	// roleObservation is the small published snapshot with ownership identity.
	roleObservation struct {
		handle, space string
		public        gensurla.TeamRoleState
	}
	// supervisedRole keeps only current terminals and current owed generations.
	// Unknown reads preserve latches; only a known clear removes a generation.
	supervisedRole struct {
		role      TeamRole
		terminals map[string]TeamTerminal
		sequence  uint64
		ready     bool
		fresh     bool
		seen      bool
		firstSeen time.Time
		// lastEvent bounds one handle's stream, independently of other active roles.
		lastEvent time.Time
		// owed maps each unread generation to whether a reminder was attempted for it.
		owed            map[string]bool
		assignmentKnown bool
		// watchKnown reports that the last Chorus view read succeeded.
		watchKnown, held bool
		absentSince      time.Time
		// listedSince is when the view began listing the handle without a break; zero
		// when the last successful read did not list it. A failed read keeps it.
		listedSince time.Time
		// idleUnreadSince is when the role began waiting with an unread assignment.
		idleUnreadSince time.Time
		public          gensurla.TeamRoleState
	}
	// teamSupervisor is private to one blocking Supervise call and owns all latches.
	teamSupervisor struct {
		server    *Server
		selection TeamSelection
		roles     map[string]*supervisedRole
		// restartReaders latches a stale stream until the driver resubscribes.
		restartReaders bool
	}
)

const (
	// teamSilenceBound is display-only: a trust prompt may report nothing for 60 seconds.
	teamSilenceBound = 60 * time.Second
	// teamWatcherGrace exceeds Chorus's 60-second view wait by 30 seconds to rearm a watcher.
	teamWatcherGrace = 90 * time.Second
	// teamObservationInterval proves continuity without filling local operation history.
	teamObservationInterval = 30 * time.Second
	// teamReaderRetry prevents reconnect failures from spinning or starting outside services.
	teamReaderRetry = 2 * time.Second
	// teamStreamStallBound allows three expected 15-second Herdr heartbeats.
	// Missing all three invalidates every role until new snapshots establish the stream.
	teamStreamStallBound = 3 * 15 * time.Second
	// teamJoinSettle lets a role that has just joined Chorus act on its first
	// message, which already asks for the read, before Surla reminds it.
	teamJoinSettle = 60 * time.Second
	// teamIdleUnreadBound is how long a role may wait with an unread assignment
	// before the user is asked to prompt it; it reuses the watcher grace.
	teamIdleUnreadBound = teamWatcherGrace
)

// Error returns fixed text; Post and Kind carry the bounded facts.
func (TeamSendFailure) Error() string { return "team reminder post failed" }

// Supervise owns one blocking lifetime, ending on either owner context. Its driver
// joins every external reader before returning so the control plane can close stores.
func (s *Server) Supervise(ctx context.Context, assignments controlplane.AssignmentReader) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(s.team.dependencies.Lifetime, cancel)
	defer stop()
	if s.team.dependencies.Lifetime.Err() != nil {
		cancel()
	}
	owner := &teamSupervisor{server: s, roles: map[string]*supervisedRole{}}
	return owner.run(ctx, assignments)
}

// owns revalidates all three labels on freshly returned facts before any action.
func (selection TeamSelection) owns(t TeamTerminal, handle string) bool {
	return t.Address != "" && t.Labels["chorus.project"] == selection.Root &&
		t.Labels["herdr.space"] == selection.Space && t.Labels["chorus.handle"] == handle
}

// syncSelection preserves latches for unchanged registered handles when another role joins.
func (o *teamSupervisor) syncSelection(selection TeamSelection) bool {
	if selection.Root == o.selection.Root && selection.Space == o.selection.Space && selection.Team == o.selection.Team && slices.Equal(selection.Roles, o.selection.Roles) {
		return false
	}
	next := make(map[string]*supervisedRole, len(selection.Roles))
	for _, role := range selection.Roles {
		current := o.roles[role.Role]
		if current == nil || current.role != role {
			current = &supervisedRole{role: role, owed: map[string]bool{}}
		}
		current.ready = false
		current.lastEvent = o.server.now()
		current.terminals = map[string]TeamTerminal{}
		current.watchKnown = false
		current.absentSince = time.Time{}
		next[role.Role] = current
	}
	o.selection, o.roles = selection, next
	o.restartReaders = false
	return true
}

// assignments replaces known owed sets only after a complete owner read succeeded.
func (o *teamSupervisor) assignments(ctx context.Context, read controlplane.AssignmentReader) {
	owed, err := read(ctx)
	for _, role := range o.roles {
		role.assignmentKnown = err == nil || role.role.Role == "guide"
		if err != nil {
			continue
		}
		current := map[string]bool{}
		for _, item := range owed {
			if item.Role == role.role.Role {
				current[item.Generation] = role.owed[item.Generation]
			}
		}
		role.owed = current
	}
}

// event applies ordered stream facts. A missed stream invalidates all roles until
// the affected subscription supplies its fresh snapshot; failure invalidates every subscription.
func (o *teamSupervisor) event(ctx context.Context, event TeamEvent, now time.Time) {
	if event.Kind == "recovering" || event.Kind == "failed" {
		for _, role := range o.roles {
			role.ready, role.fresh = false, false
			role.lastEvent = now
		}
		return
	}
	o.expireStream(now)
	role := o.roles[event.Role]
	if role == nil {
		return
	}
	if event.Kind == "missed" {
		role.ready = false
		return
	}
	if event.Kind == "snapshot" {
		role.terminals = map[string]TeamTerminal{}
		for _, terminal := range event.Terminals {
			if o.selection.owns(terminal, role.role.Handle) {
				role.terminals[terminal.Address] = terminal
				role.seen = true
			}
		}
		role.sequence, role.ready, role.fresh = event.Sequence, true, true
		role.firstSeen = now
		role.lastEvent = now
		return
	}
	if !role.ready {
		return
	}
	if event.Kind == "heartbeat" {
		role.lastEvent = now
		o.refresh(ctx, role)
		return
	}
	if event.Sequence <= role.sequence {
		return
	}
	role.sequence = event.Sequence
	role.lastEvent = now
	terminal, found := role.terminals[event.Terminal]
	switch event.Type {
	case "created", "restarted", "labels":
		// Labels must establish membership before a terminal can be read.
		if event.Labels != nil {
			terminal.Address, terminal.Labels = event.Terminal, maps.Clone(event.Labels)
			if o.selection.owns(terminal, role.role.Handle) {
				role.terminals[event.Terminal] = terminal
				role.seen = true
				o.refresh(ctx, role)
			} else {
				delete(role.terminals, event.Terminal)
			}
		}
	case "lost", "removed", "finished":
		if found {
			terminal.State = "finished"
			terminal.Foreground = nil
			role.terminals[event.Terminal] = terminal
		}
	case "status", "turn":
		if !found {
			return
		}
		// A fresh typed get also covers foreground changes, reporting changes and ownership.
		o.refresh(ctx, role)
	}
}

// refresh makes one bounded foreground read for each already-owned terminal, never an unknown handle.
func (o *teamSupervisor) refresh(ctx context.Context, role *supervisedRole) {
	for address, old := range role.terminals {
		if !o.selection.owns(old, role.role.Handle) {
			delete(role.terminals, address)
			continue
		}
		readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		terminal, err := o.server.team.dependencies.Observer.ReadTerminal(readCtx, address)
		cancel()
		if err != nil {
			role.fresh = false
			return
		}
		if !o.selection.owns(terminal, role.role.Handle) {
			delete(role.terminals, address)
			continue
		}
		role.terminals[address] = terminal
	}
	role.fresh = true
}

// watchers counts a continuous known watcher absence and a continuous listing.
// An outage makes both unknown and restarts the absence clock, but keeps the
// listing clock: only a successful read that omits the handle restarts it.
func (o *teamSupervisor) watchers(view TeamWatcherView, err error, now time.Time) {
	for _, role := range o.roles {
		role.watchKnown = err == nil
		if err != nil {
			role.absentSince = time.Time{}
			continue
		}
		held, listed := view.Held[role.role.Handle]
		role.held = held
		if role.held {
			role.absentSince = time.Time{}
		} else if role.absentSince.IsZero() {
			role.absentSince = now
		}
		if !listed {
			role.listedSince = time.Time{}
		} else if role.listedSince.IsZero() {
			role.listedSince = now
		}
	}
}

// publish derives declared values and reports whether readers need fresh subscriptions.
// A stale role invalidates all roles, including those with continuing heartbeats.
func (o *teamSupervisor) publish(ctx context.Context, now time.Time, periodic bool) bool {
	o.expireStream(now)
	stalled := o.restartReaders
	allReady := true
	for _, role := range o.roles {
		allReady = allReady && role.ready && role.fresh
	}
	observed := make(map[string]roleObservation, len(o.roles))
	for _, role := range o.roles {
		value := role.idleUnread(role.value(now, allReady, o.server.team.dependencies.Shell), now)
		if value != role.public {
			teamStateSpan(ctx, "surla.team.changed", value, "", "observed").End()
		}
		if periodic {
			teamStateSpan(ctx, "surla.team.observed", value, "", "observed").End()
		}
		role.public = value
		observed[role.role.Role] = roleObservation{handle: role.role.Handle, space: o.selection.Space, public: value}
	}
	o.server.team.observationMu.Lock()
	o.server.team.observed = observed
	o.server.team.observationMu.Unlock()
	return stalled
}

// expireStream latches unknown for every subscription, so an ordinary heartbeat
// cannot revive a timed-out stream. Only a fresh snapshot sets ready again.
func (o *teamSupervisor) expireStream(now time.Time) bool {
	stalled := false
	for _, role := range o.roles {
		stalled = stalled || now.Sub(role.lastEvent) >= teamStreamStallBound
	}
	if !stalled {
		return false
	}
	o.restartReaders = true
	for _, role := range o.roles {
		role.ready, role.fresh = false, false
		role.lastEvent = now
	}
	return true
}

// value keeps terminal availability, watcher availability and assignment availability independent.
func (role *supervisedRole) value(now time.Time, allReady bool, shell string) gensurla.TeamRoleState {
	value := gensurla.TeamRoleState{Role: role.role.Role, Handle: role.role.Handle, State: "unknown", Attention: "none", Watcher: "unknown", Assignment: "unknown"}
	if role.assignmentKnown {
		value.Assignment = "nothing_owed"
		if len(role.owed) > 0 {
			value.Assignment = "unread"
		}
	}
	if role.role.Role == "guide" {
		value.Assignment = "nothing_owed"
	}
	if role.watchKnown {
		if role.held {
			value.Watcher = "held"
		} else if !role.absentSince.IsZero() && now.Sub(role.absentSince) >= teamWatcherGrace {
			value.Watcher = "gone"
		}
	}
	if !allReady {
		return value
	}
	if len(role.terminals) == 0 {
		value.State = "not_started"
		if role.seen {
			value.State = "ended"
		}
		return value
	}
	live := 0
	for _, terminal := range role.terminals {
		if terminal.State == "live" {
			live++
		}
	}
	if live == 0 {
		value.State = "ended"
		return value
	}
	if live > 1 {
		return value
	}
	for _, terminal := range role.terminals {
		if terminal.State != "live" {
			continue
		}
		if terminal.Reason == "spawn_pending" {
			value.State = "starting"
			return value
		}
		if !roleRunning(terminal, shell) {
			value.State = "ended"
			return value
		}
		if !terminal.Reported || terminal.Status == nil {
			since := role.firstSeen
			if terminal.CreatedMS > 0 {
				since = time.UnixMilli(terminal.CreatedMS)
			}
			value.State = "starting"
			if now.Sub(since) >= teamSilenceBound {
				value.State, value.Attention = "needs_you", "silent"
			}
			return value
		}
		switch terminal.Status.State {
		case "working":
			value.State = "working"
		case "idle", "done":
			value.State = "waiting"
		case "error":
			value.State = "error"
		case "blocked":
			value.State, value.Attention = "needs_you", "unknown"
			switch terminal.Status.Kind {
			case "permission", "question", "auth":
				value.Attention = terminal.Status.Kind
			}
		}
	}
	return value
}

// idleUnread turns a role that has waited with an unread assignment for the
// bound into needs_you with attention assignment, so the page names the user's
// step. Working, reading or any unknown fact restarts the clock.
func (role *supervisedRole) idleUnread(value gensurla.TeamRoleState, now time.Time) gensurla.TeamRoleState {
	if value.State != "waiting" || value.Assignment != "unread" {
		role.idleUnreadSince = time.Time{}
		return value
	}
	if role.idleUnreadSince.IsZero() {
		role.idleUnreadSince = now
	}
	if now.Sub(role.idleUnreadSince) >= teamIdleUnreadBound {
		value.State, value.Attention = "needs_you", "assignment"
	}
	return value
}

// remind sends one Chorus direct message per unread generation. It requires a
// complete assignment read with a generation not yet attempted, and a last
// Chorus view read that succeeded and has listed the handle for the settle
// time. Herdr state and the watcher fact change nothing: the message waits in
// Chorus for the role's next read. An unmet condition is an expected negative
// and records nothing. Both outcomes of an attempt, sent and failed, latch every
// generation the attempt covered, so no generation is ever tried twice. A post
// that fails after ctx ended, as when the server stops, records nothing either.
func (o *teamSupervisor) remind(ctx context.Context, now time.Time) {
	sender := o.server.team.dependencies.Sender
	if sender == nil {
		return
	}
	for _, role := range o.roles {
		text := assignmentReminder(role.role.Role)
		if text == "" || !role.assignmentKnown || !role.watchKnown || role.listedSince.IsZero() || now.Sub(role.listedSince) < teamJoinSettle {
			continue
		}
		pending := false
		for _, attempted := range role.owed {
			pending = pending || !attempted
		}
		if !pending {
			continue
		}
		for generation := range role.owed {
			role.owed[generation] = true
		}
		err := sender.Send(ctx, TeamReminder{Team: o.selection.Team, From: o.server.teamSurlaHandle(), To: role.role.Handle, Text: text})
		if err != nil && ctx.Err() != nil {
			// The supervisor's context ended, as when the server stops: Surla cut
			// the post short, so it is no Chorus failure and records no attempt.
			return
		}
		outcome := "sent"
		if err != nil {
			outcome = "failed"
		}
		span := teamStateSpan(ctx, "surla.team.reminder", role.public, "assignment", outcome)
		if err != nil {
			var failure TeamSendFailure
			if errors.As(err, &failure) {
				span.SetAttributes(attribute.String("team.post", failure.Post), attribute.String("team.failure", failure.Kind))
			}
			span.RecordError(errors.New("team reminder post failed"))
			span.SetStatus(codes.Error, "team reminder post failed")
		}
		span.End()
	}
}

// assignmentReminder is the fixed Chorus message for a role. It names only the
// two read operations allowed to that role and nothing about the work. The
// guide has no assignments, so it has no reminder.
func assignmentReminder(role string) string {
	switch role {
	case "builder":
		return "Surla: you have an unread assignment. Use list_builder_assignments and get_builder_assignment to read it. Surla reads no replies."
	case "test_writer":
		return "Surla: you have an unread assignment. Use list_test_writer_assignments and get_test_writer_assignment to read it. Surla reads no replies."
	default:
		return ""
	}
}

// teamStateSpan exports bounded facts through the project tracer, outside the
// control plane's principal-scoped recorder. The caller ends the span after
// recording an operation failure, if any, with a fixed bounded error.
func teamStateSpan(ctx context.Context, name string, value gensurla.TeamRoleState, reason, outcome string) trace.Span {
	_, span := otel.Tracer("surla/project").Start(ctx, name)
	span.SetAttributes(attribute.String("team.role", value.Role), attribute.String("team.state", value.State), attribute.String("team.kind", value.Attention), attribute.String("team.watcher", value.Watcher), attribute.String("team.assignment", value.Assignment), attribute.String("team.reason", reason), attribute.String("team.outcome", outcome))
	return span
}

// teamSurlaHandle is Surla's own Chorus handle in the team, derived from the
// project ID as the guide's handle is. No role can take it: role handles start
// with guide, builder or test_writer.
func (s *Server) teamSurlaHandle() string {
	s.state.mu.Lock()
	defer s.state.mu.Unlock()
	return teamName("surla", s.state.Record.ID)
}

// teamReaderFailure records a start/stream failure without retaining external command output.
func teamReaderFailure(ctx context.Context) {
	_, span := otel.Tracer("surla/project").Start(ctx, "surla.team.reader_failed")
	span.RecordError(errors.New("team observation reader could not continue"))
	span.SetStatus(codes.Error, "team reader unavailable")
	span.End()
}
