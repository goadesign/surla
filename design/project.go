// This file defines the HTTP-only project management and browser exchange boundary.
package design

import . "goa.design/goa/v3/dsl"

// ProjectIdentity is the authenticated proof that a server owns this project.
var ProjectIdentity = Type("ProjectIdentity", func() {
	Attribute("project_id", String, "Stable private project identifier.")
	Attribute("directory", String, "Physical project folder.")
	Attribute("owner", String, "Immutable operating-system account identity.")
	Required("project_id", "directory", "owner")
})

// ProjectAgent names one independently revocable agent credential file.
var ProjectAgent = Type("ProjectAgent", func() {
	Attribute("actor_id", String, "Agent identity, separate from resource ownership.")
	Attribute("role", String, "Trusted agent purpose.", func() { Enum("guide", "builder", "test_writer") })
	Attribute("credential_file", String, "Private credential file for the launcher; never put its contents in arguments or labels.")
	Required("actor_id", "role", "credential_file")
})

var _ = Service("project", func() {
	Description("Owns one local project's server identity, agent credentials, and one-use browser access. Management credentials never authorize application decisions. This service is not exposed as agent tools.")
	Error("forbidden", ErrorResult, "The trusted credential cannot perform this operation.")
	Error("unprocessable", ErrorResult, "The requested credential exchange or agent does not exist or is invalid.")
	Error("conflict", ErrorResult, "An agent with this identifier already exists.")
	HTTP(func() {
		Path("/project")
		Response("forbidden", StatusForbidden)
		Response("unprocessable", StatusUnprocessableEntity)
		Response("conflict", StatusConflict)
	})
	Method("identity", func() {
		Description("The CLI proves the answering server belongs to its project using the management credential.")
		Result(ProjectIdentity)
		HTTP(func() { GET("/identity"); Response(StatusOK) })
	})
	Method("stop", func() {
		Description("The CLI requests ordered server shutdown; the server removes its running record only after its owned work closes.")
		HTTP(func() { POST("/stop"); Response(StatusAccepted) })
	})
	Method("add_agent", func() {
		Description("The CLI creates one persistent agent credential and returns only its private file path. The credential and role survive server restart.")
		Payload(func() {
			Attribute("actor_id", String, "Agent identifier.", func() { Pattern("^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$") })
			Attribute("role", String, "Agent purpose.", func() { Enum("guide", "builder", "test_writer") })
			Required("actor_id", "role")
		})
		Result(ProjectAgent)
		HTTP(func() { POST("/agents"); Response(StatusCreated) })
	})
	Method("remove_agent", func() {
		Description("The CLI revokes an agent and removes its private credential file before returning.")
		Payload(func() {
			Attribute("actor_id", String, "Agent identifier.", func() { Pattern("^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$") })
			Required("actor_id")
		})
		HTTP(func() { DELETE("/agents/{actor_id}"); Response(StatusNoContent) })
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
