// This file maps the team owner's immediate outcome after a build is saved.
package controlplane

import (
	"context"
	"errors"
	"time"

	gensurla "github.com/goadesign/surla/gen/surla"
	goa "goa.design/goa/v3/pkg"
)

// defaultBuildStartTimeout bounds opening and refreshing one copy and one
// sandboxed generation while the start holds the copy turn. The team owner's
// window phase has its own 45-second bound after that turn is released.
const defaultBuildStartTimeout = 5 * time.Minute

// admitBuildStart uses the workspace admission barrier so Close cannot miss
// accepted work. The caller's values and tracing survive, while cancellation
// belongs to the server and this start's limit. The returned finish must be
// called once after every effect, including post-save team admission, ends.
func (c *ControlPlane) admitBuildStart(ctx context.Context) (context.Context, func(), error) {
	c.workspaceMu.RLock()
	defer c.workspaceMu.RUnlock()
	if c.serverCtx.Err() != nil {
		return nil, nil, gensurla.MakeUnavailable(errors.New("the server is shutting down; call again on a running server"))
	}
	c.buildStartOps.Add(1)
	accepted, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(c.serverCtx, cancel)
	if c.serverCtx.Err() != nil {
		cancel()
	}
	return accepted, func() {
		cancel()
		stop()
		c.buildStartOps.Done()
	}, nil
}

// buildStartDeadline bounds only the work under the copy turn. Team start uses
// the server-owned admission context after this phase, with its own limit.
func (c *ControlPlane) buildStartDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	limit := defaultBuildStartTimeout
	if c.buildStartTimeout > 0 {
		limit = c.buildStartTimeout
	}
	return context.WithTimeout(ctx, limit)
}

// buildTeamOutcome preserves a saved build when its team cannot become ready.
// Outside error messages never become user steps: the team returns typed
// conditions, and this boundary owns the action shown by start_build.
func buildTeamOutcome(team *gensurla.ProjectTeam, err error) gensurla.Team {
	if err == nil {
		return gensurla.NewTeamReady(team)
	}
	var spaces *gensurla.TeamSpaceConflict
	if errors.As(err, &spaces) {
		return gensurla.NewTeamAmbiguousSpace(&gensurla.BuildTeamAmbiguousSpace{
			Count: spaces.Count,
			Step:  "Choose this project's Herdr space, then start the team agents.",
		})
	}
	var unavailable *goa.ServiceError
	if errors.As(err, &unavailable) && unavailable.Name == "window_busy" {
		return gensurla.NewTeamWindowBusy(&gensurla.BuildTeamWindowBusy{
			Step: "Close the open Herdr dialog, then start the team agents.",
		})
	}
	return gensurla.NewTeamUnavailable(&gensurla.BuildTeamUnavailable{
		Step: "Check that Herdr is available, then start the team agents for this saved build.",
	})
}
