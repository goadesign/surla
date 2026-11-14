// This file proves both team-start entries share the same server-owned deadline.
package project

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type (
	// deadlineTeamHost advances only the test's virtual clock at the first host read.
	deadlineTeamHost struct {
		TeamHost
		beforeRead func(context.Context)
		readError  error
	}
	// teamDeadlineContextKey checks preserved caller values without naming a public input.
	teamDeadlineContextKey struct{}
)

// TestTeamStartDeadlineBoundary tests one nanosecond below, at and above the shared bound.
// synctest advances the standard context timer instantly; no real deadline sleep occurs.
func TestTeamStartDeadlineBoundary(t *testing.T) {
	for _, entry := range []string{"ensure_team", "start_agents"} {
		for _, elapsed := range []time.Duration{teamStartTimeout - time.Nanosecond, teamStartTimeout, teamStartTimeout + time.Nanosecond} {
			t.Run(entry+"/"+elapsed.String(), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					state := testState(t)
					deps := fixtureTeamDependencies(state)
					host := &deadlineTeamHost{TeamHost: deps.Host}
					deps.Host = host
					server, err := NewServer(state, func() {}, deps)
					require.NoError(t, err)
					var accepted context.Context
					calls := 0
					server.team.withTimeout = func(
						ctx context.Context, limit time.Duration,
					) (context.Context, context.CancelFunc) {
						calls++
						assert.Equal(t, 45*time.Second, limit)
						assert.Equal(t, teamStartTimeout, limit)
						assert.NoError(t, ctx.Err(), "caller cancellation must not end accepted work")
						assert.Equal(t, "kept", ctx.Value(teamDeadlineContextKey{}))
						var cancel context.CancelFunc
						accepted, cancel = context.WithTimeout(ctx, limit)
						return accepted, cancel
					}
					host.beforeRead = func(ctx context.Context) {
						time.Sleep(elapsed)
						synctest.Wait()
						assert.Equal(t, "kept", ctx.Value(teamDeadlineContextKey{}))
					}
					caller, cancel := context.WithCancel(context.WithValue(teamContext(state), teamDeadlineContextKey{}, "kept"))
					cancel()
					if entry == "ensure_team" {
						_, err = server.EnsureTeam(caller)
					} else {
						_, err = server.StartAgents(caller)
					}
					assert.Equal(t, 1, calls)
					if elapsed < teamStartTimeout {
						assert.NoError(t, err)
						assert.NoError(t, host.readError)
						assert.Len(t, state.Record.Agents, 1)
						assert.ErrorIs(t, accepted.Err(), context.Canceled, "completed operations release their deadline")
					} else {
						var unavailable interface{ GoaErrorName() string }
						require.ErrorAs(t, err, &unavailable)
						assert.Equal(t, "team_unavailable", unavailable.GoaErrorName())
						assert.ErrorIs(t, host.readError, context.DeadlineExceeded)
						assert.ErrorIs(t, accepted.Err(), context.DeadlineExceeded)
						assert.Empty(t, state.Record.Agents)
						assert.Empty(t, host.TeamHost.(*teamFixture).calls)
					}
				})
			})
		}
	}
}

// TestTeamStartServerLifetime keeps both entries responsive to shutdown during an accepted read.
func TestTeamStartServerLifetime(t *testing.T) {
	for _, entry := range []string{"ensure_team", "start_agents"} {
		t.Run(entry, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				state := testState(t)
				deps := fixtureTeamDependencies(state)
				lifetime, stop := context.WithCancel(t.Context())
				defer stop()
				deps.Lifetime = lifetime
				host := &deadlineTeamHost{TeamHost: deps.Host}
				deps.Host = host
				host.beforeRead = func(context.Context) {
					stop()
					synctest.Wait()
				}
				server, err := NewServer(state, func() {}, deps)
				require.NoError(t, err)
				if entry == "ensure_team" {
					_, err = server.EnsureTeam(teamContext(state))
				} else {
					_, err = server.StartAgents(t.Context())
				}
				assert.Error(t, err)
				assert.ErrorIs(t, host.readError, context.Canceled)
				assert.Empty(t, state.Record.Agents)
				assert.Empty(t, host.TeamHost.(*teamFixture).calls)
			})
		})
	}
}

// Overlays observes the real deadline context after the controlled virtual-time transition.
func (h *deadlineTeamHost) Overlays(ctx context.Context) ([]string, error) {
	h.beforeRead(ctx)
	h.readError = ctx.Err()
	if h.readError != nil {
		return nil, h.readError
	}
	return h.TeamHost.Overlays(ctx)
}
