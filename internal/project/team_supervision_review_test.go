// These recorded cases protect freshness, unknown evidence and bounded telemetry.
package project

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goadesign/surla/internal/controlplane"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"
)

// TestTeamWatcherHeldOutageIsUnknown preserves unknown through a failed view,
// even after the grace period while Herdr continues to report normally.
func TestTeamWatcherHeldOutageIsUnknown(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	o.roles["builder"].owed = nil
	o.watchers(TeamWatcherView{Held: map[string]bool{"builder-example": true}}, nil, now)
	o.watchers(TeamWatcherView{}, assert.AnError, now)
	for elapsed := 15 * time.Second; elapsed <= teamWatcherGrace; elapsed += 15 * time.Second {
		o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "builder"}, now.Add(elapsed))
	}
	o.publish(t.Context(), now.Add(teamWatcherGrace), false)
	o.remind(t.Context(), now.Add(teamWatcherGrace))
	assert.Equal(t, "unknown", o.roles["builder"].public.Watcher)
	assert.Empty(t, f.sends)
}

// TestTeamReadPublishedFacts checks the user's actual projection, not the private cache alone.
func TestTeamReadPublishedFacts(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	for _, tc := range []struct{ state, attention, watcher, assignment string }{
		{"waiting", "none", "gone", "unread"},
		{"needs_you", "auth", "held", "nothing_owed"},
		{"unknown", "none", "unknown", "unknown"},
	} {
		role := o.roles["builder"]
		role.ready, role.fresh = tc.state != "unknown", true
		terminal := f.terminal
		terminal.Status = &TeamStatus{State: "idle"}
		if tc.state == "needs_you" {
			terminal.Status = &TeamStatus{State: "blocked", Kind: "auth"}
		}
		role.terminals[terminal.Address] = terminal
		role.watchKnown, role.held = tc.watcher != "unknown", tc.watcher == "held"
		role.absentSince = now.Add(-teamWatcherGrace)
		role.assignmentKnown = tc.assignment != "unknown"
		role.owed = map[string]bool{}
		if tc.assignment == "unread" {
			role.owed["one"] = false
		}
		o.publish(t.Context(), now, false)
		snapshot, err := o.server.ReadTeam(t.Context())
		require.NoError(t, err)
		require.Len(t, snapshot.Roles, 1)
		actual := snapshot.Roles[0]
		assert.Equal(t, "builder", actual.Role)
		assert.Equal(t, "builder-example", actual.Handle)
		assert.Equal(t, tc.state, actual.State)
		assert.Equal(t, tc.attention, actual.Attention)
		assert.Equal(t, tc.watcher, actual.Watcher)
		assert.Equal(t, tc.assignment, actual.Assignment)
	}
	assert.Empty(t, f.reads, "the published read has no external effects")
}

// TestTeamAssignmentSetProjection consumes the owner reader's permitted sets.
// A registered unused test writer owes nothing even when terminal facts are
// independently unknown; ordinary projects still expose its unread assignment.
func TestTeamAssignmentSetProjection(t *testing.T) {
	for _, unused := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary project", true: "stored evaluator"}[unused], func(t *testing.T) {
			o, f, now := newSupervisionFixture(t)
			_, err := registerTestAgent(o.server, "tests-example", "test_writer", o.server.state.Record.Directory)
			require.NoError(t, err)
			o.syncSelection(o.server.teamSelection())
			o.assignments(t.Context(), func(context.Context) ([]controlplane.AssignmentGeneration, error) {
				owed := []controlplane.AssignmentGeneration{{Role: "builder", Generation: "one"}}
				if !unused {
					owed = append(owed, controlplane.AssignmentGeneration{Role: "test_writer", Generation: "one"})
				}
				return owed, nil
			})
			o.publish(t.Context(), now, false)
			snapshot, err := o.server.ReadTeam(t.Context())
			require.NoError(t, err)
			require.Len(t, snapshot.Roles, 2)
			assert.Equal(t, "builder", snapshot.Roles[0].Role)
			assert.Equal(t, "unread", snapshot.Roles[0].Assignment)
			writer := snapshot.Roles[1]
			assert.Equal(t, "test_writer", writer.Role)
			assert.Equal(t, "tests-example", writer.Handle)
			assert.Equal(t, "unknown", writer.State)
			if unused {
				assert.Equal(t, "nothing_owed", writer.Assignment)
			} else {
				assert.Equal(t, "unread", writer.Assignment)
			}
			assert.Empty(t, f.reads)
			assert.Empty(t, f.sends)
		})
	}
}

// TestTeamReminderFailureTelemetry records each kind of failed post as outcome
// failed, with the fixed error, error status and the bounded post and failure
// attributes. No caller sees an error, and however many reads follow, that
// generation gets no second attempt.
func TestTeamReminderFailureTelemetry(t *testing.T) {
	for _, post := range []string{"who", "send"} {
		for _, kind := range []string{"connection", "timeout", "http_status", "rpc_error", "tool_error", "response"} {
			t.Run(post+" "+kind, func(t *testing.T) {
				exporter, _ := recordSpans(t)
				o, f, now := newSupervisionFixture(t)
				listed(o, now.Add(-teamJoinSettle), true, "builder-example")
				f.sendErr = TeamSendFailure{Post: post, Kind: kind}
				exporter.Reset()
				o.remind(t.Context(), now)
				for later := range 5 {
					at := now.Add(time.Duration(later+1) * time.Minute)
					setOwed(t, o, map[string][]string{"builder": {"one"}})
					listed(o, at, true, "builder-example")
					o.remind(t.Context(), at)
				}
				assert.Len(t, f.sends, 1, "a failed generation is never tried again")
				_, err := o.server.ReadTeam(t.Context())
				assert.NoError(t, err)
				spans := reminderSpans(exporter)
				require.Len(t, spans, 1)
				span := spans[0]
				assert.Equal(t, codes.Error, span.Status.Code)
				assert.Equal(t, "team reminder post failed", span.Status.Description)
				require.Len(t, span.Events, 1)
				assert.Equal(t, "failed", spanAttribute(span, "team.outcome"))
				assert.Equal(t, post, spanAttribute(span, "team.post"))
				assert.Equal(t, kind, spanAttribute(span, "team.failure"))
				for _, a := range span.Events[0].Attributes {
					if a.Key == "exception.message" {
						assert.Equal(t, "team reminder post failed", a.Value.AsString())
					}
				}
			})
		}
	}
}

// TestTeamStreamStallBoundary makes one stale role invalidate a healthy peer,
// then proves heartbeats cannot recover either role without fresh snapshots.
func TestTeamStreamStallBoundary(t *testing.T) {
	for _, age := range []time.Duration{teamStreamStallBound - time.Nanosecond, teamStreamStallBound, teamStreamStallBound + time.Nanosecond} {
		t.Run(age.String(), func(t *testing.T) {
			o, f, now := newSupervisionFixture(t)
			_, err := registerTestAgent(o.server, "guide-example", "guide", o.server.state.Record.Directory)
			require.NoError(t, err)
			o.syncSelection(o.server.teamSelection())
			builder := f.terminal
			guide := TeamTerminal{Address: "guide-terminal", State: "live", Reported: true, Status: &TeamStatus{State: "idle"}, Foreground: &TeamForeground{Argv: []string{"codex"}}, Labels: map[string]string{"chorus.project": o.selection.Root, "herdr.space": o.selection.Space, "chorus.handle": "guide-example"}}
			o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 10, Terminals: []TeamTerminal{builder}}, now)
			o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "guide", Sequence: 10, Terminals: []TeamTerminal{guide}}, now)
			f.terminal = guide
			o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "guide"}, now.Add(age))
			f.terminal = builder
			stalled := o.publish(t.Context(), now.Add(age), false)
			if age < teamStreamStallBound {
				assert.False(t, stalled)
				assert.Equal(t, "waiting", o.roles["guide"].public.State)
				return
			}
			assert.True(t, stalled)
			for _, role := range o.roles {
				assert.Equal(t, "unknown", role.public.State)
			}
			reads := len(f.reads)
			o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "builder"}, now.Add(age))
			o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "guide"}, now.Add(age))
			o.publish(t.Context(), now.Add(age), false)
			assert.Len(t, f.reads, reads, "heartbeats cannot revive a stale subscription")
			o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 20, Terminals: []TeamTerminal{builder}}, now.Add(age))
			o.publish(t.Context(), now.Add(age), false)
			assert.Equal(t, "unknown", o.roles["builder"].public.State)
			o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "guide", Sequence: 20, Terminals: []TeamTerminal{guide}}, now.Add(age))
			o.publish(t.Context(), now.Add(age), false)
			assert.Equal(t, "waiting", o.roles["builder"].public.State)
		})
	}
}

// TestTeamLateHeartbeatRequiresSnapshot prevents a late heartbeat from erasing
// the expired interval just before the display tick gets to observe it.
func TestTeamLateHeartbeatRequiresSnapshot(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	later := now.Add(teamStreamStallBound)
	o.event(t.Context(), TeamEvent{Kind: "heartbeat", Role: "builder"}, later)
	o.publish(t.Context(), later, false)
	assert.Equal(t, "unknown", o.roles["builder"].public.State)
	assert.Empty(t, f.reads)
	o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 20, Terminals: []TeamTerminal{f.terminal}}, later)
	o.publish(t.Context(), later, false)
	assert.Equal(t, "waiting", o.roles["builder"].public.State)
}

// TestTeamMultipleLivePanesAreUnknown prevents choosing a heuristic latest pane.
func TestTeamMultipleLivePanesAreUnknown(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	other := f.terminal
	other.Address = "other-terminal"
	o.event(t.Context(), TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 20, Terminals: []TeamTerminal{f.terminal, other}}, now)
	o.publish(t.Context(), now, false)
	assert.Equal(t, "unknown", o.roles["builder"].public.State)
}

// TestTeamAssignmentScanCadence exercises the real owner's initial, snapshot,
// heartbeat and status reads while a display tick does no filesystem scan.
func TestTeamAssignmentScanCadence(t *testing.T) {
	o, f, _ := newSupervisionFixture(t)
	events := make(chan TeamEvent)
	reads := make(chan struct{}, 8)
	f.follow = func(ctx context.Context, _ TeamSelection, emit func(TeamEvent)) error {
		emit(TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 10, Terminals: []TeamTerminal{f.terminal}})
		for {
			select {
			case event := <-events:
				emit(event)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- o.server.Supervise(ctx, func(ctx context.Context) ([]controlplane.AssignmentGeneration, error) {
			select {
			case reads <- struct{}{}:
			case <-ctx.Done():
			}
			return nil, nil
		})
	}()
	waitRead := func() {
		t.Helper()
		select {
		case <-reads:
		case <-time.After(3 * time.Second):
			t.Fatal("required assignment observation missing")
		}
	}
	waitRead()
	waitRead()
	select {
	case <-reads:
		t.Error("display tick repeated the assignment store scan")
	case <-time.After(1100 * time.Millisecond):
	}
	for _, event := range []TeamEvent{{Kind: "heartbeat", Role: "builder"}, {Kind: "event", Role: "builder", Type: "status", Terminal: "terminal", Sequence: 20}, {Kind: "event", Role: "builder", Type: "turn", Terminal: "terminal", Sequence: 21}} {
		select {
		case events <- event:
		case <-time.After(time.Second):
			t.Fatal("event reader stalled")
		}
		waitRead()
	}
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("supervisor did not join")
	}
}

// TestTeamStalledReaderDrains advances the injected clock, so no test waits45s.
func TestTeamStalledReaderDrains(t *testing.T) {
	o, f, now := newSupervisionFixture(t)
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	o.server.now = func() time.Time { return time.Unix(0, clock.Load()) }
	started, joined := make(chan struct{}, 2), make(chan struct{}, 2)
	f.follow = func(ctx context.Context, _ TeamSelection, emit func(TeamEvent)) error {
		started <- struct{}{}
		emit(TeamEvent{Kind: "snapshot", Role: "builder", Sequence: 10, Terminals: []TeamTerminal{f.terminal}})
		<-ctx.Done()
		joined <- struct{}{}
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- o.server.Supervise(ctx, func(context.Context) ([]controlplane.AssignmentGeneration, error) { return nil, nil })
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("reader did not start")
	}
	require.Eventually(t, func() bool {
		snapshot, err := o.server.ReadTeam(t.Context())
		return err == nil && snapshot.Roles[0].State == "waiting"
	}, time.Second, time.Millisecond)
	clock.Store(now.Add(teamStreamStallBound).UnixNano())
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("stale reader was not canceled")
	}
	snapshot, err := o.server.ReadTeam(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "unknown", snapshot.Roles[0].State)
	assert.Empty(t, started, "reconnect must wait")
	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("stale-reader retry did not cancel")
	}
}
