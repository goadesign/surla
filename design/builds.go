// This file declares builds: the user's decision to build a confirmed change
// at one revision, and the builder's and the test writer's assignments for
// that build.
package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

// TestsAssigned is the test writer's state before it reports its tests finished.
var TestsAssigned = Type("TestsAssigned", func() {
	Description("Surla assigned the tests for this build; the test writer has not reported them finished.")
})

// TestsReported is the test writer's state after it reported its tests finished.
var TestsReported = Type("TestsReported", func() {
	Description("The test writer reported its tests finished at this build's revision; Surla has not judged them yet.")
})

// TestsNeedChanges is the test writer's state after Surla found problems.
var TestsNeedChanges = Type("TestsNeedChanges", func() {
	Description("Surla checked the reported tests and found problems the test writer must fix before reporting again.")
	Attribute("findings", ArrayOf(String), "What Surla found, one problem per item: a test that does not build or a required test that is not declared.", func() {
		MinLength(1)
	})
	Required("findings")
})

// TestsAccepted is the test writer's state after Surla accepted its tests.
var TestsAccepted = Type("TestsAccepted", func() {
	Description("Surla checked the reported tests and recorded them as a commit.")
	Attribute("commit", String, "Full commit ID Surla recorded the accepted tests at (40 or 64 lowercase hex characters).", func() {
		Pattern("^([0-9a-f]{40}|[0-9a-f]{64})$")
	})
	Required("commit")
})

// TestWriterProgress is the test writer's state and whether it read its assignment.
var TestWriterProgress = Type("TestWriterProgress", func() {
	Description("Where the test writer's assignment for one build stands.")
	OneOf("tests", "The tests' state: assigned, reported, needs_changes with Surla's findings, or accepted with the commit Surla recorded.", func() {
		Meta("oneof:json:flatten")
		Meta("oneof:type:field", "state")
		Attribute("assigned", TestsAssigned)
		Attribute("reported", TestsReported)
		Attribute("needs_changes", TestsNeedChanges)
		Attribute("accepted", TestsAccepted)
	})
	Attribute("read", Boolean, "True once the test writer read this assignment in full with get_test_writer_assignment.")
	Required("tests", "read")
})

// BuilderProgress says whether the builder read its assignment.
var BuilderProgress = Type("BuilderProgress", func() {
	Description("The builder's assignment for one build. It exists from the moment the user starts the build; verification of the build's revision, not the assignment, judges the builder's work.")
	Attribute("read", Boolean, "True once the builder read this assignment in full with get_builder_assignment.")
	Required("read")
})

// StartedBuild is a change's build as the user started it.
var StartedBuild = Type("StartedBuild", func() {
	Description("The build the user started for a change, with both assignments.")
	Attribute("request_revision", String, "Change Card revision the user built.")
	Attribute("started_at", String, "When the user started this build.", func() { Format(FormatDateTime) })
	Attribute("in_force", Boolean, "True when request_revision is the change's confirmed revision in force. False when the change was revised or confirmed again since: the assignments stay as they are, for this earlier revision, until the user builds again.")
	Attribute("builder", BuilderProgress, "The builder's assignment.")
	Attribute("test_writer", TestWriterProgress, "The test writer's assignment.")
	Required("request_revision", "started_at", "in_force", "builder", "test_writer")
})

// BuildNotStarted is the build state of a change the user never built.
var BuildNotStarted = Type("BuildNotStarted", func() {
	Description("The user has not started a build of this change.")
})

// ChangeBuild is a change's build, or the fact that it has none.
var ChangeBuild = Type("ChangeBuild", func() {
	Description("A change's build as get_build reports it.")
	Attribute("change_id", String, "Change Card identifier.")
	OneOf("build", "not_started until the user starts a build of this change; started with the build afterwards.", func() {
		Meta("oneof:json:flatten")
		Meta("oneof:type:field", "state")
		Attribute("not_started", BuildNotStarted)
		Attribute("started", StartedBuild)
	})
	Required("change_id", "build")
})

// BuilderAssignment is the builder's assignment for one build.
var BuilderAssignment = Type("BuilderAssignment", func() {
	Description("The builder's assignment: build this change at this revision in the project folder, where generate_code, check_project and verify_change act.")
	Attribute("change_id", String, "Change Card to build; get_change takes it.")
	Attribute("request_revision", String, "Change Card revision to build; verify_change takes it.")
	Attribute("read", Boolean, "True once you read this assignment in full with get_builder_assignment.")
	Required("change_id", "request_revision", "read")
})

// BuilderAssignmentList lists the builder's assignments.
var BuilderAssignmentList = Type("BuilderAssignmentList", func() {
	Description("The builder's assignments, one per change the user built, most recently started first.")
	Attribute("assignments", ArrayOf(BuilderAssignment), "Assignments, most recently started first; empty when the user has built no change.")
	Required("assignments")
})

// RequiredTest names one test a build's Story requires.
var RequiredTest = Type("RequiredTest", func() {
	Description("One test the agreed Story requires: the evaluation scenario Surla generates for one finished flow and the hook method the test writer implements for it.")
	Attribute("story", String, "Name of the Story the flow belongs to.")
	Attribute("flow_key", String, "Key of the finished flow.")
	Attribute("scenario", String, "Scenario name the evaluation suite declares for the flow.")
	Attribute("hook", String, "Hook method the test writer implements for the scenario.")
	Required("story", "flow_key", "scenario", "hook")
})

// TestWriterAssignmentItem summarizes one of the test writer's assignments.
var TestWriterAssignmentItem = Type("TestWriterAssignmentItem", func() {
	Description("One of the test writer's assignments as list_test_writer_assignments reports it.")
	Attribute("change_id", String, "Change Card the tests are for; get_test_writer_assignment takes it.")
	Attribute("request_revision", String, "Change Card revision the tests are for.")
	Attribute("progress", TestWriterProgress, "Where this assignment stands.")
	Required("change_id", "request_revision", "progress")
})

// TestWriterAssignmentList lists the test writer's assignments.
var TestWriterAssignmentList = Type("TestWriterAssignmentList", func() {
	Description("The test writer's assignments, one per change the user built, most recently started first.")
	Attribute("assignments", ArrayOf(TestWriterAssignmentItem), "Assignments, most recently started first; empty when the user has built no change.")
	Required("assignments")
})

// TestWriterAssignment is the test writer's assignment for one build in full.
var TestWriterAssignment = Type("TestWriterAssignment", func() {
	Description("The test writer's assignment for one build: write the required tests for the Story as agreed at the built revision, in the working folder.")
	Attribute("change_id", String, "Change Card the tests are for.")
	Attribute("request_revision", String, "Change Card revision the tests are for; report_tests_finished takes it.")
	Attribute("progress", TestWriterProgress, "Where this assignment stands, including Surla's findings when it needs changes.")
	Attribute("working_folder", String, "Folder to write the tests in: your own working copy of the project, which the server prepares when the user starts the build. It is empty until the server prepares working copies; an empty folder means none was prepared for this build.")
	Attribute("stories", ArrayOf(StorySummaryItem), "The Story as agreed at request_revision, in declaration order. It does not change when the change is revised later.")
	Attribute("required_tests", ArrayOf(RequiredTest), "One test per finished flow of stories, in declaration order; unfinished flows require none.")
	Required("change_id", "request_revision", "progress", "working_folder", "stories", "required_tests")
})

// buildMethods exposes starting and reading builds and the role-limited
// assignment operations of the builder and the test writer.
func buildMethods() {
	Method("start_build", func() {
		Description("The user starts building a confirmed change at its revision in force. Surla saves the build with the Story as agreed at that revision and assigns the build to the builder and the test writer. Building again replaces the build and both assignments, which then count as unread. A change that is not confirmed, or a request_revision that is not the one in force, is refused and nothing changes.")
		Payload(func() {
			Attribute("change_id", String, "Confirmed Change Card identifier.", func() { MinLength(1) })
			Attribute("request_revision", String, "The Change Card's confirmed revision in force.", func() { MinLength(1) })
			Required("change_id", "request_revision")
		})
		Result(StartedBuild)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Error("conflict", ErrorResult, "The Change Card is not confirmed or request_revision is not its revision in force; nothing changed.")
		Error("unprocessable", ErrorResult, "The agreed Story has two finished flows whose required tests would have the same name; revise the Story. Nothing changed.")
		HTTP(func() {
			POST("/changes/{change_id}/build")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
			Response("unprocessable", StatusUnprocessableEntity)
		})
	})

	Method("get_build", func() {
		Description("Return a change's build: not_started until the user starts one; otherwise the revision built, when, whether the builder read its assignment, the test writer's state and whether it read its assignment, and whether the build is for the change's revision in force. Reading changes nothing.")
		Payload(func() {
			Attribute("change_id", String, "Change Card identifier.", func() { MinLength(1) })
			Required("change_id")
		})
		Result(ChangeBuild)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Tool("get_build", "Read a change's build: whether the user started one, the revision built, whether each role read its assignment, the test writer's state, and whether the build is for the change's revision in force.")
		HTTP(func() {
			GET("/changes/{change_id}/build")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
		})
	})

	Method("list_builder_assignments", func() {
		Description("List the builder's assignments, one per change the user built, most recently started first, each with its change, revision and whether it was read. Reading the list records nothing.")
		Result(BuilderAssignmentList)
		Tool("list_builder_assignments", "List your build assignments: change_id, request_revision and whether you read each. Read one in full with get_builder_assignment(change_id).")
		HTTP(func() {
			GET("/assignments/builder")
			Response(StatusOK)
		})
	})

	Method("get_builder_assignment", func() {
		Description("Return the builder's assignment for a change in full and record that the builder read it at its revision. A new build of the change replaces the assignment, which then counts as unread.")
		Payload(func() {
			Attribute("change_id", String, "Change Card identifier from list_builder_assignments.", func() { MinLength(1) })
			Required("change_id")
		})
		Result(BuilderAssignment)
		Error("not_found", ErrorResult, "You have no build assignment for this change_id.")
		Tool("get_builder_assignment", "Read your build assignment for a change: the change_id and request_revision to build in the project folder. Verification of that revision judges the work: pass both to verify_change.")
		HTTP(func() {
			GET("/assignments/builder/{change_id}")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
		})
	})

	Method("list_test_writer_assignments", func() {
		Description("List the test writer's assignments, one per change the user built, most recently started first, each with its change, revision, state and whether it was read. Reading the list records nothing.")
		Result(TestWriterAssignmentList)
		Tool("list_test_writer_assignments", "List your test assignments: change_id, request_revision, state and whether you read each. Read one in full with get_test_writer_assignment(change_id).")
		HTTP(func() {
			GET("/assignments/test-writer")
			Response(StatusOK)
		})
	})

	Method("get_test_writer_assignment", func() {
		Description("Return the test writer's assignment for a change in full (the working folder, the Story as agreed at the built revision, the tests it requires, and the state with any findings) and record that the test writer read it at its revision. A new build of the change replaces the assignment, which then counts as unread.")
		Payload(func() {
			Attribute("change_id", String, "Change Card identifier from list_test_writer_assignments.", func() { MinLength(1) })
			Required("change_id")
		})
		Result(TestWriterAssignment)
		Error("not_found", ErrorResult, "You have no test assignment for this change_id.")
		Tool("get_test_writer_assignment", "Read your test assignment for a change: the working folder, the Story as agreed, one required test per finished flow, and your state with any findings to fix. When your tests are finished, call report_tests_finished with change_id and request_revision.")
		HTTP(func() {
			GET("/assignments/test-writer/{change_id}")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
		})
	})

	Method("report_tests_finished", func() {
		Description("The test writer reports that its tests for a change at the build's revision are finished, which asks Surla to look at them. Surla records the report; the assignment's state becomes reported. A report for another revision is refused and changes nothing.")
		Payload(func() {
			Attribute("change_id", String, "Change Card identifier from your test assignment.", func() { MinLength(1) })
			Attribute("request_revision", String, "request_revision of your test assignment.", func() { MinLength(1) })
			Required("change_id", "request_revision")
		})
		Result(TestWriterAssignmentItem)
		Error("not_found", ErrorResult, "You have no test assignment for this change_id.")
		Error("conflict", ErrorResult, "request_revision is not the revision of your test assignment; read it again with get_test_writer_assignment. Nothing changed.")
		Tool("report_tests_finished", "Report that your tests for the assignment's change_id and request_revision are finished. Surla records the report and returns the assignment's new state.")
		HTTP(func() {
			POST("/assignments/test-writer/{change_id}/finished")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
		})
	})
}
