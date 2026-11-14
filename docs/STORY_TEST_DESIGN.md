# Story flows and the tests a build requires

This page is for contributors to Surla. It describes how a Story becomes the
tests a build requires, how Surla prepares the test writer's working copy and
judges what the test writer reports, and how verification chooses the tests
that judge the builder's commit. [How Surla works](HOW_IT_WORKS.md) tells the
same journey from the user's side, and the reference gives the exact contract
of [each tool](REFERENCE.md#mcp-tools) and of
[builds and assignments](REFERENCE.md#builds-and-assignments).

## Terms

| Term | Meaning |
| --- | --- |
| Story | Go source, in the project's `design/` folder, that describes what happens in the business: its actors and its flows. |
| Flow | One path through a Story, from what starts it to an outcome. For example, a customer asks for a refund above the limit, a manager approves it, and the refund is paid. |
| Finished flow | A flow with a start, at least one outcome and no open question. Any other flow is unfinished: a draft. |
| Change Card | The saved record of one requested change: the request, its open questions and the user's answers, its examples, its Story and its design source. |
| `request_revision` | The identifier of one exact saved revision of a Change Card, including its Story. |
| Agreed revision | The revision of a Change Card the user confirmed. A revision the guide proposes later waits beside it until the user adopts or declines it. |
| The server | The project's Surla server, started by `surla` in the project folder. |
| The guide | The coding agent the user talks to. It writes the Story and proposes changes. |
| The builder | The coding agent that writes the application code and asks the server to verify a commit. |
| The test writer | The coding agent that writes one test per finished flow, in its own working copy. |
| Required test | The evaluation scenario Surla generates for one finished flow, with the hook method the test writer implements for it. |
| Holdout | The scenario tag that hides a scenario's details from the builder. Tests the builder can read are never hidden, so Surla refuses the tag in written tests. |
| Run directory | The folder in which one verification run keeps its request, its lock, its result and its logs. |
| The sandbox | The operating-system sandbox (macOS `sandbox-exec` or Linux Landlock and Seccomp-BPF) in which the server evaluates Stories, builds programs and runs the candidate's programs. |
| Stored verification settings | A settings file stored for the project with `surla verification set`. It names one evaluator suite at one commit, and every run uses that suite instead of written tests. |
| Tests commit | The commit of the tests a run uses: the commit of the accepted tests on `surla/tests`, or the suite commit the stored verification settings name. |

## What a Story is

The Story language is the nested Go module `github.com/goadesign/surla/story`.
It depends only on Goa and the standard library, so a Story can be written
before any service, agent or architecture exists. Its
[README](../story/README.md) lists every declaration. A short example:

```go
var _ = Story("Refunds", func() {
	Actor("customer", "Customer")
	Actor("manager", "Support manager")

	Flow("refund-over-limit", "Refund above the limit", func() {
		StartsWith("request-refund", "customer", "Requests a refund")
		Step("approve-refund", "manager", "Approves the refund")
		Outcome("refund-paid", "customer", "Receives the refund")
	})
})
```

Every actor, flow, step and question has a stable key. Titles and summaries
can change without changing a key: renaming "Approves the refund" to
"Support manager approves the refund" keeps `approve-refund`, and the
required test of `refund-over-limit` keeps its name. A missing start, a
missing outcome or an open question leaves a valid unfinished flow. A
duplicate key, an invalid key or a step that names an undeclared actor is an
error. Actor names are the user's business roles: `warehouse-worker` and
`insurance-agent` are valid, and no keyword rule refuses one.

A project's design imports the language, so the project's `go.mod` must
require the Story module. A starter project made by `surla init` requires it
at the version Surla itself requires. It ships a first Story,
`design/story.go`, with one finished flow, `prices-cookie`, and the hook
method that flow requires, `PricesCookie` in `eval/acceptance`.

The guide changes the Story by editing its Go source, then asks the server to
evaluate it:

1. `refresh_story` reads the project's module from the project folder, starts
   its evaluation in the sandbox in the background, and returns at once with
   an `op_id`.
2. `get_story_refresh` returns the current evaluation: `none`, `running`,
   `succeeded` with the evaluated Stories, or `failed` with a diagnostic.
   The server keeps one evaluation per project in memory. A refresh of an
   unchanged module returns the running or succeeded evaluation; a changed
   module or a failed evaluation starts a new one. After a restart the state
   is `none`. When a design file imports the Story language and the
   project's `go.mod` does not require its module, the diagnostic names the
   step: in the folder that holds that `go.mod`, run
   `go get github.com/goadesign/surla/story@<version>`, at the version
   Surla's own `go.mod` requires, commit `go.mod` and `go.sum`, then try
   again.
3. The guide passes a succeeded `op_id` as `story_refresh_op_id` to
   `propose_change` or `revise_change`. The server checks that the project
   folder still holds the module that was evaluated, then saves the evaluated
   Stories and the `design/*.go` source in the Change Card. Both are part of
   its `request_revision`.

From then on, nothing reruns the current source to find out what was agreed:
a later edit, a helper outside `design/` or a dependency update cannot
change the Story an agreed revision holds.

## From a Story to the required tests

When the user builds a change, the server copies the agreed revision's Story
and design source into the build. The test writer's work is judged against
that copy, so a revision the guide proposes later, or one the user adopts,
does not change the tests this build requires. The user builds again to
require the tests of a later revision.

Each finished flow requires one test. Unfinished flows require none: they
stay in the saved Story, editable in the source, and do not stop the
finished flows from being built and checked. The test writer's assignment
lists every flow of the saved Story and one required test per finished flow.
The build also saves, from the same revision, each question with the answer
the user confirmed, the confirmation examples and the preserved behaviors,
and the assignment returns them: the test writer may not read the change,
and its tests must hold the service to them as well as to the flows. Surla's
check of a report confirms the required tests, not what the tests assert.

One rule, `mapStoryFlows` in
[`internal/designeval/eval.go`](../internal/designeval/eval.go), names each
required test from its flow's key, both in the Story table (below) and in
`StoryScenarios`, which lists the required tests and the unfinished flows the
table leaves out without generating anything:

| Flow key | Scenario name (Goa `codegen.SnakeCase`) | Hook method (Goa `codegen.Goify`) |
| --- | --- | --- |
| `refund-over-limit` | `refund_over_limit` | `RefundOverLimit` |

Two finished flows whose keys give the same scenario name or hook method,
such as `a-b` and `a_b`, or the same key in two Stories, are refused, naming
both flows. `start_build` checks this before it saves anything and refuses
with `unprocessable`; the guide then revises the Story. A finished flow whose
scenario name repeats a scenario the test module's design declares by hand
cannot be generated either: in a starter project, a flow `prices-tea` would
repeat the suite's own `prices_tea`, and generating the required tests fails
naming the duplicate.

The required tests are declared in the project's test module, the `eval/`
module that `surla init` creates in the project folder, by its Story table:
`eval/design/story_tests.go`, a source file of the module's own design.
`StoryTests`, in
[`internal/designeval/story_table.go`](../internal/designeval/story_table.go),
renders the table from the saved Story. The table declares the package the
module's other `design/*.go` files declare and registers a Goa evaluation
root, `storyScenarioRoot` from `story_scenarios.go`, with one row per
finished flow; every string in a row is a quoted Go literal. The table holds
those rows, not the Story, so the module's design never imports the
project's Story. The root runs after Goa-AI's evaluation suites root and
attaches one scenario per row to the module's single evaluation suite before
Goa validates it. Generating the module, which `Evaluator.Generate` runs in
the sandbox, then writes:

- `gen/evals/<suite>/suite.go`, whose `Hooks` interface gains one method per
  required test;
- `gen/cmd/<service>-evals/main.go`, the test program, which builds the suite
  from those hooks and the module's hand-written `acceptance` package.

No stub is written. Until the test writer writes a method for every required
test, the test program does not compile, and the compiler names each missing
method. A design that declares no suite, or more than one, is refused with
the fix; a table with no finished flow registers no row and still requires
exactly one suite. When a later revision drops or renames a flow, its method
leaves `Hooks`, and an old hand-written method still compiles and is unused.

Because the required tests are part of the module's design, every generation
of the module keeps them, whoever runs it and with any tool: a build's
preparation and Surla's check of a report, `generate_code` with
`target: "eval"`, `surla gen`, `surla init`, and `goa gen` run by hand. A
module without a Story table declares no Story scenario. The test writer
leaves the table as it is: when a build starts and again at each report,
Surla generates the module with the table it renders from the build's saved
Story in place of whatever the test writer left at that path, so the test
writer's version never counts. The table is replaced by renaming a new file
over it, so a symbolic link at its path is replaced, not followed.
`surla init` writes the starter's table from the starter's own Story. A
project whose operator stored verification settings for another suite keeps
whatever table its test module last received, because no build writes it
again, and the operator may delete it.

## The test writer's working copy

The test writer works in its own linked Git working copy of the project:
the folder `.surla-tests` in the project, on the branch `surla/tests`.
[`internal/testwriter`](../internal/testwriter/copy.go) owns it. A build
start opens it when the project has no stored verification settings and has
Surla's test layout: the `eval/` test module that `surla init` makes.
Without that layout and without stored verification settings, the start is
refused with `no_test_layout`. One build start or one report check uses the
copy at a time: each takes the project's working-copy turn first. The
reference lists every refusal of a start under
[`start_build`](REFERENCE.md#start_build).

Opening the copy:

1. adds `.surla-tests/` to the repository's own `info/exclude` file, never
   to a tracked file, so the project's `git status` stays clean;
2. creates the copy on a new `surla/tests` branch at the project's current
   commit, or on the existing `surla/tests` branch when no other folder has
   it checked out, so the commits of accepted tests survive a deleted copy.
   A copy that is already there is verified and used as it is.

Before saving the build, a start refuses, and saves nothing, when:

| Refusal | Cause | Recovery |
| --- | --- | --- |
| `not_repository` | The project folder is in no Git repository. | Run `git init` in the project folder and commit its files. |
| `no_commit` | The repository has no commit. | Commit the project's files. |
| `git_identity_missing` | Git has no name and address to commit accepted tests with. | Configure Git's name and address. |
| `copy_folder_in_use` | `.surla-tests` exists and is not Surla's working copy. | Move it out of the project. |
| `tests_branch_in_use` | Another working copy holds `surla/tests`. | The message names that folder and the step that frees the branch. |
| `sandbox_unavailable` | The system has no candidate sandbox. | Start the project on a system with Surla's sandbox (macOS or Linux with Landlock ABI v4+). |
| `working_copy_busy` | Another start or a report check holds the project's working-copy turn. | Build again when it has ended. |

The start then brings the copy forward to the project's current commit, but
only when that is safe. The result names one of three outcomes:

| Outcome | Meaning |
| --- | --- |
| `at_project_commit` | The copy is at the project's commit. |
| `not_contained` | The copy holds tests that are not merged into the project's branch, so its commit is kept. The step is to merge `surla/tests` into the project's branch, then build again. |
| `dirty` | The copy has uncommitted or untracked files, so its commit and files are kept. |

Then the start writes the required tests into the copy, after removing any
`.surla-gen-*` staging folder that a write cut short by a server's death
left in the test module. It generates the module with the Story table, then
writes the table and the generated folder. A generation that fails writes
neither, so the copy stays as it was and a later build can still bring it
forward; only a symbolic link at the table's path is replaced by the table
first. A problem the test writer can fix, such as a test module design
that does not compile, makes the test writer's state `needs_changes` with
that finding, at report number 0. A problem it cannot
fix makes the state `could_not_run`, with a reason that says what failed:
the start's time limit or a server stop, a download of the module's
dependencies that fails outside the test writer's changes (decided as for a
report, below), the generator and its phase, or writing the generated folder
into the copy. Any other cause has no text safe to show, so its reason points
at the server's trace of the start. The user's step ends with building the
change again.

## How Surla judges a report

The test writer calls `report_tests_finished` with the change and the
build's revision. The report raises the build's report number by one, sets
the state to `reported`, clears any earlier verdict, and returns at once. One
report checker per project then judges it, without trusting the copy's
author:

1. It waits for the project's working-copy turn.
2. It writes the required tests into the copy again from the build's saved
   Story, after removing, as a start does, any `.surla-gen-*` staging folder
   left in the test module. Only a folder that holds nothing but a generated
   `gen` folder is removed; a link, a file or any other folder with such a
   name is the author's and stays.
3. It copies the copy's files, as a commit would hold them, into a private
   folder, and compares them with the copy's base: the commit where
   `surla/tests` and the project's branch last met.
4. It generates the test module again in that private copy, with the Story
   table rendered from the saved Story in place of whatever the author left
   at that path, and writes the table and the generated folder there, so the
   author cannot change what is required.
5. It downloads the module's dependencies, builds the test program in the
   sandbox, and runs the program's `--describe` in the sandbox to list the
   scenarios it declares.
6. It compares those scenarios with the required tests by the generator's own
   names.

The whole judgment has a 15-minute limit. Within it, building the test
program has 10 minutes and listing its scenarios 60 seconds.

The download in step 5 runs outside the sandbox with the user's proxy
settings, so its output is withheld, and Surla decides whose failure it is
from what it can compare. A download reads only the module's `go.mod` and
`go.sum`. When they are those of the copy's base, the author changed nothing
that explains the failure. When the author changed them, Surla downloads the
base's own lists, read from the project's commit: if those fail too,
downloading does not work where Surla runs. Both cases make the check
`could_not_run`, with a step that has the user run the download where its
output can be read. When the base's lists download and the author's do not,
the failure is the author's finding. The author cannot hide a defect of its
own lists behind this comparison, because the base's lists come from the
project's commit, not from the copy. A generation, in step 2 or step 4, that
fails while loading the module's dependencies is decided the same way.

The verdict replaces `reported`:

| Verdict | When | What is saved |
| --- | --- | --- |
| `accepted` | Every step passed. | The commit of the accepted tests and the time of the check. |
| `needs_changes` | A step failed for a cause the test writer can fix. | The findings, one problem per item, with no absolute path. |
| `could_not_run` | The check failed for a cause the test writer cannot fix. | The reason and the user's step. |

Findings name the first step that failed. The test writer's assignment and
`get_test_findings` show each finding's message; the names below only sort
the causes and are not shown:

| Finding | Cause |
| --- | --- |
| `outside_module` | A file outside the test module differs from the copy's base. At most 20 are named, and one more finding counts the rest. Each names the base commit, and each named file's finding gives the step that undoes the change, committed or not. |
| `generation` | The required tests could not be generated, for example because the module's design does not compile or declares no suite or several. |
| `dependencies` | `go mod download` failed for the author's changed `go.mod` or `go.sum` while the base's own lists download, or no comparison could be made. |
| `build` | The test program does not compile (the compiler names a missing hook method) or did not build within its limit. |
| `listing` | `--describe` failed, did not finish within its limit, or did not print exactly one suite declaration. |
| `hidden_tag` | A declared scenario is tagged `holdout`. |
| `missing_scenario` | The program does not declare a required test. The finding names the scenario, its flow and its hook. |

An accepted copy is committed on `surla/tests` with the user's own Git
identity, hooks disabled and no signature, with the message
`Add the tests Surla accepted for change <change_id>, report <n>`. Only the
files that were built and listed are committed; an edit made after the check
stays in the copy as a change. When the branch already holds those files,
its commit is reused. Every read of an accepted verdict adds one fixed
sentence: "Surla checked that these tests build into the generated test
program and declare every required scenario; it did not judge what they
assert."

The verdict is saved only while the build still answers to the report that
was checked: the same change, revision, start and report number. A newer
report or a new build replaces it, and nothing is saved. A verdict marks the
test writer's assignment unread, and an acceptance also marks the builder's.
`get_build` counts the findings, the test writer reads their text in its
assignment, and the user reads them with `get_test_findings`.

When the server stops during a judgment, nothing is saved and the build
stays `reported`; the next server start judges every reported build. A
verdict that could not be saved, or a build that could not be read at a
start or before a judgment, is tried again a minute later, up to three times
in a row; then the build waits for the next report or server start. When
the project's history was rewritten so that the copy and the project share
no commit, the check could not run, and the user's step is to remove the
copy and its branch (`git worktree remove .surla-tests` and
`git branch -D surla/tests` in the project folder), then build again.

This is a separation of duties, not isolation. Every agent runs as the user
on one machine, so another program of the user's can move a branch or read
a file. The check makes sure the tests the builder is judged by are the
tests Surla checked and recorded.

## How verification chooses its tests

The builder calls `verify_change` with the change, its agreed revision
and its candidate commit: the commit of the builder's code to verify. It
never names the tests. One function, `testsChoice` in
[`verification_plan.go`](../internal/controlplane/verification_plan.go),
chooses them, for a run and for its [approval](#approval) alike, and
`verificationPlan` builds the run from that choice:

- With stored verification settings, every run uses the suite they name, at
  the commit they name. No test writer is used.
- Otherwise the change's build must be at the revision, and its tests must
  be accepted. The run builds the candidate's programs from the candidate
  commit and runs the test program generated at the tests commit: the
  commit of the accepted tests on `surla/tests`.

The refusals and their next steps:

| Refusal | Cause | Next step |
| --- | --- | --- |
| `no_build` | The change has no build with a test writer, or its build is of another revision. | The user builds the agreed revision. |
| `tests_not_accepted` | The build's tests are `assigned`, `reported`, `needs_changes` or `could_not_run`. | The message names the state and the step: the test writer reports or fixes its findings, the builder verifies again after Surla's verdict on a report, or the user takes the check's step. |
| `no_test_layout` | A commit lacks the layout `surla init` makes, the tests commit is not in the repository, or the project folder is in no Git repository. | The message names who acts: the builder commits the layout, the test writer reports again so Surla records its tests anew, or the user builds again or makes the folder a repository. |
| `conflict` | The change is not confirmed or the revision is not its agreed revision, the repository does not hold the candidate commit, or the project folder was replaced. | The builder reads its assignment again or commits its candidate; for a replaced folder, the user stops and starts Surla in the project folder. |

The server starts the run in its own run directory and returns at once. Each
run records where its tests came from, as `test_source`: `evaluator` for the
stored settings' suite, or `written_tests` with the tests commit and the
notice "These tests were written by a separate agent on this machine and are
not isolated from the builder." The views page, the page the server serves
to the user's browser, shows that notice with the run in its Proof view. A
test program the test writer wrote runs outside the sandbox with only `PATH`
and a private `TMPDIR`, and no model key; it starts the candidate's programs
in the sandbox. A run keeps its revision and its tests after the change is
revised or the server restarts.

The change list says, for each change's latest run, whether it names the
agreed revision and the tests a run of that revision would use
(`inputs_current`): the accepted tests commit of the build of that revision,
or the suite the stored settings name. The reference gives the contract of
[`verify_change`](REFERENCE.md#verify_change) and of
[`list_changes`](REFERENCE.md#list_changes).

A live target, a copy of the project that the server builds from a commit
and runs so the builder can try prompt wording, needs no accepted tests.
Without stored verification settings it builds the candidate commit and
starts the test program of that same commit's test module, the builder's
own, in its `--live` mode.

## Approval

The user approves a passed run on the views page. Approval takes its tests
commit from the same choice as verification: the stored settings' suite
commit, or the commit of the tests accepted for the revision. A run of other
tests is refused, and the builder verifies again. A saved approval holds only
while those same tests stay accepted for its revision: a new report or a new
build hides it until that commit is accepted again. The approval names the
builder's commit and the tests commit. The builder commits in the project
folder, so its commit is already on the branch checked out there. For
written tests the user merges the tests commit, on `surla/tests`, into that
branch; with stored verification settings there is nothing to merge. Surla
merges nothing. Approval needs no promotion settings; when a project
stores them, approval also checks the release binaries, as
[`approve_merge`](REFERENCE.md#approve_merge) describes.

## Where the code lives

| Concern | Source |
| --- | --- |
| Story language | [`story/`](../story/README.md) |
| Story evaluation and adoption | [`story_refresh.go`](../internal/controlplane/story_refresh.go), [`designeval/eval.go`](../internal/designeval/eval.go) (`Evaluator.Evaluate`) |
| Naming and generating the required tests | [`designeval/eval.go`](../internal/designeval/eval.go) (`Evaluator.Generate`, `mapStoryFlows`), [`story_table.go`](../internal/designeval/story_table.go) (`StoryTests`), [`story_names.go`](../internal/designeval/story_names.go) (`StoryScenarios`), [`story_scenarios.go`](../internal/designeval/story_scenarios.go), [`test_layout.go`](../internal/designeval/test_layout.go) |
| Starting a build | [`build_start.go`](../internal/controlplane/build_start.go), [`build_start_copy.go`](../internal/controlplane/build_start_copy.go), [`builds.go`](../internal/controlplane/builds.go) |
| The working copy, the check and the record | [`testwriter/copy.go`](../internal/testwriter/copy.go), [`testwriter/check.go`](../internal/testwriter/check.go), [`testwriter/git.go`](../internal/testwriter/git.go) |
| Judging reports | [`report_check.go`](../internal/controlplane/report_check.go) |
| Choosing a run's tests | [`verification_plan.go`](../internal/controlplane/verification_plan.go), [`verify/program_env.go`](../internal/verify/program_env.go) |
| Running verification | [`verification.go`](../internal/controlplane/verification.go), [`verifyrun`](../internal/verifyrun/verifyrun.go) |
