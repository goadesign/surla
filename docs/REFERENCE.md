# CLI, MCP & Repository Reference

**[← Back to README](../README.md)** · **[← How Surla Works](HOW_IT_WORKS.md)** · **[← Golden Path Architecture](ARCHITECTURE_PLAYBOOK.md)**

---

## Terminology at a Glance

| Term | Plain-English Meaning |
| :--- | :--- |
| **`CP1` – `CP5`** | Surla's five verification checkpoints: **CP1** Confirm Intent (`init_project`, `propose_change`, `revise_change`, `confirm_intent`, `configure_model`), **CP2** Preview Compiled Architecture & Swarm (`preview_architecture`, `surla preview-change`, or local `surla preview`), **CP3** Fast Draft & Live Sandbox (`draft_override`, `start_live_target`, `stop_live_target`), **CP4** Isolated Sandboxed Verification (`verify_change`, `get_verification`, `list_verifications`, `surla verify`), and **CP5** Promotion Check (`surla approve`, `approve_merge`). |
| **`Change Card`** | The versioned change plan (`ChangeCardResult`) created at `CP1` and saved on disk under `controlplane.Options.CardsDir` (`surla serve -cards-dir`, `<startup_cwd>/.surla/cards` when outside the workspace, or `<UserConfigDir>/surla/operator/project-<sha256(real_workspace)>/cards-<sha256(startup_cwd\0real_workspace)>` by default when `-cards-dir` is omitted) as `<sha256(change_id)>.json`, holding concrete input/outcome examples, open domain ambiguity questions, confirmed answers, preserved contract behaviors, and optional `CP5` approval metadata. |
| **`request_revision`** | The SHA-256 hex digest of a Change Card's canonical content. `preview_architecture`, `verify_change`, `surla verify`, `surla approve`, and `approve_merge` bind to `request_revision` so changing the agreed examples or answers automatically invalidates stale previews, reports, and approvals. When running standalone `surla verify` and `surla approve` on the CLI without a control-plane Change Card, pass any explicit revision string (such as a commit or label) consistently to both commands. |
| **`holdout`** | An unseen acceptance scenario (tagged `"holdout"` in the evaluator suite) whose name, inputs, checks, and logs are redacted to `"id": "holdout-N"` in `verify_change` / `get_verification` and the default `surla verify` report (`Report.ForBuilders()`) so coding agents cannot hardcode around the test cases. |
| **`input_binding`** | The generated input schemas and compiler-listed `injected_fields` for a tool used by an agent (`ToolInputBindingItem`). It describes the declared input choices, not which option the running application installed. |
| **`CardsDir` / `WorkspaceDir` / `TargetUpdater` / `PromotionTarget` / `Verification`** | Configuration in `controlplane.Options`: `CardsDir` (required by `controlplane.New(opts)`, locked exclusively via `<cards_dir>/lock` until `(*ControlPlane).Close()`) is the directory where the control plane saves its `CP1` Change Cards (`<UserConfigDir>/surla/operator/project-<sha256(real_workspace)>/cards-<sha256(startup_cwd\0real_workspace)>` by default on `surla serve` when `-cards-dir` is omitted and `<startup_cwd>/.surla/cards` lies inside the workspace); `WorkspaceDir` (optional, bound on `surla serve` to `-project <dir>` or the startup working directory when `-project` is omitted) is the local project workspace directory from which `propose_change`, `revise_change`, and `preview_architecture` derive `design/*.go` and `DesignSnapshot` when omitted by the caller and inside which `init_project` scaffolds a `<workspace>/<dir>` starter project (when `WorkspaceDir` is set and `Options.Evaluator` or `Options.NewGenerator` is nil, `controlplane.New` and `InitProject` construct their default `designeval.Evaluator` instances with `designeval.NewEvaluatorInDir("<CardsDir>/designeval")` outside `WorkspaceDir`, and any host-supplied `Evaluator` or `NewGenerator` for a bound workspace must likewise use a protected parent outside `WorkspaceDir`); `TargetUpdater` applies in-memory `CP3` prompt overrides; `PromotionTarget` and `ResolvePromotionTarget` (`surla serve -promotion-config`) load `CP4` verification reports and current promotion inputs for `approve_merge`, `get_change`, and no-op `confirm_intent` (approving staged binaries in `<release_dir>/<commit>/` for a fresh launch on `surla serve`, omitting `approval` when inputs drift, and returning `unverified_approval` on `get_change` / no-op `confirm_intent` when no target is bound or `CurrentInputs` returns an error); and `Verification`, `RunsDir`, and `ResolveVerification` (`*VerificationConfig`, loaded on `surla serve` via `-verification-config` or resolved dynamically from a committed starter `WorkspaceDir`) configure the operator-owned `CP4` verification target for `verify_change`, `get_verification`, and `list_verifications`. |

---

## Quickstart & MCP Configuration

### Prerequisites
- **Go 1.27.3+**, matching the repository's `go.mod` files (`surla init`, `surla gen`, and `surla preview` run Goa inside Surla's sandbox with each module's own versions, so a globally installed `goa` binary is not required)
- **Surla CLI** (`go install github.com/goadesign/surla/cmd/surla@latest`, or `go run ./cmd/surla` from a repository checkout)
- **macOS** (`sandbox-exec` is used by `verify/candidate` to isolate candidate builds and processes; non-macOS hosts fail closed with `ErrIsolationUnavailable`). Surla must be able to apply that sandbox itself. When a sandboxed command exits with status 71 (such as `compile Goa design: sandboxed build of .: exit status 71 (sandbox-exec: sandbox_apply: Operation not permitted)` when `surla preview` or `surla gen` runs inside a coding agent's restricted shell, or during `verify_change` and `start_live_target`), Surla preserves the observed error and adds conditional recovery guidance (`The sandboxed command exited with status 71. If Surla is running inside a restricted coding-agent shell, its sandbox may have been blocked; run Surla from an unrestricted host shell.`). Run Surla from an unrestricted host shell, or call `preview_architecture` on a `surla serve` started from an unrestricted host shell, so Surla's sandbox can apply. Surla never retries outside its sandbox.
- **Temporal CLI** (`brew install temporal`) for running the end-to-end durable workflow crash and replay verification suites

### Start the Surla Server

When `-model-provider`, `-model-name`, and `-model-gateway-addr` are omitted, `surla serve` inspects its process environment at startup: if `ANTHROPIC_API_KEY` is non-empty, it configures `anthropic` with `claude-sonnet-4-6`; otherwise, if `OPENAI_API_KEY` is non-empty, it configures `openai` with `gpt-5.4-mini`. To start `surla serve` with automatic provider detection (or pass `-model-provider` and `-model-name` to select a specific model ID):

```bash
export SURLA_AUTH_TOKEN="dev-secret-token"
export ANTHROPIC_API_KEY="sk-ant-..."
surla serve -addr 127.0.0.1:8090 \
  -principal operator@example.com
```

For an explicit model choice, pass `-model-provider anthropic` or `-model-provider openai` with `-model-name <model>` and the matching `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` environment variable. Alternatively, use `-model-gateway-addr <host:port>` alone to call an existing model gateway; the gateway chooses the model and must use the `goa.design/goa-ai` version selected in [`go.mod`](../go.mod). Upgrade or roll back the gateway and `surla serve` together: older clients reject the `output_limited` validation kind returned when a structured response fails validation and the provider reports its output token limit. That connection uses plaintext gRPC and is intended for loopback or a trusted network.

Omitting all model flags when neither `ANTHROPIC_API_KEY` nor `OPENAI_API_KEY` is set starts the server without feature-request analysis. The `diff` tool, Golden Path resource, and `compiled_rule` rebuild response still work; `propose_change` and `revise_change` return `cp1 semantic analyzer is required` with instructions to call `configure_model` or export a provider key, and calling `configure_model` (`POST /cp1/model`) attaches a running model gateway or switches to a direct provider whose API key is present in `surla serve`'s environment without restarting the server.

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
          "Authorization": "Bearer ${SURLA_AUTH_TOKEN}"
        }
      }
    }
  }
  ```

  Claude Code fills in `${SURLA_AUTH_TOKEN}` from its environment, so start it where that variable is exported.

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

After confirming a Change Card, include explicit builder ownership in the `preview_architecture` request (or pass the JSON array to `surla preview-change -builders <path.json>`), and optionally include `architect_paths` (`-architect-path <glob>`, repeatable) or `eval_paths` (`-eval-path <glob>`, repeatable) when you want `@architect` or `@eval-author` in the proposed roster. For example, a builder for the orders service can supply `builders` (plus optional `architect_paths` and `eval_paths`) alongside `change_id`, `request_revision`, and `design`:

```json
{
  "builders": [
    {
      "name": "orders",
      "paths": ["internal/orders/**"],
      "preferred_profile_id": "refund-builder"
    }
  ],
  "architect_paths": ["design/**"],
  "eval_paths": ["orders-eval/**"]
}
```

When using `surla preview-change -builders ./builders.json`, save the builder array itself (`[{"name": "orders", "paths": ["internal/orders/**"], "preferred_profile_id": "refund-builder"}]`) and pass repeatable `-architect-path` and `-eval-path` flags when those roles are needed (each flag value is preserved verbatim, including commas or spaces). Paths are relative globs for Chorus coordination ownership (not Surla-enforced secrecy or OS isolation): an in-repo starter suite (`eval/**`) is readable by any process in the repository, whereas keeping unseen holdouts private requires an external evaluator workspace whose sources, report directory, and work directory are inaccessible to builders. Omitting `builders` returns a solo preview (`status: "unconfigured"` with no `missing_roles`, and passing `architect_paths` or `eval_paths` without `builders` is rejected). When `builders` are supplied without authenticated profiles, `status` is `"unconfigured"` with the requested roles (`builder` and `reviewer`, plus `architect` or `eval_author` when requested) listed in `missing_roles`; if authenticated profiles lack a requested role capability, `status` is `"missing_capability"`.

A configured roster reports its actual `cross_family_review` and `cross_family_eval` values (`cross_family_eval` is `false` when `@eval-author` is not assigned, and the summary and manifest explicitly state `eval-author not assigned in this roster` rather than claiming a shared model family). The preview returns proposed assignments and Chorus coordination manifest text; it does not launch agents, prepare Git worktrees, or publish a Chorus manifest.

---

## MCP Tools & Resources

All tools and resources are served over MCP JSON-RPC at `POST /mcp` and over HTTP REST on the same listen address (authenticated via `Authorization: Bearer <token>`). Each Change Card and verification HTTP route declares its method-specific non-fault `ErrorResult` responses (`403` `forbidden`, `404` `not_found`, `409` `conflict`, `422` `unprocessable`, `501` `not_implemented`, and `502` `analysis_rejected`) in [`design/design.go`](../design/design.go) and [`gen/http/openapi3.yaml`](../gen/http/openapi3.yaml)—for example, calling `POST /changes/preview` on a `needs_confirmation` card returns HTTP `409` `conflict` (`fault: false`) and `surla preview-change` prints the message directly—while unclassified evaluator, planning, storage, and owner failures remain HTTP `500` faults and MCP `tools/call` returns non-fault tool error results (`isError: true`) with the same domain error message.

### MCP Tools

When CP1 rejects an invalid structured response because the provider reported its output token limit, the error says `provider stopped at output limit; shorten the request before retrying`. For example, retry a large feature request as a smaller change using the same tool and its required inputs. Malformed JSON alone does not establish that the provider hit its token limit. Rejected response text and provider stop reasons are not included in the public error.

Each `propose_change` or `revise_change` model completion has an allowance of 16,384 output tokens, including reasoning tokens counted against that allowance. This bounds output-token cost for one completion, not the total cost of a change or the number of tokens the model must use. Surla keeps the configured model and the provider adapter's reasoning defaults; it does not set a separate reasoning effort or thinking budget for CP1. A response the provider marks as output-limited is still rejected without saving or modifying the Change Card, even when its JSON is valid.

- **`init_project`** (`POST /workspace/init`)
  - **Input:** `module_path`, `dir?` → **Output:** `InitProjectResult` (`dir`, `module_path`, `service_name`, `commit`)
  - Scaffolds a new Goa + Goa-AI + `goa.design/model` starter service and `<dir>/eval` evaluator suite inside a direct child directory `<workspace>/<dir>` of the bound workspace (`controlplane.Options.WorkspaceDir`), runs `go mod tidy`, `go vet ./...`, and `go test ./...` in both modules, records the initial Git commit `"Initial scaffold"` (`commit`) inside the protected staging directory before atomic publication, and transitions the running control plane's active workspace and pinned evaluator `suite_commit` in place without restarting `surla serve`. When `dir` is omitted, it defaults to `service_name` (the last path element of `module_path`); when `dir` is supplied, it must be a single direct child directory name (`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`, excluding `.`, `..`, and `.surla*` prefixes). The bound workspace must contain no project markers (`go.mod`, `design`, `gen`, `internal`, `cmd`, `eval`), no existing `<workspace>/<dir>` entry, no committed Git `HEAD`, no verification runs in `RunsDir`, and no saved Change Cards whose `design_source` was captured from workspace design files; `<workspace>/<dir>` is staged in operator-owned storage (`Options.InitStageDir` or a `0700` operator staging directory on the workspace filesystem), published atomically via `RENAME_EXCL`, and recorded with `suite_commit` in `<cards_dir>/workspace.json` so subsequent builder commits and server restarts keep `CP4` verification and `start_live_target` pinned to the initial starter evaluator suite commit. When the request includes a `progressToken`, the tool sends MCP progress notifications at each initialization phase. Requires `controlplane.Options.WorkspaceDir` and a canonical semantic `controlplane.Options.SurlaVersion` (from `surla` build info on `surla serve`, or set via `controlplane.Options.SurlaVersion` in an embedding host). Returns `not_implemented` (HTTP `501`) when no workspace or canonical `SurlaVersion` is configured, `unprocessable` (HTTP `422`) when `module_path` or `dir` is invalid, and `conflict` (HTTP `409`) when `<workspace>/<dir>` already exists or workspace initialization preconditions fail. `init_project` has no `ToolUI` metadata, so calling it leaves any open `ui://surla/checkpoint` view undisturbed.
- **`generate_code`** (`POST /workspace/generate`)
  - **Input:** `target?` (`candidate`, the default, or `eval`) → **Output:** `GenerateCodeResult` (`target`, sorted `files_written`, `warnings`).
  - Reads the selected module's local files from the active workspace and compiles its Goa design in Surla's sandbox, then atomically replaces that module's `gen/`. For example, after editing `design/design.go`, call with `{}`; after editing `eval/design/design.go`, call with `{"target":"eval"}`. Repeat after another edit on the same server. The `eval` target uses only the public local `eval/` module; it does not access an externally configured verification suite. Top-level user files under `gen/` and their permissions survive, stale generated subdirectories are removed, and `go.mod` and `go.sum` stay unchanged. The result lists paths relative to the selected module. Errors mean `gen/` was not replaced. A generation failure states the failed phase and available sandbox details; correct a reported local problem or ask the server operator to restore dependency access or compiler storage, then call again. A warning after publication still means `gen/` was updated. This compiles the design only: build the application and run its tests before verification. No model configuration, Change Card, commit, or server restart is required. The tool has no `ToolUI` metadata, so it leaves an open checkpoint view undisturbed.
- **`propose_change`** (`POST /changes/propose`)
  - **Input:** `change_id`, `prompt`, `design_source?` → **Output:** `ChangeCardResult`
  - Analyzes a feature request at `CP1` using the embedded Golden Path prompt, saving the card (`card`, `owner`, `design_source`, and optional `source_origin`) atomically as `<cards_dir>/<sha256(change_id)>.json` under `controlplane.Options.CardsDir` and returning concrete `examples` (assigned deterministic `check_01`, `check_02`, ... IDs matching `goa-ai/eval` `validateID`), `ambiguities`, `preserved_behaviors`, and a canonical `request_revision` digest. When `design_source` is omitted and `controlplane.Options.WorkspaceDir` is bound, `propose_change` reads `design/*.go` from the active workspace automatically and records `source_origin: "workspace"` (or passes an empty `design_source` when the workspace has no `design/*.go` files yet); pass `design_source` explicitly when targeting a different design than the bound workspace. When the CP1 model response fails output validation or stops at the output limit, `propose_change` returns `analysis_rejected` (HTTP `502`) without saving the card. When the model also composed a decision view, the card carries `ui_composition` (`program`, `composed_for_revision`): it is presentation only, never hashed into `request_revision`, and dropped when `confirm_intent` changes answers or examples. When the request includes a `progressToken`, the tool sends MCP progress updates before and after the model call.
- **`revise_change`** (`POST /changes/revise`)
  - **Input:** `change_id`, `request_revision`, `prompt`, `design_source?` → **Output:** `ChangeCardResult`
  - Revises an existing Change Card in place at `CP1` when called by the owning principal with its current `request_revision` (verified before and after the model call). When `design_source` is omitted, a card whose `source_origin` is `"workspace"` re-reads `design/*.go` from the active `controlplane.Options.WorkspaceDir` automatically, whereas a card whose `design_source` was supplied by the caller preserves its saved `design_source`. It constructs a `CP1PriorCard` snapshot of the saved card preserving prior example IDs, and calls the CP1 analyzer with `goldenpath.ArchitectReviseSystemPrompt` giving the model the prior card's prompt, confirmed answers (`confirmed_answer`), open questions, summary, examples, and preserved behaviors as context for the revised analysis. The model returns `removed_example_ids` and optional `prior_id` on surviving or edited examples: surviving and edited prior examples keep their original IDs, IDs from the immediately prior table (including removed IDs) are reserved while constructing the current revision, and brand-new examples receive the lowest unused `check_NN` ID absent from the prior and resulting tables. It assigns fresh `<change_id>-ambiguity-NN` IDs in Go code, resets `status` to `"needs_confirmation"` (even when `0` open ambiguities remain), clears `answers`, `confirmed_by`, and `approval`, re-binds any new `ui_composition` to the new revision digest, and saves the revised card atomically in `Options.CardsDir`. When the CP1 model response fails output validation or stops at the output limit, `revise_change` returns `analysis_rejected` (HTTP `502`) without modifying the card. Earlier decisions remain only where the analysis restated them in the new `summary`, `examples`, or `preserved_behaviors`: check those, answer the current `ambiguities`, and confirm the new `request_revision` with `confirm_intent`. When the request includes a `progressToken`, the tool sends MCP progress updates before and after the model call.
- **`confirm_intent`** (`POST /changes/confirm`)
  - **Input:** `change_id`, `request_revision`, `answers`, `examples` → **Output:** `ChangeCardResult`
  - Confirms a `needs_confirmation` Change Card or refines an already `confirmed` Change Card at `CP1` when called by the owning principal with the current `request_revision` and complete `answers` and `examples`. `confirm_intent` keeps the proposal's `summary` text unchanged while recording the owner's confirmed choices in `answers` (empty when the card has no ambiguity questions), which take precedence over open phrasing in `summary`. `answers` maps every open ambiguity's `id` to exactly one string from that question's `options`, character for character; when a decision requires an option not listed, call `revise_change` with the guidance in `prompt` and confirm the revised card. A rejected call leaves the card unchanged and returns one error naming every missing answer, every answer that is not one of its question's options (with the options quoted), and every unknown question `id`. Keep each existing example's `id` when you edit it, and give an example you add a new, unique, non-blank `id`; `confirm_intent` saves the ids as sent. No-op submissions keep the stored `request_revision` and evaluate any recorded `approval` on a shallow response copy without modifying the saved card: `approval` is included only when `PromotionTarget` is bound and `PromotionTarget.CurrentInputs(ctx, approval.CandidateCommit)` still matches its `suite_id`, `suite_commit`, `environment`, and `artifacts`; both `approval` and `unverified_approval` are omitted when `CurrentInputs` succeeds with different inputs; and `unverified_approval` (`UnverifiedApprovalItem` with the saved approval metadata and a path-free operator recovery `reason`, omitting `approval`) is returned when `PromotionTarget` is nil or `CurrentInputs` returns an error (recording the raw `CurrentInputs` error on the `controlplane.check_approval` span). Changed answers or examples mint a new canonical `request_revision` digest, clear `approval`, and save the updated card in `Options.CardsDir`.
- **`get_change`** (`GET /changes/{change_id}`)
  - **Input:** `change_id` → **Output:** `ChangeCardResult`
  - Reads the saved `ChangeCardResult` for `change_id` from `Options.CardsDir` (verifying its canonical `request_revision` digest on load) without mutating saved state (restricted to the owning principal) so a stale editor can fetch the latest saved card after a `request_revision` conflict. On a `confirmed` card, `answers` holds the owner's confirmed ambiguity resolutions (empty when the card had no ambiguity questions) and takes precedence over open phrasing in `summary`. When the saved card has a recorded `approval`, `get_change` evaluates it on a shallow response copy: it includes `approval` only when `PromotionTarget` is bound and `PromotionTarget.CurrentInputs(ctx, approval.CandidateCommit)` still matches `suite_id`, `suite_commit`, `environment`, and `artifacts`; omits both `approval` and `unverified_approval` when `CurrentInputs` succeeds with different inputs; and omits `approval` while returning `unverified_approval` (`UnverifiedApprovalItem` with a path-free operator recovery `reason`) when `PromotionTarget` is nil or `CurrentInputs` returns an error (recording the raw `CurrentInputs` error on the `controlplane.check_approval` span).
- **`configure_model`** (`POST /cp1/model`)
  - **Input:** `provider?` (`anthropic` \| `openai`), `model_name?`, `gateway_addr?` → **Output:** `ModelConfigResult` (`provider?`, `model_name?`, `gateway_addr?`)
  - Configures or switches the running control plane's `CP1` semantic analyzer and `CP3` live-target model environment (`SURLA_LIVE_MODEL_*`) without restarting `surla serve`. Callers supply either `provider` and `model_name` together (reading `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` from `surla serve`'s process environment so API keys never cross MCP or HTTP requests) or `gateway_addr` alone (attaching a plaintext gRPC `goa-ai` model gateway). In-flight `propose_change` and `revise_change` calls finish on the model handle they started with and close its transport once the last reference completes. HTTP rejects schema-invalid input, such as an unknown provider name, with `400`. The method returns `unprocessable` (HTTP `422`) for an invalid field combination or a missing required environment API key, and `not_implemented` (HTTP `501`) on embedded hosts constructed without `controlplane.Options.ModelBuilder`. `configure_model` has no `ToolUI` metadata, so calling it leaves any open `ui://surla/checkpoint` view undisturbed.
  - The selection lasts for this server process. Restarting uses the startup model flags or the API keys in the new process's environment again.
- **`verify_change`** (`POST /changes/{change_id}/verifications`)
  - **Input:** `change_id`, `request_revision`, `candidate_commit` → **Output:** `VerificationStatusResult`
  - Starts a Checkpoint 4 (`CP4`) verification run for `candidate_commit` against the operator-configured evaluator suite (`controlplane.Options.Verification` or `controlplane.Options.ResolveVerification` bound to `controlplane.Options.WorkspaceDir`) when called by the owner of a `confirmed` Change Card at its current `request_revision`. The run is accepted into `<runs_dir>/<run_id>/`, carried out by a detached supervised process group (`internal/verifyrun`) that outlives the HTTP/MCP call and server restarts, and immediately returned with `state.type == "running"`. When verification is not configured on the control plane, it returns `verification is not configured on this control plane`.
- **`get_verification`** (`GET /verifications/{run_id}`)
  - **Input:** `run_id` → **Output:** `VerificationStatusResult`
  - Reads the state of verification run `run_id` from `<runs_dir>/<run_id>/` (restricted to the principal recorded as the run's `owner` in `request.json`, independent of current Change Card state so runs remain readable after subsequent card edits or server restarts). Returns `accepted_at` (RFC3339 timestamp with sub-second precision when present) and the `state` union reporting `"running"` (`started_at`), `"completed"` (`suite_id`, `environment`, and `report` projected through `Report.ForBuilders()` so holdout scenario names, checks, and diagnostics never leave the evaluator), or `"not_completed"` (`kind`: `"execution_failed"` \| `"interrupted"`, `stage`: `"checkout"` \| `"build"` \| `"describe"` \| `"suite"` \| `"report"` \| `"supervise"`, and a builder-safe `summary` with full command output kept in `<runs_dir>/<run_id>/executor.log`).
- **`list_verifications`** (`GET /changes/{change_id}/verifications`)
  - **Input:** `change_id` → **Output:** `VerificationListResult` (`change_id`, `runs: VerificationStatusResult[]`)
  - Lists all saved verification runs in `<runs_dir>/` recorded for `change_id` and owned by the authenticated principal (allowed whether the card is currently `confirmed` or `needs_confirmation`, so runs from earlier revisions remain discoverable after `revise_change`), ordered newest first by `accepted_at` (`run_id` ascending on ties) with each completed report projected through `Report.ForBuilders()`.
- **`approve_merge`** (`POST /changes/approve`)
  - **Input:** `change_id`, `request_revision`, `candidate_commit`, `report_digest` → **Output:** `ChangeCardResult`
  - Records Checkpoint 5 (`CP5`) promotion approval on the owning principal's confirmed `ChangeCardResult` when `controlplane.Options.PromotionTarget` or `controlplane.Options.ResolvePromotionTarget` is bound (`surla serve -promotion-config <path.json>` or `<operator-project-dir>/promotion.json`). It loads the full `verify.Report` for `report_digest` and the current `PromotionInputs` (`SuiteID`, `SuiteCommit`, `Environment`, measured candidate binary `Artifacts`) for `candidate_commit` from `PromotionTarget`, runs `verify.Approve`, re-verifies that the card still has `request_revision`, and saves and returns the updated `ChangeCardResult` with `approval` (`ApprovalItem`: `approved_by`, `report_digest`, `candidate_commit`, `suite_id`, `suite_commit`, `environment`, `artifacts`, `scenario_count`) in `Options.CardsDir`. Recording approval does not merge branches or deploy binaries. When no `PromotionTarget` is bound, `approve_merge` requests return `no promotion target is bound to this control plane`.
- **`preview_architecture`** (`POST /changes/preview`)
  - **Input:** `change_id`, `request_revision`, `design?`, `builders?`, `architect_paths?`, `eval_paths?` → **Output:** `ArchitecturePreviewResult`
  - Evaluates a candidate `DesignSnapshot` (or reads the active workspace's `DesignSnapshot` when `design` is omitted and `controlplane.Options.WorkspaceDir` is bound) in a sandboxed subprocess at `CP2`, returning C4 `containers` and `edges`, `agent_policies`, `tools` (`input_binding`), and the proposed `swarm` plan (returning a solo preview with `swarm.status: "unconfigured"` and no `missing_roles` when `builders` is omitted, or proposing `@builder-<scope>` and `@reviewer` plus optional `@architect` when `architect_paths` is supplied and `@eval-author` when `eval_paths` is supplied). When the request includes a `progressToken`, the tool sends MCP progress updates as design evaluation and roster planning start and finish. Apart from `swarm`, the result reports what the design declares; it does not compare the design with the Change Card's requirements. A tool's `requires_confirmation` declares a design-time `Confirmation` gate; it does not show that the application delivers the approval request to the right person or continues after the answer. [`examples/orders`](../examples/orders/README.md) implements and verifies one such flow, a manager approving refunds. Local callers and coding agents with shell access can also invoke this endpoint from another module directory on disk using [`surla preview-change`](#surla-preview-change).
- **`draft_override`** (`POST /sandbox/draft`)
  - **Input:** `target_kind` (`prompt_template` \| `compiled_rule`), `prompt_id?`, `new_content` → **Output:** `DraftOverrideResult`
  - Applies a `prompt_template` to a target configured through `controlplane.Options.TargetUpdater`, without rebuilding Go code, or reports that a `compiled_rule` requires a rebuild. `internal/controlplane` is not a public embedding API: Go permits importing it only from packages below `github.com/goadesign/surla` in the import-path tree, which includes the separate `examples/orders` module. The override changes only the running target: `CP4` verifies the committed source, so edit accepted prompt text into source, commit it, and run `surla verify` (or `verify_change`) on that commit for the same `request_revision`. `surla serve` binds no `TargetUpdater`. With a configured or workspace-resolved verification target it binds `draft_override` to the live target `start_live_target` runs; with no live target running, its `prompt_template` requests return `no target prompt updater is bound to this control plane`. A host that sets `TargetUpdater` cannot also set `Options.Live`. For the `examples/orders` platform, [`examples/orders/cmd/orders-surla`](../examples/orders/cmd/orders-surla/main.go) provides a separate reference `CP3` host that saves Change Cards in `-cards-dir` (default `.surla/orders-surla-cards`) and binds `TargetUpdater` to `PUT /prompts/{prompt_id}` using the `SURLA_AUTH_TOKEN` (MCP) and `ORDERS_PROMPTS_TOKEN` (`prompts:manage`) environment variables and the pinned `github.com/goadesign/surla` module version in `examples/orders/go.mod` (leaving `CP1` analysis, `CP4` verification, and `CP5` promotion unconfigured).
  - On a live target started with `start_live_target`, the server sends the text to the target's prompt operation (see [Live target prompt contract](#live-target-prompt-contract)) and returns typed errors:
    - `prompt_not_found` (HTTP 404) and `invalid_prompt` (HTTP 422) mean the target rejected the update, so its text is unchanged;
    - `target_rejected_credential` (HTTP 502) means the target rejected the server's key, so nothing changed; start the target again;
    - `live_target_ended` (HTTP 409) means the target is no longer running; start it again;
    - `update_not_confirmed` (HTTP 502) means the target did not answer, so it may or may not have applied the text; keep the draft and send it again.
  - With no live target and no `TargetUpdater`, `prompt_template` requests keep returning `no target prompt updater is bound to this control plane`, and `compiled_rule` requests never need a target.
- **`start_live_target`** (`POST /live-target`)
  - **Input:** `candidate_commit` (a full commit ID) → **Output:** `LiveTargetResult` (`candidate_commit`, `url`, `message`)
  - Requires a configured verification target (`-verification-config` or a committed starter workspace bound via `-project` or the default working directory): the server builds the commit with that configuration's candidate repository, binaries, and evaluator suite, exactly as `CP4` builds a candidate. It then runs the suite's `--live` mode (see [the suite contract](#surla-verify)) and binds `draft_override` to the running candidate. `url` is the candidate's loopback origin, for trying its own API.
  - The server runs at most one live target, so a second start stops the first before it starts the new one. A target runs until `stop_live_target`, its own exit, or the server's shutdown. The build continues if the caller disconnects, and is bounded by the configuration's `timeout`.
  - Starting a target first stops the one already running, which discards every prompt draft applied to it, even if the new build then fails. Prompt text survives only when it is committed.
  - Live targets get their own directories under `<cards-dir>/live`, which the cards directory's lock reserves for one `surla serve`. If the server crashes, its executor kills the target's processes, and the directory stays. At startup `surla serve` names directories an earlier server using the same cards directory left; it does not check their processes or remove them.
  - Errors: `live_target_unavailable` (HTTP 501) when verification is not configured, and `live_target_failed` (HTTP 422) with a builder-safe summary of the step that failed.
- **`stop_live_target`** (`DELETE /live-target`)
  - **Output:** `StopLiveTargetResult` (`stopped`, `message`). Stops the running live target and unbinds `draft_override`. `stopped` is `false` when none was running.
- **`diff`** (`POST /diff`)
  - **Input:** `old` (`DesignSnapshot`), `new` (`DesignSnapshot`) → **Output:** `DiffResult`
  - Compares two `DesignSnapshot` modules by evaluating their generated Goa/Goa-AI artifacts, tool schemas, and `go.mod` directives, paired with `ToolUI("ui://surla/checkpoint")` for structural diff visualization.

### Live target prompt contract

A live target serves one operation for `draft_override`. Surla calls it with a client it generates from its own description, [`design/targetprompt`](../design/targetprompt/design.go). The target declares the same method in its own Goa design and owns the implementation, so neither module depends on the other:

- `PUT /prompts/{prompt_id}` with body `{"template": "<Go text/template>"}` and header `X-Live-Target-Key: <key>` → `200` with `{"prompt_id": "...", "version": "..."}`.
- `401` (`unauthorized`) for a missing or wrong key, `404` (`unknown_prompt`) for an unregistered prompt, and `422` (`invalid_prompt`) for a template that does not render with the prompt's registered spec. The target renders the template before it saves it, so a rejected template leaves the text in use unchanged.
- The key is minted by `surla serve` for each `start_live_target`. It reaches the suite in `SURLA_LIVE_TARGET_KEY`, and the suite's `Live` function passes it to the candidate through `candidate.Spec.Env`. It lives as long as that target.
- `surla serve` also passes its active model configuration—from startup flags, startup `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` auto-detection, or the most recent `configure_model` call—so a suite's `Live` function can plan with the same model: `SURLA_LIVE_MODEL_PROVIDER` and `SURLA_LIVE_MODEL_NAME` for a direct provider (the provider's key stays in the variable `surla serve` read it from, such as `ANTHROPIC_API_KEY`), or `SURLA_LIVE_MODEL_GATEWAY` for a model gateway. Nothing is set when `surla serve` has no model configured. Suites decide whether they need a model; `start_live_target` itself does not.
- The evaluator suite that `surla init` generates implements this contract. Its `override_prompt` reads the key from `SURLA_LIVE_TARGET_KEY` and replaces `<name>.helper.system`. Its `Live` function serves a direct provider through its own loopback model gateway, keeping the provider key in the suite process. It passes a literal loopback gateway address, such as `127.0.0.1:9090` or `[::1]:9090`, to the candidate directly. It reaches any other gateway, including a hostname such as `localhost`, through a byte-only forwarder it owns on a loopback port, which resolves and dials the address outside the sandbox, because the candidate sandbox allows only loopback connections and resolves no names. In every case the candidate's environment holds only the live key.

Changing this contract is a breaking change for every target that implements it.

### MCP Resources

- **`surla://golden-path`** (`text/markdown` via MCP `resources/read` on `POST /mcp`; JSON-encoded string on `GET /golden-path`)
  - Serves the embedded Golden Path architecture playbook ([`internal/goldenpath/GOLDEN_PATH.md`](../internal/goldenpath/GOLDEN_PATH.md)) for Goa and Goa-AI platforms.
- **`ui://surla/checkpoint`** (`text/html;profile=mcp-app` via MCP `resources/read` on `POST /mcp`; JSON-encoded string on `GET /ui/checkpoint`)
  - Serves the stateless MCP Apps checkpoint viewer (`internal/controlplane/checkpoint.html`, built from [`ui/checkpoint/`](../ui/checkpoint/)). It completes the `ui/initialize` → `ui/notifications/initialized` host handshake and renders results delivered by the host via `ui/notifications/tool-result`, without server-retained preview state: the `CP1` Change Card form (saving through `confirm_intent` and recovering with `get_change` via host `tools/call`, displaying the previous saved revision in `#surla-prior-revision` when transitioning between revisions of the same `change_id`, keeping only changed draft answers and examples in `#surla-preserved-draft` across stale-revision conflicts or when switching to an incoming Change Card via `#surla-replace-incoming-card`, rendering the `#surla-cp5-approval` panel for a current approval check when `approval` is present or `#surla-cp5-unverified-approval` with `#surla-cp5-refresh-approval` when `unverified_approval` is present on the card, and refreshing a retained editor card via `get_change` after an applied `CP3` override without losing unsaved draft edits), the optional OpenUI decision view from `ui_composition` (labeled as a model-proposed illustration and sharing the form's draft), `CP2` architecture previews (a keyboard-focusable scrollable C4 graph region above inspectable lists and an expandable `#surla-cp2-swarm-manifest` Chorus manifest), unbound structural diffs, `CP3` draft results, and `CP4` verification results (`verify_change` and `get_verification`, with `#surla-cp4-verification` marked `aria-busy="true"` while a correlated `Check status` read is in flight and full `ForBuilders()` scenario/check counts and binding details when `completed`, while preserving any dirty `CP1` draft). Interactive role-switching and replay controls, and `openui.v1` action dispatch, remain planned; see [`docs/HOW_IT_WORKS.md`](HOW_IT_WORKS.md#checkpoint-3-cp3-fast-draft-overrides--live-sandbox-walkthrough).

---

## CLI Reference

Running `surla`, `surla help`, `surla -h`, or `surla --help` prints top-level usage to `stdout` and exits `0`. Running `surla help <command>` or `surla <command> -h` prints that subcommand's flags to `stderr` and exits `0`. Every subcommand rejects unexpected positional arguments (`unexpected arguments [...]`), so a stray non-flag token never causes Go's `flag` parser to silently ignore flags that follow it.

### `surla init`
Creates a Goa + Goa-AI + `goa.design/model` service in `<dir>` and a separate evaluator module in `<dir>/eval`. It resolves dependencies, generates each module's code inside Surla's sandbox with the same generator as [`surla gen`](#surla-gen) (the design code never runs on the host), checks both modules with `go vet ./...` and `go test ./...`, and prints the commands to commit, preview, and verify the project. Both module files start with the Go and library versions saved in the `surla` binary (`surla.GoMod()`). `surla init` builds one project tree in protected temporary storage outside the current working directory and destination parent, on the destination filesystem. It moves that completed tree to `<dir>` only after every step, including generation, has succeeded. A failure or interrupt leaves `<dir>` absent; publication is the final step and never replaces a directory, file, or symlink created meanwhile. Before generating the evaluator, `surla init` pins its exact Surla dependency with `go get github.com/goadesign/surla@<version>`; Go may select newer library versions required by that dependency.
- `-dir <path>` — Directory to create; it must not exist *(required)*. `surla init` never replaces an existing directory, file, or symlink, including one created while generation runs. Command logs, Go temporary files, and the unpublished tree stay in protected storage. A failed command keeps its log. A canceled or failed command removes its stage once its processes stop, or retains the stage and logs and reports their paths when cleanup cannot be confirmed. If Surla cannot find protected storage on the destination filesystem, it stops before building; choose a destination with room outside its parent for that storage.
- `-module <path>` — Go module path of the candidate service, such as `example.com/shop` *(required)*. Its last path element names the service and must be a lowercase Go identifier (`^[a-z][a-z0-9]*$`, excluding Go keywords and `main`).
- `-surla-version <version>` — Exact canonical semantic version (`golang.org/x/mod/semver`) of `github.com/goadesign/surla` required by `<dir>/eval/go.mod` (default: the build-info `Main.Version` stamped into the `surla` binary; required explicitly when running from a `(devel)` or `+dirty` build, and mutable labels such as `latest` or `main` are rejected).
- **Starter scope & visible-suite note:** The generated candidate (`cmd/<name>`) runs an in-memory Goa-AI `helper` agent (`RunPolicy`, `BindTo("price")`, and C4 `System`/`Container` model) connecting to `-model-gateway <host:port>`, registering `helper.SystemPrompt()` (`<name>.helper.system`, version `"v1"`) on each question's runtime and attaching `PromptRefs: []prompt.PromptRef{system.Ref}` on every model request. The generated `<dir>/eval/` module runs a scripted `goa-ai` model gateway and three starter scenarios: `prices_tea` tagged `"repair"` and `unknown_item` tagged `"holdout"`, checking the correlated `catalog.lookup_price` tool call and `ToolResultPart` exchange plus `pinned.Evidence("<name>")`, and `prompt_override_reaches_model` tagged `"repair"`, checking that `override_prompt` rejects a wrong key and that the next model request carries the new prompt text. Because `eval/` lives inside the same repository as the starter code, its `"holdout"` tag demonstrates `Report.ForBuilders()` redaction only; real unseen holdouts belong in an evaluator repository that builder agents cannot read.

### `surla preview`
Reads a Go design module on disk into a symlink-safe snapshot (`go.mod`, `go.sum`, and non-test `.go` files; run `go mod tidy` first if `go.sum` is missing), evaluates it inside the sandboxed Goa/Goa-AI/Model compiler (`designeval.Evaluator`) at **0 LLM tokens**, and prints the indented JSON `designeval.Result` (`system_name`, `declarations`, C4 `containers` and `edges`, `agent_policies`, and `tools`) to `stdout`, which holds nothing else. After the JSON, it writes one summary line to `stderr`, such as `surla: "orders": 3 container(s), 2 boundary edge(s), 1 agent policy(s), 4 tool contract(s)`, with the system name quoted; the `preview_architecture` `banner` carries the same summary without the `surla: ` prefix. Needs no running server, no Change Card, and no LLM API key. SIGINT and SIGTERM cancel the evaluation. Before the command exits, it removes its compiler workspace and evaluation temporary directories, which it creates in protected storage outside the current working directory and inspected module, including when `TMPDIR` points inside either; a build whose processes cannot be confirmed stopped keeps it, and the error names it.
- `-dir <path>` — Root directory of the design module holding `go.mod`, `go.sum`, and `design/` (default `.`).

### `surla preview-change`
Reads a Go design module on disk into a symlink-safe `os.Root`-confined snapshot (`go.mod`, `go.sum`, and non-test `.go` files), validates that every path and file is valid UTF-8, sends the snapshot to a running `surla serve` instance's `POST /changes/preview` endpoint via the generated Goa HTTP client bound to `-change-id` and `-request-revision`, and prints the indented snake_case JSON `ArchitecturePreviewResult` (`change_id`, `request_revision`, `design_digest`, `system_name`, `containers`, `edges`, `agent_policies`, `tools`, `swarm`, `banner`) to `stdout`. HTTP redirects are refused so the bearer token is never forwarded to a redirect target, and the command performs no automatic latest-card lookup or retry.
- `-server <url>` — HTTP or HTTPS origin URL of the Surla server, such as `http://127.0.0.1:8090` *(required; user credentials, non-root paths, queries, and fragments are rejected without echoing the raw URL)*.
- `-change-id <id>` — Confirmed Change Card ID to bind the preview to *(required)*.
- `-request-revision <digest>` — Confirmed `CP1` `request_revision` digest of the Change Card *(required)*.
- `-dir <path>` — Root directory of the design module holding `go.mod`, `go.sum`, and `design/` (default `.`).
- `-auth-token <token>` — Bearer token required on HTTP requests (default: `$SURLA_AUTH_TOKEN`, *required*; never printed in `-h` help output).
- `-builders <path.json>` — Optional JSON file containing an array of explicit builder scopes (`[{"name": "...", "paths": ["..."], "preferred_profile_id": "..."}]`) for proposing a `CP2` Chorus swarm roster (`@builder-<scope>` and `@reviewer`, plus optional `@architect` and `@eval-author` when `-architect-path` / `-eval-path` are supplied); unknown fields, `null` entries, missing required fields, and trailing data are rejected before sending the request.
- `-architect-path <glob>` — Optional repository-relative path glob for `@architect` in the `CP2` Chorus swarm roster (repeatable, requires `-builders`; each flag argument is preserved verbatim without splitting on commas or trimming spaces).
- `-eval-path <glob>` — Optional repository or evaluator-workspace relative path glob for `@eval-author` in the `CP2` Chorus swarm roster (repeatable, requires `-builders`; each flag argument is preserved verbatim without splitting on commas or trimming spaces).
- `-timeout <duration>` — Positive time limit for the HTTP preview request (default `2m`).

### `surla serve`
Starts the Surla HTTP and MCP JSON-RPC control-plane server protected by bearer-token authentication. When `-project` is omitted, `surla serve` binds to its working directory as the active workspace (or to a promoted subdirectory recorded in `<cards-dir>/workspace.json` after an earlier `init_project` call). When `-cards-dir` is omitted, `surla serve` places Change Cards (`<operator-project-dir>/cards-<sha256(startup_cwd\0real_workspace)>`, or `<startup_cwd>/.surla/cards` when that directory already lies outside the bound workspace and its enclosing Git working tree), verification runs (`<operator-project-dir>/runs`), reports (`<operator-project-dir>/reports`), design-evaluator temporary work (`<operator-project-dir>/designeval-work`), `init_project` staging (`<operator-project-dir>/init-stage`, created on demand), and live targets (`<cards-dir>/live`) in `0700` operator-private storage outside the workspace under `<UserConfigDir>/surla/operator/project-<sha256(real_workspace)>`, and refuses any configuration where `cards`, `runs`, or `reports` would lie inside the bound workspace or its enclosing Git working tree (when a builder edits a separate checkout outside the bound workspace, bind that checkout with `-project` or keep operator storage outside that additional grant). When a selected implicit workspace store (`.surla/cards`, `.surla/runs`, or `reports/`) already holds durable records and `<operator-project-dir>/imported` has not been created yet, `surla serve` stops startup and instructs the operator to run `surla serve -import-legacy-workspace` once with writers stopped before accepting new work. Once the listener is bound, `surla serve` prints a startup summary to `stderr` showing the bound HTTP URL, the MCP endpoint (`http://<addr>/mcp`), the Change Cards directory (`surla: saving Change Cards in <dir>`), and the on/off status of each checkpoint (`CP1`–`CP5`) along with the flag or host option that turns each off capability on (the bearer token is never printed in startup logs or `-h` defaults). On shutdown, `surla serve` closes the control plane (`cp.Close()`). It refuses new `init_project` calls and live targets, cancels any in-flight `init_project` or `preview_architecture`/`diff` evaluation and waits for them to finish, stops the live target, removes the shared design-evaluation build workspace (`<operator-project-dir>/designeval-work/surla-designeval-work-*`, created by the first evaluation, so a server that never evaluates a design creates none), and releases its exclusive lock on `-cards-dir` and the operator project lock. A build whose processes cannot be confirmed stopped keeps the workspace, and the exit error names it; a live target that cannot be confirmed stopped keeps the cards directory locked.
- `-addr <host:port>` — Listen address (default `127.0.0.1:8090`).
- `-principal <id>` — Authenticated operator principal ID *(required)*.
- `-auth-token <token>` — Bearer token required on HTTP and MCP requests (default: `$SURLA_AUTH_TOKEN`, *required*).
- `-cards-dir <path>` — Directory where `CP1` Change Cards are saved as `<sha256(change_id)>.json` so they survive restarts (default: `<UserConfigDir>/surla/operator/project-<sha256(real_workspace)>/cards-<sha256(startup_cwd\0real_workspace)>`, or `<startup_cwd>/.surla/cards` when outside the bound workspace and its enclosing Git working tree). One `surla serve` process at a time may use a given directory, enforced by an exclusive `flock` on `<cards-dir>/lock`, and `-cards-dir` must not lie inside the bound workspace or its enclosing Git working tree.
- `-model-gateway-addr <host:port>` — Optional model gateway for CP1 analysis over plaintext gRPC. The gateway chooses the model; follow the [gateway version and upgrade requirements](#start-the-surla-server). Cannot be combined with `-model-provider` or `-model-name`.
- `-model-provider <anthropic|openai>` — Optional direct provider for CP1 analysis. Requires `-model-name` and the matching `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` environment variable. Provider names must match exactly. When `-model-gateway-addr`, `-model-provider`, and `-model-name` are all omitted, `surla serve` auto-detects `ANTHROPIC_API_KEY` (`anthropic` / `claude-sonnet-4-6`) or `OPENAI_API_KEY` (`openai` / `gpt-5.4-mini`) from its environment, and callers can configure or switch the active model at runtime with `configure_model` (`POST /cp1/model`).
- `-model-name <model>` — Model ID for direct provider mode; required with `-model-provider` and rejected in gateway mode.
- `-swarm-profiles <path.json>` — Optional JSON array of configured coding-agent profiles for proposed CP2 assignments; see [Configure the Proposed Swarm Roster](#configure-the-proposed-swarm-roster).
- `-project <dir>` — Optional project workspace directory (default `.` when omitted). Binds `controlplane.Options.WorkspaceDir` to `<dir>`. When `-verification-config` is omitted and the workspace is an empty or uninitialized directory, `surla serve` starts ready for `init_project` over MCP and transitions in place once initialized, pinning `SuiteCommit` to the initial starter commit created by `init_project` in protected staging (`<cards-dir>/workspace.json`); when `-verification-config` is omitted and the workspace already holds a committed project created by [`surla init`](#surla-init) at server startup, `surla serve` resolves its repository and startup `HEAD` commit once at startup, reads `go.mod` and `eval/go.mod` from that pinned evaluator commit (never from working files or later builder commits), requires the starter layout (`cmd/<name>` and `eval/cmd/<name>-evals`, where `<name>` is the module path's last element), and configures `CP4` verification and `start_live_target` with `SuiteCommit` pinned to that commit, runs and reports in operator-private storage outside the workspace (`<operator-project-dir>/runs` and `<operator-project-dir>/reports`), and a `30m` timeout. Its Git reads ignore inherited `GIT_*` variables and replace refs, run no repository hooks, and never fetch objects missing from a partial clone (check out `HEAD` first so Git downloads them); that needs Git 2.45 or newer, and an older Git stops startup with a message saying so. Can be combined with `-verification-config <path.json>` so `-project <dir>` binds the workspace for `CP1`/`CP2` design reads while `-verification-config` supplies an explicit external verification plan.
- `-import-legacy-workspace` — Optional boolean flag performing a one-time import of legacy in-repository stores into `<UserConfigDir>/surla/operator/project-<sha256(real_workspace)>/imported/` (`cards-<sha256>`, `runs`, `reports/default`, and `reports/dir-<sha256(report_dir)>`):
  - **Change Cards (`cards-<sha256>`):** When `-cards-dir` is omitted, imports `<startup_cwd>/.surla/cards` into `imported/cards-<sha256(startup_cwd\0real_workspace)>` when `<startup_cwd>/.surla/cards` lies inside the bound workspace or its enclosing Git working tree, and imports `<real_workspace>/.surla/cards` into `imported/cards-<sha256(real_workspace\0real_workspace)>`. An explicit `-cards-dir` or a `<startup_cwd>/.surla/cards` outside the workspace and its enclosing Git working tree uses that external store directly.
  - **Workspace runs and default reports (`runs` and `reports/default`):** When `-verification-config` is omitted—or when `<real_workspace>/.surla/runs` holds accepted run directories or `<real_workspace>/reports` holds `<64-hex>.json` reports—imports accepted run directories from `<real_workspace>/.surla/runs` into `imported/runs` and `<real_workspace>/reports` into `imported/reports/default`.
  - **Configured and run-referenced report directories (`reports/dir-<sha256(report_dir)>`):** Independently snapshots any `report_dir` referenced by `-promotion-config` or by an accepted legacy run's `request.json` `plan.report_dir` when that directory lies anywhere inside the bound workspace or its enclosing Git working tree (including when `-verification-config` is set and `<real_workspace>/.surla/runs` and `<real_workspace>/reports` are empty); report directories outside the bound workspace and its enclosing Git working tree remain unchanged at their external paths.
  Requires at least one applicable legacy store when both `-cards-dir` and `-verification-config` point outside the workspace and its enclosing Git working tree. `<operator-project-dir>/imported` must not already exist, and every imported entry is opened without following symlinks (`O_NOFOLLOW|O_NONBLOCK`, rejecting non-regular files and active writers via non-blocking shared `flock` locks on legacy card and run locks) and staged into an `0700` staging directory before an atomic `RENAME_EXCL` publication while leaving the legacy directories untouched. Existing card, report, and accepted run files (including corrupt records) are copied verbatim so saved history and corruption errors remain inspectable; only decodable `request.json` files whose `report_dir` points inside the bound workspace or its enclosing Git working tree are rewritten to their published `<imported>/reports/...` path.
- `-verification-config <path.json>` — Optional JSON file configuring Checkpoint 4 (`CP4`) verification for `verify_change` and `get_verification`. All fields are required (`runs_dir`, `report_dir`, `candidate_repo`, `candidate_module`, `binaries`, `suite_repo`, `suite_commit`, `suite_command`, `suite_module`, and `timeout` as a positive Go duration such as `"30m"`), unknown fields or trailing data stop startup, and the config file, `runs_dir`, and `report_dir` must not lie inside the bound workspace or its enclosing Git working tree. When `-verification-config` is set, `surla serve` uses that static plan and does not fall back to starter-workspace verification resolution. When neither `-verification-config` nor a committed starter workspace is present, `verify_change` and `get_verification` return `verification is not configured on this control plane`.
  Live targets started by `start_live_target` use the same configuration and start with the server's environment, which carries the model provider settings the evaluator suite reads, without `SURLA_AUTH_TOKEN`.
- `-promotion-config <path.json>` — Optional JSON file configuring the Checkpoint 5 (`CP5`) staged-release `PromotionTarget` for `approve_merge`. All fields are required (`report_dir`, `release_dir`, `suite_id`, `suite_commit` as a 40- or 64-character lowercase hex commit ID, `environment`, and non-empty distinct artifact names in `binaries`), relative `report_dir` and `release_dir` paths are resolved against the working directory once at startup, and unknown fields or trailing data stop startup. The operator publishes a complete `<release_dir>/<commit>/` directory in one atomic rename and never edits its regular files in place. For a full candidate `commit` ID, `CurrentInputs` (called by `approve_merge` and on every `get_change` or no-op `confirm_intent` of an approved card) opens `<release_dir>/<commit>` once via `os.OpenRoot` and rehashes each configured regular-file binary (`O_NONBLOCK`) through that directory handle (a missing file, non-regular file, or symlink that leaves `<release_dir>/<commit>` fails `approve_merge`, naming the binary and commit, and causes `get_change` and no-op `confirm_intent` to omit `approval` and return `unverified_approval` with a path-free operator recovery reason while recording the error on the `controlplane.check_approval` span), approving those staged binaries for a fresh launch with the evaluator suite's fixed configuration (it makes no claim about processes already running, and replacing `<release_dir>/<commit>` with different binaries causes `get_change` and no-op `confirm_intent` to omit the stale approval). When omitted, `approve_merge` returns `no promotion target is bound to this control plane`, and reading a card with a previously saved approval returns `unverified_approval` naming `-promotion-config`.

Model, swarm-profile, verification (`-project` or `-verification-config`), and promotion-config flags are independent; `-project` supplies no credentials and does not enable `CP5`. Incomplete or mixed model options stop startup with an error. A supplied profiles file must contain one JSON array: unknown fields, trailing JSON values, duplicate profile IDs, empty required metadata, and unknown capabilities stop startup. Omitted profiles leave the proposed roster unconfigured.

### `surla diff`
Reads two Go design modules into symlink-safe snapshots (`go.mod`, `go.sum`, and non-test `.go` files), evaluates both inside the sandboxed Goa/Goa-AI compiler at their own `go.mod` dependency versions, and prints the JSON `DiffResult` to `stdout`. Both evaluations share one build workspace, so the second reuses the dependencies the first compiled. SIGINT and SIGTERM cancel them, and the workspace is removed before the command exits, as for `surla preview`.
- `--old-dir <path>` — Root directory of the previous design module *(required)*.
- `--new-dir <path>` — Root directory of the candidate design module *(required)*.

### `surla gen`
Regenerates the `gen/` directory of a design module from its `design/`, inside Surla's sandbox, and replaces `gen/` in one step. The design code and its generators are compiled and run only in the sandbox; Surla writes their output.
- `-dir <path>` — Root directory of the design module holding `go.mod`, `go.sum`, and `design/` (default `.`).

`surla gen` reads the module as `surla preview` does, then builds and runs a generation program with the module's own dependency versions. That program calls Goa's `generator.CompleteSources`, so the module must select `goa.design/goa/v3` `v3.35.1-0.20261103205041-35e789e8f9b1` or later. With an older Goa, the build fails, and the error shows the compiler output and the Goa version the module uses; update the module's `goa.design/goa/v3` requirement and `go.sum`, then run it again. A design with gRPC transports is refused, because protoc compiles its `.proto` files after generation and cannot run in the sandbox; generate those designs with the module-pinned `go run goa.design/goa/v3/cmd/goa gen <module>/design`.

The new `gen/` is written to a `.surla-gen-<random>` directory in the module and swapped with the current `gen/` in one atomic rename (or moved in when there is none), so `gen/` is never missing or half-written. As with `goa gen`, generated subdirectories that the design no longer produces disappear, and top-level files in `gen/` that are not generated are kept. Generated file headers record `$ surla gen`. Until the swap, any failure or interrupt (SIGINT or SIGTERM) leaves `gen/` untouched and exits non-zero. After the swap, `surla gen` prints `gen/ updated`, the files added, changed, and removed, and a note that it did not build the whole application against the new `gen/` (check that hand-written code such as `internal/` and `cmd/` still matches it before you run `surla verify` or start a live target), and exits 0; writing `AGENTS_QUICKSTART.md`, removing the staging directory, or listing the changes can then only print warnings. `go.mod` and `go.sum` are never changed: when the generated code imports a module `go.mod` does not require, the command says to run `go mod tidy`. Leftover `.surla-gen-*` directories from an interrupted run are listed and never deleted.

### `surla verify`
Clones the committed candidate and evaluator suite into an isolated work directory, builds the candidate binaries (`WorkDir/candidate-bin/<name>`) and suite runner (`WorkDir/suite-bin/suite`) inside `verify/candidate`'s macOS `sandbox-exec` jail, runs the suite, writes the full `verify.Report` JSON to `<report-dir>/<digest>.json`, and prints its builder-redacted JSON view to `stdout` by default (setup or build errors exit non-zero without printing report JSON, while an assembled report with a failing scenario is written and printed before exiting non-zero). Pass `-builder-view=false` to print the full report instead. `stdout` holds only that JSON. After it, `surla verify` writes one summary line to `stderr` with the report's verdict, its scenario counts, the quoted IDs of scenarios that did not pass, and the report digest, for example `surla: verification report did not pass: 1/3 scenarios passed; failed: 1, not run: 1, harness errors: 0; not passing: "refund_over_limit", "slow_refund" (report 4f2c…)`. A caller that merges the two streams sees that line after the JSON.
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
- `-timeout <duration>` — Optional time limit for the verification run (such as `30m`; default `0`, meaning no limit). When a run is canceled or exceeds `-timeout`, `surla verify` stops the run's process group, prints any partial report the suite saved before stopping, followed by its `stderr` summary line, and exits non-zero. The summary describes that partial report, which can pass even though the run was stopped.
- `-builder-view` — Boolean flag, default `true`; omitted, bare, or `-builder-view=true` prints `Report.ForBuilders()` to `stdout`, which redacts `"holdout"` scenarios to `"id": "holdout-N"` with scenario names, inputs, declarations, checks, and failure logs stripped. The `stderr` summary uses the same view, so a holdout appears only as `holdout-N`. Pass `-builder-view=false` for the full report and scenario names in the summary. The full unredacted report is always written to `-report-dir`. In either view, a run that stops with a passing partial report or no report still prints raw operator diagnostics to `stderr`; do not share those diagnostics with builder agents.

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
3. **Live target (`--live --artifact <name>=<path> ...`), optional:**
   - Verification never runs this mode. It starts the candidate for interactive `CP3` prompt drafts and is available only when the suite sets `Command.Live`. A suite without `Live` exits `2` (`suite.ExitError`) with `this evaluator suite does not support --live`, and its `--describe` and verification runs are unchanged.
   - `suite.Main` reads the target's credential from the `SURLA_LIVE_TARGET_KEY` environment variable (`suite.LiveKeyEnv`), never from a flag. It pins the binaries as in execution and calls `Command.Live(ctx, pinned, key)`. `Live` starts the candidate the way a scenario does, passes the key to the candidate only through `candidate.Spec.Env`, and returns a [`suite.LiveTarget`](../verify/suite/suite.go) once the candidate listens.
   - `suite.Main` then prints one line, `{"url":"http://127.0.0.1:<port>"}` (only an `http` origin on `127.0.0.1` is accepted), and serves until `SIGINT` or `SIGTERM` or until the target ends. After a stop request it cancels `ctx`, waits for `LiveTarget.Done`, removes the pinned-artifact store, and exits `0`. A target that ends on its own, a start failure, or an unclean stop exits `2`. `--live` cannot be combined with `--describe`, `--scenario`, or `--tag`.

### `surla approve`
Decodes a saved full `verify.Report` JSON file with `verify.DecodeReport`, checks it against explicit promotion flags with `verify.Approve`, and prints an indented JSON approval summary (`report_digest`, `binding`, `counts`) to `stdout` (exiting `0` when the report passed and matches every flag, or exiting `1` without stdout output if any flag is missing or the report is stale, failed, or unreadable). `surla approve` validates the report file only; it does not contact a control plane, record a Change Card approval, or merge/deploy.
- `-report <path>` — Path of the full verification report JSON written by `surla verify` *(required)*.
- `-candidate-commit <sha>` — Full commit ID of the candidate being promoted *(required)*.
- `-request-revision <digest>` — Current confirmed `request_revision` digest of the change *(required)*.
- `-suite-id <id>` — Evaluator suite identifier required for promotion *(required)*.
- `-suite-commit <sha>` — Full commit ID of the evaluator suite required for promotion *(required)*.
- `-environment <env>` — Dependency profile required for promotion, such as `sim` *(required)*.
- `-artifact <name=sha256>` — Candidate binary artifact name and 64-character lowercase hex SHA-256 digest measured for promotion (repeatable, at least one *required*; duplicate artifact names are rejected).

### `surla version`
Prints the module version this `surla` binary was built from (such as a pseudo-version stamped from a clean checkout, or `(devel)` when built without module version metadata) to `stdout`. Accepts `-h`/`--help` and rejects positional arguments.

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

The end-to-end and starter-project integration tests require macOS (`sandbox-exec`); `examples/orders-eval/acceptance` also requires the Temporal CLI. Run the five acceptance scenarios and the seeded token-budget regression, the verifier's complete `Run` → report → `Approve` test, the `surla gen` and `surla init` generation journeys, and the `surla init` starter-project build and sandboxed evaluator test (all run automatically on GitHub Actions in [`.github/workflows/ci.yml`](../.github/workflows/ci.yml)):

```bash
(cd examples/orders-eval && go test -race -tags e2e ./acceptance)
go test -race -tags e2e ./internal/verify
go test -race -count=1 -tags e2e ./cmd/surla
go test -race -count=1 -timeout 60m -tags scaffold ./internal/scaffold
```

See [`examples/orders/README.md`](../examples/orders/README.md) for a step-by-step walkthrough including `surla verify` (default builder view vs. full operator report with `-builder-view=false`).

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
│       └── main.go               # CLI entrypoint (`surla init`, `surla serve`, `surla preview`, `surla preview-change`, `surla diff`, `surla gen`, `surla verify`, `surla approve`, and `surla version`)
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
│   ├── candidate/                # macOS Seatbelt (`sandbox-exec`) build & runtime isolation jail (`Build`, `BuildTest`, `Pin`, `Exec`)
│   └── suite/                    # Evaluator suite command runner (`suite.Main`, `suite.Command`, `suite.Declaration`)
├── examples/
│   ├── orders/                   # Reference Goa + Goa-AI + Temporal + SQLite order-refund platform
│   └── orders-eval/              # Isolated evaluator harness, simulators, and 5 acceptance scenarios
├── gen/                          # Generated Goa HTTP, JSON-RPC, and MCP server/client packages
├── PROPOSAL.md                   # Full architecture & verification design specification
└── PRESS_RELEASE.md              # Working Backwards product vision
```
