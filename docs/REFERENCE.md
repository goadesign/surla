# CLI, MCP & Repository Reference

**[← Back to README](../README.md)** · **[← How Surla Works](HOW_IT_WORKS.md)** · **[← Golden Path Architecture](ARCHITECTURE_PLAYBOOK.md)**

---

## Terminology at a Glance

| Term | Plain-English Meaning |
| :--- | :--- |
| **`CP1` – `CP5`** | Surla's five verification checkpoints: **CP1** Confirm Intent (`propose_change`, `confirm_intent`), **CP2** Preview Compiled Architecture & Swarm (`preview_architecture`, or local `surla preview`), **CP3** Fast Draft & Live Sandbox (`draft_override`), **CP4** Isolated Sandboxed Verification (`verify_change`, `get_verification`, `surla verify`), and **CP5** Promotion Check (`surla approve`, `approve_merge`). |
| **`Change Card`** | The versioned change plan (`ChangeCardResult`) created at `CP1` and saved on disk under `controlplane.Options.CardsDir` (`surla serve -cards-dir`, default `.surla/cards`) as `<sha256(change_id)>.json`, holding concrete input/outcome examples, open domain ambiguity questions, confirmed answers, preserved contract behaviors, and optional `CP5` approval metadata. |
| **`request_revision`** | The SHA-256 hex digest of a Change Card's canonical content. `preview_architecture`, `verify_change`, `surla verify`, `surla approve`, and `approve_merge` bind to `request_revision` so changing the agreed examples or answers automatically invalidates stale previews, reports, and approvals. When running standalone `surla verify` and `surla approve` on the CLI without a control-plane Change Card, pass any explicit revision string (such as a commit or label) consistently to both commands. |
| **`holdout`** | An unseen acceptance scenario (tagged `"holdout"` in the evaluator suite) whose name, inputs, checks, and logs are redacted to `"id": "holdout-N"` in `verify_change` / `get_verification` and `surla verify -builder-view` (`Report.ForBuilders()`) so coding agents cannot hardcode around the test cases. |
| **`input_binding`** | The generated input schemas and compiler-listed `injected_fields` for a tool used by an agent (`ToolInputBindingItem`). It describes the declared input choices, not which option the running application installed. |
| **`CardsDir` / `TargetUpdater` / `PromotionTarget` / `Verification`** | Configuration in `controlplane.Options`: `CardsDir` (required by `controlplane.New(opts)`, locked exclusively via `<cards_dir>/lock` until `(*ControlPlane).Close()`) is the directory where the control plane saves its `CP1` Change Cards (`surla serve -cards-dir`, default `.surla/cards`); `TargetUpdater` applies in-memory `CP3` prompt overrides; `PromotionTarget` (`surla serve -promotion-config`) loads `CP4` verification reports and current promotion inputs for `approve_merge`, `get_change`, and no-op `confirm_intent` (approving staged binaries in `<release_dir>/<commit>/` for a fresh launch on `surla serve`, or failing `CurrentInputs` on a live host whose behavior is not fixed by its reported suite, environment, and binary digests); and `Verification` (`*VerificationConfig`, loaded on `surla serve` via `-verification-config`) configures the operator-owned `CP4` verification target for `verify_change` and `get_verification`. |

---

## Quickstart & MCP Configuration

### Prerequisites
- **Go 1.27.3+**, matching the repository's `go.mod` files (`surla init` runs module-pinned `go run goa.design/goa/v3/cmd/goa gen`, and `surla preview` uses Surla's sandboxed evaluator, so a globally installed `goa` binary is not required)
- **Surla CLI** (`go install github.com/goadesign/surla/cmd/surla@latest`, or `go run ./cmd/surla` from a repository checkout)
- **macOS** (`sandbox-exec` is used by `verify/candidate` to isolate candidate builds and processes; non-macOS hosts fail closed with `ErrIsolationUnavailable`)
- **Temporal CLI** (`brew install temporal`) for running the end-to-end durable workflow crash and replay verification suites

### Start the Surla Server

To enable feature-request analysis, set `ANTHROPIC_API_KEY` and set `SURLA_MODEL_NAME` to a model ID available to that key, then run (with `surla serve` after `go install`, or `go run ./cmd/surla serve` from a repository checkout):

```bash
export SURLA_AUTH_TOKEN="dev-secret-token"
surla serve -addr 127.0.0.1:8090 \
  -principal operator@example.com \
  -model-provider anthropic -model-name "$SURLA_MODEL_NAME"
```

For OpenAI, use `-model-provider openai`, set `OPENAI_API_KEY`, and choose a model ID available to that key. Alternatively, use `-model-gateway-addr <host:port>` alone to call an existing model gateway; the gateway chooses the model, must run a compatible `goa.design/goa-ai` gateway protocol version matching `go.mod` (`v0.90.0`, which updated the `GatewayErrorNode` protobuf wire schema), and must be upgraded or rolled back together with `surla serve`. That connection uses plaintext gRPC and is intended for loopback or a trusted network.

Omitting all model flags starts the server without feature-request analysis. The `diff` tool, Golden Path resource, and `compiled_rule` rebuild response still work; `propose_change` returns `cp1 semantic analyzer is required`.

<a id="register-surla-in-mcpjson"></a>
### Register Surla in Your MCP Client

Register `http://127.0.0.1:8090/mcp` in your coding agent's MCP configuration file:

- **Claude Code** — save `.mcp.json` in the project root (see the [Claude Code MCP documentation](https://code.claude.com/docs/en/mcp#project-scope)):

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

- **Cursor** — save `.cursor/mcp.json` in the project or `~/.cursor/mcp.json` globally (see the [Cursor MCP documentation](https://cursor.com/help/customization/mcp)):

  ```json
  {
    "mcpServers": {
      "surla": {
        "url": "http://127.0.0.1:8090/mcp",
        "headers": {
          "Authorization": "Bearer dev-secret-token"
        }
      }
    }
  }
  ```

- **Gemini CLI** — save `.gemini/settings.json` in the project or `~/.gemini/settings.json` globally, using `httpUrl` for streamable HTTP (`url` selects SSE in Gemini CLI; see the [Gemini CLI MCP documentation](https://geminicli.com/docs/tools/mcp-server/)):

  ```json
  {
    "mcpServers": {
      "surla": {
        "httpUrl": "http://127.0.0.1:8090/mcp",
        "headers": {
          "Authorization": "Bearer dev-secret-token"
        }
      }
    }
  }
  ```

- **Codex** — export `SURLA_AUTH_TOKEN="dev-secret-token"` in the shell where you launch Codex and register Surla in `~/.codex/config.toml` or a trusted project's `.codex/config.toml` (see the [Codex MCP documentation](https://learn.chatgpt.com/docs/extend/mcp?surface=cli)):

  ```toml
  [mcp_servers.surla]
  url = "http://127.0.0.1:8090/mcp"
  bearer_token_env_var = "SURLA_AUTH_TOKEN"
  ```

### Configure the Proposed Swarm Roster

To include agent assignments in an architecture preview, pass `-swarm-profiles ./swarm-profiles.json` when starting the server. Save a JSON array with these fields, replacing the model placeholders with IDs available to your coding agents:

```json
[
  {
    "id": "refund-builder",
    "provider": "anthropic",
    "model": "YOUR_ANTHROPIC_MODEL",
    "family": "claude",
    "authenticated": true,
    "capabilities": ["builder"]
  },
  {
    "id": "independent-review",
    "provider": "openai",
    "model": "YOUR_OPENAI_MODEL",
    "family": "gpt",
    "authenticated": true,
    "capabilities": ["architect", "eval_author", "reviewer"]
  }
]
```

These profiles describe coding agents separately from the CP1 analysis model. `authenticated: true` is the operator's assertion that the profile has usable credentials; the command does not test those credentials or discover installed coding agents. Keep credentials out of this file. Profiles from the same model family must use the same `family` value.

After confirming a Change Card, include explicit builder ownership in the `preview_architecture` request. For example, a builder for the orders service can supply this `builders` field alongside `change_id`, `request_revision`, and `design`:

```json
{
  "builders": [
    {
      "name": "orders",
      "paths": ["internal/orders/**"],
      "preferred_profile_id": "refund-builder"
    }
  ]
}
```

Paths are relative to the target repository and must cover the files assigned to that builder. With no eligible profiles, the architecture preview returns the evaluated design with an `unconfigured` roster. If eligible profiles lack a required role, it returns `missing_capability`. Once all roles are covered, omitting builder scopes returns `unconfigured` with `builder` listed in `missing_roles`.

A configured roster reports its actual `cross_family_review` and `cross_family_eval` values; family choices and capabilities determine whether those roles differ from every builder. The preview returns proposed assignments and manifest text; it does not launch agents or publish a Chorus manifest.

---

## MCP Tools & Resources

All tools and resources are served over MCP JSON-RPC at `POST /mcp` and over HTTP REST on the same listen address (authenticated via `Authorization: Bearer <token>`).

### MCP Tools

- **`propose_change`** (`POST /changes/propose`)
  - **Input:** `change_id`, `prompt`, `design_source?` → **Output:** `ChangeCardResult`
  - Analyzes a feature request at `CP1` using the embedded Golden Path prompt, saving the card (`card`, `owner`, `design_source`) atomically as `<cards_dir>/<sha256(change_id)>.json` under `controlplane.Options.CardsDir` and returning concrete `examples`, `ambiguities`, `preserved_behaviors`, and a canonical `request_revision` digest. When the model also composed a decision view, the card carries `ui_composition` (`program`, `composed_for_revision`): it is presentation only, never hashed into `request_revision`, and dropped when `confirm_intent` changes answers or examples. When the request includes a `progressToken`, the tool sends MCP progress updates before and after the model call.
- **`confirm_intent`** (`POST /changes/confirm`)
  - **Input:** `change_id`, `request_revision`, `answers`, `examples` → **Output:** `ChangeCardResult`
  - Confirms a `needs_confirmation` Change Card or refines an already `confirmed` Change Card at `CP1` when called by the owning principal with the current `request_revision` and complete `answers` and `examples`. No-op submissions keep the stored `request_revision` and include a recorded `approval` only when `PromotionTarget` is bound and `PromotionTarget.CurrentInputs(ctx, approval.CandidateCommit)` still matches its `suite_id`, `suite_commit`, `environment`, and `artifacts` (omitting `approval` on a shallow response copy when `PromotionTarget` is nil or inputs differ without modifying the saved card, and returning an error if `CurrentInputs` fails); changed answers or examples mint a new canonical `request_revision` digest, clear `approval`, and save the updated card in `Options.CardsDir`.
- **`get_change`** (`GET /changes/{change_id}`)
  - **Input:** `change_id` → **Output:** `ChangeCardResult`
  - Reads the saved `ChangeCardResult` for `change_id` from `Options.CardsDir` (verifying its canonical `request_revision` digest on load) without mutating saved state (restricted to the owning principal) so a stale editor can fetch the latest saved card after a `request_revision` conflict. When the saved card has a recorded `approval`, `get_change` omits `approval` on a shallow response copy if `PromotionTarget` is nil, or checks `PromotionTarget.CurrentInputs(ctx, approval.CandidateCommit)` and includes `approval` only when `suite_id`, `suite_commit`, `environment`, and `artifacts` still match (returning a card copy with `approval` omitted when they differ, and failing if `CurrentInputs` returns an error).
- **`verify_change`** (`POST /changes/{change_id}/verifications`)
  - **Input:** `change_id`, `request_revision`, `candidate_commit` → **Output:** `VerificationStatusResult`
  - Starts a Checkpoint 4 (`CP4`) verification run for `candidate_commit` against the operator-configured evaluator suite (`controlplane.Options.Verification` / `surla serve -verification-config`) when called by the owner of a `confirmed` Change Card at its current `request_revision`. The run is accepted into `<runs_dir>/<run_id>/`, carried out by a detached supervised process group (`internal/verifyrun`) that outlives the HTTP/MCP call and server restarts, and immediately returned with `state.type == "running"`. When verification is not configured on the control plane, it returns `verification is not configured on this control plane`.
- **`get_verification`** (`GET /verifications/{run_id}`)
  - **Input:** `run_id` → **Output:** `VerificationStatusResult`
  - Reads the state of verification run `run_id` from `<runs_dir>/<run_id>/` (restricted to the principal recorded as the run's `owner` in `request.json`, independent of current Change Card state so runs remain readable after subsequent card edits or server restarts). The `state` union reports `"running"` (`started_at`), `"completed"` (`suite_id`, `environment`, and `report` projected through `Report.ForBuilders()` so holdout scenario names, checks, and diagnostics never leave the evaluator), or `"not_completed"` (`kind`: `"execution_failed"` \| `"interrupted"`, `stage`: `"checkout"` \| `"build"` \| `"describe"` \| `"suite"` \| `"report"` \| `"supervise"`, and a builder-safe `summary` with full command output kept in `<runs_dir>/<run_id>/executor.log`).
- **`approve_merge`** (`POST /changes/approve`)
  - **Input:** `change_id`, `request_revision`, `candidate_commit`, `report_digest` → **Output:** `ChangeCardResult`
  - Records Checkpoint 5 (`CP5`) promotion approval on the owning principal's confirmed `ChangeCardResult` when `controlplane.Options.PromotionTarget` is bound (`surla serve -promotion-config <path.json>`). It loads the full `verify.Report` for `report_digest` and the current `PromotionInputs` (`SuiteID`, `SuiteCommit`, `Environment`, measured candidate binary `Artifacts`) for `candidate_commit` from `PromotionTarget`, runs `verify.Approve`, re-verifies that the card still has `request_revision`, and saves and returns the updated `ChangeCardResult` with `approval` (`ApprovalItem`: `approved_by`, `report_digest`, `candidate_commit`, `suite_id`, `suite_commit`, `environment`, `artifacts`, `scenario_count`) in `Options.CardsDir`. Recording approval does not merge branches or deploy binaries. When `-promotion-config` is omitted on `surla serve`, no `PromotionTarget` is bound and `approve_merge` requests return `no promotion target is bound to this control plane`.
- **`preview_architecture`** (`POST /changes/preview`)
  - **Input:** `change_id`, `request_revision`, `design`, `builders?` → **Output:** `ArchitecturePreviewResult`
  - Evaluates a candidate `DesignSnapshot` in a sandboxed subprocess at `CP2`, returning C4 `containers` and `edges`, `agent_policies`, `tools` (`input_binding`), and the proposed `swarm` plan. When the request includes a `progressToken`, the tool sends MCP progress updates as design evaluation and roster planning start and finish.
- **`draft_override`** (`POST /sandbox/draft`)
  - **Input:** `target_kind` (`prompt_template` \| `compiled_rule`), `prompt_id?`, `new_content` → **Output:** `DraftOverrideResult`
  - Applies a `prompt_template` to a target configured through `controlplane.Options.TargetUpdater`, without rebuilding Go code, or reports that a `compiled_rule` requires a rebuild. The `serve` command does not bind a prompt updater, so its `prompt_template` requests return `no target prompt updater is bound to this control plane`. For the `examples/orders` platform, [`examples/orders/cmd/orders-surla`](../examples/orders/cmd/orders-surla/main.go) provides a separate reference `CP3` host that saves Change Cards in `-cards-dir` (default `.surla/orders-surla-cards`) and binds `TargetUpdater` to `PUT /prompts/{prompt_id}` using the `SURLA_AUTH_TOKEN` (MCP) and `ORDERS_PROMPTS_TOKEN` (`prompts:manage`) environment variables and the pinned `github.com/goadesign/surla` module version in `examples/orders/go.mod` (leaving `CP1` analysis, `CP4` verification, and `CP5` promotion unconfigured).
- **`diff`** (`POST /diff`)
  - **Input:** `old` (`DesignSnapshot`), `new` (`DesignSnapshot`) → **Output:** `DiffResult`
  - Compares two `DesignSnapshot` modules by evaluating their generated Goa/Goa-AI artifacts, tool schemas, and `go.mod` directives, paired with `ToolUI("ui://surla/checkpoint")` for structural diff visualization.

### MCP Resources

- **`surla://golden-path`** (`text/markdown` via MCP `resources/read` on `POST /mcp`; JSON-encoded string on `GET /golden-path`)
  - Serves the embedded Golden Path architecture playbook ([`internal/goldenpath/GOLDEN_PATH.md`](../internal/goldenpath/GOLDEN_PATH.md)) for Goa and Goa-AI platforms.
- **`ui://surla/checkpoint`** (`text/html;profile=mcp-app` via MCP `resources/read` on `POST /mcp`; JSON-encoded string on `GET /ui/checkpoint`)
  - Serves the stateless MCP Apps checkpoint viewer (`internal/controlplane/checkpoint.html`, built from [`ui/checkpoint/`](../ui/checkpoint/)). It completes the `ui/initialize` → `ui/notifications/initialized` host handshake and renders results delivered by the host via `ui/notifications/tool-result`, without server-retained preview state: the `CP1` Change Card form (saving through `confirm_intent` and recovering with `get_change` via host `tools/call`, keeping unsaved edits in `#surla-preserved-draft` across stale-revision conflicts or when switching to an incoming Change Card via `#surla-replace-incoming-card`, rendering the `#surla-cp5-approval` panel for a recorded approval check when `approval` is present on the card, and refreshing a retained editor card via `get_change` after an applied `CP3` override without losing unsaved draft edits), the optional OpenUI decision view from `ui_composition` (labeled as a model-proposed illustration and sharing the form's draft), `CP2` architecture previews (a keyboard-focusable scrollable C4 graph region above inspectable lists and an expandable `#surla-cp2-swarm-manifest` Chorus manifest), unbound structural diffs, `CP3` draft results, and `CP4` verification results (`verify_change` and `get_verification`, with `#surla-cp4-verification` marked `aria-busy="true"` while a correlated `Check status` read is in flight and full `ForBuilders()` scenario/check counts and binding details when `completed`, while preserving any dirty `CP1` draft). Interactive role-switching and replay controls, and `openui.v1` action dispatch, remain planned; see [`docs/HOW_IT_WORKS.md`](HOW_IT_WORKS.md#checkpoint-3-cp3-fast-draft-overrides--live-sandbox-walkthrough).

---

## CLI Reference

Running `surla`, `surla help`, `surla -h`, or `surla --help` prints top-level usage to `stdout` and exits `0`. Running `surla help <command>` or `surla <command> -h` prints that subcommand's flags to `stderr` and exits `0`. Every subcommand rejects unexpected positional arguments (`unexpected arguments [...]`), so a stray non-flag token never causes Go's `flag` parser to silently ignore flags that follow it.

### `surla init`
Creates a Goa + Goa-AI + `goa.design/model` service in `<dir>` and a separate evaluator module in `<dir>/eval`. It resolves dependencies, generates code with `go run goa.design/goa/v3/cmd/goa gen`, checks both modules with `go vet ./...`, and prints the commands to commit, preview, and verify the project. Both module files start with the Go and library versions saved in the `surla` binary (`surla.GoMod()`). Before generating the evaluator, `surla init` pins its exact Surla dependency with `go get github.com/goadesign/surla@<version>`; Go may select newer library versions required by that dependency.
- `-dir <path>` — Directory to create; it must not exist *(required)*. `surla init` builds the project in a sibling staging directory (`<dir>.surla-init-*`) under `procgroup.Run` (`GOWORK=off`) and renames it to `-dir` only after every step succeeds (removing the staging directory on any command failure or cancellation when the process group exits cleanly, or keeping it and reporting its path if a child process fails to stop).
- `-module <path>` — Go module path of the candidate service, such as `example.com/shop` *(required)*. Its last path element names the service and must be a lowercase Go identifier (`^[a-z][a-z0-9]*$`, excluding Go keywords and `main`).
- `-surla-version <version>` — Exact canonical semantic version (`golang.org/x/mod/semver`) of `github.com/goadesign/surla` required by `<dir>/eval/go.mod` (default: the build-info `Main.Version` stamped into the `surla` binary; required explicitly when running from a `(devel)` or `+dirty` build, and mutable labels such as `latest` or `main` are rejected).
- **Starter scope & visible-suite note:** The generated candidate (`cmd/<name>`) runs an in-memory Goa-AI `helper` agent (`RunPolicy`, `BindTo("price")`, and C4 `System`/`Container` model) connecting to `-model-gateway <host:port>`, registering `helper.SystemPrompt()` (`<name>.helper.system`, version `"v1"`) on each question's runtime and attaching `PromptRefs: []prompt.PromptRef{system.Ref}` on every model request. The generated `<dir>/eval/` module runs a scripted `goa-ai` model gateway and two starter scenarios (`prices_tea` tagged `"repair"` and `unknown_item` tagged `"holdout"`, checking the correlated `catalog.lookup_price` tool call and `ToolResultPart` exchange plus `pinned.Evidence("<name>")`). Because `eval/` lives inside the same repository as the starter code, its `"holdout"` tag demonstrates `Report.ForBuilders()` redaction only; real unseen holdouts belong in an evaluator repository that builder agents cannot read.

### `surla preview`
Reads a Go design module on disk into a symlink-safe snapshot (`go.mod`, `go.sum`, and non-test `.go` files; run `go mod tidy` first if `go.sum` is missing), evaluates it inside the sandboxed Goa/Goa-AI/Model compiler (`designeval.Evaluate`) at **0 LLM tokens**, and prints the indented JSON `designeval.Result` (`system_name`, `declarations`, C4 `containers` and `edges`, `agent_policies`, and `tools`) to `stdout`. Needs no running server, no Change Card, and no LLM API key.
- `-dir <path>` — Root directory of the design module holding `go.mod`, `go.sum`, and `design/` (default `.`).

### `surla serve`
Starts the Surla HTTP and MCP JSON-RPC control-plane server protected by bearer-token authentication. Once the listener is bound, `surla serve` prints a startup summary to `stderr` showing the bound HTTP URL, the MCP endpoint (`http://<addr>/mcp`), the Change Cards directory (`surla: saving Change Cards in <dir>`), and the on/off status of each checkpoint (`CP1`–`CP5`) along with the flag or host option that turns each off capability on (the bearer token is never printed in startup logs or `-h` defaults). On shutdown, `surla serve` closes the control plane (`cp.Close()`) to release its exclusive lock on `-cards-dir`.
- `-addr <host:port>` — Listen address (default `127.0.0.1:8090`).
- `-principal <id>` — Authenticated operator principal ID *(required)*.
- `-auth-token <token>` — Bearer token required on HTTP and MCP requests (default: `$SURLA_AUTH_TOKEN`, *required*).
- `-cards-dir <path>` — Directory where `CP1` Change Cards are saved as `<sha256(change_id)>.json` so they survive restarts (default `.surla/cards`). One `surla serve` process at a time may use a given directory, enforced by an exclusive `flock` on `<cards-dir>/lock`.
- `-model-gateway-addr <host:port>` — Optional model gateway for CP1 analysis over plaintext gRPC. The gateway chooses the model and must run the `goa.design/goa-ai` gateway protocol version matching `go.mod` (`v0.90.0`; upgrade or roll back the gateway and `surla serve` together); cannot be combined with `-model-provider` or `-model-name`.
- `-model-provider <anthropic|openai>` — Optional direct provider for CP1 analysis. Requires `-model-name` and the matching `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` environment variable. Provider names must match exactly.
- `-model-name <model>` — Model ID for direct provider mode; required with `-model-provider` and rejected in gateway mode.
- `-swarm-profiles <path.json>` — Optional JSON array of configured coding-agent profiles for proposed CP2 assignments; see [Configure the Proposed Swarm Roster](#configure-the-proposed-swarm-roster).
- `-verification-config <path.json>` — Optional JSON file configuring Checkpoint 4 (`CP4`) verification for `verify_change` and `get_verification`. All fields are required (`runs_dir`, `report_dir`, `candidate_repo`, `candidate_module`, `binaries`, `suite_repo`, `suite_commit`, `suite_module`, `suite_command`, and `timeout` as a positive Go duration such as `"30m"`), and unknown fields or trailing data stop startup. When omitted, `verify_change` and `get_verification` return `verification is not configured on this control plane`.
- `-promotion-config <path.json>` — Optional JSON file configuring the Checkpoint 5 (`CP5`) staged-release `PromotionTarget` for `approve_merge`. All fields are required (`report_dir`, `release_dir`, `suite_id`, `suite_commit` as a 40- or 64-character lowercase hex commit ID, `environment`, and non-empty distinct artifact names in `binaries`), relative `report_dir` and `release_dir` paths are resolved against the working directory once at startup, and unknown fields or trailing data stop startup. The operator publishes a complete `<release_dir>/<commit>/` directory in one atomic rename and never edits its regular files in place. For a full candidate `commit` ID, `CurrentInputs` (called by `approve_merge` and on every `get_change` or no-op `confirm_intent` of an approved card) opens `<release_dir>/<commit>` once via `os.OpenRoot` and rehashes each configured regular-file binary (`O_NONBLOCK`) through that directory handle (a missing file, non-regular file, or symlink that leaves `<release_dir>/<commit>` fails `approve_merge` and `get_change`, naming the binary and commit), approving those staged binaries for a fresh launch with the evaluator suite's fixed configuration (it makes no claim about processes already running, and replacing `<release_dir>/<commit>` with different binaries causes `get_change` and no-op `confirm_intent` to omit the stale approval). When omitted, `approve_merge` returns `no promotion target is bound to this control plane`.

Model, swarm-profile, verification-config, and promotion-config flags are independent. Incomplete or mixed model options stop startup with an error. A supplied profiles file must contain one JSON array: unknown fields, trailing JSON values, duplicate profile IDs, empty required metadata, and unknown capabilities stop startup. Omitted profiles leave the proposed roster unconfigured.

### `surla diff`
Reads two Go design modules into symlink-safe snapshots (`go.mod`, `go.sum`, and non-test `.go` files), evaluates both inside the sandboxed Goa/Goa-AI compiler at their own `go.mod` dependency versions, and prints the JSON `DiffResult` to `stdout`.
- `--old-dir <path>` — Root directory of the previous design module *(required)*.
- `--new-dir <path>` — Root directory of the candidate design module *(required)*.

### `surla verify`
Clones the committed candidate and evaluator suite into an isolated work directory, builds the candidate binaries (`WorkDir/candidate-bin/<name>`) and suite runner (`WorkDir/suite-bin/suite`) inside `verify/candidate`'s macOS `sandbox-exec` jail, runs the suite, writes the canonical `verify.Report` JSON to `<report-dir>/<digest>.json`, and prints the JSON report to `stdout` (setup or build errors exit non-zero without printing report JSON, while an assembled report with a failing scenario is written and printed before exiting non-zero).
- `-candidate-repo <path|url>` — Git repository holding the candidate *(required)*.
- `-candidate-commit <sha>` — Full commit ID of the candidate to build *(required)*.
- `-candidate-module <dir>` — Candidate Go module directory relative to `-candidate-repo` (default `.`).
- `-binary <name=package>` — Candidate binary artifact name and relative package path (`.` or a path beginning with `./`, such as `./cmd/orders`) inside `-candidate-module` (repeatable, at least one *required*; `<name>` must be a single path element).
- `-suite-repo <path|url>` — Git repository holding the evaluator suite *(required)*.
- `-suite-commit <sha>` — Full commit ID of the evaluator suite *(required)*.
- `-suite-module <dir>` — Suite Go module directory relative to `-suite-repo` (default `.`).
- `-suite-command <package>` — Suite command relative package path (`.` or a path beginning with `./`, such as `./cmd/orders_acceptance-evals`) inside `-suite-module` *(required)*.
- `-request-revision <digest>` — Confirmed `request_revision` digest bound into the verification report *(required)*.
- `-work-dir <path>` — Private working directory for clones, sandboxed builds, and evidence; must be missing or empty at run start and must not be shared with or readable by builder agents *(required)*.
- `-report-dir <path>` — Directory that receives the full canonical report as `<digest>.json`; must not be readable by builder agents when holdout privacy is required *(required)*.
- `-timeout <duration>` — Optional time limit for the verification run (such as `30m`; default `0`, meaning no limit). When a run is canceled or exceeds `-timeout`, `surla verify` stops the run's process group, prints any partial report the suite saved before stopping, and exits non-zero.
- `-builder-view` — Optional boolean flag; when set, `stdout` prints `Report.ForBuilders()`, which redacts `"holdout"` scenarios to `"id": "holdout-N"` with scenario names, inputs, declarations, checks, and failure logs stripped (the full unredacted report is still written to `-report-dir`).

#### Evaluator Suite Command Contract (`-suite-command`)

`surla verify` compiles the Go package named by `-suite-command` inside the macOS build sandbox and invokes the resulting executable twice. Evaluator suite `main` packages use [`verify/suite`](../verify/suite/suite.go) to implement this contract with [`suite.Main(suite.Command{...})`](../verify/suite/suite.go):

```go
func main() {
    suite.Main(suite.Command{
        Environment:    "sim",
        Binaries:       []string{"orders", "orders-worker"},
        MaxConcurrency: 4,
        Declare:        declare,
        Suite:          newSuite,
    })
}
```

1. **Suite discovery (`--describe`):**
   - `surla verify` runs `<suite-bin> --describe` with no `--artifact` flags.
   - `suite.Main` calls `Command.Declare()` (which returns the generated `eval.Suite` without requiring candidate binaries or constructing runtime state), writes [`suite.Declaration`](../verify/suite/suite.go) as JSON to `stdout`, and exits `0`:
     ```json
     {
       "id": "orders_acceptance",
       "environment": "sim",
       "scenarios": [
         {"id": "store_credit_within_limit", "tags": ["repair"]},
         {"id": "other_customers_order_refused", "tags": ["holdout"]}
       ]
     }
     ```
   - Scenarios whose `tags` contain `"holdout"` are marked `Holdout: true` in the assembled `verify.Report` and redacted in `Report.ForBuilders()` (`-builder-view`).
2. **Suite execution (`--artifact <name>=<path>`):**
   - `surla verify` runs `<suite-bin> --artifact <name>=<path> ...` passing one `--artifact` flag for every `-binary <name>=<package>` bound in the verification plan (the suite binary also accepts `--scenario <id>`, mutually exclusive `--tag <tag>`, and `--max-concurrency <n>` for direct runs).
   - `suite.Main` verifies that the `--artifact` flags match `Command.Binaries` (each required binary once), pins each candidate binary into a temporary read-only store with [`candidate.Pin(store, path)`](../verify/candidate/candidate.go) (cleaning up the store with [`candidate.RemoveStore`](../verify/candidate/candidate.go) when the run finishes), passes the `pinned map[string]candidate.Artifact` to `Command.Suite(pinned)`, and runs the scenarios with `eval.NewRunner`.
   - Inside each scenario hook, the suite starts candidate processes inside the runtime Seatbelt jail with [`candidate.Exec(ctx, pinned["orders"], spec)`](../verify/candidate/candidate.go) and attaches [`pinned["orders"].Evidence("orders")`](../verify/candidate/candidate.go) to every scenario's `eval.Result.Artifacts`:
     ```go
     res.Artifacts = append(res.Artifacts,
         h.env.Orders.Evidence("orders"),
         h.env.Worker.Evidence("orders-worker"),
     )
     ```
     `pinned["orders"].Evidence("orders")` produces `eval.Artifact{Name: "candidate:orders", URI: "sha256:<digest>"}`, which `surla verify` checks against the SHA-256 digest it measured when building `orders`. A scenario that omits a bound binary's evidence record or reports a different digest produces a failing `verify.Report` with scenario outcome `"harness_error"`.
   - `suite.Main` writes `goa.design/goa-ai/eval.Report` JSON to `stdout` whenever the runner produced one and exits with:
     - **`0` (`suite.ExitPassed`)** when all scenarios passed and the temporary pinned-artifact store was removed cleanly,
     - **`1` (`suite.ExitFailed`)** when the suite ran, cleaned up its temporary artifact store, and wrote a valid `eval.Report` JSON to `stdout` with one or more failed scenarios (`!report.Passed`), or
     - **`2` (`suite.ExitError`)** when the suite could not run or finish cleanly (invalid flags, unknown/missing/duplicate `--artifact`, `--scenario` combined with `--tag`, harness setup failure, crash before producing a report, or failure removing the temporary pinned-artifact store—even if a passing `eval.Report` was already written to `stdout`).
   - See [`examples/orders-eval/cmd/orders_acceptance-evals/main.go`](../examples/orders-eval/cmd/orders_acceptance-evals/main.go) and [`examples/orders-eval/acceptance/hooks.go`](../examples/orders-eval/acceptance/hooks.go) for the complete reference implementation.

### `surla approve`
Decodes a saved full `verify.Report` JSON file with `verify.DecodeReport`, checks it against explicit promotion flags with `verify.Approve`, and prints an indented JSON approval summary (`report_digest`, `binding`, `counts`) to `stdout` (exiting `0` when the report passed and matches every flag, or exiting `1` without stdout output if any flag is missing or the report is stale, failed, or unreadable). `surla approve` validates the report file only; it does not contact a control plane, record a Change Card approval, or merge/deploy.
- `-report <path>` — Path of the full verification report JSON written by `surla verify` *(required)*.
- `-candidate-commit <sha>` — Full commit ID of the candidate being promoted *(required)*.
- `-request-revision <digest>` — Current confirmed `request_revision` digest of the change *(required)*.
- `-suite-id <id>` — Evaluator suite identifier required for promotion *(required)*.
- `-suite-commit <sha>` — Full commit ID of the evaluator suite required for promotion *(required)*.
- `-environment <env>` — Dependency profile required for promotion, such as `sim` *(required)*.
- `-artifact <name=sha256>` — Candidate binary artifact name and 64-character lowercase hex SHA-256 digest measured for promotion (repeatable, at least one *required*; duplicate artifact names are rejected).

---

## Reference Platform: `examples/orders` & `examples/orders-eval`

Surla includes an order-refund service and an isolated evaluator for checking payment effects, manager approval, worker recovery, and token budgets:

### [`examples/orders`](../examples/orders/) — The Candidate Platform
- **[`design/`](../examples/orders/design/)**: Declares the public `orders` HTTP service (`chat`, `run_status`, `approve_refund`, and operator `override_prompt`), internal `commerce` and `payments` services, the `goa-ai` `refund` agent (`Inject("tenant_id", "customer_id")`, `BoundedResult`, and `Confirmation`), and the C4 architecture model rendered to [`gen/structurizr/model.json`](../examples/orders/gen/structurizr/model.json).
- **[`AGENTS_QUICKSTART.md`](../examples/orders/AGENTS_QUICKSTART.md)**: Generated contextual map for coding agents, surfacing per-tool `BindTo` method links, server-injected fields, `BoundedResult` cursor guidance, `Confirmation` gates, and `gen/orders/agents/refund/specs/tool_schemas.json`.
- **[`internal/orders`](../examples/orders/internal/orders/)**: The refund rule (`StoreCreditLimitCents` in `refunds.go`): an order total up to `\$50.00`, including tax and shipping, is refunded as instant store credit; a larger total is refunded to the card after a manager approves. Also serves `PUT /prompts/{prompt_id}` (`prompts:manage` scope) for runtime prompt overrides.
- **[`internal/assistant`](../examples/orders/internal/assistant/)**: Runs the `refund` agent, resumes a suspended run after the manager's decision (`PrepareContinuation`, then `StartPrepared`), and validates and saves runtime prompt overrides in the process-owned in-memory `prompt.Store` (`assistant.Prompts`).
- **[`internal/refundflow`](../examples/orders/internal/refundflow/)**: Durable Temporal workflow that executes each refund exactly once, with workflow IDs and payment idempotency keys derived from the tenant and order.
- **[`internal/runstore`](../examples/orders/internal/runstore/)**: Transactional SQLite implementation of `goa-ai`'s durable `storage.Store`.

### [`examples/orders-eval`](../examples/orders-eval/) — The Isolated Evaluator
- **[`acceptance/acceptance_test.go`](../examples/orders-eval/acceptance/acceptance_test.go)**: Compiles and boots the candidate inside `verify/candidate`'s macOS `sandbox-exec` jail against stateful commerce and payment simulators ([`sim/`](../examples/orders-eval/sim/)) and a scripted model gateway ([`scriptedmodel/`](../examples/orders-eval/scriptedmodel/)).
- Exercises five acceptance scenarios (`store_credit_within_limit`, `card_refund_needs_manager`, `refund_survives_worker_crash`, `pending_approval_survives_api_restart`, and the holdout `other_customers_order_refused`) plus `TestSeededPayloadBloatFailsTheBudget`.

Run the unit and race checks across all three modules:

```bash
go test -race ./...
(cd examples/orders && go test -race ./...)
(cd examples/orders-eval && go test -race ./...)
```

The end-to-end and starter-project integration tests require macOS (`sandbox-exec`); `examples/orders-eval/acceptance` also requires the Temporal CLI. Run the five acceptance scenarios and the seeded token-budget regression, the verifier's complete `Run` → report → `Approve` test, and the `surla init` starter-project build and sandboxed evaluator test (all run automatically on GitHub Actions in [`.github/workflows/ci.yml`](../.github/workflows/ci.yml)):

```bash
(cd examples/orders-eval && go test -race -tags e2e ./acceptance)
go test -race -tags e2e ./internal/verify
go test -race -tags scaffold ./internal/scaffold
```

See [`examples/orders/README.md`](../examples/orders/README.md) for a step-by-step walkthrough including `surla verify` (full operator report vs. `-builder-view`).

---

## Project Layout

```text
.
├── .github/
│   └── workflows/
│       └── ci.yml                # macOS GitHub Actions CI (`tests` unit/typecheck/codegen-drift job and `e2e` verifier/scaffold/acceptance job)
├── design/
│   └── design.go                 # Surla Goa & Goa-AI MCP 1.0.0 DSL contract
├── cmd/
│   └── surla/
│       └── main.go               # CLI entrypoint (`surla init`, `surla serve`, `surla preview`, `surla diff`, `surla verify`, and `surla approve`)
├── docs/
│   ├── HOW_IT_WORKS.md           # The 5 checkpoints, Chorus routing & independent verification
│   ├── ARCHITECTURE_PLAYBOOK.md  # Golden Path rules & Goa Design suite overview
│   └── REFERENCE.md              # MCP tools/resources, CLI flags & repo layout
├── internal/
│   ├── controlplane/             # CP1–CP5 handlers, bearer auth, embedded checkpoint viewer, CP1 schema validator
│   ├── designeval/               # Sandboxed Goa/Goa-AI/Model subprocess evaluator & C4 extractor
│   ├── diff/                     # Structural declaration, tool schema, and go.mod version diff engine
│   ├── gitcapture/               # Read-only dirty git working-tree snapshot capture
│   ├── goldenpath/               # Embedded GOLDEN_PATH.md playbook & CP1 architect system prompt
│   ├── procgroup/                # Process-group lifecycle management for sandboxed child processes
│   ├── scaffold/                 # Starter project scaffolder (`surla init`) & embedded templates
│   ├── swarm/                    # Deterministic Chorus role assignment & cross-family model router
│   ├── verify/                   # Worktree verifier (`surla verify`), holdout redactor, and promotion check (`surla approve`)
│   └── verifyrun/                # Detached supervised process-group runner and run-directory store for CP4 verifications
├── ui/
│   └── checkpoint/               # React + OpenUI checkpoint viewer source; build.mjs writes checkpoint.html, decision_catalog.md, and generated types
├── verify/
│   ├── candidate/                # macOS Seatbelt (`sandbox-exec`) build & runtime isolation jail
│   └── suite/                    # Evaluator suite command runner (`suite.Main`, `suite.Command`, `suite.Declaration`)
├── examples/
│   ├── orders/                   # Reference Goa + Goa-AI + Temporal + SQLite order-refund platform
│   └── orders-eval/              # Isolated evaluator harness, simulators, and 5 acceptance scenarios
├── gen/                          # Generated Goa HTTP, JSON-RPC, and MCP server/client packages
├── PROPOSAL.md                   # Full architecture & verification design specification
└── PRESS_RELEASE.md              # Working Backwards product vision
```
