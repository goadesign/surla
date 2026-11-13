<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/img/surla-banner-dark.png">
    <img alt="Surla — one compiled contract every coding agent builds to and an independent verifier checks against." src="docs/img/surla-banner.png" width="960">
  </picture>
</p>

<p align="center">
  <a href="https://pkg.go.dev/github.com/goadesign/surla"><img alt="Go Reference" src="https://pkg.go.dev/badge/github.com/goadesign/surla.svg"></a>
  <a href="https://modelcontextprotocol.io"><img alt="MCP" src="https://img.shields.io/badge/MCP-2026--07--28-6E56CF"></a>
  <a href="https://github.com/goadesign/surla/blob/main/LICENSE"><img alt="MIT License" src="https://img.shields.io/badge/license-MIT-blue.svg"></a>
</p>

# Surla

**An agentic software factory that builds, verifies, and evolves production services at a fraction of the token cost.**

Solo coding agents—and most multi-agent "team" wrappers—still ask LLMs to hand-write every line of HTTP routing, auth middleware, workflow plumbing, and tests from scratch. That works on day one and collapses by feature three: the codebase drifts into an inconsistent maze that burns more tokens on every edit, and the agent writing the code writes tests that rubber-stamp its own bugs.

**Surla** pairs the coding agents you already use with a deterministic compiler stack. Because [Goa](https://github.com/goadesign/goa) generates transports, schemas, and validation at **0 LLM tokens**, and [Goa-AI](https://github.com/goadesign/goa-ai) provides built-in durable workflows, tool guardrails, and isolated evaluation, your agents only write compact contracts and pure domain logic, and a separate `surla verify` step checks the candidate build in an OS sandbox before you merge.

```text
You: "Add a refund assistant to our orders service. Refunds up to $50 get
      instant store credit; anything larger waits for a manager."

Surla: Before I propose a build plan, two quick edge cases:
  1. Does "$50" mean item subtotal, or total including tax and shipping?
  2. What happens at exactly $50.00 — instant credit or manager approval?

You: "Total including tax and shipping, and $50.00 is instant."

Surla: Locked the acceptance checks and compiled design/orders.go (0 tokens).
       The preview shows the contracts and guardrails the design declares.

```

---

## Why Engineers Use Surla

### 1. 0-token code generation—agents write the contract, the compiler writes the plumbing
Why pay an LLM to hallucinate 3,000 lines of HTTP handlers, gRPC stubs, OpenAPI specs, MCP schemas, and JSON validators? In Surla, your coding agents express your API, agent tools, and security rules in a compact, strongly typed design (`design/*.go`). Goa deterministically generates the transports, OpenAPI specs, MCP schemas, and validators from that design with no model inference (**0 LLM tokens**)—eliminating entire classes of glue-code bugs and the token spend of writing them by hand.

### 2. A built-in Golden Path that stays cheap and effortless to evolve
Most AI-built codebases rot after a few weeks because every prompt invents a new folder structure, a new way to call LLMs, or a new auth pattern—forcing future agents to burn tens of thousands of tokens just reverse-engineering the repo. Surla enforces an embedded **Golden Path architecture**:
- **Predictable layout & generated agent maps:** Every service follows the same clean structure and ships an auto-generated `AGENTS_QUICKSTART.md` map, so any coding agent can jump in months later and extend the platform with minimal context tokens.
- **Production patterns by default:** One HTTP/MCP edge gateway, internal gRPC services, a central LLM gateway with rate limiting and cost tracking, durable Temporal workflows, and server-injected `tenant_id`/`user_id` fields that the LLM can never spoof.

### 3. Independent verification of each candidate
Final verification (`surla verify` on the CLI, or `verify_change`, `get_verification`, and `list_verifications` over MCP; see [`docs/REFERENCE.md`](docs/REFERENCE.md#mcp-tools)) is independent of the builder: it builds the candidate from its exact commit inside an **OS sandbox** (`verify/candidate`) and runs tests the builder did not write, and the reference suite starts each candidate process in its own sandbox. With stored verification settings, those tests are the operator's evaluator. Without them, they are the tests a separate test writer agent wrote and Surla accepted for the change's build; each result says so, because that agent runs on the same machine and is not isolated from the builder:
- **Unseen holdout tests:** Hidden scenarios verify security and edge cases while [`Report.ForBuilders()`](internal/verify/report.go) strips holdout names, inputs, and logs from the builder's view. For those scenarios to stay unseen, builders must not be able to read the evaluator suite or its report and work directories; share only the redacted builder report with them.
- **Real `kill -9` crash & budget proof:** The included [`examples/orders-eval`](examples/orders-eval/) reference suite (`surla verify` or `go test -race -tags e2e`) kills the Temporal worker mid-transaction (`kill -9`), restarts the API during paused approvals, and fails any scenario whose runtime LLM token use exceeds its budget (`TestSeededPayloadBloatFailsTheBudget` proves it with a seeded payload bloat).

### 4. Fast draft overrides and live role-switched testing
Instead of reading a 2,000-line diff blindly, Surla separates fast prompt tuning from compiled business rules and ships stateful dependency simulators in [`examples/orders-eval`](examples/orders-eval/):
- **Test across real auth boundaries:** In the reference platform, trigger a `\$54.00` refund as a **Customer**, verify that customer credentials cannot approve it (`403 Forbidden`), and switch to **Approving Manager** to approve or decline the paused workflow through the real HTTP API.
- **Iterate on prompts without rebuilding:** Run `surla` from your project directory; the builder then calls `start_live_target` with a `candidate_commit`: the Surla server builds that commit the way verification does and runs it as a live target through the evaluator suite's `--live` mode. `draft_override` (`POST /sandbox/draft`) then sends new prompt wording to that running target, without rebuilding Go code (see the [`draft_override` and `start_live_target` reference](docs/REFERENCE.md#mcp-tools)). A project created with `init_project` over MCP or `surla init` on the CLI has an evaluator suite that supports `--live`; when the Surla server runs from an empty directory and the guide calls `init_project`, the Surla server scaffolds and commits the starter project in `<workspace>/<dir>`, and transitions the active workspace in place without restarting. Without stored verification settings, `start_live_target` runs the candidate commit's own test module, and `verify_change` runs the tests Surla accepted for the change's build; neither depends on the commit the Surla server started at. The live target plans with the model the Surla server passes it: `ANTHROPIC_API_KEY` with `claude-sonnet-4-6` or `OPENAI_API_KEY` with `gpt-5.4-mini`, chosen at startup, which you can change in the views page's **Live target** view. Surla refuses any attempt to override compiled Go business rules (`target_kind: "compiled_rule"`, `requires_rebuild: true`). A draft lasts only as long as the live target: to keep it, edit the text into source, build and test it with `check_project`, commit, and run `verify_change` (or `surla verify`) on that commit for the same `request_revision`.
- **The views page in your browser:** Running `surla` opens this project's views page at the Surla server's own loopback address with a one-time link. The page trades that link for a session it keeps only in memory and sends with every request, so reloading or closing the tab ends the session and you run `surla` again to open a new one. The page lists the project's Change Cards, most recently saved first, opens the only one directly, and updates itself when the guide saves a new revision or a verification run finishes, without discarding what you typed or moving your cursor. A revision of a change you already agreed appears as a proposal, which you adopt or decline. For a Change Card, the page shows a form with every open question, example, and preserved behavior, saves through `confirm_intent` (recovering a newer saved card with `get_change`, preserving only changed draft answers and examples in `#surla-preserved-draft` alongside the `#surla-prior-revision` comparison when the card is revised), and, when a promotion approval is recorded, renders the Checkpoint 5 current approval summary (`approval`) or recorded-unverified approval notice (`unverified_approval`, when promotion inputs cannot currently be checked) and warns if local draft edits would supersede the approved revision. When the coding agent also composed a decision view in its analysis (`ui_composition`), the page renders it with OpenUI components such as a policy simulator table, ambiguity choice cards, a workflow timeline, and impact callouts. Choices made there change the same draft the form submits, and the view is labeled as a model-proposed illustration. An invalid or stale composition leaves the full form usable. The **Story** stage, open with or without a change, draws the Story saved with the change you are looking at: each flow a row of step cards by role, complete flows marked to be tested end to end and drafts marked as not tested, with why. It also shows the guide's current Story draft as it runs, fails or succeeds, and says whether that draft differs from the change's Story; a draft becomes part of a change only when the guide revises the change with it and you confirm. The confirm view states how many flows the change's Story has and how many are drafts. Opening the **Architecture** stage asks the server to preview the bound design for the revision shown (`CP2`); the **Proof** stage lists the change's saved verification runs (`CP4`) and offers **Approve this result** (`approve_merge`) for a passed run of the shown revision. Approval needs the project's promotion settings; without them the page says approval is unavailable and what enables it. Raw IDs and digests stay in expandable Details. The page shows the live target with the prompts its drafts changed and lets you stop it and change the model live targets use; it does not show structural design comparisons or prompt text. See [the views page reference](docs/REFERENCE.md#views-page).

The views page's **Traces** control shows your saved operation diagnostics.
Read safe summaries, filter recorded errors, and load older pages without losing
unsaved Change Card edits. See [Operation diagnostics](docs/REFERENCE.md#operation-diagnostics)
for recording coverage and full operator telemetry.

> **Why the name "Surla"?** Named after **Tambdi Surla** in Goa—home to the carved basalt Mahadeva temple beside the Surla River. In Surla (*SOOR-lah*), `design/*.go` is that enduring stone contract: one compiled specification every coding agent builds to, and an independent verifier checks against.

---

## Try It in 60 Seconds

### 1. Inspect, Diff, or Scaffold a Goa-AI Project Locally (`0` API Keys Needed)

Surla requires **macOS** (`sandbox-exec` isolates candidate builds and processes; run `surla` commands and the Surla server from an unrestricted host shell rather than inside a sandboxed coding-agent CLI, because macOS does not nest `sandbox-exec` profiles—see [`docs/REFERENCE.md`](docs/REFERENCE.md#terminology-at-a-glance)) and **Go 1.27.3+**. Before configuring an LLM key or starting a server, install the `surla` CLI to scaffold and preview a new starter service from anywhere (`surla init` and `surla gen` generate code, and `surla preview` evaluates designs, in Surla's sandbox, so no separate global `goa` install is needed):

```bash
# Install the Surla CLI (embeds the published module version for surla init):
go install github.com/goadesign/surla/cmd/surla@latest

# Scaffold a new Goa + Goa-AI + Model starter service and visible eval/ suite:
surla init -dir /tmp/shop -module example.com/shop

# Compile /tmp/shop/design/*.go in an OS sandbox at 0 LLM tokens and print
# its C4 containers, boundary edges, agent policies, and tool guardrails:
surla preview -dir /tmp/shop

# List all commands and flags:
surla help
```

Or clone this repository to inspect and diff the included [`examples/orders`](examples/orders/README.md) reference platform (from a checkout, you can run either the installed `surla` binary or `go run ./cmd/surla`; when running `go run ./cmd/surla init` from a checkout, pass `-surla-version v0.0.0-20261110163737-d9beac313142`):

```bash
git clone https://github.com/goadesign/surla.git
cd surla

# Preview the Orders reference platform's C4 architecture and tool guardrails at 0 LLM tokens:
surla preview -dir examples/orders

# Compare two Goa design modules structurally at 0 LLM tokens:
surla diff --old-dir examples/orders --new-dir examples/orders
```

After you edit a module's `design/`, `surla gen -dir <module>` regenerates its `gen/` inside the same sandbox and swaps it in atomically. In a running project, the builder calls [`generate_code`](docs/REFERENCE.md#mcp-tools) to regenerate the active application module, or passes `target: "eval"` for its public local `eval/` module, without uploading source or restarting the server. Generation compiles the design only; the builder then calls [`check_project`](docs/REFERENCE.md#mcp-tools) to build and test the uncommitted application and `eval/` modules in a Linux container (Docker Desktop required), and [`update_dependencies`](docs/REFERENCE.md#mcp-tools) when Go's output shows a missing `go.sum` or `go.work.sum` entry or a `go.mod` update. It needs the module to use `goa.design/goa/v3` `v3.35.1-0.20261103205041-35e789e8f9b1` or later, and doesn't yet support gRPC designs; see [`surla gen`](docs/REFERENCE.md#surla-gen).

### 2. Work through the change checkpoints

Install [Herdr core](https://herdr.dev/core/), the [Herdr app](https://herdr.dev/)
and [Chorus](https://github.com/goadesign/chorus) v0.11.0 or newer, then run
`surla` in your project folder. Before creating project state, Surla checks the
required programs and Chorus version and names the install or update step if
anything is missing. It starts or attaches to one server in a parentless Herdr
terminal, starts an absent Chorus service or Herdr window, and prepares this
project's guide team. If the server stops while starting, Surla shows its last
lines, including its error, and removes its ended terminal. Choose an agent and
model in the Chorus launcher in Herdr; the guide uses its own Surla connection
to work through the Story and change with you. Surla then opens the views page
through a short-lived link.

Starting again reuses the team's stable handles and Herdr space. No launcher
opens when every role listed in the roster is running. Surla opens the launcher
when any listed role has no running pane, including one that holds only an
[idle shell](docs/REFERENCE.md#idle-shell). For example, a running guide with a
builder listed in this project's roster whose pane is not running still needs
the launcher. Roles whose saved working folder is missing are omitted from the roster and
do not by themselves open the launcher. An already open launcher stays open
with its original project. If it is this project's dialog, click **Reload team**
to read the current roles before choosing agents; otherwise close it and run
`surla` again. Close any other dialog before retrying. Surla bounds readiness
checks without terminating your Herdr window or Chorus service.
The server also follows its registered roles in memory. Its user-only team API
reports terminal state, watcher availability and unread assignments; the views
page does not draw these team facts yet. Automatic reminder delivery is currently
unavailable. See [Team supervision](docs/REFERENCE.md#team-supervision).
`surla stop` shuts that project down in order. Subfolders and physical path
aliases find the same project; moving the folder starts a new, empty project.
The guide drafts the Story and Change Card; only your browser session confirms
intent or approves results. See the [project server
reference](docs/REFERENCE.md#surla-serve) for state, credentials and settings.

Surla saves the coding agent's analysis as a Change Card: the request, open
questions, concrete examples, and existing behavior to preserve. The server
reads the bound workspace's design itself and hashes that private snapshot into
`request_revision`. An adopted Story stays paired with its saved design source
when the workspace changes later.

The guide uses `refresh_story` to evaluate the workspace's domain Story, then
passes the succeeded operation's `op_id` as `story_refresh_op_id` to
`propose_change` or `revise_change`. Review and confirm the card's examples and answers in your browser, including a
card without questions, before requesting an
architecture preview. Once you have confirmed a change, the guide's later
revision does not replace it: it waits beside the agreed change as a proposal
until you adopt it (answering its questions and confirming it) or decline it,
and the agreed revision stays in force for previews, verification and approval
until then. `preview_architecture` returns the compiled C4 topology,
agent policies, and tool contracts. It reports the design's declarations; it
does not check that they satisfy the requested behavior.

Starting a build saves the agreed Story and design, prepares the roles and
returns the team's next step. A project with stored verification settings adds
only the builder. A starter project also gets a separate test writer, whose Git
working copy receives one required test entry per finished Story flow. Draft
flows stay editable and require no test. A busy or unavailable window leaves
the saved build and its registered roles ready for a later start-agents action.
See [Builds and assignments](docs/REFERENCE.md#builds-and-assignments) for
prerequisites, working-copy outcomes and recovery.

The builder builds and tests working changes with `generate_code` and
`check_project` before committing, then verifies the commit against the agreed
`request_revision`.
See [How it works](docs/HOW_IT_WORKS.md) and the [tool reference](docs/REFERENCE.md#mcp-tools).

## Dive Deeper

- **[Orders Reference Platform (`examples/orders/README.md`)](examples/orders/README.md)** — Walk through the Golden Path `orders` platform, run `surla verify` (default builder view vs. full report with `-builder-view=false`), and watch `TestSeededPayloadBloatFailsTheBudget` catch a token regression.
- **[How Surla Works (`docs/HOW_IT_WORKS.md`)](docs/HOW_IT_WORKS.md)** — Walk through the 5 checkpoints (`CP1`–`CP5`), fast draft overrides, and OS-isolated verification.
- **[Golden Path Architecture (`docs/ARCHITECTURE_PLAYBOOK.md`)](docs/ARCHITECTURE_PLAYBOOK.md)** — Explore the 7 Golden Path rules that keep Surla platforms maintainable and see how the Goa Design suite powers them.
- **[CLI, MCP & Repo Reference (`docs/REFERENCE.md`)](docs/REFERENCE.md)** — Browse the MCP tools and CLI flags, and run the [`examples/orders`](examples/orders/) reference suite.
- **[Product Direction (`docs/PRODUCT_DIRECTION.md`)](docs/PRODUCT_DIRECTION.md)** — What Surla is meant to be, who it is for, and the product decisions that follow.

## License

Surla is released under the [MIT License](LICENSE).
