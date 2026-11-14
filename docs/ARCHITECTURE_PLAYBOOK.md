# Golden Path Architecture & the Goa Design Suite

**[← Back to README](../README.md)** · **[← How Surla Works](HOW_IT_WORKS.md)** · **[CLI & MCP Reference →](REFERENCE.md)**

When coding agents build a platform from scratch without architectural guardrails, they default to fragile shortcuts: every service exposes ad-hoc HTTP JSON routes, every package calls LLM provider SDKs directly with no central rate limiting or cost tracking, `tenant_id` is passed inside LLM prompt arguments, and multi-step workflows run in fragile in-memory goroutines that lose state on deploy.

Surla embeds a single-source-of-truth **Golden Path Playbook** ([`internal/goldenpath/GOLDEN_PATH.md`](../internal/goldenpath/GOLDEN_PATH.md)) and serves it as an MCP resource (`surla://golden-path`) to every agent on the project's team. The guide, the coding agent the user talks to, reads it before it writes the analysis it sends with `propose_change` or `revise_change`. The same resource ends with the decision component catalog for that analysis's optional `ui_composition`.

This page has two readers. A platform author follows [the seven rules](#the-7-golden-path-rules) and [the tools behind them](#how-the-goa-design-suite-fits-together). A contributor to Surla finds [who owns what in Surla](#who-owns-what-in-surla).

---

## The 7 Golden Path Rules

### 1. Design Is the Single Source of Truth (`design/*.go` → `goa gen`)
Every service, method, payload, error contract, agent, toolset, MCP resource, and C4 container is declared in `design/*.go` first. Running `goa gen` deterministically generates transport codecs, OpenAPI specs, gRPC protobufs, workflow registrations, and `AGENTS_QUICKSTART.md` at **0 LLM tokens**. Files under `gen/` are never edited by hand.

### 2. Single HTTP/SSE/MCP Edge Gateway, Internal gRPC Services
- **One Edge Service**: Expose a single stateless HTTP/SSE/MCP edge service that terminates TLS, validates authentication tokens (`JWTAuth`, `OAuth2`, `APIKeyAuth`), enforces CORS, and streams real-time updates to clients.
- **Internal gRPC Topology**: All internal microservices, domain backends, and model gateways communicate over generated Goa `gRPC` clients and servers with deadline propagation and strict schema validation.

### 3. Central Stateless LLM Model Gateway
No domain service or agent worker holds raw LLM provider API keys or calls external provider endpoints directly:
- **Gateway Service**: Front all model calls with [`goa.design/goa-ai/features/model/gateway`](https://github.com/goadesign/goa-ai) (`RegisterGRPCServer` and `NewGRPCClient`) over `Complete`, `Stream`, and `CountTokens`.
- **Distributed Token Rate Limiting**: Wrap the gateway with `middleware.NewAdaptiveRateLimiter` backed by a [Pulse](https://github.com/goadesign/pulse) replicated map (`pulse/rmap`) so token-per-minute budgets and AIMD backpressure are coordinated across replicas.
- **Fail-Closed Usage Attribution**: Emit OpenTelemetry `gen_ai.*` spans via [Clue](https://github.com/goadesign/clue) and record `TokenUsage` on every call. Treat a call whose usage is missing or only partly reported as unknown, never as zero, so budget gates fail closed (the `examples/orders-eval` gateway counts such calls in `Usage.Unreported` and fails the budget check).

### 4. Security & Identity by Construction (`Inject`, `BindField`, `ServerData`)
Never trust an LLM to supply or preserve security principals, tenant IDs, or credentials:
- **Edge Extraction & `Inject`**: Extract caller identity from verified tokens at the HTTP/MCP edge, store it in a typed Go context, and declare `Inject("tenant_id", "user_id")` on tools so those fields are stripped from model-visible JSON schemas and populated server-side by `ToolInterceptor.Inject`.
- **Consumer-Side `BindField`**: When an agent consumes a broader internal service method, use `BindField` to narrow the model-facing input schema to only the arguments the LLM is allowed to choose.
- **Off-Wire `ServerData`**: Keep audit metadata, citations, and UI artifacts in `ServerData` so they never bloat model context windows (only `ServerData` explicitly bridged via `NativeImage()` is attached to the model turn).

### 5. Deterministic Service Toolsets (`BindTo`) vs. Agent-as-Tool (`Use(AgentToolset(...))`)
- **Service Toolsets (`BindTo`)**: Keep CRUD operations, state transitions, and external API calls as deterministic Goa service methods bound via `BindTo`.
- **Agent-as-Tool (`Use(AgentToolset(...))`)**: When a sub-task requires multi-step reasoning over noisy data, encapsulate it in a dedicated child `Agent` (`Export`) and consume it via `Use(AgentToolset(...))` so the child runs in its own isolated context window and returns a bounded summary to the parent.
- **Generated Agent Guardrails (`AGENTS_QUICKSTART.md`)**: `goa gen` automatically documents every tool's `BindTo` target, server-injected fields, `BoundedResult` pagination, `Confirmation` gates, and `specs/tool_schemas.json` (`input_binding`) in `AGENTS_QUICKSTART.md`.

### 6. Durable Temporal Workflows & Zero-Wait Human Suspension
- **Deterministic Replay**: Multi-step agent runs execute on the Goa-AI Temporal engine with a durable run log (`runtime/agent/runlog`; [`examples/orders`](../examples/orders/) stores runs in SQLite through `internal/runstore`), keeping workflow definitions pure and isolating I/O inside registered activities.
- **Zero-Wait Human Approvals**: High-consequence tools declare design-time `Confirmation(...)`. The run then suspends and saves its suspension, holding no worker thread, until an authorized principal decides through the public API, which resumes it by calling `PrepareContinuation` and then `StartPrepared` (as `examples/orders/internal/assistant` does).
- **Payload & Context Bounds**: Paginate collection tools with `BoundedResult`, configure `RunPolicy` (`MaxToolCalls`, `TimeBudget`, `Cache`, `History` `Compress`), and keep each tool and activity payload small: Goa-AI rejects any single encoded value that crosses one workflow-engine call above 1 MiB (`engine.MaxPayloadBytes`), well under [Temporal's 2 MB per-request payload limit](https://docs.temporal.io/evaluate/cloud/limits#transaction-payload-size-limit). Reduce large domain results to durable references before they reach that boundary.

### 7. Customizing Generated Artifacts via Goa Plugins (`codegen.RegisterPluginLast`)
When a platform needs custom generated files or wants to tailor artifacts produced by `goa` or `goa-ai`, register a Goa plugin via `codegen.RegisterPluginLast("my-plugin", "gen", nil, generate)` (`goa.design/goa/v3/codegen`). Your `GenerateFunc` receives the evaluated `eval.Root` roots and the complete `[]*codegen.File` list, allowing you to inspect or transform generated sections cleanly during `goa gen` without post-processing scripts.

---

## How the Goa Design Suite Fits Together

A Golden Path platform is built with these tools:

- **[Goa](https://github.com/goadesign/goa)** — Compiles `design/*.go` into HTTP, gRPC, OpenAPI, CLI, and validation packages (`goa gen`)—plus in-memory `generator.Sources` for sandboxed design evaluation—at **0 LLM tokens**.
- **[Goa-AI](https://github.com/goadesign/goa-ai)** — Adds `Agent`, `Toolset`, `MCP`, durable Temporal workflows, `eval` suites, gRPC model gateways, and runtime token controls (`Inject`, `BindField`, `BoundedResult`, `ServerData`, `Cache`, `Compress`).
- **[Pulse](https://github.com/goadesign/pulse)** — Streams live agent events (`pulse/streaming`) and coordinates distributed rate limits and replicated state maps (`pulse/rmap`) over Redis.
- **[Clue](https://github.com/goadesign/clue)** — Emits OpenTelemetry `gen_ai.*` spans, structured logs, and health checks across services and model gateways.
- **[Model](https://github.com/goadesign/model)** — Declares C4 architecture models (`SoftwareSystem`, `Container`, `Views`) alongside `design/*.go` and renders Structurizr JSON (`gen/structurizr/model.json`).

The team of coding agents that builds it runs on two more:

- **[Chorus](https://github.com/goadesign/chorus)** — Lets coding agents and people talk in real time through channels and direct messages, with `0`-token background waiting (`chorus watch`). Its dialog in Herdr is where the user picks the agent app for each role.
- **[Herdr](https://herdr.dev/)** — A terminal program that runs each agent in its own pane. Surla starts its server in a Herdr terminal and gives the project its own Herdr space.

---

## Who owns what in Surla

The `surla` command, run in a project folder, starts the project's Surla server (from here on, the server). The server serves the views page in the user's browser and one HTTP and MCP endpoint to the project's team of coding agents:

- **The guide** is the agent the user talks to. It writes the Story, the Go source in `design/` that describes the business's flows, and proposes changes. A flow is one path of who does what, from the event that starts it to its outcomes.
- **The builder** writes the application code and asks Surla to verify a commit.
- **The test writer** writes the tests a build requires, in its own working copy of the project.

Each change is saved as a Change Card: the record of the request, its open questions and the user's answers, the examples that become checks, the Story and the design source. Its `request_revision` identifies one exact saved revision. Only the user confirms a change, starts a build and approves a result, on the views page. One table, `operationRoles` in [`operation_policy.go`](../internal/controlplane/operation_policy.go), says which caller may call each operation; HTTP, MCP and the tool list each caller sees all follow it.

| Owner | Source | What it owns |
| --- | --- | --- |
| The `surla` command | `cmd/surla` | Finding the project's private state folder, starting the server in Herdr, starting Chorus and Herdr, the stored settings |
| The server | `internal/project` | The project's identity and port, credentials and browser sessions, the team record, the Chorus roster (the list of roles the Chorus dialog shows) and the team's observed state |
| The control plane | `internal/controlplane` | Change Cards, builds and assignments, the report checker, the verification plan, the live target and operation diagnostics |
| The test writer's working copy | `internal/testwriter` | The `.surla-tests` folder, the `surla/tests` branch, the required tests, the check of a report and the commit of accepted tests |
| Verification runs | `internal/verifyrun`, `internal/verify` | One folder per verification run (its run directory) and the processes that build, run and report it |
| Herdr | outside Surla | Terminals, panes and spaces |
| Chorus | outside Surla | Agent messaging and the dialog that starts each role's agent |

### The server and its state

A project is one physical folder. `surla` looks for the nearest folder that has a project record and otherwise creates a new, empty project, so moving or renaming the folder starts a new project. Each project has one private state folder under the user's configuration folder, outside the project. It holds the project record (its ID, folder, owning operating-system account, port, team name and Herdr space, and each registered agent's role and working folder), one credential file per agent, the Change Card folder, verification runs and reports, and the stored verification and promotion settings. A new project saves a free loopback port that no other project of the account saved, and keeps it, because its agents' connections and the Chorus roster name that address. When `surla` cannot use a project's record or state folder, it names the project folder, the path and the repair.

When the user runs `surla`:

1. A start lock lets one `surla` start the server at a time. The folder lock, an operating-system file lock on the Change Card folder that a running server holds for its whole life, is the only proof that a server is running.
2. When no server is running, `surla` starts one in its own parentless Herdr terminal, labelled `surla_project=<project ID>`, and passes the state folder with `-state`. The server never derives that folder itself, because a Herdr terminal runs with the environment of Herdr's core, not the user's shell.
3. After binding its port, the server publishes a fresh management credential in a private record. `surla` reads that credential for each request, confirms that the answering server names the same project, folder and owner, and that the same build of `surla` started it (the server reports the SHA-256 of its executable, read when it started), and only then continues. `surla stop` accepts a server of any build. Before the listener closes, the server revokes the credential and withdraws the record, even when accepting connections failed.
4. If the server's terminal ends before it answers, `surla` reads the server's last lines (at most 20 lines and 4 KiB), removes the terminal, and prints those lines with the error.

The management credential lets `surla` prepare the team, stop the server and ask for a browser link; it cannot confirm or approve anything. The browser gets a single-use link that expires after two minutes. The page exchanges it for a session that lives only in the server's memory and the open page, and the server answers that session only for requests from its own page. Each agent authenticates with its own credential file. The caller's role comes from the credential, never from a name.

`surla verification set` and `surla promotion set` validate a settings file and copy it into the state folder. The server reads both at start, so a change applies when Surla is started again.

### The team: Herdr and Chorus

Herdr is the terminal program the agents run in, one pane per agent. Chorus is the messaging service the agents use, and its dialog in Herdr is where the user picks which installed agent app plays each role.

Before creating any state, `surla` checks that Herdr core (`herdr-core` and `hc`), the Herdr app (`herdr-app`), Chorus v0.11.0 or newer (`chorus`) and the browser opener are installed. For a missing Herdr or Chorus program it names the install command, or the folder to add to the path when the program is in `~/.local/bin`; for a missing browser opener it names the program. Once the server answers, `surla` starts Chorus and the Herdr app when they are not running, then asks the server to prepare the team. When either step fails, `surla` still opens the views page, where confirming and approving need no agent, and then reports the failure. A failure of the server's team preparation names only its cause (`stopping`, `herdr`, `roster_folder`, `project_state` or `role_folder`), never its own text, and `surla` and the views page each turn that cause into a fixed step. The server:

1. adopts the project's saved Herdr space, or the single space with the team's name and the project's folder, or creates one; several matching spaces are refused, and the user closes the duplicates;
2. registers the guide, working in the project folder;
3. writes the Chorus roster: one row per registered role whose folder exists, with that folder, its first message, and a connection to the server's MCP endpoint that names the role's credential file, never the credential itself;
4. opens the Chorus dialog when a listed role is not running.

`surla` prints `Team <name>.` and the user's next step, opens the views page and prints `Surla is running for <folder> at <address>`. The roster places the builder's pane to the right of the guide's and the test writer's below the builder's.

A build adds roles. The control plane asks the server to register the builder, working in the project folder, and, when tests are written for this build, the test writer, working in its own working copy. A build that uses no test writer removes a former test writer and its credential. The server derives every first message and the team's next step in one place and never keeps them in its saved state.

While the team runs, the server watches each registered role's terminal through Herdr and whether Chorus reports a watcher for each agent: the wait through which the agent receives its messages. It keeps the terminal's state and reported status, the foreground program's arguments and the watcher facts, never screen text, titles or messages. Herdr sends a heartbeat every 15 seconds; when three are missed, every role is unknown until fresh snapshots arrive. Chorus answers are read only up to their agent list, at most 256 KiB, before any conversation data. The control plane gives the server a read-only callback that lists the unread assignments; calling it never marks work read. Surla types nothing into a pane. It reminds a role of an unread assignment with two tool calls posted to Chorus's `/mcp` endpoint, written as Chorus's dashboard writes them: `who` registers Surla's own handle in the team, then `send` hands the role one fixed direct message. The sender has its own client, bounded at 5 seconds per post; it fails a response longer than 2 MiB and decodes only whether each call failed. The supervisor alone keeps, in memory, which generations of an unread assignment it has already tried. [Team supervision](REFERENCE.md#team-supervision) gives the user's view of this.

### Change Cards and builds

The control plane keeps Change Cards in the Change Card folder, under the folder lock the running server holds. Only the guide proposes or revises a change. Once the user confirms a change, its agreed revision stays in force while a revision the guide proposes waits beside it, until the user adopts or declines that proposed revision.

A build is the user's decision to build the agreed revision. The control plane saves one build per change, in `<sha256(change_id)>.build.json` beside the cards, by writing a staging file, renaming it and syncing the folder, under the card lock. The card lock (`ControlPlane.mu`) is an in-process lock that guards every read and write of a card or a build inside the server; it is not the folder lock. The build file's reader refuses unknown fields, trailing data, an unknown role selection, a negative report number and a test writer state that contradicts its other fields. A build holds:

| Field | Meaning |
| --- | --- |
| `change_id`, `request_revision`, `started_at` | The change, the agreed revision the build started from, and when the user pressed **Build this change** |
| `stories`, `design_source` | The Story and design as agreed, copied when the build started; later revisions do not change them |
| `answers`, `examples`, `preserved_behaviors` | Each question with the answer the user confirmed, the confirmation examples and the preserved behaviors, as agreed, copied at the same time; the test writer, which may not read the change, receives them in its assignment |
| `roles` | `builder_and_test_writer`, or `builder_only` when stored verification settings supply the tests |
| `builder`, `test_writer` | Each assignment; the test writer's carries its state, findings, accepted tests commit and working folder |
| `report_number` | The test writer's latest report, 0 before the first |

The server's agent record maps each role to its folder and credential file. The build file holds the test writer's working folder but no credential, and neither store writes the other's data.

When the user presses **Build this change** on the views page, `start_build`:

1. checks that the caller is the project's owner, then takes the project's working-copy turn without waiting; while another start or a report check holds the turn, the start is refused and nothing is saved;
2. checks the agreed revision, opens and refreshes the test writer's working copy when tests are written for this build, and checks the revision again;
3. saves the build, with the Story, design, answers, examples and preserved behaviors of the revision that second check read, then registers its roles with the server, so a saved build never lacks its agents;
4. writes the required tests into the working copy, releases the turn and asks the server to start the agents.

The work under the turn has one five-minute limit; starting the agents has its own 45-second limit. Accepted work belongs to the server and survives the page closing. A build started with only the builder stays usable while stored verification settings supply its tests; when those settings are removed, the user builds again.

The views page keeps one waiting read (`wait_for_changes`) open. Its answer is a version of the change list that changes whenever a card, a change's latest verification run, a build as `get_build` shows it, the team, the Story evaluation or the live target changes, and the page then reads again. The page sends a build or a window action only when the user acts.

### The test writer's working copy and the report checker

`internal/testwriter` owns the test writer's working copy: a linked Git working copy of the project at `.surla-tests` on the branch `surla/tests`. It prepares the required tests, checks a report and records accepted tests as a commit on that branch. The control plane runs one report checker per project: it starts when the server binds the team and drains before the stores close. A check holds the working-copy turn, so a build start cannot change the copy under it. Three in-process locks are always taken in this order: the working-copy turn, the report lock, the card lock. A report check holds the working-copy turn from start to verdict, and a build start holds it until the required tests are written; either can take minutes. The report lock (`ControlPlane.reportMu`) makes a newly arriving report wait while the checker confirms the build is the one it checked, records the accepted tests and saves the verdict. Git and the operating-system sandbox (macOS `sandbox-exec`) never run under the card lock. [Story flows and the tests a build requires](STORY_TEST_DESIGN.md) describes [the copy](STORY_TEST_DESIGN.md#the-test-writers-working-copy) and [the check and its findings](STORY_TEST_DESIGN.md#how-surla-judges-a-report).

### Verification runs and the live target

One function, `testsChoice` in [`verification_plan.go`](../internal/controlplane/verification_plan.go), chooses which tests judge a change: the suite named in the stored verification settings, or the tests accepted for the change's build at its revision. Verification runs those tests, `approve_merge` requires the approved report to have run them, and a saved approval is shown only while they stay the same; `verificationPlan` builds a run's configuration from that choice. The builder names only the change, the revision and its commit.

`internal/verifyrun` runs each verification in its run directory, one folder per run under the state folder's runs folder that holds the run's request, lock, result and logs. A supervisor process, started for the run, holds the run's lock for its whole life, so a run outlives the server that accepted it and its state is read back from the lock and the result file alone. `internal/verify` builds the candidate (the builder's commit under verification) and the test program in the sandbox, then runs the test program outside it; the test program starts the candidate's programs in the sandbox.

The control plane runs at most one live target: the commit the builder names in `start_live_target`, built as verification builds it and started by its test program's `--live` mode, under the Change Card folder. Surla sends prompt drafts to it with a key it minted for that target. Closing the control plane stops it.

### Operation diagnostics

The control plane records each of its operations as an OpenTelemetry span and saves a bounded, safe view of each one in one history file, `traces.jsonl` in the Change Card folder. The history belongs to the project's owner: every caller (the user's page, the management credential and each agent) acts for that owner, and each record names its caller and the caller's kind, such as `user`, `management` or the agent's role. The server's composition root, `projectRuntime.serve` in [`project_server.go`](../cmd/surla/project_server.go), owns the tracer provider, and exports full spans over OTLP when an OTLP endpoint is set in the server's environment. The exported spans carry no added identity. The user's page reads that succeed or are refused, and its waiting read, are not saved, so the page's repeated reads do not fill the history. Spans that end after the control plane closes are not saved. [Operation diagnostics](REFERENCE.md#operation-diagnostics) gives the caller's view.
