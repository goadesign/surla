// This file declares a build start's immediate team and working-copy outcomes.
package design

import . "goa.design/goa/v3/dsl"

// BuildStartStep gives the user one bounded action after a refusal or warning.
var BuildStartStep = Type("BuildStartStep", String, func() {
	MinLength(1)
	MaxLength(512)
})

// BuildTeamWindowBusy leaves the saved build intact while the user closes a dialog.
var BuildTeamWindowBusy = Type("BuildTeamWindowBusy", func() {
	Attribute("step", BuildStartStep, "Close the open dialog, then start the team agents.")
	Required("step")
})

// BuildTeamAmbiguousSpace leaves the saved build intact while the user closes
// the duplicate spaces.
var BuildTeamAmbiguousSpace = Type("BuildTeamAmbiguousSpace", func() {
	Attribute("count", Int, "Number of spaces matching this project.", func() { Minimum(2) })
	Attribute("step", BuildStartStep, "Close the duplicate Herdr spaces, then start the agents.")
	Required("count", "step")
})

// BuildTeamUnavailable leaves the saved build intact when team admission cannot finish.
var BuildTeamUnavailable = Type("BuildTeamUnavailable", func() {
	Attribute("step", BuildStartStep, "What the user does to make the team ready for the saved build.")
	Required("step")
})

// BuildCopyNotUsed says stored verification settings need no test-writer
// working copy.
var BuildCopyNotUsed = Type("BuildCopyNotUsed", func() {
	Description("This project has stored verification settings, so this start uses no test-writer working copy.")
})

// BuildCopyAtProjectCommit reports the copy's postcondition without claiming a ref moved.
var BuildCopyAtProjectCommit = Type("BuildCopyAtProjectCommit", func() {
	Description("The test-writer working copy is at the project's commit. It may be new, already current, or advanced by this start.")
	Attribute("commit", String, "The working copy's current commit.", func() { Pattern("^([0-9a-f]{40}|[0-9a-f]{64})$") })
	Required("commit")
})

// BuildCopyNotContained reports a copy retained because its tests are not merged.
var BuildCopyNotContained = Type("BuildCopyNotContained", func() {
	Description("The tests branch is not contained in the project's branch, so this start retained the copy's commit.")
	Attribute("commit", String, "The retained working-copy commit.", func() { Pattern("^([0-9a-f]{40}|[0-9a-f]{64})$") })
	Attribute("step", BuildStartStep, "Merge the tests branch into the project's branch, then build again.")
	Required("commit", "step")
})

// BuildCopyDirty reports a copy retained to preserve uncommitted work.
var BuildCopyDirty = Type("BuildCopyDirty", func() {
	Description("The working copy has uncommitted changes, so this start retained its commit and files.")
	Attribute("commit", String, "The retained working-copy commit.", func() { Pattern("^([0-9a-f]{40}|[0-9a-f]{64})$") })
	Required("commit")
})

// StartBuildResult separates the saved build from this start's transient outcomes.
var StartBuildResult = Type("StartBuildResult", func() {
	Description("The saved build and this start's working-copy and team outcomes. A team warning never means the build was refused; get_build returns the build without these transient outcomes.")
	Attribute("build", StartedBuild, "The build saved before generation or team admission.")
	OneOf("team", "The team's immediate outcome after the build was saved.", func() {
		Meta("oneof:json:flatten")
		Meta("oneof:type:field", "state")
		Attribute("ready", ProjectTeam)
		Attribute("window_busy", BuildTeamWindowBusy)
		Attribute("ambiguous_space", BuildTeamAmbiguousSpace)
		Attribute("unavailable", BuildTeamUnavailable)
	})
	OneOf("working_copy", "The test-writer working copy after this start's refresh; not_used with stored verification settings.", func() {
		Meta("oneof:json:flatten")
		Meta("oneof:type:field", "state")
		Attribute("not_used", BuildCopyNotUsed)
		Attribute("at_project_commit", BuildCopyAtProjectCommit)
		Attribute("not_contained", BuildCopyNotContained)
		Attribute("dirty", BuildCopyDirty)
	})
	Required("build", "team", "working_copy")
})

// buildStartRefusals declares the conditions that refuse a start before it saves.
func buildStartRefusals() {
	Error("not_repository", ErrorResult, "The project folder is not in a Git repository; nothing was saved. The message ends with the step to create or select a repository.")
	Error("no_commit", ErrorResult, "The project repository has no commit; nothing was saved. The message ends with the step to make its first commit.")
	Error("no_test_layout", ErrorResult, "This project has neither stored verification settings nor Surla's test layout; nothing was saved. Store verification settings, then build again.")
	Error("git_identity_missing", ErrorResult, "Git has no configured name and address; nothing was saved. Configure Git's name and address, then build again.")
	Error("copy_folder_in_use", ErrorResult, "The test-writer copy's named folder is occupied; nothing was saved. The message ends with the step to free that folder and build again.")
	Error("tests_branch_in_use", ErrorResult, "The named tests branch is held by another working copy; nothing was saved. The message ends with the library's step to free that branch and build again.")
	Error("working_copy_busy", ErrorResult, "Another build is starting or a test check holds the project's working-copy turn; nothing was saved. Start the build again when that has ended.")
	Error("sandbox_unavailable", ErrorResult, "This system has no candidate sandbox; nothing was saved. When the host reports a specific sandbox prerequisite failure, the message includes its cause and remedy before: Start this project on a system with Surla's candidate sandbox, then build again.")
}

// buildStartRefusalResponses maps project state and host availability distinctly.
func buildStartRefusalResponses() {
	Response("not_repository", StatusConflict)
	Response("no_commit", StatusConflict)
	Response("no_test_layout", StatusConflict)
	Response("git_identity_missing", StatusConflict)
	Response("copy_folder_in_use", StatusConflict)
	Response("tests_branch_in_use", StatusConflict)
	Response("working_copy_busy", StatusConflict)
	Response("sandbox_unavailable", StatusNotImplemented)
}
