// This file maps the team owner's immediate outcome after a build is saved.
package controlplane

import (
	"errors"

	gensurla "github.com/goadesign/surla/gen/surla"
	goa "goa.design/goa/v3/pkg"
)

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
