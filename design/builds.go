// This file declares builds: the user's decision to build a confirmed change
// at one revision, and the builder's and the test writer's assignments for
// that build.
package design

import (
	. "goa.design/goa-ai/dsl"
	. "goa.design/goa/v3/dsl"
)

// TestsNotUsed is how a build reads when no test writer is used.
var TestsNotUsed = Type("TestsNotUsed", func() {
	Description("No test writer is used: the project verifies with the evaluator stored with surla verification set. Every read of a build in such a project shows this, whatever the build recorded.")
})

// TestsAssigned is the test writer's state before it reports its tests finished.
var TestsAssigned = Type("TestsAssigned", func() {
	Description("Surla assigned the tests for this build; the test writer has not reported them finished.")
})

// TestsReported is the test writer's state after it reported its tests finished.
var TestsReported = Type("TestsReported", func() {
	Description("The test writer reported its tests finished at this build's revision; Surla has not checked them.")
})

// TestsNeedChanges is the test writer's state after Surla found problems.
var TestsNeedChanges = Type("TestsNeedChanges", func() {
	Description("Surla checked the reported tests and found problems the test writer must fix before reporting again.")
	Attribute("findings", ArrayOf(String), "What Surla found, one problem per item: a test that does not build or a required test that is not declared.", func() {
		MinLength(1)
	})
	Attribute("checked_at", String, "When Surla's check reached this verdict.", func() { Format(FormatDateTime) })
	Required("findings", "checked_at")
})

// TestsAccepted is the test writer's state after Surla accepted its tests.
var TestsAccepted = Type("TestsAccepted", func() {
	Description("Surla checked the reported tests and recorded them as a commit.")
	Attribute("commit", String, "Full commit ID Surla recorded the accepted tests at (40 or 64 lowercase hex characters).", func() {
		Pattern("^([0-9a-f]{40}|[0-9a-f]{64})$")
	})
	Attribute("checked_at", String, "When Surla's check reached this verdict.", func() { Format(FormatDateTime) })
	Required("commit", "checked_at")
})

// TestsCouldNotRun is the test writer's state when Surla's check could not run.
var TestsCouldNotRun = Type("TestsCouldNotRun", func() {
	Description("Surla's check of the reported tests could not run, for a cause the test writer cannot fix; the user's step makes it possible.")
	Attribute("reason", String, "Why the check could not run.", func() {
		MinLength(1)
		MaxLength(1024)
	})
	Attribute("step", String, "What the user does so the check can run.", func() {
		MinLength(1)
		MaxLength(512)
	})
	Attribute("checked_at", String, "When Surla's check stopped.", func() { Format(FormatDateTime) })
	Required("reason", "step", "checked_at")
})

// TestsNotYetAccepted is the builder's view of a build with no accepted tests.
var TestsNotYetAccepted = Type("TestsNotYetAccepted", func() {
	Description("The build records no accepted tests.")
})

// TestWriterProgress is the test writer's state and whether it read its assignment.
var TestWriterProgress = Type("TestWriterProgress", func() {
	Description("Where the test writer's assignment for one build stands.")
	OneOf("tests", "The tests' state: not_used when the project verifies with its stored evaluator; otherwise assigned, reported, needs_changes with Surla's findings, accepted with the commit Surla recorded, or could_not_run with the reason and the user's step. Each verdict carries the time Surla reached it.", func() {
		Meta("oneof:json:flatten")
		Meta("oneof:type:field", "state")
		Attribute("not_used", TestsNotUsed)
		Attribute("assigned", TestsAssigned)
		Attribute("reported", TestsReported)
		Attribute("needs_changes", TestsNeedChanges)
		Attribute("accepted", TestsAccepted)
		Attribute("could_not_run", TestsCouldNotRun)
	})
	Attribute("read", Boolean, "True once the test writer read this assignment in full with get_test_writer_assignment.")
	Attribute("report", Int, "Number of the test writer's latest counted report of this build; 0 when none was counted. Reports are not counted yet, so it is 0.", func() {
		Minimum(0)
	})
	Required("tests", "read", "report")
})

// BuilderProgress says whether the builder read its assignment.
var BuilderProgress = Type("BuilderProgress", func() {
	Description("The builder's assignment for one build. It exists from the moment the user starts the build; verification of the build's revision, not the assignment, judges the builder's work.")
	Attribute("read", Boolean, "True once the builder read this assignment in full with get_builder_assignment.")
	Required("read")
})

// StartedBuild is a change's build as the user started it.
var StartedBuild = Type("StartedBuild", func() {
	Description("The usable build the user started for a change, with the builder's assignment and the test writer's progress when one is used. A build started with only the builder remains usable while stored verification settings supply its tests.")
	Attribute("request_revision", String, "Change Card revision the user built.")
	Attribute("started_at", String, "When the user started this build.", func() { Format(FormatDateTime) })
	Attribute("in_force", Boolean, "True when request_revision is the change's agreed revision, the one in force. It stays true while a revision the guide proposed waits beside the agreed one, and turns false once the user adopts a proposed revision or confirms another revision: the assignments then stay as they are, for this earlier revision, until the user builds again.")
	Attribute("builder", BuilderProgress, "The builder's assignment.")
	Attribute("test_writer", TestWriterProgress, "The test writer's assignment.")
	Attribute("required_test_count", Int, "Number of finished flows in the agreed Story. Zero means no test of this change's Story was required.", func() { Minimum(0) })
	Required("request_revision", "started_at", "in_force", "builder", "test_writer", "required_test_count")
})

// BuildNotStarted says the change has no usable build for its current test source.
var BuildNotStarted = Type("BuildNotStarted", func() {
	Description("This change has no usable build for the current test source: the user has not started one, or its builder-only build lost the stored verification settings that supplied its tests. Start a new build on the views page.")
})

// ChangeBuild is a change's build, or the fact that it has none.
var ChangeBuild = Type("ChangeBuild", func() {
	Description("A change's build as get_build reports it.")
	Attribute("change_id", String, "Change Card identifier.")
	OneOf("build", "not_started when the change has no usable build for the current test source; start a new build on the views page. Otherwise started with the usable build.", func() {
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
	Attribute("in_force", Boolean, "True when request_revision is the change's confirmed revision in force, as get_build reports it. It stays true while a revision the guide proposed waits beside the agreed one, and is false once the user has adopted a proposed revision or confirmed another revision: verify_change then refuses this revision, and the assignment stays as it is until the user builds again.")
	Attribute("read", Boolean, "True once you read this assignment in full with get_builder_assignment.")
	OneOf("written_tests", "Where the test writer's tests for this build stand: not_used when no test writer is used because the project verifies with its stored evaluator; otherwise accepted, with the commit and time, when the build records accepted tests, and not_yet_accepted when it records none.", func() {
		Meta("oneof:json:flatten")
		Meta("oneof:type:field", "state")
		Attribute("not_used", TestsNotUsed)
		Attribute("not_yet_accepted", TestsNotYetAccepted)
		Attribute("accepted", TestsAccepted)
	})
	Required("change_id", "request_revision", "in_force", "read", "written_tests")
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
	Attribute("in_force", Boolean, "True when request_revision is the change's confirmed revision in force, as get_build reports it. It stays true while a revision the guide proposed waits beside the agreed one, and is false once the user has adopted a proposed revision or confirmed another revision: the assignment then stays for this earlier revision until the user builds again.")
	Attribute("progress", TestWriterProgress, "Where this assignment stands.")
	Required("change_id", "request_revision", "in_force", "progress")
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
	Attribute("in_force", Boolean, "True when request_revision is the change's confirmed revision in force, as get_build reports it. It stays true while a revision the guide proposed waits beside the agreed one, and is false once the user has adopted a proposed revision or confirmed another revision: the assignment then stays for this earlier revision until the user builds again.")
	Attribute("progress", TestWriterProgress, "Where this assignment stands, including Surla's findings when it needs changes.")
	Attribute("working_folder", String, "Folder to write the tests in: your own working copy of the project.")
	Attribute("design_source", String, "The canonical design source as agreed at request_revision, copied when this build started; later revisions do not change it.")
	Attribute("stories", ArrayOf(StorySummaryItem), "The Story as agreed at request_revision, in declaration order. It does not change when the change is revised later.")
	Attribute("required_tests", ArrayOf(RequiredTest), "One test per finished flow of stories, in declaration order; unfinished flows require none.")
	Required("change_id", "request_revision", "in_force", "progress", "working_folder", "design_source", "stories", "required_tests")
})

// buildMethods exposes starting and reading builds and the role-limited
// assignment operations of the builder and the test writer.
func buildMethods() {
	Method("start_build", func() {
		Description("The user starts building a confirmed change at its agreed revision, the one in force, on the project owner's bound team; a revision the guide proposed beside it is built only after the user adopts it. Surla checks the change and prerequisites without effects, opens and refreshes the test-writer copy when used, checks the revision again, and saves the build with the agreed Story and design. Surla immediately registers the build's exact roles and folders before generating required tests. Stored verification settings use only the builder and remove a former test writer. Building again replaces the build and both unread assignments. A refused start saves no build and registers no role; a refusal at the second revision check may leave a reusable working copy. Generation trouble and team warnings after saving leave the saved build intact. Accepted work belongs to the server and survives client disconnection. Copy work and generation have a five-minute limit under the project's turn; after that turn is released, bringing the team up has its own 45-second limit. A server stop after saving leaves the saved assignment or verdict; build again to complete it. get_build reads the saved build without this start's transient team or copy outcome.")
		Payload(func() {
			Attribute("change_id", String, "Confirmed Change Card identifier.", func() { MinLength(1) })
			Attribute("request_revision", String, "The Change Card's agreed revision, the one in force (not a waiting proposal's).", func() { MinLength(1) })
			Required("change_id", "request_revision")
		})
		Result(StartBuildResult)
		buildStartRefusals()
		Error("not_implemented", ErrorResult, "Start this project with surla to bind the team; nothing was saved.")
		Error("unavailable", ErrorResult, "The server is stopping or the start's time limit ended before saving; call again on a running server. Nothing was saved.")
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Error("conflict", ErrorResult, "The Change Card is not confirmed or request_revision is not its agreed revision (a waiting proposal's revision cannot be built); nothing changed.")
		Error("unprocessable", ErrorResult, "The agreed Story has a finished flow whose key gives its required test an empty name, or two finished flows whose required tests would have the same name; revise the Story. Nothing changed.")
		HTTP(func() {
			POST("/changes/{change_id}/build")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
			Response("unprocessable", StatusUnprocessableEntity)
			Response("not_implemented", StatusNotImplemented)
			Response("unavailable", StatusServiceUnavailable)
			buildStartRefusalResponses()
		})
	})

	Method("get_build", func() {
		Description("Return a change's usable build for the current test source: not_started when none exists, including a builder-only build whose stored verification settings were removed; start a new build on the views page. Otherwise return the revision built, when, whether the builder read its assignment, the test writer's state and whether it read its assignment, and whether the build is for the change's revision in force. Reading changes nothing.")
		Payload(func() {
			Attribute("change_id", String, "Change Card identifier.", func() { MinLength(1) })
			Required("change_id")
		})
		Result(ChangeBuild)
		Error("not_found", ErrorResult, "No Change Card exists with this change_id.")
		Tool("get_build", "Read a change's usable build for its current test source: the revision built, whether each role read its assignment, the test writer's state, and whether the build is for the change's revision in force. not_started means there is no usable build; the user starts a new build on the views page.")
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
		Description("Return the test writer's assignment for a change in full (the working folder, the design and Story as agreed at the built revision, the tests it requires, and the state with any findings) and record that the test writer read it at its revision. Later revisions do not change the saved design or Story. A new build of the change replaces the assignment, which then counts as unread.")
		Payload(func() {
			Attribute("change_id", String, "Change Card identifier from list_test_writer_assignments.", func() { MinLength(1) })
			Required("change_id")
		})
		Result(TestWriterAssignment)
		Error("not_found", ErrorResult, "You have no test assignment for this change_id.")
		Tool("get_test_writer_assignment", "Read your test assignment for a change: the working folder, the design and Story as agreed, one required test per finished flow, and your state with any findings to fix. Align the service interface in your copy with the agreed design and write tests inside its test module. When your tests are finished, call report_tests_finished with change_id and request_revision.")
		HTTP(func() {
			GET("/assignments/test-writer/{change_id}")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
		})
	})

	Method("report_tests_finished", func() {
		Description("The test writer reports that its tests for a change at the build's revision are finished. Surla only records the report: the assignment's state becomes reported from any state, earlier findings or a recorded commit are cleared, and the tests are not checked. A report for another revision is refused and changes nothing.")
		Payload(func() {
			Attribute("change_id", String, "Change Card identifier from your test assignment.", func() { MinLength(1) })
			Attribute("request_revision", String, "request_revision of your test assignment.", func() { MinLength(1) })
			Required("change_id", "request_revision")
		})
		Result(TestWriterAssignmentItem)
		Error("not_found", ErrorResult, "You have no test assignment for this change_id.")
		Error("conflict", ErrorResult, "request_revision is not the revision of your test assignment; read it again with get_test_writer_assignment. Nothing changed.")
		Tool("report_tests_finished", "Report that your tests for the assignment's change_id and request_revision are finished. Surla records the report as reported, without checking the tests, and returns the assignment's new state.")
		HTTP(func() {
			POST("/assignments/test-writer/{change_id}/finished")
			Response(StatusOK)
			Response("not_found", StatusNotFound)
			Response("conflict", StatusConflict)
		})
	})
}
