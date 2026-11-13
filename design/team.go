// This file declares the user's read and actions for the server-owned team.
package design

import . "goa.design/goa/v3/dsl"

// TeamRoleState contains only the facts the user needs to decide whether to open a pane.
var TeamRoleState = Type("TeamRoleState", func() {
	Attribute("role", String, "Registered role.", func() { Enum("guide", "builder", "test_writer") })
	Attribute("handle", String, "Registered Chorus handle.")
	Attribute("state", String, "Current terminal state; unknown means Herdr cannot establish it.", func() {
		Enum("not_started", "ended", "starting", "needs_you", "working", "waiting", "error", "unknown")
	})
	Attribute("attention", String, "Reported block kind or the reason a silent running agent needs the user.", func() {
		Enum("none", "permission", "question", "auth", "silent", "unknown")
	})
	Attribute("watcher", String, "held when Chorus reports a watcher, gone after its absence grace, unknown otherwise.", func() { Enum("held", "gone", "unknown") })
	Attribute("assignment", String, "unread when this role owes an assignment read, nothing_owed when the owner established none, unknown when the owner could not read assignments. The guide has no assignments.", func() { Enum("unread", "nothing_owed", "unknown") })
	Required("role", "handle", "state", "attention", "watcher", "assignment")
})

// TeamSnapshot is rebuilt in memory from the project registry and outside observations.
var TeamSnapshot = Type("TeamSnapshot", func() {
	Attribute("team", String, "Stable team name, also returned before a team is prepared.")
	Attribute("roles", ArrayOf(TeamRoleState), "Registered roles in guide, builder, test_writer order; empty before registration.")
	Required("team", "roles")
})

// teamMethods deliberately exposes no agent MCP tools and no second waiting read.
func teamMethods() {
	Method("get_team", func() {
		Description("Read the project team's registered roles and current terminal, watcher and unread-assignment facts. Reading creates nothing and marks no assignment read. Only the user's browser session may call it. wait_for_changes covers this snapshot as well as the change list.")
		Result(TeamSnapshot)
		HTTP(func() { GET("/team"); Response(StatusOK) })
	})
	Method("show_team_pane", func() {
		Description("Show this project's space and focus the requested registered role's pane, only at the user's request. Any open Herdr dialog prevents all window changes. Fresh project, handle and space labels must still identify the pane.")
		Payload(func() {
			Attribute("role", String, "Registered role whose pane the user requests.", func() { Enum("guide", "builder", "test_writer") })
			Required("role")
		})
		Error("window_busy", ErrorResult, "Close the open Herdr dialog before showing a pane.")
		Error("pane_unavailable", ErrorResult, "The requested registered role has no owned pane to show.")
		Error("team_unavailable", ErrorResult, "Herdr could not establish the owned pane.")
		HTTP(func() {
			POST("/team/pane")
			Response(StatusNoContent)
			Response("window_busy", StatusConflict)
			Response("pane_unavailable", StatusConflict)
			Response("team_unavailable", StatusServiceUnavailable)
		})
	})
	Method("start_team_agents", func() {
		Description("Ask the project's existing team-start owner to prepare the registered roles and open the Chorus launcher when a listed role is not running. Only the user may call it. It has the same bounded server-owned lifetime and dialog, space and availability refusals as the management team-start operation.")
		Result(ProjectTeam)
		Error("ambiguous_space", TeamSpaceConflict, "Several spaces match the project; choose one before retrying.")
		Error("window_busy", ErrorResult, "Close the open Herdr dialog before starting agents.")
		Error("team_unavailable", ErrorResult, "The project team could not be prepared.")
		HTTP(func() {
			POST("/team/agents")
			Response(StatusOK)
			Response("ambiguous_space", StatusConflict)
			Response("window_busy", StatusConflict)
			Response("team_unavailable", StatusServiceUnavailable)
		})
	})
}
