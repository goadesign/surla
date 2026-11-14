// These tests use recorded observations only; no terminal, service or window is contacted.
package project

import (
	"context"
	"regexp"
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
		sendErr  error
		follow   func(context.Context, TeamSelection, func(TeamEvent)) error
		watch    func(context.Context) (TeamWatcherView, error)
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
		return f.watch(ctx)
	}
	<-ctx.Done()
	return TeamWatcherView{}, ctx.Err()
}

// Send records the exact reminder and never contacts Chorus.
func (f *supervisionFixture) Send(_ context.Context, reminder TeamReminder) error {
	f.sends = append(f.sends, reminder)
	return f.sendErr
}

// newSupervisionFixture returns one live waiting builder with one unread
// generation and all effects recorded. No Chorus view has been read yet, so
// the builder's handle is not listed and no reminder is eligible.
func newSupervisionFixture(t *testing.T) (*teamSupervisor, *supervisionFixture, time.Time) {
	t.Helper()
	state := testState(t)
	state.Record.Team, state.Record.Space = "example-team", "space"
	f := &supervisionFixture{}
	f.terminal = TeamTerminal{Address: "terminal", State: "live", Reported: true, Status: &TeamStatus{State: "idle"}, Foreground: &TeamForeground{Argv: []string{"codex"}}, Labels: map[string]string{"chorus.project": state.Record.Directory, "herdr.space": "space", "chorus.handle": "builder-example"}}
	deps := fixtureTeamDependencies(state)
	deps.Observer, deps.Sender = f, f
	s, err := NewServer(state, func() {}, deps)
	require.NoError(t, err)
	_, err = registerTestAgent(s, "builder-example", "builder", state.Record.Directory)
	require.NoError(t, err)
	now := time.Unix(1000, 0)
	s.now = func() time.Time { return now }
	o := &teamSupervisor{server: s, roles: map[string]*supervisedRole{}}
	o.syncSelection(s.teamSelection())
	o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 10, Terminals: []TeamTerminal{f.terminal}}, now)
	setOwed(t, o, map[string][]string{"builder": {"one"}})
	o.publish(t.Context(), now, false)
	return o, f, now
}

// setOwed replaces the owner's assignment read with the given unread generations per role.
func setOwed(t *testing.T, o *teamSupervisor, owed map[string][]string) {
	t.Helper()
	o.assignments(t.Context(), func(context.Context) ([]controlplane.AssignmentGeneration, error) {
		var generations []controlplane.AssignmentGeneration
		for role, names := range owed {
			for _, name := range names {
				generations = append(generations, controlplane.AssignmentGeneration{Role: role, Generation: name})
			}
		}
		return generations, nil
	})
}

// listed records a successful Chorus view read at the given time that lists
// the given handles, each with the given watcher fact.
func listed(o *teamSupervisor, at time.Time, held bool, handles ...string) {
	view := TeamWatcherView{Version: "v", Held: map[string]bool{}}
	for _, handle := range handles {
		view.Held[handle] = held
	}
	o.watchers(view, nil, at)
}

// recordSpans installs an in-memory exporter as the global tracer provider for
// one test and returns both.
func recordSpans(t *testing.T) (*tracetest.InMemoryExporter, *sdktrace.TracerProvider) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() { otel.SetTracerProvider(previous); assert.NoError(t, provider.Shutdown(context.Background())) })
	return exporter, provider
}

// reminderSpans returns only the surla.team.reminder spans.
func reminderSpans(exporter *tracetest.InMemoryExporter) tracetest.SpanStubs {
	var spans tracetest.SpanStubs
	for _, span := range exporter.GetSpans() {
		if span.Name == "surla.team.reminder" {
			spans = append(spans, span)
		}
	}
	return spans
}

// spanAttribute returns one string attribute of a recorded span, or "".
func spanAttribute(span tracetest.SpanStub, key string) string {
	for _, a := range span.Attributes {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	return ""
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

// TestTeamReminderSendsOnce sends exactly one Chorus message to a role with an
// unread generation whose handle has been listed for the settle time after a
// good read, latches the generation and records a sent span without error.
// Neither the watcher fact nor the Herdr state changes the outcome: a role that
// polls its watcher inside its turn shows working.
func TestTeamReminderSendsOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		held  bool
		state string
	}{
		{"watcher held, waiting", true, "idle"},
		{"watcher held, working", true, "working"},
		{"watcher absent, waiting", false, "idle"},
		{"watcher absent, working", false, "working"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exporter, _ := recordSpans(t)
			o, f, now := newSupervisionFixture(t)
			listed(o, now.Add(-teamJoinSettle), tc.held, "builder-example")
			terminal := f.terminal
			terminal.Status = &TeamStatus{State: tc.state}
			o.roles["builder"].terminals[terminal.Address] = terminal
			o.publish(t.Context(), now, false)
			assert.Equal(t, map[string]string{"idle": "waiting", "working": "working"}[tc.state], o.roles["builder"].public.State)
			exporter.Reset()

			o.remind(t.Context(), now)
			o.remind(t.Context(), now.Add(time.Minute))

			require.Len(t, f.sends, 1)
			assert.Equal(t, TeamReminder{
				Team: "example-team",
				From: teamName("surla", o.server.state.Record.ID),
				To:   "builder-example",
				Text: "Surla: you have an unread assignment. Use list_builder_assignments and get_builder_assignment to read it. Surla reads no replies.",
			}, f.sends[0])
			assert.True(t, o.roles["builder"].owed["one"], "the generation is latched")
			spans := reminderSpans(exporter)
			require.Len(t, spans, 1)
			assert.Equal(t, "sent", spanAttribute(spans[0], "team.outcome"))
			assert.Equal(t, "assignment", spanAttribute(spans[0], "team.reason"))
			assert.NotEqual(t, codes.Error, spans[0].Status.Code)
			assert.Empty(t, spans[0].Events)
			assert.Empty(t, f.reads, "a reminder reads no terminal")
		})
	}
}

// TestTeamReminderUnmetConditions sends nothing and records no reminder span
// when the last Chorus read failed, when it did not list the handle, when the
// assignment read failed, or for the guide, which has no assignments.
func TestTeamReminderUnmetConditions(t *testing.T) {
	for _, condition := range []string{"last Chorus read failed", "handle not listed", "assignment read failed", "guide"} {
		t.Run(condition, func(t *testing.T) {
			exporter, _ := recordSpans(t)
			o, f, now := newSupervisionFixture(t)
			settled := now.Add(-teamJoinSettle)
			listed(o, settled, false, "builder-example")
			switch condition {
			case "last Chorus read failed":
				o.watchers(TeamWatcherView{}, assert.AnError, now)
			case "handle not listed":
				listed(o, now, false, "another-handle")
			case "assignment read failed":
				o.assignments(t.Context(), func(context.Context) ([]controlplane.AssignmentGeneration, error) { return nil, assert.AnError })
				assert.Contains(t, o.roles["builder"].owed, "one", "the unsent generation is kept")
			case "guide":
				_, err := registerTestAgent(o.server, "guide-example", "guide", o.server.state.Record.Directory)
				require.NoError(t, err)
				o.syncSelection(o.server.teamSelection())
				setOwed(t, o, map[string][]string{"guide": {"g"}})
				listed(o, settled, false, "builder-example", "guide-example")
			}
			o.publish(t.Context(), now, false)
			o.remind(t.Context(), now)
			o.remind(t.Context(), now.Add(time.Hour))
			assert.Empty(t, f.sends)
			assert.Empty(t, reminderSpans(exporter))
		})
	}
}

// TestTeamReminderSettleTime sends nothing until a handle has been listed for
// 60 seconds. A read that does not list the handle restarts the clock; a
// failed read and an unchanged (304) view do not. A role that reads its first
// assignment within the settle time gets no message.
func TestTeamReminderSettleTime(t *testing.T) {
	for _, tc := range []struct {
		listedFor time.Duration
		want      int
	}{
		{teamJoinSettle - time.Second, 0},
		{teamJoinSettle - time.Nanosecond, 0},
		{teamJoinSettle, 1},
		{teamJoinSettle + time.Second, 1},
	} {
		t.Run("listed "+tc.listedFor.String(), func(t *testing.T) {
			o, f, now := newSupervisionFixture(t)
			listed(o, now, false, "builder-example")
			o.remind(t.Context(), now.Add(tc.listedFor))
			assert.Len(t, f.sends, tc.want)
		})
	}
	t.Run("a read without the handle restarts the clock", func(t *testing.T) {
		o, f, now := newSupervisionFixture(t)
		listed(o, now, false, "builder-example")
		listed(o, now.Add(30*time.Second), false)
		listed(o, now.Add(31*time.Second), false, "builder-example")
		o.remind(t.Context(), now.Add(teamJoinSettle))
		o.remind(t.Context(), now.Add(31*time.Second+teamJoinSettle-time.Nanosecond))
		assert.Empty(t, f.sends)
		o.remind(t.Context(), now.Add(31*time.Second+teamJoinSettle))
		assert.Len(t, f.sends, 1)
	})
	t.Run("a failed read keeps the clock", func(t *testing.T) {
		o, f, now := newSupervisionFixture(t)
		listed(o, now, false, "builder-example")
		o.watchers(TeamWatcherView{}, assert.AnError, now.Add(30*time.Second))
		o.remind(t.Context(), now.Add(teamJoinSettle))
		assert.Empty(t, f.sends, "no post while the last read failed")
		listed(o, now.Add(40*time.Second), false, "builder-example")
		o.remind(t.Context(), now.Add(teamJoinSettle))
		assert.Len(t, f.sends, 1)
	})
	t.Run("an unchanged view keeps the clock", func(t *testing.T) {
		o, f, now := newSupervisionFixture(t)
		views := []TeamWatcherView{
			{Version: "v1", Held: map[string]bool{"builder-example": false}},
			{Version: "v1", Unchanged: true},
		}
		f.watch = func(ctx context.Context) (TeamWatcherView, error) {
			if len(views) == 0 {
				<-ctx.Done()
				return TeamWatcherView{}, ctx.Err()
			}
			view := views[0]
			views = views[1:]
			return view, nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		updates := make(chan watcherObservation, 2)
		done := make(chan struct{})
		go func() { followWatchers(ctx, f, o.selection, updates); close(done) }()
		for _, at := range []time.Time{now, now.Add(30 * time.Second)} {
			select {
			case observation := <-updates:
				o.watchers(observation.view, observation.err, at)
			case <-time.After(5 * time.Second):
				t.Fatal("watcher observation missing")
			}
		}
		cancel()
		<-done
		o.remind(t.Context(), now.Add(teamJoinSettle))
		assert.Len(t, f.sends, 1)
	})
	t.Run("a role that reads within the settle time gets none", func(t *testing.T) {
		o, f, now := newSupervisionFixture(t)
		listed(o, now, false, "builder-example")
		o.remind(t.Context(), now.Add(30*time.Second))
		setOwed(t, o, nil)
		o.remind(t.Context(), now.Add(teamJoinSettle))
		o.remind(t.Context(), now.Add(2*teamJoinSettle))
		assert.Empty(t, f.sends)
	})
}

// TestTeamReminderNewGeneration sends one new message for each new generation,
// such as a new verdict or a new build, and none for an unchanged one; a new
// build that gives both roles a generation sends one message to each.
func TestTeamReminderNewGeneration(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	_, err := registerTestAgent(o.server, "writer-example", "test_writer", o.server.state.Record.Directory)
	require.NoError(t, err)
	o.syncSelection(o.server.teamSelection())
	listed(o, now.Add(-teamJoinSettle), false, "builder-example", "writer-example")
	setOwed(t, o, map[string][]string{"builder": {"one"}})
	recipients := func() []string {
		var to []string
		for _, sent := range f.sends {
			to = append(to, sent.To)
		}
		return to
	}
	o.remind(t.Context(), now)
	assert.Equal(t, []string{"builder-example"}, recipients())
	setOwed(t, o, map[string][]string{"builder": {"one"}})
	o.remind(t.Context(), now)
	assert.Len(t, f.sends, 1, "an unchanged generation sends nothing")
	setOwed(t, o, map[string][]string{"builder": {"one"}, "test_writer": {"verdict-2"}})
	o.remind(t.Context(), now)
	assert.Equal(t, []string{"builder-example", "writer-example"}, recipients(), "a verdict is the test writer's new generation")
	setOwed(t, o, map[string][]string{"builder": {"one", "two"}, "test_writer": {"verdict-2"}})
	o.remind(t.Context(), now)
	assert.Equal(t, []string{"builder-example", "writer-example", "builder-example"}, recipients())
	setOwed(t, o, map[string][]string{"builder": {"build-2"}, "test_writer": {"build-2"}})
	o.remind(t.Context(), now)
	assert.ElementsMatch(t, []string{"builder-example", "writer-example"}, recipients()[3:], "a new build reminds each role once")
	assert.Len(t, f.sends, 5)
	// An observed role join must not forget existing assignments' latch.
	selection := o.selection
	selection.Roles = append(slicesTeamRoles(selection.Roles), TeamRole{Role: "guide", Handle: "guide-example"})
	o.syncSelection(selection)
	assert.True(t, o.roles["builder"].owed["build-2"])
}

// slicesTeamRoles copies selection data so tests do not mutate a live owner snapshot.
func slicesTeamRoles(roles []TeamRole) []TeamRole { return append([]TeamRole(nil), roles...) }

// TestTeamIdleUnreadNeedsYou keeps a waiting role with an unread assignment
// waiting just below 90 seconds and makes it needs_you with attention
// assignment at and above them; working or reading clears it.
func TestTeamIdleUnreadNeedsYou(t *testing.T) {
	at := func(o *teamSupervisor, now time.Time) gensurla.TeamRoleState {
		o.roles["builder"].lastEvent = now
		o.publish(t.Context(), now, false)
		return o.roles["builder"].public
	}
	for _, tc := range []struct {
		idle           time.Duration
		state, reasons string
	}{
		{teamIdleUnreadBound - time.Nanosecond, "waiting", "none"},
		{teamIdleUnreadBound, "needs_you", "assignment"},
		{teamIdleUnreadBound + time.Second, "needs_you", "assignment"},
	} {
		t.Run(tc.idle.String(), func(t *testing.T) {
			o, _, now := newSupervisionFixture(t)
			assert.Equal(t, "waiting", o.roles["builder"].public.State)
			value := at(o, now.Add(tc.idle))
			assert.Equal(t, tc.state, value.State)
			assert.Equal(t, tc.reasons, value.Attention)
			assert.Equal(t, "unread", value.Assignment)
		})
	}
	t.Run("working clears it", func(t *testing.T) {
		o, f, now := newSupervisionFixture(t)
		assert.Equal(t, "needs_you", at(o, now.Add(teamIdleUnreadBound)).State)
		working := f.terminal
		working.Status = &TeamStatus{State: "working"}
		o.roles["builder"].terminals[working.Address] = working
		assert.Equal(t, "working", at(o, now.Add(teamIdleUnreadBound+time.Second)).State)
		o.roles["builder"].terminals[working.Address] = f.terminal
		assert.Equal(t, "waiting", at(o, now.Add(teamIdleUnreadBound+2*time.Second)).State, "the idle clock restarts")
	})
	t.Run("reading clears it", func(t *testing.T) {
		o, _, now := newSupervisionFixture(t)
		assert.Equal(t, "needs_you", at(o, now.Add(teamIdleUnreadBound)).State)
		setOwed(t, o, nil)
		value := at(o, now.Add(teamIdleUnreadBound+time.Second))
		assert.Equal(t, "waiting", value.State)
		assert.Equal(t, "none", value.Attention)
	})
	t.Run("the user's read shows it", func(t *testing.T) {
		o, _, now := newSupervisionFixture(t)
		at(o, now.Add(teamIdleUnreadBound))
		snapshot, err := o.server.ReadTeam(t.Context())
		require.NoError(t, err)
		require.Len(t, snapshot.Roles, 1)
		assert.Equal(t, "needs_you", snapshot.Roles[0].State)
		assert.Equal(t, "assignment", snapshot.Roles[0].Attention)
	})
}

// TestTeamReminderAfterRestart shows that nothing about reminders is saved: a
// new supervisor that finds the same unread generation sends once, 60 seconds
// after it first sees the handle listed.
func TestTeamReminderAfterRestart(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	listed(o, now.Add(-teamJoinSettle), false, "builder-example")
	o.remind(t.Context(), now)
	require.Len(t, f.sends, 1)

	restarted := now.Add(time.Hour)
	next := &teamSupervisor{server: o.server, roles: map[string]*supervisedRole{}}
	next.syncSelection(o.server.teamSelection())
	next.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 1, Terminals: []TeamTerminal{f.terminal}}, restarted)
	setOwed(t, next, map[string][]string{"builder": {"one"}})
	next.publish(t.Context(), restarted, false)
	next.remind(t.Context(), restarted)
	listed(next, restarted.Add(5*time.Second), true, "builder-example")
	next.remind(t.Context(), restarted.Add(5*time.Second+teamJoinSettle-time.Nanosecond))
	assert.Len(t, f.sends, 1)
	next.remind(t.Context(), restarted.Add(5*time.Second+teamJoinSettle))
	next.remind(t.Context(), restarted.Add(5*time.Second+2*teamJoinSettle))
	assert.Len(t, f.sends, 2)
}

// TestTeamWatcherAbsences proves grace and outage uncertainty for the watcher
// fact, which is shown on the page and gates nothing: no message asks an agent
// to start its watcher.
func TestTeamWatcherAbsences(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	setOwed(t, o, nil)
	view := TeamWatcherView{Held: map[string]bool{"builder-example": false}}
	o.watchers(view, nil, now.Add(-teamWatcherGrace))
	o.publish(t.Context(), now, false)
	assert.Equal(t, "gone", o.roles["builder"].public.Watcher)
	o.remind(t.Context(), now)
	o.watchers(view, assert.AnError, now)
	o.publish(t.Context(), now, false)
	assert.Equal(t, "unknown", o.roles["builder"].public.Watcher)
	o.watchers(view, nil, now)
	o.publish(t.Context(), now.Add(teamWatcherGrace-time.Nanosecond), false)
	assert.Equal(t, "unknown", o.roles["builder"].public.Watcher, "an outage restarts the absence clock")
	o.watchers(TeamWatcherView{Held: map[string]bool{"builder-example": true}}, nil, now)
	o.roles["builder"].lastEvent = now
	o.publish(t.Context(), now, false)
	assert.Equal(t, "held", o.roles["builder"].public.Watcher)
	o.remind(t.Context(), now.Add(teamWatcherGrace))
	assert.Empty(t, f.sends)
}

// TestTeamStreamRecovery protects against out-of-order events and stale ownership.
func TestTeamStreamRecovery(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	o.event(t.Context(), TeamEvent{Kind: "event", Role: "builder", Type: "finished", Terminal: "terminal", Sequence: 9}, now)
	assert.Equal(t, "live", o.roles["builder"].terminals["terminal"].State)
	o.event(t.Context(), TeamEvent{Kind: "missed", Role: "builder"}, now)
	o.publish(t.Context(), now, false)
	assert.Equal(t, "unknown", o.roles["builder"].public.State)
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

// TestTeamStreamEndThenReadFailure keeps every role unknown through reconnect
// and a failed fresh foreground read; a later heartbeat recovers.
func TestTeamStreamEndThenReadFailure(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	o.event(t.Context(), TeamEvent{Kind: "failed"}, now)
	o.publish(t.Context(), now, false)
	assert.Equal(t, "unknown", o.roles["builder"].public.State)
	o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 20, Terminals: []TeamTerminal{f.terminal}}, now)
	f.readErr = assert.AnError
	o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "builder"}, now)
	o.publish(t.Context(), now, false)
	assert.Equal(t, "unknown", o.roles["builder"].public.State)
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

// TestTeamTextsAndSpans checks that each fixed reminder names only operations
// its role may call, and that team spans carry no project path or handle.
func TestTeamTextsAndSpans(t *testing.T) {
	names := regexp.MustCompile(`\b[a-z]+(?:_[a-z]+)+\b`)
	for _, role := range []string{"builder", "test_writer"} {
		text := assignmentReminder(role)
		assert.LessOrEqual(t, len(text), 4000, "Chorus accepts at most 4000 characters")
		kind := controlplane.CallerKind(role)
		operations := names.FindAllString(text, -1)
		assert.ElementsMatch(t, []string{"list_" + role + "_assignments", "get_" + role + "_assignment"}, operations)
		for _, operation := range operations {
			assert.Contains(t, gensurla.MethodNames, operation)
			assert.True(t, controlplane.OperationAllowed(kind, operation), "%s may not call %s", role, operation)
		}
	}
	assert.Empty(t, assignmentReminder("guide"), "the guide has no assignments")
	exporter, provider := recordSpans(t)
	o, _, now := newSupervisionFixture(t)
	listed(o, now.Add(-teamJoinSettle), true, "builder-example")
	o.publish(t.Context(), now, true)
	o.remind(t.Context(), now)
	require.Len(t, reminderSpans(exporter), 1)
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

// TestTeamTextsNeverSayTurn keeps every first message and reminder free of any
// instruction about turns: each agent application waits for Chorus in its own way.
func TestTeamTextsNeverSayTurn(t *testing.T) {
	turn := regexp.MustCompile(`(?i)\bturns?\b`)
	for _, role := range []string{"guide", "builder", "test_writer"} {
		_, message := roleFirstMessage(role)
		assert.False(t, turn.MatchString(message), "%s first message: %s", role, message)
		assert.False(t, turn.MatchString(assignmentReminder(role)), "%s reminder", role)
	}
}

// TestTeamFirstMessagesAskForWatcher checks that the builder and the test writer
// are told to keep their Chorus watcher and that Surla's message only asks for a
// read, and that the test writer learns its verdict arrives the same way.
func TestTeamFirstMessagesAskForWatcher(t *testing.T) {
	watcher := "Keep your Chorus watcher running. Surla messages you there when you have an unread assignment. Such a message only asks you to read your assignments: it never changes what is owed."
	for _, role := range []string{"builder", "test_writer"} {
		_, message := roleFirstMessage(role)
		assert.Contains(t, message, watcher, role)
	}
	_, writer := roleFirstMessage("test_writer")
	assert.Contains(t, writer, "Surla messages you on Chorus when its verdict is in; then read get_test_writer_assignment again")
	assert.NotContains(t, writer, "read get_test_writer_assignment again for its verdict")
	_, guide := roleFirstMessage("guide")
	assert.NotContains(t, guide, "watcher", "the guide has no assignments and gets no reminder")
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
			f.watch = func(ctx context.Context) (TeamWatcherView, error) {
				started <- "chorus"
				<-ctx.Done()
				joined <- "chorus"
				return TeamWatcherView{}, ctx.Err()
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
