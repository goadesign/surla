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

**Surla** pairs mixed-model agent teams (combining frontier reasoners and fast coding models across **Claude, Codex, and Gemini**) with a deterministic compiler stack. Because [Goa](https://github.com/goadesign/goa) generates transports, schemas, and validation at **0 LLM tokens**, and [Goa-AI](https://github.com/goadesign/goa-ai) provides built-in durable workflows, tool guardrails, and isolated evaluation, your agents only write compact contracts and pure domain logic, and a separate `surla verify` step checks the candidate build in an OS sandbox before you merge.

```text
You: "Add a refund assistant to our orders service. Refunds up to $50 get
      instant store credit; anything larger waits for a manager."

Surla: Before I propose a build plan, two quick edge cases:
  1. Does "$50" mean item subtotal, or total including tax and shipping?
  2. What happens at exactly $50.00 — instant credit or manager approval?

You: "Total including tax and shipping, and $50.00 is instant."

Surla: Locked 5 acceptance checks, compiled design/orders.go (0 tokens),
       and proposed a team across model families & classes:
  • Architect (Claude Opus)     → API, auth & workflow contract
  • Builder (Claude Sonnet)     → Refund workflow & state store
  • Test Author (Gemini Pro)    → Isolated acceptance & crash suite
  • Reviewer (Codex)            → Adversarial security & code review
```

---

## Why Engineers Use Surla

### 1. 0-token code generation—agents write the contract, the compiler writes the plumbing
Why pay an LLM to hallucinate 3,000 lines of HTTP handlers, gRPC stubs, OpenAPI specs, MCP schemas, and JSON validators? In Surla, the Architect agent expresses your API, agent tools, and security rules in a compact, strongly typed design (`design/*.go`). Goa deterministically generates the transports, OpenAPI specs, MCP schemas, and validators from that design with no model inference (**0 LLM tokens**)—eliminating entire classes of glue-code bugs and the token spend of writing them by hand.

### 2. A built-in Golden Path that stays cheap and effortless to evolve
Most AI-built codebases rot after a few weeks because every prompt invents a new folder structure, a new way to call LLMs, or a new auth pattern—forcing future agents to burn tens of thousands of tokens just reverse-engineering the repo. Surla enforces an embedded **Golden Path architecture**:
- **Predictable layout & generated agent maps:** Every service follows the same clean structure and ships an auto-generated `AGENTS_QUICKSTART.md` map, so any coding agent can jump in months later and extend the platform with minimal context tokens.
- **Production patterns by default:** One HTTP/MCP edge gateway, internal gRPC services, a central LLM gateway with rate limiting and cost tracking, durable Temporal workflows, and server-injected `tenant_id`/`user_id` fields that the LLM can never spoof.

### 3. Mixed-model teams where the builder never grades its own homework
When configured with provider profiles (`-swarm-profiles`) and explicit builder path scopes, Surla proposes a team that routes each role to the right model family and class—frontier reasoning models for architecture and security review, fast coding models for scoped implementation, and a **disjoint model family** for test authoring. Final verification (`surla verify` on the CLI, or `verify_change`, `get_verification`, and `list_verifications` over MCP when `-project` or `-verification-config` is configured; see [`docs/REFERENCE.md`](docs/REFERENCE.md#mcp-tools)) is independent of the builder: it builds the candidate from its exact commit inside an **OS sandbox** (`verify/candidate`) and runs an evaluator suite the builder does not control, and the reference suite starts each candidate process in its own sandbox:
- **Unseen holdout tests:** Hidden scenarios verify security and edge cases while [`Report.ForBuilders()`](internal/verify/report.go) strips holdout names, inputs, and logs from the builder's view. For those scenarios to stay unseen, builders must not be able to read the evaluator suite or its report and work directories; share only the redacted builder report with them.
- **Real `kill -9` crash & budget proof:** The included [`examples/orders-eval`](examples/orders-eval/) reference suite (`surla verify` or `go test -race -tags e2e`) kills the Temporal worker mid-transaction (`kill -9`), restarts the API during paused approvals, and fails any scenario whose runtime LLM token use exceeds its budget (`TestSeededPayloadBloatFailsTheBudget` proves it with a seeded payload bloat).

### 4. Fast draft overrides and live role-switched testing
Instead of reading a 2,000-line diff blindly, Surla separates fast prompt tuning from compiled business rules and ships stateful dependency simulators in [`examples/orders-eval`](examples/orders-eval/):
- **Test across real auth boundaries:** In the reference platform, trigger a `\$54.00` refund as a **Customer**, verify that customer credentials cannot approve it (`403 Forbidden`), and switch to **Approving Manager** to approve or decline the paused workflow through the real HTTP API.
- **Iterate on prompts without rebuilding:** Start `surla serve` from your project directory (or pass `-project <dir>` or `-verification-config`), then call `start_live_target` with a `candidate_commit`: `surla serve` builds that commit the way verification does and runs it as a live target through the evaluator suite's `--live` mode. `draft_override` (`POST /sandbox/draft`) then sends new prompt wording to that running target, without rebuilding Go code (see the [`draft_override` and `start_live_target` reference](docs/REFERENCE.md#mcp-tools)). A project created with `init_project` over MCP or `surla init` on the CLI has an evaluator suite that supports `--live`; when `surla serve` runs from an empty directory and your agent calls `init_project`, `surla serve` scaffolds and commits the starter project in `<workspace>/<dir>`, transitions the active workspace in place without restarting, and pins `start_live_target` and `verify_change` to that initial starter commit (`surla serve` uses `ANTHROPIC_API_KEY` with `claude-sonnet-4-6` or `OPENAI_API_KEY` with `gpt-5.4-mini` when model flags are omitted, or pass `-model-provider` and `-model-name` or `-model-gateway-addr`, or call `configure_model` at runtime). When `surla serve` starts in an already-committed starter repository, `start_live_target` and `verify_change` pin the evaluator suite to the repository's `HEAD` commit at server startup. Surla refuses any attempt to override compiled Go business rules (`target_kind: "compiled_rule"`, `requires_rebuild: true`). A draft lasts only as long as the live target: to keep it, edit the text into source, commit, and run `verify_change` (or `surla verify`) on that commit for the same `request_revision`. The in-repository [`examples/orders/cmd/orders-surla`](examples/orders/cmd/orders-surla/main.go) host instead binds `Options.TargetUpdater` to edit prompts on an already-running orders deployment.
- **Interactive MCP Apps checkpoint viewer (`ui://surla/checkpoint`):** An MCP Apps host loads the viewer through MCP `resources/read` (`text/html;profile=mcp-app`) and shows each tool result in it. For a Change Card, the viewer shows a form with every open question, example, and preserved behavior, saves through `confirm_intent` (recovering a newer saved card with `get_change`, preserving only changed draft answers and examples in `#surla-preserved-draft` alongside the `#surla-prior-revision` comparison when a card is revised via `revise_change` or `confirm_intent`), and—when `approve_merge` has recorded promotion approval via `Options.PromotionTarget`—renders the Checkpoint 5 current approval summary (`approval`) or recorded-unverified approval notice (`unverified_approval`, when promotion inputs cannot currently be checked) and warns if local draft edits would supersede the approved revision. When the CP1 model also composed a decision view, the viewer renders it with OpenUI components such as a policy simulator table, ambiguity choice cards, a workflow timeline, and impact callouts. Choices made there change the same draft the form submits, and the view is labeled as a model-proposed illustration. An invalid or stale composition leaves the full form usable. Across checkpoints, the viewer retains received session state and renders a session navigation bar plus in-view actions (`View Saved Verifications`, `Load Change Card`, and `Return to Change Card`) so you can move back and forth between the Change Card (`CP1`/`CP5`), session-only architecture previews (`CP2`), structural diffs, live-target and fast-draft results (`CP3`), and Checkpoint 4 verification runs (`verify_change`, `get_verification`, and `list_verifications`; see [`docs/REFERENCE.md`](docs/REFERENCE.md#mcp-tools)) without losing unsaved CP1 edits. In `CP4`, the viewer loads saved runs for the active `change_id`, warns when a displayed run is not included in the last loaded list or when a `CP2` preview or `CP4` run applies to a different `request_revision` or `change_id` than the loaded card, and keeps raw IDs and digests in expandable Details. The planned role-switcher and replay controls are described in [`docs/HOW_IT_WORKS.md`](docs/HOW_IT_WORKS.md#checkpoint-3-cp3-fast-draft-overrides--live-sandbox-walkthrough).

> **Why the name "Surla"?** Named after **Tambdi Surla** in Goa—home to the carved basalt Mahadeva temple beside the Surla River. In Surla (*SOOR-lah*), `design/*.go` is that enduring stone contract: one compiled specification every coding agent builds to, and an independent verifier checks against.

---

## Try It in 60 Seconds

### 1. Inspect, Diff, or Scaffold a Goa-AI Project Locally (`0` API Keys Needed)

Surla requires **macOS** (`sandbox-exec` isolates candidate builds and processes; run `surla` commands and `surla serve` from an unrestricted host shell rather than inside a sandboxed coding-agent CLI, because macOS does not nest `sandbox-exec` profiles—see [`docs/REFERENCE.md`](docs/REFERENCE.md#terminology-at-a-glance)) and **Go 1.27.3+**. Before configuring an LLM key or starting a server, install the `surla` CLI to scaffold and preview a new starter service from anywhere (`surla init` and `surla gen` generate code, and `surla preview` evaluates designs, in Surla's sandbox, so no separate global `goa` install is needed):

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

Or clone this repository to inspect and diff the included [`examples/orders`](examples/orders/README.md) reference platform (from a checkout, you can run either the installed `surla` binary or `go run ./cmd/surla`; when running `go run ./cmd/surla init` from a checkout, pass `-surla-version v0.0.0-20261104215521-580509970fb6`):

```bash
git clone https://github.com/goadesign/surla.git
cd surla

# Preview the Orders reference platform's C4 architecture and tool guardrails at 0 LLM tokens:
surla preview -dir examples/orders

# Compare two Goa design modules structurally at 0 LLM tokens:
surla diff --old-dir examples/orders --new-dir examples/orders
```

After you edit a module's `design/`, `surla gen -dir <module>` regenerates its `gen/` inside the same sandbox and swaps it in atomically. From a connected coding agent, call [`generate_code`](docs/REFERENCE.md#mcp-tools) to regenerate the active application module, or pass `target: "eval"` for its public local `eval/` module, without uploading source or restarting the server. Generation compiles the design only; build the application and run its tests before verification. It needs the module to use `goa.design/goa/v3` `v3.35.1-0.20261103205041-35e789e8f9b1` or later, and doesn't yet support gRPC designs; see [`surla gen`](docs/REFERENCE.md#surla-gen).

### 2. Connect Your Coding Agent to the Surla Control Plane (`surla serve`)

Surla organizes development around five checkpoints (**CP1** Confirm Intent → **CP2** Preview Architecture → **CP3** Fast Draft & Sandbox → **CP4** Isolated Verification → **CP5** Promotion Check; see [`docs/HOW_IT_WORKS.md`](docs/HOW_IT_WORKS.md)). Start the standalone Surla control plane and connect your coding assistant over MCP: when `-project` and `-cards-dir` are omitted, `surla serve` binds to its working directory as the active workspace and stores Change Cards, verification runs, reports, and live-target directories in operator-private storage outside the workspace (`<UserConfigDir>/surla/operator/project-<sha256>/`). Starting `surla serve` in an empty directory lets your agent call `init_project` over MCP to scaffold and commit a starter project in `<workspace>/<dir>`, transition the active workspace in place without restarting, and pin `verify_change` and `start_live_target` to that initial starter commit; when `surla serve` starts in an already-committed starter repository, `verify_change` and `start_live_target` pin the evaluator suite to its `HEAD` commit at server startup (pass `-project <dir>` to bind an explicit workspace directory, `-verification-config <path.json>` to use an explicit verification plan, `-cards-dir <dir>` to override the cards directory, `-import-legacy-workspace` to migrate an existing in-workspace `.surla/` and `reports/` tree once, and `-promotion-config` to bind the **CP5** staged-release `PromotionTarget` for `approve_merge`; the [CLI verification walkthrough](examples/orders/README.md#3-run-os-sandboxed-end-to-end-verification-surla-verify) covers standalone `surla verify` and `surla approve`):

```bash
# Start the Surla MCP & HTTP control plane with CP1 model analysis and multi-family swarm profiles.
# When model flags are omitted, surla serve uses ANTHROPIC_API_KEY (claude-sonnet-4-6) or
# OPENAI_API_KEY (gpt-5.4-mini) from its environment, and configure_model can switch models
# or attach a model gateway at runtime without restarting:
export SURLA_AUTH_TOKEN="dev-secret-token"
export ANTHROPIC_API_KEY="sk-ant-..."
surla serve \
  -addr 127.0.0.1:8090 \
  -principal operator@example.com \
  -swarm-profiles ./examples/orders/swarm-profiles.example.json

# Or select a specific OpenAI or Anthropic model explicitly with -model-provider and -model-name:
export OPENAI_API_KEY="sk-..."
MODEL_NAME="gpt-5.4-mini"
surla serve \
  -addr 127.0.0.1:8090 \
  -principal operator@example.com \
  -model-provider openai \
  -model-name "$MODEL_NAME" \
  -swarm-profiles ./examples/orders/swarm-profiles.example.json
```

Register `http://127.0.0.1:8090/mcp` in your coding agent (connected agents can also read the embedded Surla Golden Path architecture and evaluator guide at the MCP resource `surla://golden-path` or `GET http://127.0.0.1:8090/golden-path`). For Claude Code, save this as `.mcp.json`; Claude Code fills in `${SURLA_AUTH_TOKEN}` from the environment it starts in:

```json
{
  "mcpServers": {
    "surla": {
      "type": "http",
      "url": "http://127.0.0.1:8090/mcp",
      "headers": {"Authorization": "Bearer ${SURLA_AUTH_TOKEN}"}
    }
  }
}
```

Other clients use the client-specific configuration in **[Register Surla in Your MCP Client (`docs/REFERENCE.md`)](docs/REFERENCE.md#register-surla-in-your-mcp-client)** (covering **Cursor** `.cursor/mcp.json`, **Gemini CLI** `.gemini/settings.json` with `httpUrl`, and **Codex** `.codex/config.toml`; see also [Terminology at a Glance](docs/REFERENCE.md#terminology-at-a-glance) and [`-swarm-profiles` configuration](docs/REFERENCE.md#configure-the-proposed-swarm-roster)—[`examples/orders/swarm-profiles.example.json`](examples/orders/swarm-profiles.example.json) is an illustrative operator-edited profile file where `"authenticated": true` is an operator assertion rather than a live credential check, and setting `ANTHROPIC_API_KEY` for CP1 analysis does not authenticate the OpenAI or Google coding-agent CLIs).

When `surla serve` is bound to a workspace that holds `design/*.go`, `propose_change` and `revise_change` automatically read `design/*.go` from the workspace when `design_source` is omitted (for cards whose `design_source` was read from the workspace), and `preview_architecture` reads the workspace's `DesignSnapshot` when `design` is omitted (pass `design_source` or `design` explicitly when targeting a different directory, such as `examples/orders` from the repository root).

Then prompt your agent:

> *"Using Surla, propose a change to `examples/orders` so VIP customers get instant store-credit refunds up to \$150, then preview the compiled C4 architecture and Chorus swarm plan for builder scope `orders` (`internal/**`)."*

Your agent will create the **Change Card** (`propose_change`, using `revise_change` when requirements evolve on the same `change_id` and calling `confirm_intent` to answer open ambiguity questions or refine confirmed answers and examples, which returns the card's `change_id` and locks a SHA-256 `request_revision` fingerprint), compile the candidate Goa contract at **0 LLM tokens** (`preview_architecture` over MCP—omitting `design` when `surla serve` is bound to that module's workspace—or `surla preview-change` from a local module directory), and return the updated C4 topology, tool guardrails, and—when explicit builder scopes are supplied—a proposed cross-family Chorus roster (`@builder-orders` and `@reviewer` when `builders` are supplied, plus `@architect` when `architect_paths` / `-architect-path` are supplied and `@eval-author` when `eval_paths` / `-eval-path` are supplied). The preview proposes the Chorus coordination roster; it does not launch agents. From a shell, pass the confirmed card's `change_id` and `request_revision` to `surla preview-change` (without `-builders`, the command returns a solo preview of the evaluated C4 architecture, agent policies, and tool contracts with `swarm.status: "unconfigured"` and no `missing_roles`; pass `-builders <file.json>` and optional repeatable `-architect-path` / `-eval-path` flags as shown in [Configure the Proposed Swarm Roster (`docs/REFERENCE.md`)](docs/REFERENCE.md#configure-the-proposed-swarm-roster) to include the proposed team assignments):

```bash
# Send the local module snapshot to CP2 for the confirmed Change Card's change_id and request_revision
# (returns the evaluated C4 architecture, agent policies, and tool contracts; add -builders to plan the team roster):
surla preview-change \
  -server http://127.0.0.1:8090 \
  -change-id vip-refunds \
  -request-revision "$REQUEST_REVISION" \
  -dir examples/orders
```

To run the full independent verification suite (`surla verify` and `surla approve`) and compare the default builder-redacted report (where unseen `"holdout"` acceptance tests are redacted to `"id": "holdout-1"`) with the full operator report (`-builder-view=false`), see the **[Orders Reference Platform Walkthrough (`examples/orders/README.md`)](examples/orders/README.md)**.

---

## Dive Deeper

- **[Orders Reference Platform (`examples/orders/README.md`)](examples/orders/README.md)** — Walk through the Golden Path `orders` platform, run `surla verify` (default builder view vs. full report with `-builder-view=false`), and watch `TestSeededPayloadBloatFailsTheBudget` catch a token regression.
- **[How Surla Works (`docs/HOW_IT_WORKS.md`)](docs/HOW_IT_WORKS.md)** — Walk through the 5 checkpoints (`CP1`–`CP5`), the proposed cross-model team, fast draft overrides, and OS-isolated verification.
- **[Golden Path Architecture (`docs/ARCHITECTURE_PLAYBOOK.md`)](docs/ARCHITECTURE_PLAYBOOK.md)** — Explore the 7 Golden Path rules that keep Surla platforms maintainable and see how the Goa Design suite powers them.
- **[CLI, MCP & Repo Reference (`docs/REFERENCE.md`)](docs/REFERENCE.md)** — Configure MCP clients (Claude Code, Cursor, Gemini CLI, and Codex), browse the MCP tools and CLI flags, and run the [`examples/orders`](examples/orders/) reference suite.
- **[Architecture Proposal (`PROPOSAL.md`)](PROPOSAL.md)** & **[Product Vision (`PRESS_RELEASE.md`)](PRESS_RELEASE.md)** — Read the full system design, threat model, and Working Backwards press release.

## License

Surla is released under the [MIT License](LICENSE).
