// This file defines the HTTP-only project management and browser exchange boundary.
package design

import . "goa.design/goa/v3/dsl"

// ProjectIdentity identifies the project reported by the answering server.
var ProjectIdentity = Type("ProjectIdentity", func() {
	Attribute("project_id", String, "Stable private project identifier.")
	Attribute("directory", String, "Physical project folder.")
	Attribute("owner", String, "Immutable operating-system account identity.")
	Required("project_id", "directory", "owner")
})

// ProjectTeamRole is the observed state of one server-owned role.
var ProjectTeamRole = Type("ProjectTeamRole", func() {
	Attribute("role", String, "Trusted role.", func() { Enum("guide", "builder", "test_writer") })
	Attribute("handle", String, "Stable Chorus handle and Surla actor identity.")
	Attribute("running", Boolean, "The matching live terminal is not reported as an idle shell; missing foreground information counts as running.")
	Required("role", "handle", "running")
})

// ProjectTeam describes the server-owned team and the launcher's visible state.
var ProjectTeam = Type("ProjectTeam", func() {
	Attribute("team", String, "Stable Chorus team name.", func() { Pattern("^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$") })
	Attribute("space", String, "Local Herdr space identifier.")
	Attribute("roles", ArrayOf(ProjectTeamRole))
	Attribute("dialog", String, "Launcher state after ensuring the team.", func() { Enum("opened", "already_open", "not_needed") })
	Attribute("step", String, "What the user does next for the prepared team.", func() { MinLength(1); MaxLength(512) })
	Required("team", "space", "roles", "dialog", "step")
})

// TeamSpaceConflict carries a safe count independently of peer-authored error messages.
var TeamSpaceConflict = Type("TeamSpaceConflict", func() {
	Attribute("name", String, "Error name.", func() { Meta("struct:error:name") })
	Attribute("count", Int, "Number of local spaces matching the team's name and physical folder.", func() { Minimum(2) })
	Required("name", "count")
})

// TeamUnavailable names which part of team preparation failed, so the CLI can give
// that part's own recovery step without printing peer-authored error text.
var TeamUnavailable = Type("TeamUnavailable", func() {
	Attribute("name", String, "Error name.", func() { Meta("struct:error:name") })
	Attribute("cause", String, "What could not be used: stopping (the server is shutting down), herdr (a Herdr command failed), roster_folder (Herdr's launcher roster folder could not be written), project_state (the project's private record or an agent credential could not be read or written), or role_folder (a role's working folder could not be read).", func() {
		Enum("stopping", "herdr", "roster_folder", "project_state", "role_folder")
	})
	Required("name", "cause")
})

var _ = Service("project", func() {
	Description("Owns one local project's server identity, agent credentials, and one-use browser access. Management credentials last for one listening server and never authorize application decisions. This service is not exposed as agent tools.")
	Error("forbidden", ErrorResult, "The trusted credential cannot perform this operation.")
	Error("unprocessable", ErrorResult, "The requested credential exchange does not exist or is invalid.")
	HTTP(func() {
		Path("/project")
		Response("forbidden", StatusForbidden)
		Response("unprocessable", StatusUnprocessableEntity)
	})
	Method("identity", func() {
		Description("The CLI checks the answering server reports its expected project identity using the current server-lifetime management credential.")
		Result(ProjectIdentity)
		HTTP(func() { GET("/identity"); Response(StatusOK) })
	})
	Method("ensure_team", func() {
		Description("The CLI asks the project server to ensure its team, guide credential, Herdr space and atomic launcher roster, then open the launcher only when an agent is missing. Accepted work has a bounded server-owned lifetime independent of client disconnection. Only the server management credential may call this operation; it is never an MCP tool.")
		Result(ProjectTeam)
		Error("ambiguous_space", TeamSpaceConflict, "Several local spaces match the team name and folder; close the duplicates before starting agents again.")
		Error("window_busy", ErrorResult, "Another Herdr dialog is open; close it and run surla again.")
		Error("team_unavailable", TeamUnavailable, "A bounded Herdr operation, roster write or project state access failed; its cause selects the recovery step.")
		HTTP(func() {
			POST("/team")
			Response(StatusOK)
			Response("ambiguous_space", StatusConflict)
			Response("window_busy", StatusConflict)
			Response("team_unavailable", StatusServiceUnavailable)
		})
	})
	Method("stop", func() {
		Description("The CLI requests ordered server shutdown. Before its listener closes, the server revokes management access and withdraws its running record; an authorized stop response can finish.")
		HTTP(func() { POST("/stop"); Response(StatusAccepted) })
	})
	Method("browser_link", func() {
		Description("The CLI mints a short-lived single-use browser link. The URL is the only credential allowed in a browser-launch command line.")
		Result(func() {
			Attribute("url", String, "Same-address views URL carrying the single-use link in its fragment.")
			Attribute("expires_at", String, "Time after which exchange is refused.", func() { Format(FormatDateTime) })
			Required("url", "expires_at")
		})
		HTTP(func() { POST("/browser-link"); Response(StatusCreated) })
	})
	Method("exchange_browser_link", func() {
		Description("The browser exchanges the single-use link once for an in-memory user session. The session is never a cookie or stored on disk and is sent only to this server address.")
		Payload(func() {
			Attribute("link", String, "Unexpired single-use link value.", func() { MinLength(1); MaxLength(256) })
			Required("link")
		})
		Result(func() {
			Attribute("session", String, "User session kept only in the page's memory.")
			Required("session")
		})
		HTTP(func() { POST("/browser-session"); Response(StatusCreated) })
	})
})
