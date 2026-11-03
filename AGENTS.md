# Surla Agent Guide

Surla is a Goa and goa-ai control plane that takes a feature request through
intent (CP1), architecture preview (CP2), live prompt drafts (CP3), sandboxed
verification (CP4), and promotion checks (CP5). Make the smallest correct
change: strong contracts, clear ownership, and less code. Delete code when
every caller still gets the behavior it needs.

These instructions apply throughout this repository; a nested `AGENTS.md`
specializes its directory. Follow the user's authorized goal and applicable
safety constraints. Explain a material conflict before editing.

Use [README.md](README.md) for the product and entry points,
[the reference](docs/REFERENCE.md) for commands and contracts, and
[the architecture playbook](docs/ARCHITECTURE_PLAYBOOK.md) when changing
service boundaries. Read the parts relevant to the task, not a stack of
documents before a mechanical correction.

This repository is public. Never mention private projects, customers,
internal services, private repositories, credentials, or telemetry in code,
comments, tests, fixtures, docs, commits, branches, issues, or pull requests.

## 1. How to work

1. **Read before you edit or propose.** Search code instead of guessing,
   including in answers with no edits. Summaries, trackers, and PR/peer notes
   are progress pointers, never architectural truth. Read owning files
   (`main.go`, `design/`, and service interfaces) before architectural or UX
   assessments and behavior proposals. Fix the cause, not the symptom's file.
2. **Trace the whole flow.** Follow a concrete MCP, HTTP, or CLI request
   through its owners to the result; inventory affected callers on
   `origin/main` as required by the design gate below. Walk the first-time
   user, operator, and agent journey from install and startup through every
   step and backward loop before designing a command, API, MCP tool, or UI—
   never design outward from an internal digest, RPC, or file format. Derive
   setup and inputs from known workspace and credential facts so the first
   run asks only for genuine trust or target choices, each off capability
   names the one step that enables it, backward loops state what stays valid
   and what needs re-confirmation without silently carrying stale decisions
   forward, and system-spawned and user-attached agents share one execution
   boundary.
3. **Explain plans for larger changes.** Before changing three or more files,
   state the intended outcome, the steps, and the main risks, then proceed.
4. **Answer before acting.** Reply to questions before edits or mutations.
   When questioned about a proposal, falsify its premise against source and
   ownership rules instead of defending an earlier answer.
5. **Communicate honestly at meaningful points.** At task start, milestone
   completion, or a material finding, state the result, evidence, uncertainty,
   and next step. Disclose failed/skipped checks; explain mistakes before
   repairing them. Track outcomes with statuses updated in place and evidence
   linked once, not a command diary.
6. **Run tools non-interactively and bounded.** Set `CI=true` and close stdin
   for package runners and CLIs (`node`, `npx`, `gh`). Bound long waits by a
   wall-clock deadline; exit on success, failure, unexpected state, or timeout.
   `gh pr checks --watch --fail-fast` and `gh run watch --exit-status` still
   need deadlines for stalled/queued jobs. Before yielding with background
   work running, arrange a bounded wake-up check.
7. **Finish authorized work.** Carry the change through implementation,
   verification, and authorized publication without asking at each step.
   Ask only for missing facts or material user decisions; do not turn an
   implementation gap into a preference or expand to unrelated fixes.
8. **Reuse evidence; batch independent reads.** Prefer `rg` and direct tools.
   Inspect every parallel result; keep dependent actions and mutations
   sequential. Repeat checks only for incomplete evidence, changed state,
   or a consequential action requiring fresh proof. Freeze test/review inputs
   and disclose later changes.
9. **Preserve diagnostic evidence.** Before parsing live probes or costly
   failures, save bounded stdout/stderr, including partial failure/timeout
   output, in private local files. Record the credential-free command, target,
   timestamps, exit status, byte counts, truncation, and capture errors. Share
   only redacted evidence. Fix a failed reader and reuse the capture. Neither
   non-empty stderr nor exit zero alone proves the requested outcome.

In replies, comments, docs, and PRs, state the input, action, and caller's
result before naming an abstraction. For example:

- Write: "`get_change` reads the saved card file for the caller's
  `change_id` and returns it only to the principal who created it."
- Not: "Card hydration enforces owner-scoped projection."

Define Surla terms (Change Card, `request_revision`, holdout, run directory)
on first use; avoid session shorthand. Give an example when a label could
hide accepted input, a decision, or changed output. Before asking a design
question, trace who supplies, saves, and uses each value and what returns.
Separate current behavior, unfinished work, and proposals; explain the
choice evidence cannot settle and its consequences. If unclear, revisit the
premise rather than renaming options.

## 2. Design gate for every non-mechanical change

Fix mechanical issues directly: formatting, declaration order, a missing
test of existing behavior, or a doc that no longer matches the code. Every
other change (a Goa design, an auth or trust rule, an error contract, a
lifecycle, storage, or a new flag, field, or dependency)—and every
architectural or UX assessment or proposal in prose—must pass this gate,
stated in writing before the first edit or recommendation:

1. **Current behavior and callers.** Give an example and inventory every
   affected caller on `origin/main`, tracing entry, downstream calls, storage,
   and result. Prove this change causes the requested outcome; report
   unrelated findings without expanding scope.
2. **Before and after contract.** What becomes required, rejected, removed,
   or unchanged, and whether the contract becomes stronger or weaker.
   Optional fields, modes, presence-dependent behavior, and wider enums
   weaken a contract; two honest functions beat one function with a mode.
3. **Owning layer.** Identify the owner of the concern, state, Goa design,
   and generated client. Check composition roots, clients, and relevant
   dependency contracts before moving work. Apply Section 3 entry validation.
   Internal call shapes, old tests, framework constants, and transport hops
   show an implementation, not automatically product requirements.
   Consult Git/PR history before assigning meaning to absent values, empty
   results, defaults, or degraded behavior.
4. **Delete first.** Ask which caller breaks if the state, copy, hop, or
   check that causes the problem is deleted outright. Compare the result with
   a from-scratch design. Reject a proposal that adds any of these:
   - limits, eviction, or expiry on retained in-memory state;
   - in-process caches or overflow stores;
   - duplicate copies of the same data;
   - filters that hide truthful errors, spans, or payloads;
   - repeated checks, wrappers, or mode flags inside an already validated
     flow.
5. **Journey proof.** For every user-, operator-, or agent-facing CLI, API,
   MCP tool, workflow, or UI change, state the input, owner action, visible
   result, backward loop, failure recovery, and preserved valid cases before
   choosing endpoints or views. When a change alters onboarding or multi-step
   loops, verify it with `surla-ux-review` or an equivalent end-to-end
   walkthrough and record the findings.
6. **Effects and proof.** State user-visible behavior, compatibility, risk,
   and verification, including persisted data, deployment order, mixed
   versions, and rollback when relevant. Re-run the gate for a newly involved
   owner, external behavior, migration, or public contract.

For each addition, ask: "If omitted, which required case fails, and why can't
the existing owner handle it?" Compare deletion, correcting the owner, and a
separate mechanism, including storage, API, caller, migration, and cleanup
costs. Imagined future features do not establish necessity. Map each limit to
its protected decision/resource and narrowest invariant lifetime; test a valid
case at the next wider lifetime rather than making a per-call fact run-wide.

When an evaluator, UI, alert, or caller reaches a wrong conclusion, inspect
every existing payload/span field: is the fact false, or is the consumer's
rule wrong? Never change a true fact to steer a consumer.

### Preserve existing capabilities

Before removing, hiding, renaming, or materially changing a documented,
configured, persisted, or caller-visible behavior, identify the user request
or product contract that requires it. A broken presentation does not prove
the capability is unwanted. Inspect documented and configured callers,
actual execution evidence when available, and a valid case outside the
failure. Record their before/after outcomes and add positive preservation
tests, not only tests of rejection. An evaluation forbidding one exact call
does not authorize removing that tool for every user.
Include affected profiles/configurations, interactive and background flows,
seeded consumers, and direct callers when they exist.

Migrate every caller in the same change or verify its migration before
removing the old behavior. Keep limit, routing, error, and capability changes
separate where possible. Check values below, at, and above numeric limits,
including units and inclusive/exclusive boundaries. Stop a rollout on new
rejections, missing data, changed routing, or material latency regressions.

When you remove or replace a producer (a tool, event, enum value, API method,
or CLI flag), search the repository for anything that still consumes its
output, and delete each piece that no longer has a producer (types,
handlers, UI views, flags, docs, and test fixtures) in the same change.
Include saved state variants, continuations, generated clients, and SDK hooks.
A helper must enforce an invariant, own a contract, remove meaningful
duplication, or add useful domain language; do not wrap a lookup, literal,
field access, or pass-through value merely to name it.

### Reviews

Before finalizing a design-touching change or pull request, run a review by
a fresh read-only subagent that has not seen your reasoning. Give it the
problem, the owning contracts, the callers, and the diff, and tell it to
assume the answer to "Is this the simplest, most elegant from-scratch design
given every flow on `origin/main`?" is "No" until it has checked every caller
and tried net deletion. Give it the original requested outcome, not only an
expanded plan. When a review finds a high-priority or design-touching issue,
fix it and run another fresh review until none remain. Explain findings
against the owning contract. Before merging, check every review comment
against the current diff; fix confirmed issues and resolve or explain the
rest. A new head must not inherit approval for unreviewed changes.

Before delegating, explain the task and purpose to the user. Set each
subagent's model and reasoning level explicitly: Sol 6.1, Opus 5.5, or
Gemini 4 with high reasoning / `pro` for implementation and review; Luna 6,
Haiku, or Gemini 3.8 (`flash` / `flash_lite`) only for mechanical read-only
status checks that return raw failure evidence without diagnosis or writes.
Use Astra 6 only when requested. Give unrelated work a fresh agent. Assign
write ownership and tell agents to preserve others' edits. Explain important
results yourself; the user may not see the subagent's work.

Before resolving a merge or rebase conflict, read both sides' PR descriptions,
discussion, commits, and linked decisions. State each intended outcome and
preserve compatible requirements; never choose an entire side wholesale.
Stop if that context is unavailable or contradicts the code.

## 3. Ownership and state

- **One owner per durable state.** Replicas of one service may share its
  private storage, and two distinct services never read or write the same
  files, tables, or keys. One package owns each on-disk format and the only
  code that reads or writes it: `internal/controlplane` owns Change Card
  files, and `internal/verifyrun` owns verification run directories, which
  `surla serve` and the `surla verify-run` processes it starts share through
  that package.
  Shared packages may contain value types, codecs, and pure algorithms, not
  persistence that lets distinct services access each other's data. Before
  adding a registry or handle store, identify its owner, consumers, existing
  API, and whether its values can be derived without storage. Prefer deriving
  facts from authorized IDs, then calling the owner's generated API, then
  private storage behind that API, or an owned event for asynchronous use.
- **Ask the owner.** Get a running service's facts through its API, not by
  copying them into configuration. A host that embeds the control plane binds
  `TargetUpdater` or `PromotionTarget`; Surla never fabricates the facts those
  interfaces report.
- **Complete work server-side.** A multi-step operation, such as a
  verification run, completes in a process the server owns and survives
  client disconnects. Once accepted, multi-service or multi-transaction work
  completes through durable execution owned by the initiating service, not
  client retries, except a step requiring a fresh credential held by the
  client. Retry-specific authorization or a historical record used to bypass
  entry checks is a reason to re-examine completion ownership.
- **Finish migrations.** Give every compatibility path a stated removal
  condition and final contract, and delete it after verified cutover,
  including obsolete schemas, flags, adapters, tests, and docs. Do not leave
  permanent dual reads/writes or fallback modes to avoid completing a change.
- **Validate and authorize at the entry.** Put structural validation in Goa
  design for HTTP/MCP and reject unknown fields and trailing data when
  decoding stored files. Trust constructed values internally. Check authentication,
  resource access, storage loads, events, third-party results, `ctx.Value`,
  type assertions, and required lookups at their owning entry points. Do not
  infer authorization from role or privilege names or repeat authorization
  inside an already authorized workflow loop. Normalize strings only at an
  external boundary where whitespace is insignificant.
  Fail fast if an internally guaranteed value is missing; do not conceal
  violated invariants with guards, defaults, or silent recovery.
- **Use the owning transport.** Internal service calls use generated typed
  clients and gRPC/Protobuf contracts. Reserve JSON for external/browser or
  model interfaces, package-owned files, and established workflow/session
  persistence. When a call moves inside a trusted flow, remove serialization, repeated
  size checks, and other work that belonged only to the old external hop.
  Domain APIs describe resources and operations; the UI owns their layout.
- **Verify external behavior.** Before encoding a deployed external system's
  behavior, compare its public/generated contract, actual target settings,
  and observed execution evidence. Defaults, comments, and one passing test
  do not prove live behavior or a retention limit. Report inaccessible
  evidence and stop on disagreement instead of inventing a fallback.
- **Keep deployment ownership clear.** Deployment resources and settings
  belong with the service that owns them. Do not copy running-service facts
  into unrelated configuration repositories or create another service's
  resource overrides to hide an ownership problem.

## 4. Model-facing tools and prompts

- **Decide mechanically in code.** Values derivable from authentication,
  generated contracts, typed results, or run state (identity, ownership,
  correlation, pagination, valid transitions) belong to code, schemas, and
  goa-ai `Inject`, never to the model. Ask the model only for semantic
  decisions, such as interpreting the user's request.
  Derive from typed results and runtime state first; use generated metadata
  for facts fixed per tool. Keep language interpretation, relevance, and
  tradeoffs model-driven unless the domain really has a closed vocabulary.
  Do not replace open intent with keyword rules, exhaustive modes, or a
  planner decision tree just to make an evaluation pass.
- **Treat every tool description as a prompt.** Describe only that tool's
  contract, never internal orchestration or tools the model cannot see. When
  a result returns an ID another call needs, name that field.
  Author against the rendered prompt, visible schemas, prior results, and
  actual next calls. Specialize facts known during generation or rendering;
  keep conditionals for runtime facts. Include an instruction only when it
  changes a valid action, argument, or stop condition. Cross-tool routing
  belongs in the caller's prompt; tool-only prompts produce tool calls.
- **Never repair model output with heuristics.** Validate model output where
  it enters code and reject what violates the contract.
  Before changing prompts, tool contracts, or routing to fix model behavior,
  reconstruct the visible messages, calls, arguments, and results through
  the first wrong decision. Trace claims such as "truncated", "failed", or
  "timed out" to their raw input and stop
  condition, including child calls. Call ordering or count alone is not a
  correctness requirement.
- **Keep execution context out of model choices.** Do not ask the model to
  repeat authenticated identity, opaque proofs, cursors, or prior result
  handles that code can derive or correlate. Keep stable resource IDs visible
  when the model must choose among resources. `Inject` is for required string
  context fixed at run start, not changing result history. A cross-tool value
  needs an explicit producer, consumer, correlation, ordering, replay, and
  parallel-call contract; never pick a heuristic "latest" result. Preserve
  the model's original arguments separately from values code supplies.
- **Review the whole tool change.** Before adding a model control, trace its
  schema, generated codec, executor, correction path, prompt, UI, persistence,
  and tests. Do not add a model-visible protocol to compensate for a missing
  service API or ambiguous result type.
- **Bound work at the resource it protects.** A limit names the exact
  resource and lifetime it guards: goa-ai `BoundedResult` pages bound one
  model-visible tool result, and an operator's verification `timeout` bounds
  one CP4 run. Apply a limit before the expensive read, never by fetching
  everything and truncating; a bounded result states what it covers and
  whether more remains.
  Use the existing generated continuation mechanism. Code owns cursors and
  progress for each independent call chain; the model chooses whether more
  evidence is useful. Each continuation makes bounded progress or explains
  its blocker. Do not add unbounded pagination or identical retries. Keep
  counts and summaries separate from detail pages, and claim a full-range
  result only when every required source range completed. Empty or partial
  evidence is not proof of zero matches or complete coverage.
- **Keep holdouts hidden.** Builder-facing output goes through
  `Report.ForBuilders()`; never expose holdout scenario names, checks, or
  logs to a coding agent.
- **Treat candidate code as untrusted.** Build and run a candidate only
  through `verify/candidate`, which jails each process in an OS sandbox.
  Never run candidate binaries or its test suite outside it.

## 5. Code

### Go

- Use the toolchain declared by the module's `go.mod`; do not import another
  repository's version policy. Format with `go fmt ./...`. Separate
  standard-library imports from others.
  Alias every generated package with a `gen` prefix (`gensurla`,
  `genorders`, `genordersclient`).
- Use `lower_snake_case.go` files under 1000 lines, `any` instead of
  `interface{}`, `%w` wrapping with `errors.Is` and `errors.As`, and
  `len(x) == 0` without nil checks. Never discard an error with `_ =`.
- Order declarations: types, consts, vars, public funcs, public methods,
  private funcs, private methods.
  Keep main logic before helpers; prefer named helpers for non-trivial
  concurrent flows. Use short lowercase package names, combined adjacent
  same-type parameters, multi-line blocks and trailing-comma struct literals,
  and `min`, `max`, and `clear`. Keep one-line signatures within 100 columns;
  unused parameters are only for interface satisfaction.
- `main` is the composition root: it reads flags and environment, builds
  clients, and passes them in. Library packages never read addresses or
  environment variables to build clients.
- Comment every declaration that is not trivial, stating its contract, its
  callers, and what its errors mean.
  Include non-trivial fields and private helpers, exported ownership and
  lifetime requirements, and phase transitions in multi-step flows. Re-read
  each changed file end to end before finishing.

### Goa and generated code

- Never edit generated files by hand: `gen/`, `internal/targetprompt/gen/`,
  `ui/checkpoint/src/types.ts`,
  `internal/controlplane/checkpoint.html`, and
  `internal/goldenpath/decision_catalog.md`.
- After changing `design/`, run the module-pinned generator:
  `CI=true go run goa.design/goa/v3/cmd/goa gen github.com/goadesign/surla/design </dev/null`.
  After changing `design/targetprompt/`, run
  `CI=true go run goa.design/goa/v3/cmd/goa gen github.com/goadesign/surla/design/targetprompt -o internal/targetprompt </dev/null`.
  After changing `design/` or `ui/checkpoint/src/`, rebuild with
  `CI=true node ui/checkpoint/build.mjs </dev/null`; check that nothing is
  stale with `CI=true node ui/checkpoint/build.mjs --check </dev/null`.
- The examples under `examples/` are separate Go modules with their own
  `design/` and `gen/`; run that module's pinned generator from its directory
  using its design import path. Do not edit generated files to repair a type
  mismatch or maintain duplicate transport types.
- Service and method descriptions explain the domain role, the flow that
  calls the method, and what it returns.
- Return expected absence as a value; reserve `not_found` for a required
  resource that failed to load.
- Never use nil versus empty collections to mean different operations, or
  make a required field optional merely to repair one caller. State required
  fields, enums, formats, and lengths in the design; hand-written validation
  is for cross-field rules, authorization, storage loads, model semantics,
  security checks, or documented generator gaps. Keep raw tool schemas and
  payloads in their generated/raw JSON representation until their owning
  codec needs to decode them, rather than adding duplicate decoded maps.
  Use generated codecs and field metadata; do not walk JSON Schema yourself.

### Observability

Use OpenTelemetry spans for meaningful operations, preserving parent/child
links, bounded attributes, and events for state changes/results. Record a
failure with both its error and error status; expected negative results are
not failures. Use spans rather than application logs or custom metrics.
Never filter truthful spans to quiet an alert; fix its predicate or resource
lifecycle. Missing telemetry means unknown; periodic observed-state spans
can distinguish a continuing condition. Infrastructure metrics remain
appropriate for CPU, memory, and restarts.

### Tests

- Write fast, deterministic, table-driven tests using `testify/assert`, and
  `testify/require` only when a test cannot continue.
- When a contract changes, update every affected test and fixture end to
  end. Never rewrite an assertion just to match new output.
  Search all affected Go/TypeScript tests and fixture callers; distinguish a
  regression from an intentionally replaced contract. Do not add dummy data
  merely to make tests compile.
- Tests write only inside `t.TempDir()`, never into the source tree.
- Parallelize independent tests with isolated mutable state; construct
  expensive immutable fixtures once per package. Keep concurrency bounded.
  Do not turn functional tests into GC, memory-sizing, or timing sweeps. Code-owned
  invariants get deterministic tests; model judgment gets focused model
  evaluations with valid counterexamples. Diagnose a failed evaluation from
  the actual visible calls/results before changing assertions, prompts, tools,
  or routing. Do not overfit one scenario or use keywords to replace judgment.
- Run the smallest test set that exercises the change, then the affected
  module's checks at the integration/merge gate. The checked-in
  [CI workflow](.github/workflows/ci.yml) is the command reference. From the
  repository root, use the applicable checks below; change into a named
  example module before running its commands:
  - `go test -race ./...`
  - `go test -race -tags e2e ./internal/verify`
  - `go test -race -count=1 -tags e2e ./cmd/surla` for `surla gen` and
    `surla init` generation
  - `(cd examples/orders && go test -race ./...)` when `examples/orders`
    changed
  - `(cd examples/orders-eval && go test -race -tags e2e ./acceptance)` when
    `examples/orders` or `examples/orders-eval` changed
  - `go test -race -count=1 -timeout 60m -tags scaffold ./internal/scaffold`
    for starter generation
  - `npm --prefix ui/checkpoint run typecheck` for viewer TypeScript changes
- Set `CI=true` and close stdin on these invocations. Do not regenerate or
  run runtime/browser suites for text-only instructions or documentation;
  verify accuracy, references, links, and applicable doc checks instead.
  Generated-consistency and other repository CI still run on the final PR
  head. Reuse completed results until changed inputs or a failure justify
  another run. Record exact packages, scenarios/tags, model settings,
  duration, skipped cases, and setup failures; never claim that a mocked or
  credential-blocked path was verified end to end. Do not change credentials
  or access policy to make an evaluation run.

### TypeScript (`ui/checkpoint/`)

Use explicit types that match the generated contracts. Avoid `any`, and use
`unknown` only for untyped external input that you narrow immediately. Design
for a first-time user, applying Google's UX principles:

- One clear primary action per view, and clarity before density.
- Visible state instead of hidden modes; IDs and digests go in expandable
  details, not headings.
- Never lose the user's unsaved edits on a conflict or refresh.
- Semantic, accessible (WCAG AA), keyboard-navigable components, with empty,
  loading, and error states designed as carefully as success.
- Reuse the current components, tokens, and generated client/types. Do not
  repair schema drift with casts, duplicate enums, type augmentation, or a
  second handwritten transport. Keep component and state ownership small and
  explicit. Check responsive behavior, focus, and long/error content; preserve
  the existing visual language for a scoped fix.

## 6. Documentation and pull requests

- When behavior changes, update every doc that describes it in the same
  change: `README.md`, `docs/HOW_IT_WORKS.md`, `docs/REFERENCE.md`,
  `docs/ARCHITECTURE_PLAYBOOK.md`, or the affected `examples/*/README.md`.
- Write docs in active voice using Google technical writing style. Name the
  exact actor, command, and file at each step. Describe the current state;
  never write changelogs, rollout diaries, or completed migration phases into
  docs. Replace outdated text in place and keep `#anchor` targets stable.
  Link the authoritative contract rather than copying it into several files.
  Keep user guidance appropriate to its audience and separate from private
  engineering/operations material. Read changed pages end to end. Record only
  verified reusable architectural insights, not local incident diaries or
  hypotheses presented as fact.
- A pull request description is standalone: the affected feature, the
  concrete failure, why it matters, and the resulting behavior, with a
  before-and-after example that says whether it came from a test or a real
  run. Omit routine lists of test commands and files.
  Explain why the change solves the problem before implementation details,
  including material ownership, behavior, compatibility, migration, or
  verification limits. Use `author-pull-request` when available, and check
  that the title and description make sense without the diff or chat. A
  follow-up PR must explain its own problem.
- Derive release notes from shipped commits/diffs since the prior release.
  Describe affected capabilities, the old problem and new behavior, and the
  required upgrade actions: regeneration, data migration, deployment order,
  mixed versions, and rollback where applicable. State when no action is
  needed and link the owning docs; do not announce unfinished behavior.

## 7. Git safety

- Keep your primary checkout clean on `origin/main`, and do each task on a
  branch from `origin/main` in a separate clone, for example
  `git clone --reference <primary-checkout> <remote> <task-clone>`. Never run
  two write-enabled agents, or concurrent Git commands that change state, in
  the same working tree; read-only reviewers may inspect it.
  Keep task clones under `~/src`, never temporary directories. Do not switch
  branches, reset, restore, clean, stash, or rewrite Git refs/indexes in the
  primary checkout. Stop and report unexpected primary modifications.
- Stage explicit paths only (never `git add .` or `git add -u`). Check
  `git diff --stat origin/main...HEAD` before pushing, opening a pull request,
  or merging. Never force-push.
- Never run `go clean -cache`. Explain why before adding a dependency.
- Use the authenticated `gh` CLI for GitHub API writes (pull requests,
  reviews, issues, comments, and releases), and Git for authorized branch
  pushes. Never add AI co-author trailers or assistant mentions to commits,
  pull requests, or comments.
  Publish only to authorized repositories and branches, preserve other
  contributors' authorship, and verify the user's authorship before handoff.
  Do not prefix branch names with model or assistant names. Use a body file
  for multi-line `gh` descriptions so real newlines and literal text survive.
- Before deleting a completed task clone, inspect the exact directory,
  nested repositories, `git status`, and commits not pushed to its upstream.
  Stop if anything is uncommitted, unpublished, or not owned by the task;
  never delete directories by a shared name prefix. Retained QA projects and
  diagnostic captures are not disposable merely because they sit beside task
  clones. After verified merge/cleanup, fast-forward the clean primary
  checkout without discarding local work.
- Before destructive filesystem, database, or cloud work, resolve the exact
  target read-only, explain impact and recovery, and confirm it is within
  the user's authorization. Use the smallest necessary data reset; preserve
  unrelated clusters, images, volumes, and services. Before a production
  write, migration, rollout, or traffic change, state the exact action,
  evidence, stop condition, and rollback. Keep authorized read-only probes
  scoped, bounded, and credential-safe; they do not authorize repairs or
  permission changes.
- When using `kubectl`, name an explicit `--context` and one resource kind
  per positional command. Do not repurpose `HOME`, `CODEX_HOME`, or shell
  reserved parameters as task variables; use descriptive task-specific names.
  Treat command text as code: quote literal data safely, especially backticks,
  dollar substitutions, and multi-line descriptions.
