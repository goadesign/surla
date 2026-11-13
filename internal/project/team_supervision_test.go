// These tests use recorded observations only; no terminal, service or window is contacted.
package project

import (
	"context"
	"strings"
	"testing"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	"github.com/goadesign/surla/internal/controlplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type (
	// supervisionFixture records every effect and supplies only current narrow facts.
	supervisionFixture struct {
		terminal TeamTerminal
		readErr  error
		reads    []string
		sends    []TeamReminder
		outcome  string
		sendErr  error
		follow   func(context.Context, TeamSelection, func(TeamEvent)) error
		watch    func(context.Context) error
	}
)

// FindTerminals returns this fixture's owned candidates without a discovery effect.
func (f *supervisionFixture) FindTerminals(context.Context, TeamSelection) ([]TeamTerminal, error) {
	return []TeamTerminal{f.terminal}, f.readErr
}

// ReadTerminal records the exact already-selected identifier.
func (f *supervisionFixture) ReadTerminal(_ context.Context, address string) (TeamTerminal, error) {
	f.reads = append(f.reads, address)
	return f.terminal, f.readErr
}

// Follow supplies a cancellable recorded event source.
func (f *supervisionFixture) Follow(ctx context.Context, selection TeamSelection, emit func(TeamEvent)) error {
	if f.follow != nil {
		return f.follow(ctx, selection, emit)
	}
	<-ctx.Done()
	return ctx.Err()
}

// ReadWatchers ends with its owner; no real Chorus view is read.
func (f *supervisionFixture) ReadWatchers(ctx context.Context, _, _ string, _ []TeamRole) (TeamWatcherView, error) {
	if f.watch != nil {
		return TeamWatcherView{}, f.watch(ctx)
	}
	<-ctx.Done()
	return TeamWatcherView{}, ctx.Err()
}

// Send records exact harmless text and never changes a terminal.
func (f *supervisionFixture) Send(_ context.Context, reminder TeamReminder) (string, error) {
	f.sends = append(f.sends, reminder)
	return f.outcome, f.sendErr
}

// newSupervisionFixture returns one live waiting builder with all effects recorded.
func newSupervisionFixture(t *testing.T) (*teamSupervisor, *supervisionFixture, time.Time) {
	t.Helper()
	state := testState(t)
	state.Record.Team, state.Record.Space = "example-team", "space"
	state.Record.Agents = map[string]string{"builder-example": "builder"}
	f := &supervisionFixture{outcome: "sent"}
	f.terminal = TeamTerminal{Address: "terminal", State: "live", Reported: true, Status: &TeamStatus{State: "idle"}, Foreground: &TeamForeground{Argv: []string{"codex"}}, Labels: map[string]string{"chorus.project": state.Record.Directory, "herdr.space": "space", "chorus.handle": "builder-example"}}
	deps := fixtureTeamDependencies(state)
	deps.Observer, deps.Sender = f, f
	s, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	o := &teamSupervisor{server: s, roles: map[string]*supervisedRole{}, started: now.Add(-teamWatcherGrace)}
	o.syncSelection(s.teamSelection())
	o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 10, Terminals: []TeamTerminal{f.terminal}}, now)
	o.assignments(t.Context(), func(context.Context) ([]controlplane.AssignmentGeneration, error) {
		return []controlplane.AssignmentGeneration{{Role: "builder", Generation: "one"}}, nil
	})
	o.publish(t.Context(), now, false)
	return o, f, now
}

// TestTeamRoleStates tests every declared state and both display/grace boundaries.
func TestTeamRoleStates(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	role := o.roles["builder"]
	for _, tc := range []struct {
		status, kind, want, attention string
		reported                      bool
	}{
		{"working", "", "working", "none", true}, {"idle", "", "waiting", "none", true}, {"done", "", "waiting", "none", true},
		{"blocked", "permission", "needs_you", "permission", true}, {"blocked", "question", "needs_you", "question", true}, {"blocked", "auth", "needs_you", "auth", true}, {"blocked", "unexpected", "needs_you", "unknown", true},
		{"error", "", "error", "none", true}, {"unexpected", "", "unknown", "none", true}, {"", "", "starting", "none", false},
	} {
		t.Run(tc.status+tc.kind, func(t *testing.T) {
			terminal := f.terminal
			terminal.Status = &TeamStatus{State: tc.status, Kind: tc.kind}
			terminal.Reported = tc.reported
			role.terminals = map[string]TeamTerminal{terminal.Address: terminal}
			value := role.value(now, true, "/bin/zsh")
			assert.Equal(t, tc.want, value.State)
			assert.Equal(t, tc.attention, value.Attention)
		})
	}
	for _, age := range []time.Duration{teamSilenceBound - time.Nanosecond, teamSilenceBound, teamSilenceBound + time.Nanosecond} {
		terminal := f.terminal
		terminal.Reported = false
		role.terminals = map[string]TeamTerminal{terminal.Address: terminal}
		role.firstSeen = now.Add(-age)
		value := role.value(now, true, "/bin/zsh")
		if age < teamSilenceBound {
			assert.Equal(t, "starting", value.State)
		} else {
			assert.Equal(t, "needs_you", value.State)
			assert.Equal(t, "silent", value.Attention)
		}
	}
	for _, age := range []time.Duration{teamWatcherGrace - time.Nanosecond, teamWatcherGrace, teamWatcherGrace + time.Nanosecond} {
		role.watchKnown = true
		role.held = false
		role.absentSince = now.Add(-age)
		if age < teamWatcherGrace {
			assert.Equal(t, "unknown", role.value(now, true, "zsh").Watcher)
		} else {
			assert.Equal(t, "gone", role.value(now, true, "zsh").Watcher)
		}
	}
	role.terminals = nil
	role.seen = false
	assert.Equal(t, "not_started", role.value(now, true, "zsh").State)
	for _, state := range []string{"live", "finished"} {
		terminal := f.terminal
		terminal.State = state
		terminal.Foreground = &TeamForeground{Argv: []string{"/bin/zsh", "-l"}}
		role.terminals = map[string]TeamTerminal{terminal.Address: terminal}
		assert.Equal(t, "ended", role.value(now, true, "zsh").State)
	}
	terminal := f.terminal
	terminal.Reason = "spawn_pending"
	role.terminals = map[string]TeamTerminal{terminal.Address: terminal}
	assert.Equal(t, "starting", role.value(now, true, "zsh").State)
	assert.Equal(t, "unknown", role.value(now, false, "zsh").State)
}

// TestTeamReminderSafetyTable challenges every reason and foreground/status combination.
func TestTeamReminderSafetyTable(t *testing.T) {
	for _, scenario := range []string{"idle", "done", "working", "blocked", "error", "silent", "unknown", "starting", "shell", "missing foreground", "empty argv", "empty argv0", "wrong project", "wrong space", "wrong handle", "fresh working", "read failed", "startup", "no debt", "watcher", "watcher unknown", "assignment unknown watcher gone"} {
		t.Run(scenario, func(t *testing.T) {
			o, f, now := newSupervisionFixture(t)
			role := o.roles["builder"]
			want := scenario == "idle" || scenario == "done" || scenario == "watcher" || scenario == "assignment unknown watcher gone"
			switch scenario {
			case "done", "working", "error", "blocked":
				f.terminal.Status.State = scenario
			case "silent", "starting":
				f.terminal.Reported = false
				role.firstSeen = now.Add(-teamSilenceBound)
				if scenario == "starting" {
					role.firstSeen = now
				}
			case "unknown":
				role.ready = false
			case "shell":
				f.terminal.Foreground.Argv = []string{"/bin/zsh", "-l"}
			case "missing foreground":
				f.terminal.Foreground = nil
			case "empty argv":
				f.terminal.Foreground.Argv = nil
			case "empty argv0":
				f.terminal.Foreground.Argv = []string{""}
			case "wrong project":
				f.terminal.Labels["chorus.project"] = "/another"
			case "wrong space":
				f.terminal.Labels["herdr.space"] = "another"
			case "wrong handle":
				f.terminal.Labels["chorus.handle"] = "another"
			case "read failed":
				f.readErr = assert.AnError
			case "startup":
				o.started = now.Add(-teamWatcherGrace + time.Nanosecond)
			case "no debt", "watcher", "watcher unknown", "assignment unknown watcher gone":
				role.owed = nil
			}
			role.terminals = map[string]TeamTerminal{f.terminal.Address: f.terminal}
			if scenario == "watcher" || scenario == "assignment unknown watcher gone" {
				role.watchKnown = true
				role.absentSince = now.Add(-teamWatcherGrace)
			}
			if scenario == "assignment unknown watcher gone" {
				role.assignmentKnown = false
			}
			o.publish(t.Context(), now, false)
			if scenario == "fresh working" {
				f.terminal.Status = &TeamStatus{State: "working"}
			}
			o.remind(t.Context(), now)
			assert.Len(t, f.sends, map[bool]int{true: 1, false: 0}[want])
			if want {
				assert.Equal(t, map[bool]string{true: "", false: "idle"}[scenario == "done"], f.sends[0].Condition)
			}
		})
	}
}

// TestTeamReminderGenerations preserves old latches through unknown reads and
// covers multiple simultaneously owed generations with one fixed reminder.
func TestTeamReminderGenerations(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	set := func(generations ...string) {
		o.assignments(t.Context(), func(context.Context) ([]controlplane.AssignmentGeneration, error) {
			var owed []controlplane.AssignmentGeneration
			for _, g := range generations {
				owed = append(owed, controlplane.AssignmentGeneration{Role: "builder", Generation: g})
			}
			return owed, nil
		})
		o.publish(t.Context(), now, false)
	}
	set("one", "two")
	o.remind(t.Context(), now)
	assert.Len(t, f.sends, 1)
	o.assignments(t.Context(), func(context.Context) ([]controlplane.AssignmentGeneration, error) { return nil, assert.AnError })
	o.publish(t.Context(), now, false)
	assert.Equal(t, "unknown", o.roles["builder"].public.Assignment)
	o.remind(t.Context(), now)
	set("one", "two")
	o.remind(t.Context(), now)
	assert.Len(t, f.sends, 1)
	set("one", "two", "three")
	o.remind(t.Context(), now)
	assert.Len(t, f.sends, 2)
	set()
	assert.Empty(t, o.roles["builder"].owed)
	set("one")
	o.remind(t.Context(), now)
	assert.Len(t, f.sends, 3)
	// An observed role join must not forget existing assignments' latch.
	selection := o.selection
	selection.Roles = append(slicesTeamRoles(selection.Roles), TeamRole{Role: "test_writer", Handle: "writer"})
	o.syncSelection(selection)
	assert.True(t, o.roles["builder"].owed["one"])
}

// slicesTeamRoles copies selection data so tests do not mutate a live owner snapshot.
func slicesTeamRoles(roles []TeamRole) []TeamRole { return append([]TeamRole(nil), roles...) }

// TestTeamReminderRefusals waits for a public role-state transition before retrying.
func TestTeamReminderRefusals(t *testing.T) {
	for _, outcome := range []string{"blocked", "status_changed", "finished", "unavailable"} {
		t.Run(outcome, func(t *testing.T) {
			o, f, now := newSupervisionFixture(t)
			f.outcome = outcome
			o.remind(t.Context(), now)
			o.remind(t.Context(), now)
			assert.Len(t, f.sends, 1)
			role := o.roles["builder"]
			terminal := f.terminal
			terminal.Status = &TeamStatus{State: "working"}
			role.terminals[terminal.Address] = terminal
			o.publish(t.Context(), now, false)
			role.terminals[f.terminal.Address] = f.terminal
			o.publish(t.Context(), now, false)
			f.outcome = "sent"
			o.remind(t.Context(), now)
			assert.Len(t, f.sends, 2)
		})
	}
}

// TestTeamWatcherAbsences proves grace, outage uncertainty and once-per-absence behavior.
func TestTeamWatcherAbsences(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	o.roles["builder"].owed = nil
	view := TeamWatcherView{Held: map[string]bool{}}
	o.watchers(view, nil, now.Add(-teamWatcherGrace))
	o.publish(t.Context(), now, false)
	o.remind(t.Context(), now)
	o.remind(t.Context(), now)
	assert.Len(t, f.sends, 1)
	o.watchers(view, assert.AnError, now)
	o.publish(t.Context(), now, false)
	assert.Equal(t, "unknown", o.roles["builder"].public.Watcher)
	o.watchers(view, nil, now)
	for elapsed := 15 * time.Second; elapsed <= teamWatcherGrace; elapsed += 15 * time.Second {
		o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "builder"}, now.Add(elapsed))
	}
	recovered := now.Add(teamWatcherGrace)
	o.publish(t.Context(), recovered, false)
	o.remind(t.Context(), recovered)
	assert.Len(t, f.sends, 1)
	o.watchers(TeamWatcherView{Held: map[string]bool{"builder-example": true}}, nil, recovered)
	o.watchers(view, nil, recovered)
	for elapsed := 15 * time.Second; elapsed <= teamWatcherGrace; elapsed += 15 * time.Second {
		o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "builder"}, recovered.Add(elapsed))
	}
	o.publish(t.Context(), recovered.Add(teamWatcherGrace), false)
	o.remind(t.Context(), recovered.Add(teamWatcherGrace))
	assert.Len(t, f.sends, 2)
}

// TestTeamStreamRecovery protects against out-of-order events and stale ownership.
func TestTeamStreamRecovery(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	o.event(t.Context(), TeamEvent{Kind: "event", Role: "builder", Type: "finished", Terminal: "terminal", Sequence: 9}, now)
	assert.Equal(t, "live", o.roles["builder"].terminals["terminal"].State)
	o.event(t.Context(), TeamEvent{Kind: "missed", Role: "builder"}, now)
	o.publish(t.Context(), now, false)
	o.remind(t.Context(), now)
	assert.Equal(t, "unknown", o.roles["builder"].public.State)
	assert.Empty(t, f.sends)
	o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 20, Terminals: []TeamTerminal{f.terminal}}, now)
	f.terminal.Foreground.Argv = []string{"zsh", "-l"}
	o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "builder"}, now)
	o.publish(t.Context(), now, false)
	assert.Equal(t, "ended", o.roles["builder"].public.State)
	for _, key := range []string{"chorus.project", "herdr.space", "chorus.handle"} {
		terminal := f.terminal
		terminal.Labels = map[string]string{"chorus.project": o.selection.Root, "herdr.space": o.selection.Space, "chorus.handle": "builder-example"}
		terminal.Labels[key] = "other"
		before := len(f.reads)
		o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 30, Terminals: []TeamTerminal{terminal}}, now)
		o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "builder"}, now)
		assert.Len(t, f.reads, before)
	}
	o.event(t.Context(), TeamEvent{Kind: "failed"}, now)
	o.publish(t.Context(), now, false)
	assert.Equal(t, "unknown", o.roles["builder"].public.State)
}

// TestTeamStreamEndThenReadFailure keeps every role unknown and forbids input
// through reconnect and a failed fresh foreground read; a later heartbeat recovers.
func TestTeamStreamEndThenReadFailure(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	o.event(t.Context(), TeamEvent{Kind: "failed"}, now)
	o.publish(t.Context(), now, false)
	o.remind(t.Context(), now)
	assert.Equal(t, "unknown", o.roles["builder"].public.State)
	assert.Empty(t, f.sends)
	o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 20, Terminals: []TeamTerminal{f.terminal}}, now)
	f.readErr = assert.AnError
	o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "builder"}, now)
	o.publish(t.Context(), now, false)
	o.remind(t.Context(), now)
	assert.Equal(t, "unknown", o.roles["builder"].public.State)
	assert.Empty(t, f.sends)
	f.readErr = nil
	o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "builder"}, now.Add(teamReaderRetry))
	o.publish(t.Context(), now.Add(teamReaderRetry), false)
	assert.Equal(t, "waiting", o.roles["builder"].public.State)

	// A disconnected reader's retry wait is cancellable and cannot spin.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	events := make(chan TeamEvent, 4)
	calls := make(chan struct{}, 2)
	f.follow = func(context.Context, TeamSelection, func(TeamEvent)) error {
		calls <- struct{}{}
		return assert.AnError
	}
	done := make(chan struct{})
	go func() { followTeam(ctx, f, o.selection, events); close(done) }()
	select {
	case <-calls:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}
	for range 2 {
		select {
		case <-events:
		case <-time.After(time.Second):
			t.Fatal("failure was not reported")
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retry wait did not cancel")
	}
	assert.Empty(t, calls, "failure must wait before retrying")
}

// TestTeamTextsAndSpans fixes the harmless alphabet and checks both attributes and events.
func TestTeamTextsAndSpans(t *testing.T) {
	for _, role := range []string{"builder", "test_writer"} {
		text := assignmentReminder(role)
		assert.True(t, strings.HasPrefix(text, "SurlaReminder."))
		for _, char := range text + watcherReminder {
			assert.True(t, char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char == ' ' || char == '_' || char == '.', "%q", char)
		}
		kind := controlplane.CallerKind(role)
		for _, operation := range []string{"list_" + role + "_assignments", "get_" + role + "_assignment"} {
			assert.Contains(t, text, operation)
			assert.True(t, controlplane.OperationAllowed(kind, operation))
		}
	}
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(old); assert.NoError(t, provider.Shutdown(context.Background())) })
	o, f, now := newSupervisionFixture(t)
	o.publish(t.Context(), now, true)
	f.outcome = "blocked"
	o.remind(t.Context(), now)
	for _, span := range exporter.GetSpans() {
		assert.NotEqual(t, codes.Error, span.Status.Code)
		for _, attr := range span.Attributes {
			assert.NotContains(t, attr.Value.AsString(), o.selection.Root)
			assert.NotContains(t, attr.Value.AsString(), "builder-example")
		}
		for _, event := range span.Events {
			assert.NotContains(t, event.Name, o.selection.Root)
			assert.Empty(t, event.Attributes)
		}
	}
	cp, err := controlplane.New(controlplane.Options{CardsDir: t.TempDir(), TracerProvider: provider})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, cp.Close()) })
	user := controlplane.WithPrincipal(t.Context(), controlplane.Principal{ID: "owner", Kind: controlplane.CallerUser})
	o.publish(user, now, true)
	history, err := cp.ListTraces(user, &gensurla.ListTracesPayload{Status: "all", Limit: 100})
	require.NoError(t, err)
	assert.Empty(t, history.Spans, "plain project observation spans never enter the control plane's local recorder")
}

// TestTeamSupervisorLifetime joins readers when either independent owner ends.
func TestTeamSupervisorLifetime(t *testing.T) {
	for _, end := range []string{"controlplane", "project"} {
		t.Run(end, func(t *testing.T) {
			o, f, _ := newSupervisionFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			lifetime, stop := context.WithCancel(t.Context())
			defer stop()
			o.server.team.dependencies.Lifetime = lifetime
			started := make(chan string, 2)
			joined := make(chan string, 2)
			f.follow = func(ctx context.Context, _ TeamSelection, _ func(TeamEvent)) error {
				started <- "herdr"
				<-ctx.Done()
				joined <- "herdr"
				return ctx.Err()
			}
			f.watch = func(ctx context.Context) error {
				started <- "chorus"
				<-ctx.Done()
				joined <- "chorus"
				return ctx.Err()
			}
			done := make(chan error, 1)
			go func() {
				done <- o.server.Supervise(ctx, func(context.Context) ([]controlplane.AssignmentGeneration, error) { return nil, nil })
			}()
			for range 2 {
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("reader did not start")
				}
			}
			if end == "controlplane" {
				cancel()
				assert.NoError(t, lifetime.Err())
			} else {
				stop()
				assert.NoError(t, ctx.Err())
			}
			select {
			case err := <-done:
				assert.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("supervisor did not drain")
			}
			assert.Len(t, joined, 2)
		})
	}
}
