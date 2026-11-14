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
| [`design/orders.go`](design/orders.go) | Public `orders` HTTP service (`POST /chat`, `GET /runs/{run_id}`, `POST /runs/{run_id}/approval`), the `prompts` service a live copy serves to Surla (`PUT /prompts/{prompt_id}`), `orders.refund` agent, `orders.orders` toolset (`Inject`, `BoundedResult`, `Confirmation`, `RunPolicy`). |
| [`design/deps.go`](design/deps.go) | External `commerce` and `payments` HTTP/JSON service contracts (`GET /tenants/{tenant_id}/customers/{customer_id}/orders`, `POST /tenants/{tenant_id}/refunds`). |
| [`design/model.go`](design/model.go) | `goa.design/model` C4 architecture containers and relationships rendered to `gen/structurizr/model.json`. |
| [`AGENTS_QUICKSTART.md`](AGENTS_QUICKSTART.md) | Auto-generated contextual map read by coding agents before touching implementation files. |
| [`internal/orders/`](internal/orders/service.go) | Authenticated HTTP service, server-side `tenant_id`/`customer_id` tool methods, `ordersPageSize = 10` pagination, manager approval gate (`POST /runs/{run_id}/approval`), and the live copy's prompt operation (`PromptService`, `PUT /prompts/{prompt_id}` requiring the live key Surla passes in `SURLA_LIVE_TARGET_KEY`). |
| [`internal/assistant/`](internal/assistant/) & [`internal/refundflow/`](internal/refundflow/) | Refund agent planner, runtime prompt override validation and store updates (`assistant.Prompts`), and Temporal refund workflow execution. |
| [`internal/runstore/`](internal/runstore/) | SQLite-backed durable run, transcript, and pending-approval store owned by the Orders API. |
| [`cmd/orders/`](cmd/orders/main.go) & [`cmd/orders-worker/`](cmd/orders-worker/main.go) | Public Orders API server and Refund worker binaries verified by `surla verify`. |

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

## 2. Try Prompt Drafts on a Live Copy (`start_live_target` & `draft_override`)

Surla changes the refund agent's prompts only in a live copy it builds and
runs itself, never in an Orders process you started. With the verification
settings from section 3 stored, the builder calls `start_live_target` with a
commit: the project server builds `orders` and `orders-worker` from that commit
as verification does, and the evaluator suite's `--live` mode starts them with
simulated commerce and payments, a Temporal dev server, and the evaluator's
scripted model. `draft_override` then sends new text for
`orders.refund.system` to that copy's `prompts` service with the key Surla
minted for it, and returns the accepted text's `sha256:` version. A draft with
no live copy running is refused with `no_live_target`.

Drafts are not verified and end with the copy. To keep a wording, edit the
agent's `SystemPrompt` in `design/orders.go`, regenerate, commit, and verify
that commit. The scripted model needs no provider key and answers from a fixed
script, so a draft changes the prompt text and version the copy's model
requests carry, not its replies. The copy's chat API needs bearer tokens
signed with a key only the evaluator suite holds.

Run the whole journey against the committed `HEAD` from the repository root
(it needs the `temporal` CLI and macOS):

```bash
go test -race -count=1 -tags e2e -run TestOrdersLiveCopyAppliesADraftThroughTheProjectServer ./cmd/surla
```

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
  -report-dir "$WORK_DIR/reports" \
  -builder-view=false
```

The **builder-redacted report** (`Report.ForBuilders()`) is the default. The explicit `-builder-view` below also requests it:

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

By default, stdout prints `Report.ForBuilders()` where the `"holdout"` scenario is redacted to `"id": "holdout-1"` with all scenario inputs, declarations, checks, and failure logs stripped. In either view, a run that stops with a passing partial report or no report still prints raw operator diagnostics to stderr; do not share those diagnostics with builder agents. *(Note: in this public reference repo, `examples/orders-eval` is checked in alongside `examples/orders` to demonstrate the mechanism; in production, actual unseen holdouts require keeping the evaluator suite repository (`-suite-repo`), `-work-dir`, and `-report-dir` unreadable to builder agents, since `ForBuilders()` redacts the report JSON while workspace boundaries protect the suite source and raw evidence on disk.)*

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

To configure the project's independent verification and promotion inputs, write the following operator files outside the workspace. Surla owns run/report storage; these settings name the repositories, evaluator commit and staged release directory:

```bash
cat > "$WORK_DIR/verification.json" <<EOF
{
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
  "release_dir": "$WORK_DIR/releases",
  "binaries": ["orders", "orders-worker"]
}
EOF

(
  cd examples/orders
  surla verification set "$WORK_DIR/verification.json"
  surla promotion set "$WORK_DIR/promotion.json"
  surla
)
```

These commands use a `surla` built from this checkout
(`go install ./cmd/surla` from the repository root), and the subshell leaves
your shell in the repository root for the next section. Settings apply at the
next project-server start. The user confirms changes and approves results in
the views page; agent tools cannot make those decisions. See the
[project server reference](../../docs/REFERENCE.md#surla-serve), which also
names the Herdr prerequisite for starting `surla`.

---

## 4. See the Token Budget Catch Seeded Payload Bloat

Want to watch Surla reject a functional change that regresses LLM token efficiency? Run the acceptance suite directly:

```bash
(cd examples/orders-eval && go test -race -tags e2e -v ./acceptance)
```

In addition to `TestAcceptanceSuite`, this runs `TestSeededPayloadBloatFailsTheBudget`, which rebuilds the candidate with a Go `-overlay` replacing `ordersPageSize = 10` with `ordersPageSize = 50` in [`internal/orders/service.go`](internal/orders/service.go#L73) and proves that the evaluator's gRPC model gateway fails `within_token_budget` (input tokens jump from `2,563` to `4,193`, exceeding `refundBudget`'s `3,100` input-token / `\$0.011200` cap in [`examples/orders-eval/acceptance/budget.go`](../orders-eval/acceptance/budget.go#L56)) even though every functional refund assertion still passes.
