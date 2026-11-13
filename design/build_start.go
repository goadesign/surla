// This file declares a build start's immediate team and working-copy outcomes.
package design

import . "goa.design/goa/v3/dsl"

// BuildStartStep gives the user one bounded action after a refusal or warning.
var BuildStartStep = Type("BuildStartStep", String, func() {
	MinLength(1)
	MaxLength(512)
})

// BuildStartRefusal names a pre-save condition and the action that enables a start.
var BuildStartRefusal = Type("BuildStartRefusal", func() {
	Description("The build was refused before saving; follow step before starting it again.")
	Attribute("name", String, "Declared refusal name.", func() { Meta("struct:error:name") })
	Attribute("step", BuildStartStep, "What the user does before starting the build again.")
	Required("name", "step")
})

// BuildTeamWindowBusy leaves the saved build intact while the user closes a dialog.
var BuildTeamWindowBusy = Type("BuildTeamWindowBusy", func() {
	Attribute("step", BuildStartStep, "Close the open dialog, then start the team agents.")
	Required("step")
})

// BuildTeamAmbiguousSpace leaves the saved build intact while the user selects a space.
var BuildTeamAmbiguousSpace = Type("BuildTeamAmbiguousSpace", func() {
	Attribute("count", Int, "Number of spaces matching this project.", func() { Minimum(2) })
	Attribute("step", BuildStartStep, "Choose the project's space, then start the team agents.")
	Required("count", "step")
})

// BuildTeamUnavailable leaves the saved build intact when team admission cannot finish.
var BuildTeamUnavailable = Type("BuildTeamUnavailable", func() {
	Attribute("step", BuildStartStep, "What the user does to make the team ready for the saved build.")
	Required("step")
})

// BuildCopyNotUsed says the stored evaluator needs no test-writer working copy.
var BuildCopyNotUsed = Type("BuildCopyNotUsed", func() {
	Description("This project uses its stored evaluator, so this start uses no test-writer working copy.")
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
	OneOf("working_copy", "The test-writer working copy after this start's refresh; not_used with a stored evaluator.", func() {
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
	Error("not_repository", BuildStartRefusal, "The project folder is not in a Git repository; nothing was saved.")
	Error("no_commit", BuildStartRefusal, "The project repository has no commit; nothing was saved.")
	Error("no_test_layout", BuildStartRefusal, "This project has neither stored verification settings nor Surla's test layout; nothing was saved.")
	Error("git_identity_missing", BuildStartRefusal, "Git has no configured name and address; nothing was saved.")
	Error("copy_folder_in_use", BuildStartRefusal, "The test-writer copy's folder is occupied; nothing was saved.")
	Error("tests_branch_in_use", BuildStartRefusal, "The tests branch is held by another working copy; nothing was saved.")
	Error("working_copy_busy", BuildStartRefusal, "A build start or test check holds the project's working-copy turn; nothing was saved.")
	Error("sandbox_unavailable", BuildStartRefusal, "This system has no candidate sandbox; nothing was saved.")
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
