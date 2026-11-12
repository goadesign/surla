// This file declares the bound-workspace Story refresh and status operations.
package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

// StoryQuestionItem describes an unresolved decision attached to a Story flow or step.
var StoryQuestionItem = Type("StoryQuestionItem", func() {
	Description("Unresolved decision attached to a Story flow or step in declaration order.")
	Attribute("key", String, "Flow-scoped question key.")
	Attribute("prompt", String, "Human-facing question text.")
	Attribute("description", String, "Optional longer context for the question.")
	Attribute("options", ArrayOf(String), "Suggested choices in declaration order.")
	Required("key", "prompt", "options")
})

// StoryStepItem describes one starting event, intermediate action, or outcome in a Story flow.
var StoryStepItem = Type("StoryStepItem", func() {
	Description("Starting event, intermediate action, or outcome in a Story flow in declaration order.")
	Attribute("key", String, "Flow-scoped step key.")
	Attribute("actor_key", String, "Story-scoped key of the actor performing or observing the step.")
	Attribute("summary", String, "Timeline sticky-note sentence for the step.")
	Attribute("description", String, "Optional longer explanation of the step.")
	Attribute("kind", String, "Step position in the flow: start, step, or outcome.", func() {
		Enum("start", "step", "outcome")
	})
	Attribute("questions", ArrayOf(StoryQuestionItem), "Unresolved step-level decisions in declaration order.")
	Required("key", "actor_key", "summary", "kind", "questions")
})

// StoryFlowItem describes one ordered timeline path through a domain Story.
var StoryFlowItem = Type("StoryFlowItem", func() {
	Description("Ordered timeline path through a domain Story in declaration order.")
	Attribute("key", String, "Story-scoped flow key.")
	Attribute("title", String, "Human-facing flow title.")
	Attribute("description", String, "Optional longer explanation of the flow.")
	Attribute("complete", Boolean, "True when the flow has a start, at least one outcome, and no open questions on the flow or any of its steps.")
	Attribute("steps", ArrayOf(StoryStepItem), "Starting, intermediate, and outcome steps in declaration order.")
	Attribute("questions", ArrayOf(StoryQuestionItem), "Unresolved flow-level decisions in declaration order.")
	Required("key", "title", "complete", "steps", "questions")
})

// StoryActorItem describes one domain role declared in a Story.
var StoryActorItem = Type("StoryActorItem", func() {
	Description("Domain role declared in a Story in declaration order.")
	Attribute("key", String, "Story-scoped actor key.")
	Attribute("title", String, "Human-facing actor label.")
	Attribute("description", String, "Optional longer explanation of the actor role.")
	Required("key", "title")
})

// StorySummaryItem describes one evaluated domain Story in declaration order.
var StorySummaryItem = Type("StorySummaryItem", func() {
	Description("Evaluated domain Story extracted from the bound workspace design in declaration order.")
	Attribute("name", String, "Human-facing Story title.")
	Attribute("description", String, "Optional longer explanation of the Story.")
	Attribute("actors", ArrayOf(StoryActorItem), "Domain roles declared in the Story in declaration order.")
	Attribute("flows", ArrayOf(StoryFlowItem), "Timeline paths declared in the Story in declaration order.")
	Required("name", "actors", "flows")
})

// StoryRefreshResult reports the caller's current Story refresh operation for the bound workspace.
var StoryRefreshResult = Type("StoryRefreshResult", func() {
	Description("Current Story refresh operation for the authenticated caller and bound workspace. state is none when no refresh has run yet in this server process for the bound workspace; running while sandboxed evaluation is in progress; succeeded with the evaluated stories (empty when the design does not load the Story module); or failed with a safe diagnostic.")
	Attribute("op_id", String, "Identifier of the caller's current Story refresh operation for the bound workspace; present when state is running, succeeded, or failed.")
	Attribute("state", String, "Current operation state: none, running, succeeded, or failed.", func() {
		Enum("none", "running", "succeeded", "failed")
	})
	Attribute("snapshot_digest", String, "Canonical SHA-256 digest (sha256:<hex>) of the workspace module snapshot read when this refresh started; present when state is running, succeeded, or failed.")
	Attribute("stories", ArrayOf(StorySummaryItem), "Evaluated domain Stories in declaration order when state is succeeded (empty when the design declares no Stories or when state is none, running, or failed).")
	Attribute("diagnostic", String, "Safe failure or timeout diagnostic when state is failed; omitted otherwise.")
	Attribute("message", String, "Summary of the current Story refresh state and next action.")
	Required("state", "stories", "message")
})

// storyRefreshMethods exposes bound-workspace Story evaluation over HTTP and MCP.
func storyRefreshMethods() {
	Method("refresh_story", func() {
		Description("Read the bound workspace's Go module snapshot, compute its canonical snapshot_digest, and start or reuse the authenticated caller's in-memory Story refresh operation for that workspace. Return promptly with the operation's op_id, state, and snapshot_digest while sandboxed evaluation continues on a server-owned context that survives client disconnects. Refreshing an unchanged snapshot whose current operation is running or succeeded returns that operation without starting another evaluation; refreshing a changed snapshot or a failed operation cancels and replaces the prior operation.")
		Result(StoryRefreshResult)
		Error("unprocessable", ErrorResult, "The bound workspace is absent, was replaced, contains no go.mod file, or has invalid module files; run init_project or fix the local module files and call refresh_story again.")
		Error("not_implemented", ErrorResult, "No workspace directory is bound to this control plane; start Surla from a project directory or pass -project.")
		Error("unavailable", ErrorResult, "The control plane is shutting down and is no longer starting Story refresh operations.")
		Tool("refresh_story", "Start or reuse a sandboxed Story evaluation of the bound workspace's current design files and return op_id, state, and snapshot_digest promptly. Takes no input. Poll get_story_refresh until state is succeeded or failed.")
		HTTP(func() {
			POST("/workspace/story/refresh")
			Response(StatusOK)
			Response("unprocessable", StatusUnprocessableEntity)
			Response("not_implemented", StatusNotImplemented)
			Response("unavailable", StatusServiceUnavailable)
		})
	})

	Method("get_story_refresh", func() {
		Description("Return the authenticated caller's current in-memory Story refresh operation for the bound workspace: op_id, state (none, running, succeeded, or failed), snapshot_digest, and the evaluated stories (empty when the design does not load the Story module) or a failure diagnostic. When no refresh has been run yet in this server process for the bound workspace, state is none with a message instructing the caller to run refresh_story.")
		Result(StoryRefreshResult)
		Error("not_implemented", ErrorResult, "No workspace directory is bound to this control plane; start Surla from a project directory or pass -project.")
		Tool("get_story_refresh", "Read the current Story refresh operation for the bound workspace: op_id, state (none, running, succeeded, or failed), snapshot_digest, stories, or diagnostic. Takes no input. When state is none, call refresh_story first.")
		HTTP(func() {
			GET("/workspace/story/refresh")
			Response(StatusOK)
			Response("not_implemented", StatusNotImplemented)
		})
	})
}
