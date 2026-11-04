# Orders Reference Platform (`examples/orders`)

**[← Back to Surla README](../../README.md)** · **[Agent Quickstart (`AGENTS_QUICKSTART.md`)](AGENTS_QUICKSTART.md)** · **[Isolated Acceptance Suite (`../orders-eval`)](../orders-eval/acceptance/acceptance_test.go)**

`examples/orders` is a complete, Golden Path Goa-AI platform that demonstrates why teams build on Surla:

1. **0-token transport & schema compilation (`design/*.go` → `gen/`):** One compact Goa + Goa-AI design compiles the public HTTP service (`orders`), the external `commerce` and `payments` HTTP/JSON service contracts, and the durable `refund` agent (`orders.refund`)—without paying an LLM to write boilerplate handlers, OpenAPI specs, or JSON validators.
2. **Contract-backed security & cost guardrails:**
   - **Anti-spoofing (`Inject` + server lookup scoping):** `Inject("tenant_id", "customer_id")` omits `tenant_id` and `customer_id` from the LLM-visible JSON Schema on every tool in `orders.orders` (`list_orders`, `continue_orders`, `issue_store_credit`, `request_refund`) and marks them as server-supplied fields, while the `orders` service (`internal/orders/service.go`) binds the authenticated caller's tenant/customer identity and scopes `commerce.get_order` lookups so another customer's `order_id` is rejected.
   - **Cursor pagination (`BoundedResult` + page cap + token budget):** `BoundedResult` declares the `returned`/`truncated`/`next_cursor` continuation contract on `list_orders` and `continue_orders`, `internal/orders/service.go` enforces the page limit (`ordersPageSize = 10`), and the evaluator's `within_token_budget` gate catches oversized tool responses.
   - **Human-in-the-loop pause/resume (`Confirmation`):** `request_refund` declares a design-time `Confirmation` gate that pauses the run until a manager holding `refunds:approve` scope approves or denies via `POST /runs/{run_id}/approval` (while customer tokens holding only `orders:chat` receive `403 Forbidden`).
   - **Bounded execution (`RunPolicy`):** Caps `orders.refund` at `MaxToolCalls(8)`, `MaxRecoveryTurns(2)`, and `TimeBudget("2m")`.
3. **OS-sandboxed verification & holdout redaction (`surla verify` + `examples/orders-eval`):** The isolated evaluator builds and runs `orders` and `orders-worker` inside a macOS `sandbox-exec` jail, routes LLM calls through an evaluator-owned gRPC model gateway, kills the worker mid-refund (`kill -9`) to prove payment idempotency, restarts the API server while a refund awaits manager approval, and enforces a hard token/cost budget that catches seeded payload bloat (`TestSeededPayloadBloatFailsTheBudget`).

---

## Architecture at a Glance (`design/model.go`)

```mermaid
flowchart LR
    Customer["Customer / Manager\n(JWT Bearer Token)"] -->|"HTTPS, JSON"| API["Orders API (cmd/orders)\nPublic HTTP API + orders.refund Agent"]
    API -->|"SQLite"| RunStore["Run store\nRuns, Transcripts & Pending Approvals"]
    API -->|"HTTPS, JSON"| Commerce["Commerce system\nCustomer Order Records"]
    API -->|"HTTPS or gRPC Model Gateway"| Model["Language model\nTool Planning & Token Accounting"]
    API -->|"Temporal, gRPC"| Workflow["Workflow engine\nAgent Turns & Refund Workflows"]
    Worker["Refund worker (cmd/orders-worker)\nIdempotent Refund Execution"] -->|"Temporal, gRPC"| Workflow
    Worker -->|"HTTPS, JSON (Idempotency-Key)"| Payments["Payment provider\nStore Credit & Card Refunds"]
```

| Path | Ownership & Purpose |
| :--- | :--- |
| [`design/orders.go`](design/orders.go) | Public `orders` HTTP service (`POST /chat`, `GET /runs/{run_id}`, `POST /runs/{run_id}/approval`, `PUT /prompts/{prompt_id}`), `orders.refund` agent, `orders.orders` toolset (`Inject`, `BoundedResult`, `Confirmation`, `RunPolicy`). |
| [`design/deps.go`](design/deps.go) | External `commerce` and `payments` HTTP/JSON service contracts (`GET /tenants/{tenant_id}/customers/{customer_id}/orders`, `POST /tenants/{tenant_id}/refunds`). |
| [`design/model.go`](design/model.go) | `goa.design/model` C4 architecture containers and relationships rendered to `gen/structurizr/model.json`. |
| [`AGENTS_QUICKSTART.md`](AGENTS_QUICKSTART.md) | Auto-generated contextual map read by coding agents before touching implementation files. |
| [`internal/orders/`](internal/orders/service.go) | Authenticated HTTP service, server-side `tenant_id`/`customer_id` tool methods, `ordersPageSize = 10` pagination, manager approval gate (`POST /runs/{run_id}/approval`), and operator prompt override handler (`PUT /prompts/{prompt_id}` requiring `prompts:manage`). |
| [`internal/assistant/`](internal/assistant/) & [`internal/refundflow/`](internal/refundflow/) | Refund agent planner, runtime prompt override validation and store updates (`assistant.Prompts`), and Temporal refund workflow execution. |
| [`internal/runstore/`](internal/runstore/) | SQLite-backed durable run, transcript, and pending-approval store owned by the Orders API. |
| [`cmd/orders/`](cmd/orders/main.go) & [`cmd/orders-worker/`](cmd/orders-worker/main.go) | Public Orders API server and Refund worker binaries verified by `surla verify`. |
| [`cmd/orders-surla/`](cmd/orders-surla/main.go) | Separate reference `CP3` MCP/HTTP host that binds `controlplane.Options.TargetUpdater` to a running `orders` API's `override_prompt` (`PUT /prompts/{prompt_id}`) endpoint using the pinned `github.com/goadesign/surla` module version in `go.mod`. |

---

## 1. Run Unit Tests, Local Architecture Preview & Structural Diffs (`0` API Keys Needed)

Run the platform's unit tests, Surla's 0-token local architecture preview (`surla preview`), and Surla's structural Goa/Goa-AI design diff (`surla diff`) from the repository root:

```bash
# Run the orders platform unit tests
(cd examples/orders && go test -race ./...)

# Inspect the compiled C4 topology, agent policies, and tool guardrails at 0 LLM tokens
go run ./cmd/surla preview -dir examples/orders

# Compare two Goa design modules structurally at 0 LLM tokens
go run ./cmd/surla diff --old-dir examples/orders --new-dir examples/orders
```

---

## 2. Inspect CP1–CP3 via `surla serve` & the MCP Apps Checkpoint Viewer

Start the Surla control plane with the included illustrative multi-family swarm profile file ([`swarm-profiles.example.json`](swarm-profiles.example.json)). Note that `swarm-profiles.example.json` is an operator-edited example where `"authenticated": true` is an operator assertion (not a live credential probe), and setting `ANTHROPIC_API_KEY` for CP1 analysis does not itself establish credentials for the OpenAI or Google coding-agent CLIs listed in the file:

```bash
export SURLA_AUTH_TOKEN="dev-secret-token"
export ANTHROPIC_API_KEY="sk-ant-..."

go run ./cmd/surla serve \
  -addr 127.0.0.1:8090 \
  -principal operator@example.com \
  -model-provider anthropic \
  -model-name claude-opus-4-6 \
  -swarm-profiles ./examples/orders/swarm-profiles.example.json
```

When an MCP Apps-capable client calls `propose_change`, `confirm_intent`, `get_change`, `preview_architecture`, `diff`, or `draft_override`, Surla pairs the structured tool result with the stateless `ui://surla/checkpoint` (`text/html;profile=mcp-app`) viewer read over MCP `resources/read` (`POST /mcp`; the companion HTTP route `GET /ui/checkpoint` returns the HTML document as a JSON-encoded string). The viewer completes the MCP Apps `ui/initialize` → `ui/notifications/initialized` handshake with the host frame and renders an interactive CP1 Change Card form (`confirm_intent` and `get_change` via host `tools/call` with unsaved-draft preservation on stale `request_revision` conflicts), an inline scrollable C4 SVG directed graph above the inspectable containers/edges, tool guardrails (`Inject`, `BoundedResult`, `Confirmation`), and cross-family Chorus swarm manifest (`design_digest`), or an unbound structural `DiffResult` view (`diff`).

Standalone `surla serve` leaves `controlplane.Options.TargetUpdater` unbound and rejects `prompt_template` overrides. To exercise live `CP3` prompt edits (`draft_override`) against a running `orders` API process (`cmd/orders`) without rebuilding, run the reference `CP3` host [`cmd/orders-surla`](cmd/orders-surla/main.go). Prefer the `SURLA_AUTH_TOKEN` (MCP caller bearer token) and `ORDERS_PROMPTS_TOKEN` (an HS256 JWT signed with the same `ORDERS_JWT_KEY` the `orders` API was started with, carrying `sub`, `tenant_id`, `exp`, and `scope: "prompts:manage"`) environment variables over `-auth-token` and `-orders-token` flags so credentials stay out of command-line arguments:

```bash
export SURLA_AUTH_TOKEN="dev-mcp-secret"
export ORDERS_PROMPTS_TOKEN="<jwt-with-prompts:manage>"

(cd examples/orders && go run ./cmd/orders-surla \
  -listen 127.0.0.1:8091 \
  -principal operator@example.com \
  -orders-url http://127.0.0.1:8080)
```

`orders-surla` is a single-operator `CP3` reference host: it saves Change Cards in `-cards-dir` (default `.surla/orders-surla-cards`, locked exclusively by that `orders-surla` process and reported on startup as `orders-surla: saving Change Cards in <dir>`), and any authenticated MCP caller on that host can call `draft_override` (`target_kind: "prompt_template"`, `prompt_id: "orders.refund.system"`) to update the target `orders` process's in-memory prompt store and receive the accepted text's `sha256:...` version (`compiled_rule` edits are still rejected with `requires_rebuild: true` without calling `orders`). It leaves `CP1` model analysis, `CP4` verification, and `CP5` `PromotionTarget` (`approve_merge`) unconfigured, and it consumes the pinned `github.com/goadesign/surla` version in [`examples/orders/go.mod`](go.mod) (`v0.0.0-20261104215521-580509970fb6`), so later `internal/controlplane` changes reach `orders-surla` after bumping that pinned module version.

---

## 3. Run OS-Sandboxed End-to-End Verification (`surla verify`)

`surla verify` checks out the exact committed `CandidateCommit` and `SuiteCommit` (uncommitted working-tree edits are never tested), builds `orders` and `orders-worker` inside an OS sandbox (`WorkDir/candidate-bin/`), runs the `orders_acceptance` suite in [`examples/orders-eval`](../orders-eval/design/suite.go) (`WorkDir/suite-bin/suite`), and writes the cryptographically bound `verify.Report` JSON:

| Scenario (`orders_acceptance`) | Tag | What It Proves |
| :--- | :--- | :--- |
| `store_credit_within_limit` | `repair` | Order totalling `\$38.50` with tax is refunded as instant store credit without a manager (`0` card payment writes). |
| `card_refund_needs_manager` | `repair` | Order totalling `\$54.00` including shipping pauses at `Confirmation`; customer and cross-tenant tokens cannot approve it (`403`), and that tenant's manager approval yields `1` card refund. |
| `refund_survives_worker_crash` | `repair` | Killing `orders-worker` (`SIGKILL` / `kill -9`) right after the payment provider accepts a refund still yields `1` refund and a completed run on replay. |
| `pending_approval_survives_api_restart` | `repair` | A refund waiting for a manager survives killing and restarting the `orders` API process; subsequent manager approval still yields `1` card refund. |
| `other_customers_order_refused` | **`holdout`** | A customer asking to refund another customer's order is refused by server-bound `tenant_id`/`customer_id` lookup scoping (`0` writes). |

Run the full evaluator report from the repository root:

```bash
COMMIT="$(git rev-parse HEAD)"
WORK_DIR="$(mktemp -d)"

# Full operator/evaluator view (includes holdout scenario details)
go run ./cmd/surla verify \
  -candidate-repo . \
  -candidate-commit "$COMMIT" \
  -candidate-module examples/orders \
  -binary orders=./cmd/orders \
  -binary orders-worker=./cmd/orders-worker \
  -suite-repo . \
  -suite-commit "$COMMIT" \
  -suite-module examples/orders-eval \
  -suite-command ./cmd/orders_acceptance-evals \
  -request-revision rev-orders-demo \
  -work-dir "$WORK_DIR/work" \
  -report-dir "$WORK_DIR/reports"
```

To see the **builder-redacted report** (`Report.ForBuilders()`), pass `-builder-view`:

```bash
go run ./cmd/surla verify \
  -candidate-repo . \
  -candidate-commit "$COMMIT" \
  -candidate-module examples/orders \
  -binary orders=./cmd/orders \
  -binary orders-worker=./cmd/orders-worker \
  -suite-repo . \
  -suite-commit "$COMMIT" \
  -suite-module examples/orders-eval \
  -suite-command ./cmd/orders_acceptance-evals \
  -request-revision rev-orders-demo \
  -work-dir "$WORK_DIR/work-builder" \
  -report-dir "$WORK_DIR/reports-builder" \
  -builder-view
```

In `-builder-view`, stdout prints `Report.ForBuilders()` where the `"holdout"` scenario is redacted to `"id": "holdout-1"` with all scenario inputs, declarations, checks, and failure logs stripped. *(Note: in this public reference repo, `examples/orders-eval` is checked in alongside `examples/orders` to demonstrate the mechanism; in production, actual unseen holdouts require keeping the evaluator suite repository (`-suite-repo`), `-work-dir`, and `-report-dir` unreadable to builder agents, since `ForBuilders()` redacts the report JSON while workspace boundaries protect the suite source and raw evidence on disk.)*

Once `surla verify` writes the full report to `"$WORK_DIR/reports/<digest>.json"`, run `surla approve` (`verify.Approve`) to re-derive the report from its raw evidence and verify that the candidate commit, confirmed `request_revision`, evaluator suite, environment, and measured candidate binary SHA-256 digests match the change being promoted:

```bash
REPORT_FILE="$(ls "$WORK_DIR/reports/"*.json | head -n 1)"
ORDERS_SHA="$(shasum -a 256 "$WORK_DIR/work/candidate-bin/orders" | awk '{print $1}')"
WORKER_SHA="$(shasum -a 256 "$WORK_DIR/work/candidate-bin/orders-worker" | awk '{print $1}')"

go run ./cmd/surla approve \
  -report "$REPORT_FILE" \
  -candidate-commit "$COMMIT" \
  -request-revision rev-orders-demo \
  -suite-id orders_acceptance \
  -suite-commit "$COMMIT" \
  -environment sim \
  -artifact "orders=$ORDERS_SHA" \
  -artifact "orders-worker=$WORKER_SHA"
```

To expose `CP4` (`verify_change` / `get_verification`) and `CP5` (`approve_merge`) over MCP on `surla serve`, pass `-verification-config` and `-promotion-config` pointing to JSON files with the repository root, `orders_acceptance` suite commit, report/run directories, and staged release directory (`<release_dir>/<candidate_commit>/{orders,orders-worker}`):

```bash
cat > "$WORK_DIR/verification.json" <<EOF
{
  "runs_dir": "$WORK_DIR/runs",
  "report_dir": "$WORK_DIR/reports",
  "candidate_repo": "$(pwd)",
  "candidate_module": "examples/orders",
  "binaries": {
    "orders": "./cmd/orders",
    "orders-worker": "./cmd/orders-worker"
  },
  "suite_repo": "$(pwd)",
  "suite_commit": "$COMMIT",
  "suite_module": "examples/orders-eval",
  "suite_command": "./cmd/orders_acceptance-evals",
  "timeout": "30m"
}
EOF

cat > "$WORK_DIR/promotion.json" <<EOF
{
  "report_dir": "$WORK_DIR/reports",
  "release_dir": "$WORK_DIR/releases",
  "suite_id": "orders_acceptance",
  "suite_commit": "$COMMIT",
  "environment": "sim",
  "binaries": ["orders", "orders-worker"]
}
EOF
```

---

## 4. See the Token Budget Catch Seeded Payload Bloat

Want to watch Surla reject a functional change that regresses LLM token efficiency? Run the acceptance suite directly:

```bash
(cd examples/orders-eval && go test -race -tags e2e -v ./acceptance)
```

In addition to `TestAcceptanceSuite`, this runs `TestSeededPayloadBloatFailsTheBudget`, which rebuilds the candidate with a Go `-overlay` replacing `ordersPageSize = 10` with `ordersPageSize = 50` in [`internal/orders/service.go`](internal/orders/service.go#L73) and proves that the evaluator's gRPC model gateway fails `within_token_budget` (input tokens jump from `2,563` to `4,193`, exceeding `refundBudget`'s `3,100` input-token / `\$0.011200` cap in [`examples/orders-eval/acceptance/budget.go`](../orders-eval/acceptance/budget.go#L56)) even though every functional refund assertion still passes.
