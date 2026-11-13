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
	// TeamReminder carries only code-selected harmless text and the accepted Herdr condition.
	TeamReminder struct{ Terminal, Text, Condition string }
	// TeamSender reports sent, blocked, status_changed, finished or unavailable.
	// It never forces input or waits for an agent's next turn.
	TeamSender interface {
		Send(context.Context, TeamReminder) (string, error)
	}
	// roleObservation is the small published snapshot with ownership identity.
	roleObservation struct {
		handle, space string
		public        gensurla.TeamRoleState
	}
	// supervisedRole keeps only current terminals and current owed generations.
	// Unknown reads preserve latches; only a known clear removes a generation.
	supervisedRole struct {
		role             TeamRole
		terminals        map[string]TeamTerminal
		sequence         uint64
		ready            bool
		fresh            bool
		seen             bool
		firstSeen        time.Time
		owed             map[string]bool
		assignmentKnown  bool
		watchKnown, held bool
		absentSince      time.Time
		watchSent        bool
		refusedState     string
		public           gensurla.TeamRoleState
	}
	// teamSupervisor is private to one blocking Supervise call and owns all latches.
	teamSupervisor struct {
		server    *Server
		selection TeamSelection
		roles     map[string]*supervisedRole
		started   time.Time
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
)

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
	owner := &teamSupervisor{server: s, roles: map[string]*supervisedRole{}, started: s.now()}
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
		current.terminals = map[string]TeamTerminal{}
		current.watchKnown = false
		current.absentSince = time.Time{}
		next[role.Role] = current
	}
	o.selection, o.roles = selection, next
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
			role.ready = false
		}
		return
	}
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
		return
	}
	if !role.ready {
		return
	}
	if event.Kind == "heartbeat" {
		o.refresh(ctx, role)
		return
	}
	if event.Sequence <= role.sequence {
		return
	}
	role.sequence = event.Sequence
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

// watchers counts a continuous known absence; outages forbid watcher action and
// restart the grace clock, while preserving the once-per-absence reminder latch.
func (o *teamSupervisor) watchers(view TeamWatcherView, err error, now time.Time) {
	for _, role := range o.roles {
		role.watchKnown = err == nil
		if err != nil {
			role.absentSince = time.Time{}
			continue
		}
		role.held = view.Held[role.role.Handle]
		if role.held {
			role.absentSince = time.Time{}
			role.watchSent = false
		} else if role.absentSince.IsZero() {
			role.absentSince = now
		}
	}
}

// publish derives only declared values, records transitions and optionally fixed continuity spans.
func (o *teamSupervisor) publish(ctx context.Context, now time.Time, periodic bool) {
	allReady := true
	for _, role := range o.roles {
		allReady = allReady && role.ready && role.fresh
	}
	observed := make(map[string]roleObservation, len(o.roles))
	for _, role := range o.roles {
		value := role.value(now, allReady, o.server.team.dependencies.Shell)
		if value.State != role.public.State {
			role.refusedState = ""
		}
		if value != role.public {
			teamStateSpan(ctx, "surla.team.changed", value, "", "observed")
		}
		if periodic {
			teamStateSpan(ctx, "surla.team.observed", value, "", "observed")
		}
		role.public = value
		observed[role.role.Role] = roleObservation{handle: role.role.Handle, space: o.selection.Space, public: value}
	}
	o.server.team.observationMu.Lock()
	o.server.team.observed = observed
	o.server.team.observationMu.Unlock()
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

// remind acts only on a waiting role with a positively identified foreground
// from a fresh read. One physical reminder covers the currently owed generation set.
func (o *teamSupervisor) remind(ctx context.Context, now time.Time) {
	if now.Sub(o.started) < teamWatcherGrace {
		return
	}
	for _, role := range o.roles {
		if role.public.State != "waiting" || role.refusedState == role.public.State {
			continue
		}
		reason, text := "", ""
		if role.assignmentKnown {
			for _, sent := range role.owed {
				if !sent {
					reason, text = "assignment", assignmentReminder(role.role.Role)
					break
				}
			}
		}
		if reason == "" && role.public.Watcher == "gone" && !role.watchSent {
			reason, text = "watcher", watcherReminder
		}
		if text == "" {
			continue
		}
		for address := range role.terminals {
			readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			terminal, err := o.server.team.dependencies.Observer.ReadTerminal(readCtx, address)
			cancel()
			if err != nil {
				role.fresh = false
				o.publish(ctx, now, false)
				return
			}
			if !o.selection.owns(terminal, role.role.Handle) {
				continue
			}
			if terminal.Foreground == nil || len(terminal.Foreground.Argv) == 0 || terminal.Foreground.Argv[0] == "" || !roleRunning(terminal, o.server.team.dependencies.Shell) {
				continue
			}
			if !terminal.Reported || terminal.Status == nil || terminal.Status.State != "idle" && terminal.Status.State != "done" {
				continue
			}
			condition := ""
			if terminal.Status.State == "idle" {
				condition = "idle"
			}
			outcome := "unavailable"
			if o.server.team.dependencies.Sender != nil {
				sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				outcome, err = o.server.team.dependencies.Sender.Send(sendCtx, TeamReminder{Terminal: address, Text: text, Condition: condition})
				cancel()
			}
			if err != nil {
				outcome = "unavailable"
			}
			switch outcome {
			case "sent", "blocked", "status_changed", "finished", "unavailable":
			default:
				outcome = "unavailable"
			}
			teamStateSpan(ctx, "surla.team.reminder", role.public, reason, outcome)
			if outcome != "sent" {
				role.refusedState = role.public.State
				continue
			}
			if reason == "assignment" {
				for generation := range role.owed {
					role.owed[generation] = true
				}
			} else {
				role.watchSent = true
			}
		}
	}
}

// Fixed reminder text starts with a non-command word and contains only letters,
// digits, spaces, underscores and full stops. It carries no work details.
const watcherReminder = "SurlaReminder. Please start your Chorus watcher again."

// assignmentReminder names only the operations allowed to the addressed role.
func assignmentReminder(role string) string {
	switch role {
	case "builder":
		return "SurlaReminder. Please use list_builder_assignments and get_builder_assignment to read your assignments."
	case "test_writer":
		return "SurlaReminder. Please use list_test_writer_assignments and get_test_writer_assignment to read your assignments."
	default:
		return ""
	}
}

// teamStateSpan exports bounded facts through the project tracer, outside the
// control plane's principal-scoped bounded diagnostics recorder.
func teamStateSpan(ctx context.Context, name string, value gensurla.TeamRoleState, reason, outcome string) {
	_, span := otel.Tracer("surla/project").Start(ctx, name)
	span.SetAttributes(attribute.String("team.role", value.Role), attribute.String("team.state", value.State), attribute.String("team.kind", value.Attention), attribute.String("team.watcher", value.Watcher), attribute.String("team.assignment", value.Assignment), attribute.String("team.reason", reason), attribute.String("team.outcome", outcome))
	span.End()
}

// teamReaderFailure records a start/stream failure without retaining external command output.
func teamReaderFailure(ctx context.Context) {
	_, span := otel.Tracer("surla/project").Start(ctx, "surla.team.reader_failed")
	span.RecordError(errors.New("team observation reader could not continue"))
	span.SetStatus(codes.Error, "team reader unavailable")
	span.End()
}
