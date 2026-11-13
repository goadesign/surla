# Story flows and independently written tests

Status: engineering draft. The portable Story language
(`github.com/goadesign/surla/story`), synchronous sandboxed typed Story
observation in `internal/designeval` (`Evaluator.Evaluate`), in-memory
background Story refresh in `internal/controlplane` (`refresh_story` and
`get_story_refresh`), adopting the observed Story into the Change Card's
saved `request_revision` (`story_refresh_op_id` on `propose_change` and
`revise_change`), and scenario scaffold generation in `internal/designeval`
(`Evaluator.GenerateWithStories`, with no production caller until the
independent test-authoring lifecycle lands) have shipped. The independent
test-authoring lifecycle and the timeline and proof UI and MCP surfaces
described here have not shipped. The [product direction](PRODUCT_DIRECTION.md)
records the intended experience; this document defines the engineering work
needed to deliver it.

## Problem and intended result

An engineer describing a major feature first needs to agree on what happens
in the business. For example, a customer requests a refund above the limit,
a manager approves it, and the refund is paid. Surla calls that path a
**flow**. The **Story** is repository-versioned Go source describing those
flows, their participants, questions and outcomes.

When the engineer edits the Story through chat or the timeline, the guide
changes its Go source. Surla evaluates the changed source in the background
inside its existing sandbox and returns the observed Story for the timeline.
Unfinished drafts remain valid so the engineer can refine them in place.
Building includes the finished flows. Unfinished drafts stay editable and are
explicitly shown as untested and excluded from that build; they do not block
the finished flows from being built and checked.
Stable step keys keep a step's identity when its wording changes. Participants
describe business roles, such as a customer or manager, without requiring an
architecture to exist first.

Surla's generator adapter attaches finished Story flows to the evaluator
suite so the existing Goa-AI evaluator generator writes their hook methods on
the suite's `Hooks` interface for an agent that did not write the platform
code. That agent implements tests which interact with the built platform
through its public interfaces. Generating a scaffold does not prove that the
implemented test makes the right assertions.

Surla must keep each verification result connected to the requirements it
checked, the test program it ran and the candidate commit. Later edits must
leave older results readable with their original meaning. The workflow must
support changing requirements and tests on the same running server.

## How the current system works

The calling model sends its request analysis through `propose_change` or
`revise_change`. The control plane validates and saves a Change Card, the
record of the requested behavior, open questions, answers and examples.
When the workspace supplies design source, the control plane reads Go files
from `design/` and includes those bytes in the card's request revision.
That revision identifies the exact saved card contents and their owner.

The saved source is input provenance. It does not capture the meaning of
every future execution of ordinary Go: a helper outside `design/`, a module
dependency or an execution-time value can change an evaluated result.

The design evaluator already compiles and runs workspace Go in
`verify/candidate`, the package that applies Surla's operating-system sandbox.
Architecture preview and structural comparison call that evaluator
synchronously; when the evaluated target imports
`github.com/goadesign/surla/story`, `Evaluator.Evaluate` also returns the
observed typed Story (`Result.Stories`) without adding `story` to targets that
do not import it. `refresh_story` and `get_story_refresh` evaluate the bound
workspace's Story in the background and keep one current operation in memory
per project owner and bound workspace. Passing a succeeded operation's `op_id` as
`story_refresh_op_id` on `propose_change` or `revise_change` verifies the
bound workspace snapshot digest and records the observed `stories` and
`design/*.go` source together in the saved Change Card and its
`request_revision` (`confirm_intent` preserves those saved `stories` without
re-evaluating, and `revise_change` without `story_refresh_op_id` keeps the
agreed `stories` and saved `design_source` together).
Code generation reads the active application or its local
`eval/` module and publishes generated files without changing its module
requirements. `check_project` takes no input and tests every discovered Go
module in a protected workspace copy inside Linux containers.

For verification, a caller names the confirmed card revision and candidate
commit. The operator's configuration determines which evaluator suite runs.
An **evaluator suite** is the trusted program that declares and executes the
verification scenarios. The server starts a detached verification process,
returns its run ID, and later returns saved status and builder-safe results.
Every declared scenario remains part of the required result; an omitted
scenario is not a pass.

For an automatically configured starter, the evaluator commit is selected
at server startup or saved when `init_project` publishes the initial project.
The current running server has no lifecycle for accepting newly authored
Story tests for a particular request revision. Promotion has a separate
owner which supplies its expected test and artifact inputs.

### Existing contracts used by this design

| Concern | Owning source and contract |
|---|---|
| Read workspace design source | [`workspace.go`](../internal/controlplane/workspace.go), `readWorkspaceDesignSource`; [`snapshot.go`](../internal/designeval/snapshot.go), `ReadDesignFilesFromRoot`. Reads eligible Go files recursively under `design/`. |
| Save and identify intent | [`controlplane.go`](../internal/controlplane/controlplane.go), `ProposeChange` and `computeCanonicalRevision`; [`revise.go`](../internal/controlplane/revise.go), `ReviseChange`; [`cp1_validation.go`](../internal/controlplane/cp1_validation.go), `confirm`; [`cards.go`](../internal/controlplane/cards.go), `cardStore` and `decodeCardBytes`. |
| Preserve target module versions | [`module.go`](../internal/designeval/module.go), `prepareHarnessModule` and `sameVersions`. The evaluator must retain the dependency versions selected by the target. |
| Evaluate and generate | [`eval.go`](../internal/designeval/eval.go), `Evaluator.Evaluate`, `Evaluator.Generate`, and `Evaluator.GenerateWithStories`; [`story_refresh.go`](../internal/controlplane/story_refresh.go), `RefreshStory` and `GetStoryRefresh`; [`generate_code.go`](../internal/controlplane/generate_code.go), `GenerateCode`. |
| Check uncommitted work | [`check_project.go`](../internal/controlplane/check_project.go), `CheckProject`; [`design.go`](../design/design.go), `check_project`. The method takes no payload. |
| Choose verification inputs | [`verification.go`](../internal/controlplane/verification.go), `VerificationConfig` and `VerifyChange`; [`workspace_serve.go`](../cmd/surla/workspace_serve.go), `configureServeWorkspace`. Builders do not choose their evaluator commit. |
| Run and recover verification | [`verifyrun`](../internal/verifyrun), `Request`; [`verification.go`](../internal/controlplane/verification.go), `GetVerification` and `ListVerifications`. |
| Count required scenarios | [`report.go`](../internal/verify/report.go), `Assemble`. The selected suite's declaration determines the required scenarios. |
| Interactive candidate execution | [`suite.go`](../verify/suite/suite.go), `Command.Live`. Starts sandboxed candidate binaries; it does not run flow tests against a user-supplied external endpoint. |
| Expected promotion inputs | [`approval.go`](../internal/controlplane/approval.go), `PromotionTarget`; [`promotion.go`](../cmd/surla/promotion.go), the command's configured target. Verification does not rewrite those inputs. |

## Proposed Story and build contract

The portable Story language lives in a nested module,
`github.com/goadesign/surla/story`. Its syntax and expression packages depend
on Goa and the standard library, without importing Surla's control plane,
Goa-AI, Model, or telemetry. This keeps ordinary domain descriptions usable
before architecture exists and avoids bringing the control plane into a
platform's dependency graph. Integration with the existing evaluator generator
belongs with the code that already depends on that generator. When
`Evaluator.GenerateWithStories` runs with at least one finished flow, the host
maps the saved `[]StorySummary` into scenario specifications and renders them
into the sandboxed generation harness, which registers a Goa evaluation root
after the evaluator suites root (`evalexpr.Root`). Its preparation phase adds
scenarios for finished flows to the single declared suite before the owning
evaluator validates the combined suite. Neither the portable Story module nor
the evaluator module imports the other.

A flow declares a stable key, a title, its starting step, intermediate steps,
outcomes, and questions. Steps have stable keys and refer to declared domain
actors. Titles and descriptions can change without changing these keys. For
example, changing "Manager approves" to "Support manager approves the refund"
preserves `approve-refund` and the test associated with that step.

Missing starts, missing outcomes, and unresolved questions describe unfinished
flows rather than invalid Go declarations. A finished flow has a start, an
outcome, and no unresolved question. Invalid references, duplicate keys, and
invalid key syntax remain errors: allowing unfinished content does not make a
reference to an undeclared actor meaningful. Actor meaning stays with the
guide and user; names such as `warehouse-worker` and `insurance-agent` are
valid and must not be rejected through keyword rules.

Building includes finished flows only. The observed Story and result view
retain all unfinished flows and identify them as untested and excluded from
that build. Questions attached to an excluded draft are not silently promoted
into prerequisites for building another finished flow. Other unresolved
questions about the requested build still follow the existing intent rules.

The evaluator observes the typed Story inside the existing candidate sandbox.
The saved card includes that observed value in its request revision; saved
`design/` bytes remain source provenance. An author or verifier does not
reconstruct accepted requirements by rerunning the current checkout. A later
source or dependency change therefore cannot silently redefine the Story
that an earlier result claims to have checked.

### Background evaluation

Editing through chat or the timeline changes the same repository source.
`refresh_story` reads the bound workspace's module snapshot itself, starts
sandboxed evaluation under a fixed control-plane deadline, and returns
promptly with the operation's `op_id`, `state`, and `snapshot_digest`.
`get_story_refresh` returns the owner's current operation (`none`, `running`,
`succeeded`, or `failed`) with the observed `stories` or a safe `diagnostic`.
Clients provide neither source blobs nor a filesystem path the server already
knows.

The control plane owns admission and completion in memory, keeping one current
operation per project owner and bound workspace identity without writing anything
to disk. It captures a full module snapshot, uses the existing snapshot digest
to reuse a `running` or `succeeded` operation for an unchanged snapshot, and
cancels and replaces a prior operation when the snapshot changes or after a
`failed` run. Completion publishes only for its own operation and workspace; a
replaced or late evaluation publishes nothing. Because the operation runs in
the server process on a server-owned context, status and results remain
readable after a client disconnect while the server stays up. Before any
refresh or after a server restart, `get_story_refresh` returns `state: "none"`
as an expected absence and instructs the caller to run `refresh_story`. Server
shutdown cancels and awaits in-flight evaluations and clears interrupted
running entries before closing the evaluator, so an interrupted evaluation is
never presented as a current Story.

Story-only input must be evaluable before the application has services or C4
elements. Existing architecture preview and difference callers keep their
current contracts. Story support is imported into an evaluation harness only
when that target actually loads the Story module; targets without it retain
their existing dependency graph. The harness continues checking the target's
selected dependency versions instead of upgrading them for its own needs.

### Existing generator integration

The portable module supplies Story syntax and expressions, while
`internal/designeval` (`Evaluator.GenerateWithStories`) supplies evaluator
scenario generation. Saved typed Stories (`[]StorySummary`) reach
`Evaluator.GenerateWithStories(ctx, files, stories)` as server-owned input;
the evaluator module's own design is neither edited nor made to import the
candidate's Story. `Evaluator.Generate(ctx, files)` and all its callers
(`generate_code`, `surla gen`, `scaffold`, and `init_project`) remain
unchanged and never receive Stories. Only the authoring owner regenerates the
authoring module, always passing its frozen Story input to
`Evaluator.GenerateWithStories`, because running ordinary `surla gen` or
`generate_code` on that module would run `Evaluator.Generate` and drop the
flow scenarios.

One host-side mapping function converts `[]StorySummary` into the scenario
list rendered into the generation harness (with every string literal passed
through `strconv.Quote`) and into the returned `StoryGeneration` (`Files`,
`Included`, and `Excluded`):

- Each flow is identified by `(Story name, flow key)`.
- A flow is included when `Complete == true`; otherwise it is recorded in
  `StoryGeneration.Excluded` as `(Story name, flow key)` and omitted from the
  suite.
- Scenario names use Goa's `codegen.SnakeCase(flow.Key)`, hook methods use
  `codegen.Goify(scenarioName, true)`, and the scenario description uses
  `flow.Title` (or `flow.Key` when `flow.Title` is blank). For example,
  `refund-over-limit` becomes scenario `refund_over_limit` and hook method
  `RefundOverLimit`.
- Two included flows that map to the same scenario name or hook method (for
  example, the same flow key in two Stories, or `a-b` and `a_b`) are rejected
  on the host before compiling the harness, naming both source flows.

When at least one flow is included, the generation harness registers a Goa
evaluation root (`storyScenarioRoot`) that depends on Goa-AI's evaluation
suites root (`evalexpr.Root`). During `Prepare()`, before validation runs, it
appends `&evalexpr.ScenarioExpr{Name, Description, Suite}` to the single
declared suite (`evalexpr.Root.Suites[0]`). During `Validate()`, it rejects
designs declaring zero or multiple suites, naming the fix, while collisions
with manually declared scenarios in the suite are rejected by
`SuiteExpr.Validate()`. When no flow is finished, no scenario root is
registered, no suite-count rule is applied, and `StoryGeneration.Files`
equals `Evaluator.Generate(ctx, files)`'s output.

Generation only regenerates `gen/`: the suite's hook interface (`Hooks` in
`gen/evals/<suite>/suite.go`) gains one method per included flow, while the
starter owns the runner command at `cmd/<name>-evals/main.go`. No create-once
stub file is written. Until the author's hand-written hooks implement every
method, the evaluator module fails to compile naming the missing methods; that
compile failure is the unimplemented state. When a later revision removes or
renames a flow, its method leaves the generated `Hooks` interface and
`--describe`, while any old hand-written method on the concrete hooks type
still compiles and is unused.

## Proposed independent test-authoring operation

When the user asks to build the confirmed refund flow, the control plane
starts a separate test-authoring operation for that card revision. It gives
the author the saved observed Story, generated scaffolds, and the platform's
public contracts. The author implements the platform interactions and
assertions. The control plane accepts the resulting suite through the
authoring operation it started, then uses that suite for verification of the
same revision.

This operation is required because the existing verification resolver cannot
accept newly authored tests without a restart, and a generic "select this
suite commit" tool would give the builder control over its grader. The
builder-facing operation requests authoring; it does not submit a repository,
suite commit, author identity, credential, or acceptance claim.

### Execution and authority

The control plane owns authoring requests and accepted suite selection. A
dedicated package owns the authoring operation's on-disk format and process
lifecycle. It reuses existing process-group and sandbox primitives, rather
than generalizing Checkpoint 4's verification request into an arbitrary-job
framework. Accepted work completes server-side and is readable after the
requesting client disconnects.

A test-author execution adapter creates a fresh agent execution, grants it
access to the evaluator-owned workspace, and returns completion through a
trusted runtime connection. Agent identity and assignment are runtime facts,
never fields chosen by the model. The builder's ordinary MCP session cannot
call the completion path or choose the accepted commit. The operator remains
trusted; the protection concerns agents and candidate code running under the
operator's configured execution permissions.

Managed and user-attached authors implement this same assignment and
completion contract. A managed adapter starts the author automatically from
an available model connection. An attached adapter gives an authenticated
external agent the same scoped assignment. Neither adapter may turn an
ordinary builder credential into test-submission authority. No roster is
required at startup; connection or trust choices appear when authoring first
needs them, and the running server acquires the capability without restart.

An adapter must enforce the authoring workspace's write ownership and any
hidden-test isolation it claims. Separate role names, prompt instructions,
or two sessions sharing unrestricted filesystem access do not establish
these properties. Such a host must offer an isolated authoring connection
before this new independent-authoring capability is available. Existing
operator-configured verification remains usable; its guarantees are not
renamed or weakened to make the new laptop flow appear complete.

### Scoped worker sessions

The execution contract gives each worker a fresh tool session constructed by
the runtime for one admitted assignment. Tool executors capture the assignment
and its permitted filesystem root; the model never repeats its identity,
assignment, suite commit, or authorization proof. The ordinary builder MCP
connection has none of the author-session executors.

An author session exposes only reading its frozen input and public platform
contracts; listing, reading, and changing files in its suite tree; requesting
sandboxed checks; and finishing its attempt. Filesystem operations use an
owner-opened `os.Root`, not a caller-supplied root path. Check execution uses
the existing sandbox and does not grant the model a general host shell or
unrestricted network access. The authoring owner reads the resulting worktree,
creates the immutable commit, builds and inspects its suite, and decides
acceptance. A worker's "finished" message is a completion signal, not evidence
that the tests passed or authority to select a commit.

| Adapter | Machine identity and tool delivery | Model access |
| --- | --- | --- |
| Managed | The server creates a fresh Goa-AI agent run and registers only the assignment's scoped toolset. Runtime context binds that run to its admitted attempt. | The composition root supplies a configured model-gateway client; raw provider credentials stay with that gateway. |
| User-attached | An authenticated execution host opens a dedicated worker connection. The server binds that connection to an admitted assignment and serves the same scoped toolset. Worker identity comes from that authenticated host connection, never an agent name or model field. | The execution host owns the worker's model connection. It must support the scoped-session contract rather than inherit the builder's unrestricted tool environment. |

The attached-worker connection is a new generated transport contract, not
another builder-facing submit tool. Its fixed session context supplies the
assignment to every call. The operator connects an execution host through its
machine authentication when that capability is first needed; the server
verifies that connection and exposes its capabilities without restart or an
upfront agent roster. Connection setup belongs to an operator-owned control
API, separate from the builder's tool credentials. A builder cannot register
its own author host or change the managed author's model connection. The
host-control transport authenticates the machine connection and binds the
worker session in code; a supplied principal string is not authentication.
An ordinary subagent on the builder's shared MCP connection does not satisfy this
contract. The first connector must be implemented against a host that can
enforce it; compatibility with arbitrary existing agent CLIs is not promised.

These permissions must also constrain any builder for which Surla claims
hidden evaluator files were inaccessible. Managed builders receive their own
candidate-only file tools and run their candidate checks through the sandbox.
An attached host must enforce equivalent permissions. An unrestricted agent
with a shell under the operator's account is outside that guarantee: hiding
tools from its MCP connection does not revoke its separate filesystem access.
Directory ownership alone cannot identify or confine an agent. Surla must
explain a missing execution capability and offer a supported managed or
attached host. A role label cannot supply the missing permission enforcement.

The current `configure_model` operation controls environments for future live
targets only. It is not an existing factory-agent launcher. The managed agent
runtime, model-gateway binding, attached-worker transport and machine
authentication, scoped file tools, and capability connection operation are
new implementation work. The guide can continue Story and architecture work
before this capability is connected; it asks for the missing host/model
connection only when execution needs it.

### Inputs, completion, and later edits

The proposed builder-facing operations are:

| Operation | Caller supplies | Server action and result |
| --- | --- | --- |
| Request test authoring | Change ID and confirmed request revision | Authorize the card owner, freeze that revision's observed Story and finished-flow set, start independent authoring, return operation status. |
| Read test authoring | Operation ID | Authorize the operation owner and return state, actionable failure, or accepted suite summary without hidden tests. |
| Verify the change | Existing change ID, request revision, and candidate commit | Keep a configured operator suite; otherwise resolve the accepted authoring attempt for a Story-bearing request. Selection comes from server configuration, never the builder. |

The authoring owner stores one immutable input for each confirmed request
revision: its exact observed Story, included finished flows, and excluded
drafts. Attempts refer to that input instead of copying it on each retry.
Keeping the original input is necessary because a change keeps only its
revision in force and at most one waiting proposal: a guide's revision of an
agreed change waits as a proposal, but adopting it, or refining the agreed
revision, replaces the earlier agreed revision. Deleting the input would make
history depend on current requirements.
The request revision already includes the card ID and owner. No per-flow
fingerprint or scenario-tag protocol is needed.

The authoring owner accepts a result only from the execution admitted for that
operation. It builds the suite from an immutable commit and checks its
declared scenarios against the required finished-flow set. Each flow maps to
the existing evaluator's scenario ID and hook method. Labels and reordering
preserve identity; naming collisions are rejected without restricting
unrelated manually authored scenario IDs. A missing required scenario or an
unimplemented scaffold cannot pass.

### Selection and concurrent attempts

The authoring package owns attempt selection for each request revision.
Selection is not another card field: workspace-derived Story verification
asks the authoring owner for the selected attempt of its exact revision. This
removes
a second write in which an authoring result would also need to update the
mutable card, and lets completion finish without a callback to a still-running
control plane.

Admission checks the current confirmed card under the existing control-plane
lock, then creates or reads its immutable authoring input. A request while
authoring is active returns that active attempt. A new request after a terminal
result starts a new attempt and explicitly selects it before work begins.
The user can therefore ask for tests to be revised without changing the
Story or deleting previous work. Starting that attempt means new verification
waits for its result; the old passing suite is not silently used meanwhile.

The authoring owner serializes admission and saves the selected attempt and
its immutable request in its protected directory before launch. Its supervisor
holds the operation lock and publishes one terminal result. Process death or
an incomplete launch is recorded as interrupted, never accepted as a passing
suite. Admission returns only after the supervisor has accepted the work.
The worker can complete only its own attempt; completion never changes which
attempt is selected. An older completion therefore cannot replace a newer
selection, even after restart.

Completed attempts remain historical records. A later Story revision resolves
only its own authoring resource, so completion for an older revision cannot
select its suite. Workspace-derived Story verification keeps the immutable
accepted attempt it started with. New verification and approval through that
selection require the selected attempt to have accepted the named suite;
promotion-owner checks remain additional requirements. Failures retain
diagnostics and do not select the original workspace suite as a fallback.
An explicitly configured operator suite does not use this selection or acquire
an authoring reference merely because the card contains a Story.

A Story edit creates a new request revision. Workspace-derived verification
requires authoring for that revision before it can be verified as current.
An operator-configured suite remains selected and must cover the new build's
finished-flow set. Authored-suite selection deliberately uses the existing
whole-request identity. Reusing tests across superficially similar
flows would require another semantic comparison policy and is not part of
the first implementation. Earlier reports remain valid evidence for their
recorded inputs, with their excluded drafts still visible.

## Verification, history, and preserved callers

Suite selection belongs to the control plane's verification plan, chosen
for each request. With stored verification settings (`Options.Verification`),
every change verifies with the operator's evaluator. Without them
(`Options.WrittenTestRuns`), a change verifies with the tests Surla accepted
for its build, at the commit it recorded, and is refused until they are
accepted. No builder-facing source flag or suite argument is added.

Every Story-bearing verification request preserves its observed Story and
included/excluded flow set with its candidate, request revision, and suite
commit. A run using an authored suite additionally records the immutable
authoring-attempt reference; an operator-configured run has no such reference.
Its history must not imply that Surla launched an author for it. Coverage
checks compare the selected suite's declared scenarios with the finished-flow
set; they do not prove that its assertions implement the intended meaning.

The completed report shows those recorded inputs and the suite source.
Later `get_verification` and `list_verifications` reads do not load the current
card to reconstruct old requirements. Builder views still pass through
`Report.ForBuilders()`. Approval of an authored-suite run also checks the
currently selected accepted attempt. Approval of an operator-configured run
keeps the operator's suite selection without an authoring prerequisite. Both
retain the independent promotion owner's existing expected-input checks.

| Existing caller | Required preservation |
| --- | --- |
| Verification without a Story | Existing operator/workspace suite selection and manually authored scenarios continue working. |
| Verification with static operator configuration | Keep the configured suite and check finished-flow coverage. No authoring operation is required or fabricated; the builder receives no suite-selection authority. |
| Verification using newly authored Story tests | Require the accepted operation for the exact request revision; no fallback to the workspace's original evaluator commit. |
| `start_live_target` and `draft_override` | Keep their workspace-level live-target behavior. Do not guess a Change Card from whichever card was used most recently. |
| `approve_merge` | Keep the independent promotion owner's expected suite, environment, and artifact checks. Authoring never rewrites promotion configuration. |
| Historical run reads | Return the run's original requirements, included flows, excluded drafts, suite, candidate, environment, and outcome after later edits or restart. |

Testing an already deployed environment is a separate execution target, not
part of deploying a candidate. The user supplies the running copy; Surla
neither provisions it nor runs Terraform planning, apply, or deployment. A
simulation result does not establish a pass against that address. Safe test
side effects, access to that environment's API, and outside-service behavior
need their own design before this execution target is implemented.

## Delivery and acceptance

The first increment (shipped) delivers the portable Story language
(`github.com/goadesign/surla/story`), synchronous sandboxed typed
observation in `internal/designeval` (`Evaluator.Evaluate`) with
legacy-target dependency preservation, in-memory background Story refresh in
`internal/controlplane` (`refresh_story` and `get_story_refresh`), and
adopting the observed Story into the Change Card's saved `request_revision`
(`story_refresh_op_id` on `propose_change` and `revise_change`). A Go-only
Story with a finished refund flow and an unfinished claim evaluates; both
remain visible, and only the refund is eligible for the build. Invalid actor
references and duplicate domain keys fail with actionable errors.

The second increment (shipped in `internal/designeval` via
`Evaluator.GenerateWithStories`, with no production caller until the third
increment) connects server-owned Story summaries to the existing evaluator
generator. Finished flows attach to the single declared suite during
preparation and add their methods to the generated `Hooks` interface in
`gen/evals/<suite>/suite.go`, while incomplete drafts are reported in
`StoryGeneration.Excluded`. No stub files are written; unchanged hand-written
hooks fail to compile naming the missing methods until implemented, and
existing hand-written methods remain untouched across regenerations. Existing
manually named scenarios and non-Story generation (`Evaluator.Generate`)
continue working; cross-Story flow collisions, suite-count errors, and
combined-suite naming collisions fail with actionable errors. This increment
is scaffold generation, not independent test authorship or verified behavior.

The third increment implements durable independent authoring, the two
execution adapters, and exact-revision suite acceptance. Acceptance must
prove that builder credentials cannot submit results, separate changes and
concurrent operations cannot select each other's suites, stale completion
cannot publish to a newer revision, accepted work survives disconnect, and
restart preserves both successful and failed operation history. Both adapters
must exercise the same session contract: builder credentials cannot register
an author host, open an author session, or submit results; a worker cannot
select another assignment, escape its filesystem root, or choose the accepted
commit. Unsupported host permissions fail before a claim of hidden-test
isolation. A declared capability or mocked transport does not prove that a
real host enforces it.

Preservation checks must include an operator-configured suite that declares
`refund_over_limit` and covers a confirmed refund Story: verification keeps
that suite without starting an author or inventing an authoring reference.
Removing its required refund scenario must fail coverage rather than replace
the operator's suite. The equivalent workspace-derived request must require
its selected accepted authoring attempt instead, including an automatic
starter with both a startup verification configuration and a bound resolver.

The final increment exposes timeline editing and proof/history through MCP
and the views page, and verifies the full first-time journey and backward
loops. The views page is a standalone page the Surla server serves and that
calls the server directly; showing the timeline on it and a timeline-to-guide
editing handoff remain to build. A prototype export does not prove those
interactions work. Deterministic checks must not be described as evidence
from real model execution or agent interaction.

### Review and verification limits

The implementation must preserve scoped-worker and authoring ownership, keep
Story-only input usable before architecture exists, and retain legacy
verification behavior. Managed model execution and attached-host permission
enforcement require integration checks against real adapters; mocked
transports alone do not establish those properties.
