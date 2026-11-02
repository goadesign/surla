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
When configured with provider profiles (`-swarm-profiles`) and explicit builder path scopes, Surla proposes a team that routes each role to the right model family and class—frontier reasoning models for architecture and security review, fast coding models for scoped implementation, and a **disjoint model family** for test authoring. Final verification (`surla verify` on the CLI, or `verify_change` and `get_verification` over MCP when `-verification-config` is configured) is independent of the builder: it builds the candidate from its exact commit inside an **OS sandbox** (`verify/candidate`) and runs an evaluator suite the builder does not control, and the reference suite starts each candidate process in its own sandbox:
- **Unseen holdout tests:** Hidden scenarios verify security and edge cases while [`Report.ForBuilders()`](internal/verify/report.go) strips holdout names, inputs, and logs from the builder's view. For those scenarios to stay unseen, builders must not be able to read the evaluator suite or its report and work directories; share only the redacted builder report with them.
- **Real `kill -9` crash & budget proof:** The included [`examples/orders-eval`](examples/orders-eval/) reference suite (`surla verify` or `go test -race -tags e2e`) kills the Temporal worker mid-transaction (`kill -9`), restarts the API during paused approvals, and fails any scenario whose runtime LLM token use exceeds its budget (`TestSeededPayloadBloatFailsTheBudget` proves it with a seeded payload bloat).

### 4. Fast draft overrides and live role-switched testing
Instead of reading a 2,000-line diff blindly, Surla separates fast prompt tuning from compiled business rules and ships stateful dependency simulators in [`examples/orders-eval`](examples/orders-eval/):
- **Test across real auth boundaries:** In the reference platform, trigger a `\$54.00` refund as a **Customer**, verify that customer credentials cannot approve it (`403 Forbidden`), and switch to **Approving Manager** to approve or decline the paused workflow through the real HTTP API.
- **Iterate on prompts without rebuilding:** When a host binds `Options.TargetUpdater`, `draft_override` (`POST /sandbox/draft`) updates prompt wording in the target's `prompt.Store` in memory, without rebuilding Go code—while Surla refuses any attempt to override compiled Go business rules (`target_kind: "compiled_rule"`, `requires_rebuild: true`; standalone `surla serve` binds no `TargetUpdater`, so `prompt_template` overrides are rejected unless embedded).
- **Interactive MCP Apps checkpoint viewer (`ui://surla/checkpoint`):** An MCP Apps host loads the viewer through MCP `resources/read` (`text/html;profile=mcp-app`) and shows each tool result in it. For a Change Card, the viewer shows a form with every open question, example, and preserved behavior, saves through `confirm_intent` (recovering a newer saved card with `get_change`, without losing unsaved edits), and—when `approve_merge` has recorded promotion approval via `Options.PromotionTarget`—renders the Checkpoint 5 approval summary and warns if local draft edits would supersede the approved revision. When the CP1 model also composed a decision view, the viewer renders it with OpenUI components such as a policy simulator table, ambiguity choice cards, a workflow timeline, and impact callouts. Choices made there change the same draft the form submits, and the view is labeled as a model-proposed illustration. An invalid or stale composition leaves the full form usable. The viewer also renders architecture previews (a C4 graph above inspectable lists), structural diffs, fast-draft results, and Checkpoint 4 verification runs (`verify_change` and `get_verification`, with a correlated `Check status` action while `running`), keeping IDs and digests in expandable Details. The planned role-switcher and replay controls are described in [`docs/HOW_IT_WORKS.md`](docs/HOW_IT_WORKS.md#checkpoint-3-cp3-fast-draft-overrides--live-sandbox-walkthrough).

> **Why the name "Surla"?** Named after **Tambdi Surla** in Goa—home to the carved basalt Mahadeva temple beside the Surla River. In Surla (*SOOR-lah*), `design/*.go` is that enduring stone contract: one compiled specification every coding agent builds to, and an independent verifier checks against.

---

## Try It in 60 Seconds

### 1. Inspect, Diff, or Scaffold a Goa-AI Project Locally (`0` API Keys Needed)

Before configuring an LLM key or starting a server, run Surla's sandboxed Goa/Goa-AI compiler directly on the included [`examples/orders`](examples/orders/README.md) reference platform—or scaffold a new starter service and evaluator suite with `surla init`:

```bash
# Install the surla CLI (embeds the published module version for surla init):
go install github.com/goadesign/surla/cmd/surla@latest

# Compile examples/orders/design/*.go in an OS sandbox at 0 LLM tokens and print
# its C4 containers, boundary edges, agent policies, and tool guardrails:
surla preview -dir examples/orders

# Compare two Goa design modules structurally at 0 LLM tokens:
surla diff --old-dir examples/orders --new-dir examples/orders

# Scaffold a new Goa + Goa-AI + Model starter service and visible eval/ suite
# (when running from a local git checkout with `go run ./cmd/surla init`, pass
# `-surla-version v0.0.0-20261031211028-8ec6e0997e6b`):
surla init -dir /tmp/shop -module example.com/shop

# List all commands and flags:
surla help
```

### 2. Connect Your Coding Agent to the Surla Control Plane (`surla serve`)

Surla organizes development around five checkpoints (**CP1** Confirm Intent → **CP2** Preview Architecture → **CP3** Fast Draft & Sandbox → **CP4** Isolated Verification → **CP5** Promotion Check; see [`docs/HOW_IT_WORKS.md`](docs/HOW_IT_WORKS.md)). Start the standalone Surla control plane and connect your coding assistant over MCP to propose, confirm, and preview a feature change at **CP1** and **CP2** (`surla serve` saves Change Cards in `-cards-dir`, default `.surla/cards`, so confirmed cards survive restarts; add `-verification-config` to enable **CP4** `verify_change` and `get_verification` over MCP, and add `-promotion-config` to bind the **CP5** staged-release `PromotionTarget` for `approve_merge`; standalone `surla serve` binds no `TargetUpdater`, and the [CLI verification walkthrough](examples/orders/README.md#3-run-os-sandboxed-end-to-end-verification-surla-verify) covers standalone `surla verify` and `surla approve`):

```bash
# Start the Surla MCP & HTTP control plane with CP1 model analysis and multi-family swarm profiles
export SURLA_AUTH_TOKEN="dev-secret-token"
export ANTHROPIC_API_KEY="sk-ant-..."
go run ./cmd/surla serve \
  -addr 127.0.0.1:8090 \
  -principal operator@example.com \
  -model-provider anthropic \
  -model-name claude-opus-4-6 \
  -swarm-profiles ./examples/orders/swarm-profiles.example.json
```

Point **Claude Code, Codex, Gemini, or Cursor** at `http://127.0.0.1:8090/mcp` by adding Surla to your project's `.mcp.json` (see [Terminology at a Glance](docs/REFERENCE.md#terminology-at-a-glance) and [`-swarm-profiles` configuration](docs/REFERENCE.md#configure-the-proposed-swarm-roster); [`examples/orders/swarm-profiles.example.json`](examples/orders/swarm-profiles.example.json) is an illustrative operator-edited profile file where `"authenticated": true` is an operator assertion rather than a live credential check, and setting `ANTHROPIC_API_KEY` for CP1 analysis does not authenticate the OpenAI or Google coding-agent CLIs):

```json
{
  "mcpServers": {
    "surla": {
      "type": "http",
      "url": "http://127.0.0.1:8090/mcp",
      "headers": {
        "Authorization": "Bearer dev-secret-token"
      }
    }
  }
}
```

Then prompt your agent:

> *"Using Surla, propose a change to `examples/orders` so VIP customers get instant store-credit refunds up to \$150, then preview the compiled C4 architecture and Chorus swarm plan for builder scope `orders` (`internal/**`)."*

Your agent will create the **Change Card** (`propose_change`, calling `confirm_intent` to answer open ambiguity questions or refine confirmed answers and examples, which locks a SHA-256 `request_revision` fingerprint), compile the candidate Goa contract at **0 LLM tokens** (`preview_architecture`), and return the updated C4 topology, tool guardrails, and a proposed cross-family team roster (`@architect`, `@builder-orders`, `@eval-author`, `@reviewer`). The preview proposes the team; it does not launch agents.

To run the full independent verification suite (`surla verify` and `surla approve`) and compare the full operator report with the builder-redacted `-builder-view` report (where unseen `"holdout"` acceptance tests are redacted to `"id": "holdout-1"`), see the **[Orders Reference Platform Walkthrough (`examples/orders/README.md`)](examples/orders/README.md)**.

---

## Dive Deeper

- **[Orders Reference Platform (`examples/orders/README.md`)](examples/orders/README.md)** — Walk through the Golden Path `orders` platform, run `surla verify` (full vs. `-builder-view`), and watch `TestSeededPayloadBloatFailsTheBudget` catch a token regression.
- **[How Surla Works (`docs/HOW_IT_WORKS.md`)](docs/HOW_IT_WORKS.md)** — Walk through the 5 checkpoints (`CP1`–`CP5`), the proposed cross-model team, fast draft overrides, and OS-isolated verification.
- **[Golden Path Architecture (`docs/ARCHITECTURE_PLAYBOOK.md`)](docs/ARCHITECTURE_PLAYBOOK.md)** — Explore the 7 Golden Path rules that keep Surla platforms maintainable and see how the Goa Design suite powers them.
- **[CLI, MCP & Repo Reference (`docs/REFERENCE.md`)](docs/REFERENCE.md)** — Copy `.mcp.json` configuration, browse the MCP tools and CLI flags, and run the [`examples/orders`](examples/orders/) reference suite.
- **[Architecture Proposal (`PROPOSAL.md`)](PROPOSAL.md)** & **[Product Vision (`PRESS_RELEASE.md`)](PRESS_RELEASE.md)** — Read the full system design, threat model, and Working Backwards press release.

## License

Surla is released under the [MIT License](LICENSE).
