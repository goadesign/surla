# CLI, MCP & Repository Reference

**[← Back to README](../README.md)** · **[← How Surla Works](HOW_IT_WORKS.md)** · **[← Golden Path Architecture](ARCHITECTURE_PLAYBOOK.md)**

---

## Terminology at a Glance

| Term | Plain-English Meaning |
| :--- | :--- |
| **`CP1` – `CP5`** | Surla's five verification checkpoints: **CP1** Confirm Intent (`init_project`, `propose_change`, `revise_change`, `confirm_intent`, `decline_revision`), **CP2** Preview Compiled Architecture (`preview_architecture`, or local `surla preview`), **CP3** Fast Draft & Live Sandbox (`draft_override`, `start_live_target`, `stop_live_target`), **CP4** Isolated Sandboxed Verification (`verify_change`, `get_verification`, `list_verifications`, `surla verify`), and **CP5** Promotion Check (`surla approve`, `approve_merge`). |
| **`Change Card`** | The versioned request, questions, examples, preserved behavior, Story and approval saved by the project server. See [project state](#surla-serve). |
| **`request_revision`** | The SHA-256 hex digest of a Change Card's canonical content (including `design_source` and any adopted `stories`). `preview_architecture`, `verify_change`, `surla verify`, `surla approve`, and `approve_merge` bind to `request_revision`, so when the user refines the agreed examples or answers, or adopts a revision with other examples, answers or Story flows, previews, reports and approvals of the earlier revision no longer match it. A revision the guide proposes beside an agreed change leaves them valid until the user adopts it. When running standalone `surla verify` and `surla approve` on the CLI without a control-plane Change Card, pass any explicit revision string (such as a commit or label) consistently to both commands. |
| **`holdout`** | An unseen acceptance scenario (tagged `"holdout"` in the evaluator suite) whose name, inputs, checks, and logs are redacted to `"id": "holdout-N"` in `verify_change` / `get_verification` and the default `surla verify` report (`Report.ForBuilders()`) so coding agents cannot hardcode around the test cases. |
| **`input_binding`** | The generated input schemas and compiler-listed `injected_fields` for a tool used by an agent (`ToolInputBindingItem`). It describes the declared input choices, not which option the running application installed. |
| **`CardsDir` / `WorkspaceDir` / `PromotionTarget` / `Verification` / `WrittenTestRuns` / `Live`** | Options of `controlplane.New`, owned by `internal/controlplane`: private card storage, the workspace, independently measured promotion inputs, how `CP4` verifies (`Verification`, the operator's stored evaluator, or `WrittenTestRuns`, runs of the tests Surla accepted for each change's build; never both), and the live targets `start_live_target` runs. The CLI derives storage and identity from its [project state](#surla-serve). |

---

## Prerequisites

- **Go 1.27.3+**, matching the repository's `go.mod` files (`surla init`, `surla gen`, and `surla preview` run Goa inside Surla's sandbox with each module's own versions, so a globally installed `goa` binary is not required)
- **Surla CLI** (`go install github.com/goadesign/surla/cmd/surla@latest`, or `go run ./cmd/surla` from a repository checkout)
- **macOS** (`sandbox-exec` is used by `verify/candidate` to isolate candidate builds and processes; non-macOS hosts fail closed with `ErrIsolationUnavailable`). Surla must be able to apply that sandbox itself. When a sandboxed command exits with status 71 (such as `compile Goa design: sandboxed build of .: exit status 71 (sandbox-exec: sandbox_apply: Operation not permitted)` when `surla preview` or `surla gen` runs inside a coding agent's restricted shell, or during `verify_change` and `start_live_target`), Surla preserves the observed error and adds conditional recovery guidance (`The sandboxed command exited with status 71. If Surla is running inside a restricted coding-agent shell, its sandbox may have been blocked; run Surla from an unrestricted host shell.`). Run Surla from an unrestricted host shell, or call `preview_architecture` on a Surla project server started from an unrestricted host shell, so Surla's sandbox can apply. Surla never retries outside its sandbox.
- **Docker Desktop** (or another local Docker engine reached through a `unix://` socket) for `check_project` and `update_dependencies`, which run Go commands in Linux containers. The first check pulls the pinned `golang:1.27.3` image (about 295 MiB).
- **Temporal CLI** (`brew install temporal`) for running the end-to-end durable workflow crash and replay verification suites

## MCP Tools & Resources

All tools and resources are served over MCP JSON-RPC at `POST /mcp` plus HTTP REST on the same listen address by the project server (authenticated with its issued caller credentials). Each Change Card and verification HTTP route declares its method-specific non-fault `ErrorResult` responses (`403` `forbidden`, `404` `not_found`, `409` `conflict`, `422` `unprocessable`, and `501` `not_implemented`) in [`design/design.go`](../design/design.go) and [`gen/http/openapi3.yaml`](../gen/http/openapi3.yaml)—for example, calling `POST /changes/preview` on a `needs_confirmation` card returns HTTP `409` `conflict` (`fault: false`)—while unclassified evaluator, storage, and owner failures remain HTTP `500` faults and MCP `tools/call` returns non-fault tool error results (`isError: true`) with the same domain error message. On every HTTP route, a JSON request body that carries a field the route's design does not declare, at any depth, is refused before the service runs with HTTP `400` and error name `decode_payload`, whose message names the field (for example `json: unknown field "id"`); remove undeclared fields from the request. MCP tool arguments with undeclared fields are refused as `invalid arguments:` tool errors.

The same policy governs HTTP calls, MCP calls, and MCP tool discovery. Only the
user session can confirm intent, approve a result, or configure the model. The
guide proposes/revises changes, refreshes Story, and initializes the project.
The builder generates/checks code, updates dependencies, starts live targets,
drafts prompts, and requests verification. The user and builder can stop a live
target. User, guide, and builder share change, architecture, diff, verification,
trace, check-output, and Story-refresh reads. Only the user session starts a
build; all four read a change's build, and only the user session reads the
text of Surla's findings on its tests. The builder reads its own build
assignments, and the test writer reads its own test assignments and reports
them finished (see [Builds and assignments](#builds-and-assignments)).
Management operations are not MCP tools. A refused
call changes no domain state. Every proposal waits for user confirmation even
when it has no questions. Resources belong to the immutable project owner;
caller attribution stays in private operation diagnostics, while exported spans
carry no caller identity.

### MCP Tools

`propose_change` and `revise_change` take the coding agent's own analysis of the feature request as the required `analysis` argument; Surla runs no model to propose or revise a Change Card, so these tools need no model credential. The agent reads the target's `design/*.go` and the `surla://golden-path` playbook, then sends a `summary`, the `ambiguities` only the requester can answer (each a `question` with at least two distinct `options`; an empty list when nothing is open), at least one concrete example (`scenario`, `input_summary`, `expected_outcome`, `verification_kind` of `deterministic` or `model_judged`), the `preserved_behaviors` the change must keep (possibly empty), and an optional `ui_composition`. The tool and field descriptions in the MCP input schema state what a good analysis contains. Surla checks the analysis in two places and saves nothing when either check fails:

- The generated MCP and HTTP decoders check its structure: a missing analysis or required field, a blank text field (one made only of whitespace, ASCII or Unicode), a `null` list element, fewer than two options or no example, or an unknown `verification_kind`. MCP returns a tool error starting with `invalid arguments:` that names the field; HTTP returns `400` (for example `missing_field` or `invalid_pattern`). Both refuse a field the contract does not declare, such as a caller-chosen ambiguity `id`: MCP with an `invalid arguments:` tool error, HTTP with `400` `decode_payload` (see [MCP Tools & Resources](#mcp-tools--resources)).
- The service checks the rules that span fields and returns `unprocessable` (HTTP `422`, a tool error over MCP) whose message starts with `invalid analysis:` and names the broken rule: an ambiguity repeats an option (compared exactly and with surrounding whitespace removed), or, for `revise_change`, a current example is neither kept through an `examples[].prior_id` nor dropped through `removed_example_ids`, is named in both, is named twice, or an ID names no current example.

Surla cannot judge whether an analysis is complete or correct; the requester does that when reviewing and confirming the card with `confirm_intent`.

- **`init_project`** (`POST /workspace/init`)
  - **Input:** `module_path`, `dir?` → **Output:** `InitProjectResult` (`dir`, `module_path`, `service_name`, `commit`)
  - Scaffolds a new Goa + Goa-AI + `goa.design/model` starter service and `<dir>/eval` evaluator suite inside a direct child directory `<workspace>/<dir>` of the bound workspace (`controlplane.Options.WorkspaceDir`), runs `go mod tidy`, `go vet ./...`, and `go test ./...` in both modules, records the initial Git commit `"Initial scaffold"` (`commit`) inside the protected staging directory before atomic publication (init_project creates the initial commit in private staging using the server's configured Git author, committer and signing policy. Git rules selected only by the final project folder apply to later commits; configure identity and any required signing credentials for the server before initialization), and transitions the running control plane's active workspace in place without restarting the Surla project server. When `dir` is omitted, it defaults to `service_name` (the last path element of `module_path`); when `dir` is supplied, it must be a single direct child directory name (`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`, excluding `.`, `..`, and `.surla*` prefixes). The bound workspace must contain no project markers (`go.mod`, `design`, `gen`, `internal`, `cmd`, `eval`), no existing `<workspace>/<dir>` entry, no committed Git `HEAD`, no verification runs in `RunsDir`, and no saved Change Cards whose `design_source` was captured from workspace design files; `<workspace>/<dir>` is staged in operator-owned storage (`Options.InitStageDir` or a `0700` operator staging directory on the workspace filesystem), published atomically without overwriting an existing destination (`RENAME_EXCL` on macOS, `RENAME_NOREPLACE` on Linux), and recorded in `<cards_dir>/workspace.json`, so a server restart keeps the initialized project as the active workspace. When the request includes a `progressToken`, the tool sends MCP progress notifications at each initialization phase. Requires `controlplane.Options.WorkspaceDir` and a canonical semantic `controlplane.Options.SurlaVersion` (from `surla` build info on the Surla project server). Returns `not_implemented` (HTTP `501`) when no workspace or canonical `SurlaVersion` is configured, `unprocessable` (HTTP `422`) when `module_path` or `dir` is invalid, and `conflict` (HTTP `409`) when `<workspace>/<dir>` already exists or workspace initialization preconditions fail.
- **`generate_code`** (`POST /workspace/generate`)
  - **Input:** `target?` (`candidate`, the default, or `eval`) → **Output:** `GenerateCodeResult` (`target`, sorted `files_written`, `warnings`).
  - Reads the selected module's local files from the active workspace and compiles its Goa design in Surla's sandbox, then atomically replaces that module's `gen/`. For example, after editing `design/design.go`, call with `{}`; after editing `eval/design/design.go`, call with `{"target":"eval"}`. Repeat after another edit on the same server. The `eval` target uses only the public local `eval/` module; it does not access an externally configured verification suite. Top-level user files under `gen/` and their permissions survive, stale generated subdirectories are removed, and `go.mod` and `go.sum` stay unchanged. The result lists paths relative to the selected module. Errors mean `gen/` was not replaced. A generation failure states the failed phase and available sandbox details; correct a reported local problem or ask the server operator to restore dependency access or compiler storage, then call again. A warning after publication still means `gen/` was updated. This compiles the design only: then call `check_project` to build and test the uncommitted modules before committing for verification. No model configuration, Change Card, commit, or server restart is required.
- **`refresh_story`** (`POST /workspace/story/refresh`)
  - **Input:** none → **Output:** `StoryRefreshResult` (`op_id?`, `state`, `snapshot_digest?`, `stories`, `diagnostic?`, `message`).
  - Reads the active workspace's Go module snapshot (`go.mod`, `go.sum`, and non-test `.go` files) itself, computes its canonical `snapshot_digest` (the hex-encoded SHA-256 of the snapshot), and starts or reuses the project owner's background Story evaluation for that workspace inside Surla's sandbox without holding control-plane locks during the snapshot read or sandboxed evaluation. Returns promptly with `state` (`running` or `succeeded`), `op_id`, `snapshot_digest`, `stories` (when already `succeeded`), and `message`; call `get_story_refresh` with `{}` to read the observed `stories` or `diagnostic` once a `running` evaluation finishes. Pass a succeeded `op_id` as `story_refresh_op_id` on `propose_change` or `revise_change` to adopt the evaluated Stories and matching `design/*.go` snapshot into the Change Card's `request_revision`.
  - The control plane keeps one current operation in memory per project owner and bound workspace identity (`(owner, workspaceDir, workspaceDev, workspaceIno)`) and writes nothing to disk. Every caller acts for the owner, so the user, the guide and the builder read the same operation: the guide starts it and the user's [views page](#views-page) shows it. Refreshing an unchanged snapshot whose current operation is `running` or `succeeded` returns that operation without starting another evaluation. Refreshing a changed snapshot, or one whose operation `failed`, cancels and replaces the prior operation; a replaced or late evaluation publishes nothing. Each refresh runs on a server-owned context with a fixed 5-minute deadline in the control plane (`defaultStoryRefreshTimeout`), so an accepted evaluation survives client disconnects while the server runs and reports `state: "failed"` with a deadline `diagnostic` if the limit expires. Story-only designs evaluate before any Goa service or C4 container exists (`preview_architecture` still requires at least one Goa API or Service container), and a design that does not load `github.com/goadesign/surla/story` succeeds with `stories: []`. Returns `not_implemented` (HTTP `501`) naming the enabling step (`start Surla from a project directory`) when no workspace is bound, and `unprocessable` (HTTP `422`) when the bound workspace has no `go.mod`, was replaced, or contains invalid UTF-8 files.
- **`get_story_refresh`** (`GET /workspace/story/refresh`)
  - **Input:** none → **Output:** `StoryRefreshResult` (`op_id?`, `state`, `snapshot_digest?`, `stories`, `diagnostic?`, `message`).
  - Reads the project owner's current in-memory Story refresh operation for the active workspace without starting an evaluation. `state` is `none`, `running`, `succeeded`, or `failed`. When `state` is `succeeded`, `stories` holds the observed domain Stories in declaration order (`name`, `description?`, `actors`, and `flows` with `complete`, `steps`, and step or flow `questions`, empty when the design declares no Stories), and `op_id` can be passed as `story_refresh_op_id` to `propose_change` or `revise_change`. When `state` is `failed`, `diagnostic` holds the sanitized failure or timeout diagnostic. Before any refresh—or after a server restart, or when the bound workspace has no `go.mod` yet—`get_story_refresh` returns `state: "none"` as an expected absence with a `message` instructing the caller to run `refresh_story`. Returns `not_implemented` (HTTP `501`) naming the enabling step when no workspace is bound.
- **`check_project`** (`POST /workspace/check`)
  - **Input:** none → **Output:** `CheckProjectResult` (`op_id`, `passed`, `message`).
  - Runs `go test ./...` for every Go module under the active workspace (the application module and nested modules such as `eval/`, skipping `testdata`, `vendor` and directories starting with `.` or `_`) on the current files, uncommitted edits included, without changing the workspace. For example, after editing `internal/orders/service.go`, call it with `{}`.
  - **What runs where:** the server copies the workspace's live files (the enclosing Git working tree accepted when the Surla project server started, when there is one, so sibling modules used by `replace` resolve) into `check-work/<op_id>` inside the server's private Change Card directory, which only this server can use while it runs. The copy keeps ignored files and symlink targets as they are, never follows a link, and leaves out `.git` and `.surla*`. Each module then runs in two Linux containers from the pinned `golang:1.27.3` image, with the copy mounted at its original path: first `go list -deps -test ./...` downloads dependencies with the copy read-only (using the operator's `GOPROXY`, `GOSUMDB`, `GOPRIVATE`, `GONOPROXY`, `GONOSUMDB` and `GOINSECURE`, but no credentials or HTTP proxy settings), then `go test ./...` runs with no network. Go selects the toolchain itself (`GOTOOLCHAIN=auto`). the Surla project server runs each call in its own helper process, `surla check-run`, started in a separate session. The helper creates, runs and removes the containers and saves the output, and it finishes that work even when the call is canceled or the Surla project server stops or dies during it: the helper stops and removes the container and confirms the removal without waiting for another server (when it cannot confirm it, the container stays recorded, as **Errors** describes), and no container is started or dependency file written after the cancellation.
  - **Result:** `passed` is true only when every module's tests exit 0. `message` (at most 16 KiB) names the modules tested and the first failure with the end of its output. When dependencies cannot be downloaded, the message says to call `update_dependencies` if Go's output shows a missing `go.sum` or `go.work.sum` entry or a `go.mod` update. The full output stays in the operation directory; read it with `read_check_output`.
  - **Errors:** `not_implemented` (HTTP `501`) when no workspace is bound; `unprocessable` (HTTP `422`) when the workspace has no `go.mod`; `unavailable` (HTTP `503`) when `docker` is missing, the engine does not answer or is not local, or the image cannot be pulled (start Docker, or set `DOCKER_HOST` or the Docker context for the Surla project server; Docker is looked up on every call, so no restart is needed). When the workspace changes while it is being copied, or a container's removal cannot be confirmed, the call fails with a message naming the path or the container (`docker rm -f <name>`); the operation directory is kept until removal is confirmed, and the Surla project server retries on shutdown and at the next start. Surla treats a container as gone only when a listing of the operation's containers (`docker container ls --all` filtered by the operation's `surla.check` label) completes without its name; it never relies on what `docker rm -f` prints or on its exit status, which is 0 even for a missing container on Docker Engine 29.9.3, and a listing that fails proves nothing. A container whose creation Docker never confirmed (Docker reported an error, or did not answer within 30 seconds of a cancellation) could still appear later, so its name stays recorded until a listing shows the container and a later listing, after its removal, no longer does; the Surla project server checks again on every shutdown and start. Such a container was never started, so its operation's copy of the workspace is removed while its name and output stay. A start skips any operation whose helper is still running. These rules were checked live on Docker Engine 29.9.3 for created and exited containers only; a running container in the listing, a creation still in flight and a Docker restart were not observed live. When the Surla project server died during a call and no OpenTelemetry collector is configured (see [Operation diagnostics](#operation-diagnostics)), the cleanup is recorded only in the operation directory `<cards-dir>/check-work/<op_id>/`: `owner.json` lists in `containers` every container whose removal is not confirmed, each with a `state` of `creation_pending` (Docker never confirmed creating it, and it was never started), `creation_answered` (created, and possibly started), and the list is empty once every removal is confirmed; `output.json` exists once the output is final, and `read_check_output(op_id)` then returns it. No tool lists operation IDs or reads `owner.json`, and `list_traces` does not show the helper's spans.
- **`update_dependencies`** (`POST /workspace/dependencies`)
  - **Input:** none → **Output:** `UpdateDependenciesResult` (`op_id`, `files_written`, `message`).
  - Runs `go mod tidy` for every workspace module in the same kind of protected copy and containers (with network), and `go list -deps -test ./...` when the copy has a `go.work` so `go.work.sum` gets its entries, then writes only the `go.mod`, `go.sum` and `go.work.sum` files whose bytes changed back to the workspace. Before replacing a file, the server locks its directory and compares the live file with the bytes it read when the call started. A file that is a symlink, was edited before that comparison, or was created by someone else meanwhile is not written, and `message` says why. If another `update_dependencies` call, from this server or another one on the same workspace, is writing the same directory, the file is not written and `message` asks you to call again. An editor that writes the file between the comparison and the replacement is not detected. Modules are written one at a time, so a later module's failure leaves earlier modules' files written. Files are written one at a time too: once the call is canceled, times out, or the Surla project server stops, no further file is written, while a file already being written is finished. Such a stop can leave a module with its new `go.mod` and its old `go.sum`; `message` lists each file written and each file not written, and the saved output records each file as it is written. A call stopped before writing any file fails and writes nothing.
- **`read_check_output`** (`POST /workspace/check/output`)
  - **Input:** `op_id`, `cursor?` → **Output:** `CheckOutputPage` (`text` of at most 16 KiB, `next_cursor?`, `captured_bytes`, `dropped_bytes`).
  - Use the `op_id` from a `check_project` or `update_dependencies` result, or from its error message when an operation failed after opening its output capture. A failed operation's output may be incomplete; the operation still reports failure.
  - Returns one page of an operation's captured output. Omit `cursor` for the first page and pass `next_cursor` for the next; `next_cursor` is absent at the end. Each operation keeps at most 64 MiB of output; `dropped_bytes` counts what was not kept. `text` is valid UTF-8: each byte of output that is not valid UTF-8 appears as U+FFFD, and a page never ends inside a multi-byte character, so one page can cover fewer than 16 KiB of captured bytes. Returns an error when the saved output is shorter than its recorded size. Returns `not_found` when no output exists for `op_id`, and `unprocessable` for a malformed or foreign cursor.
- **`list_traces`** (`GET /traces`)
  - **Input:** `status?` (`all`, `error`, or `ok`, default `all`), `limit?` (1–100, default 20), `cursor?` → **Output:** `TraceList` (`spans`, `has_more`, `next_cursor?`).
  - Reads the project owner's saved operation diagnostics, newest first; the user, the guide and the builder act for the owner and read the same history. For example, call with `{"status":"error"}` to inspect recorded failures. A span contains its operation name, safe summary, timestamps, duration, original status, and selected identifiers and results. Raw errors, events, prompts and credentials are excluded. `shortened: true` marks text shortened to fit the safe record.
  - Each call examines at most 256 KiB of history. An empty `spans` array with `has_more: true` leaves older history unexamined: pass `next_cursor` as `cursor` with the same filter to continue. Omit the cursor to refresh from the newest records. `unprocessable` (HTTP 422) means the cursor is invalid; refresh without it. Storage failures return a path-free message asking the server operator to check private storage.
  - `error` selects spans whose operation recorded an error status; `ok` selects spans without that status, including `unset`. An `unset` status does not prove success. A test run with `passed: false` is a completed check result, so its span keeps that result rather than becoming an operation error. See [Operation diagnostics](#operation-diagnostics) for recording and coverage.
- **`propose_change`** (`POST /changes/propose`)
  - **Input:** `change_id`, `prompt`, `story_refresh_op_id?`, `analysis` (`summary`, `ambiguities`, `examples`, `preserved_behaviors`, `ui_composition?`) → **Output:** `ChangeCardResult`
  - Saves the caller's feature request and analysis as a new Change Card at `CP1`: it gives the ambiguities the IDs `<change_id>-ambiguity-NN` and the examples the IDs `check_01`, `check_02`, ... (matching `goa-ai/eval` `validateID`) in list order, keeps every text exactly as sent, binds the card to the project owner, computes the canonical `request_revision` digest, and saves the card (`card`, `owner`, `design_source`, `stories`) atomically as `<cards_dir>/<sha256(change_id)>.json` under `controlplane.Options.CardsDir`. Every card is saved `needs_confirmation` with `confirmed_by` unset, including a card without ambiguities. When `story_refresh_op_id` is supplied, `propose_change` reads one module snapshot from the bound workspace, requires its `designeval.SnapshotDigest(files)` to match that succeeded `refresh_story` operation's `snapshot_digest` for the same principal and bound workspace identity, encodes `design_source` from that snapshot's `design/*.go` files, and adopts the operation's evaluated `stories` into `ChangeCardResult.stories`, the saved card file, and `request_revision` (a succeeded refresh that declared zero Stories is valid and produces the same `request_revision` as proposing without `story_refresh_op_id`). When `story_refresh_op_id` is omitted, `propose_change` reads the active workspace's `design/*.go` files when `controlplane.Options.WorkspaceDir` is bound. Returns `conflict` (HTTP `409`) when `change_id` already has a card, and `unprocessable` (HTTP `422`) when `change_id` or `prompt` is blank, the analysis breaks a cross-field rule, `story_refresh_op_id` is unknown, replaced, foreign, non-succeeded, unbound, or stale against the current workspace snapshot (naming `refresh_story`), or the bound workspace's design cannot be read. When the analysis carries `ui_composition`, the card carries it as `ui_composition` (`program`, `composed_for_revision`): it is presentation only, never checked by the server, never hashed into `request_revision`, and dropped when `confirm_intent` changes answers or examples. The decision components a composition may use are listed at the end of `surla://golden-path`.
- **`revise_change`** (`POST /changes/revise`)
  - **Input:** `change_id`, `request_revision`, `prompt`, `story_refresh_op_id?`, `analysis` (`summary`, `ambiguities`, `examples` with optional `prior_id`, `removed_example_ids`, `preserved_behaviors`, `ui_composition?`) → **Output:** `ChangeCardResult` (the change as `get_change` returns it)
  - Saves the next revision of an existing Change Card at `CP1`. Only the guide calls it, acting for the owner; only the user confirms or adopts a revision, with `confirm_intent` on the views page. The guide names the latest revision's `request_revision`: the waiting proposal's (`proposed_revision.request_revision`) when one waits, otherwise the card's. It is checked when the call starts and again just before the save. What happens to the change depends on whether the user ever confirmed it:

    | The change is | The new revision |
    | --- | --- |
    | `needs_confirmation` (never confirmed) | Replaces it. |
    | `confirmed` | Is saved as the proposal (`proposed_revision`) beside the confirmed revision, replacing any earlier proposal. The confirmed revision stays in force with its answers, confirmation, approval, `stories` and design source, and `preview_architecture`, `verify_change` and `approve_merge` keep working on it, until the user adopts the proposal with `confirm_intent` or declines it with `decline_revision`. The guide cannot withdraw a proposal; it replaces it by revising again. |

    The guide builds the analysis from the revision it revises (as `get_change` returns it), the revised request and the target's design: it keeps or edits each of that revision's examples by giving its ID as `prior_id`, lists each dropped example's ID in `removed_example_ids`, and restates confirmed decisions that still hold in `summary`, `examples` or `preserved_behaviors`, because the new revision has no answers of its own. Kept and edited examples keep their IDs; IDs from that revision's table (including removed ones) stay reserved while building this revision, and each new example receives the lowest `check_NN` ID absent from both tables. Surla assigns fresh `<change_id>-ambiguity-NN` IDs, builds the revision in `"needs_confirmation"` status (even when no ambiguities remain) with no `answers`, `confirmed_by` or `approval`, binds any `ui_composition` to the new revision digest, and saves the change atomically in `Options.CardsDir`. When `story_refresh_op_id` is supplied, `revise_change` reads one module snapshot from the bound workspace, requires its `SnapshotDigest` to match the succeeded `refresh_story` operation, and replaces both the private `design_source` and `stories` from that single verified snapshot. When omitted on a revision with Stories, it keeps that revision's Story/source pair without re-reading the workspace. When omitted on a revision without Stories, it reads `design/*.go` again from the bound workspace, or keeps the saved source if no workspace is bound. Returns `not_found`, `forbidden`, `conflict` (HTTP `409`) when `request_revision` does not name the latest revision (including the confirmed revision while a proposal waits, and a replaced proposal), and `unprocessable` (HTTP `422`) when a required text is blank, the analysis breaks a cross-field rule such as the example accounting, `story_refresh_op_id` is invalid or stale, or the bound workspace's design cannot be read; a refused call leaves the change unchanged. The guide does not confirm its own revision: the user answers the new revision's `ambiguities` on the views page and confirms it with `confirm_intent`, which for a proposal is adopting it.
- **`confirm_intent`** (`POST /changes/confirm`)
  - **Input:** `change_id`, `request_revision`, `answers`, `examples` → **Output:** `ChangeCardResult`
  - Confirms a `needs_confirmation` Change Card, refines an already `confirmed` Change Card, or adopts the revision proposed beside a confirmed one, at `CP1`. Only the user's browser session calls it, from the views page (**Confirm** and **Adopt this revision**), with complete `answers` and `examples` and the `request_revision` it acts on; the guide and the builder cannot. Naming the waiting proposal's `request_revision` adopts it: its questions are answered, it becomes the confirmed revision in force under a new `request_revision`, `confirmed_by` records the user, no `approval` carries over from the earlier revision, and the proposal is gone. While a proposal waits, naming the confirmed revision to refine it is refused with `conflict` (HTTP `409`) and a message naming Adopt and Decline; nothing is saved. `confirm_intent` keeps the proposal's `summary` text and any saved `stories` unchanged (without re-reading or re-evaluating the workspace) while recording the owner's confirmed choices in `answers` (empty when the card has no ambiguity questions), which take precedence over open phrasing in `summary`. `answers` maps every open ambiguity's `id` to exactly one string from that question's `options`, character for character. When a decision requires an option not listed, the user tells the guide, which records it with `revise_change`; the user then confirms the resulting revision, or adopts it when the change was already confirmed. A rejected call leaves the card unchanged and returns one error naming every missing answer, every answer that is not one of its question's options (with the options quoted), and every unknown question `id`. An edited example keeps its `id` and an added one has a new, unique, non-blank `id`; `confirm_intent` saves the ids as sent. No-op submissions keep the stored `request_revision` and evaluate any recorded `approval` on a shallow response copy without modifying the saved card: `approval` is included only when `PromotionTarget` is bound and `PromotionTarget.CurrentInputs(ctx, approval.CandidateCommit)` still matches its `suite_id`, `suite_commit`, `environment`, and `artifacts`; both `approval` and `unverified_approval` are omitted when `CurrentInputs` succeeds with different inputs; and `unverified_approval` (`UnverifiedApprovalItem` with the saved approval metadata and a path-free operator recovery `reason`, omitting `approval`) is returned when `PromotionTarget` is nil or `CurrentInputs` returns an error (recording the raw `CurrentInputs` error on the `controlplane.check_approval` span). Changed answers or examples mint a new canonical `request_revision` digest (covering any saved `stories`), clear `approval`, and save the updated card in `Options.CardsDir`.
- **`list_changes`** (`GET /changes`)
  - **Input:** none → **Output:** `ChangeList` (`changes`, `version`, `approval_available`)
  - Lists the project owner's saved Change Cards for this project, most recently saved first (ties by `change_id`). Each item carries `change_id`, `summary`, `status`, the `request_revision` in force, `proposed_revision` (the waiting proposal's `request_revision`, while one waits beside a confirmed change), `saved_at` (when the card was last saved; not part of `request_revision`), and `latest_verification` (`run_id` and `state` of the most recently accepted run: `running`, `passed`, `failed`, or `not_completed`), which is absent when no run was started. `approval_available` says whether `approve_merge` can record approvals on this server: it is `false` when no promotion target is configured (store the promotion settings with `surla promotion set`, then restart Surla) or when the server could not resolve its promotion target. The list still answers, and the resolver's error is recorded on the span of the read that resolved it: `surla.controlplane.list_changes`, or `surla.controlplane.wait_for_changes` on each of its polls. It names no setting or path. `version` changes whenever any listed value changes, including `approval_available`, and when the canonical `get_build` rendering, the bound owner's Team snapshot, the owner's Story evaluation (`get_story_refresh`), or the live target changes; the [views page](#views-page) waits on it. Read one card with `get_change`. A card, build or run record that cannot be read fails the call instead of being left out.
- **`get_change`** (`GET /changes/{change_id}`)
  - **Input:** `change_id` → **Output:** `ChangeCardResult`
  - Reads the saved `ChangeCardResult` (including `stories` when present) for `change_id` from `Options.CardsDir` (verifying the canonical `request_revision` digest of the revision in force and of any waiting proposal on load, and refusing a file with a proposal beside an unconfirmed revision) without mutating saved state. The card's fields describe the revision in force; a revision the guide proposed beside a confirmed card is returned in `proposed_revision` (its `prompt`, `summary`, `ambiguities`, `examples`, `preserved_behaviors`, `stories`, `request_revision`, `ui_composition`, and `changed_design_files`: the `design/*.go` paths whose content differs from the files the confirmed revision was made from) while it waits (restricted to the owning principal) so a stale editor can fetch the latest saved card after a `request_revision` conflict. On a `confirmed` card, `answers` holds the owner's confirmed ambiguity resolutions (empty when the card had no ambiguity questions) and takes precedence over open phrasing in `summary`. When the saved card has a recorded `approval`, `get_change` evaluates it on a shallow response copy: it includes `approval` only when `PromotionTarget` is bound and `PromotionTarget.CurrentInputs(ctx, approval.CandidateCommit)` still matches `suite_id`, `suite_commit`, `environment`, and `artifacts`; omits both `approval` and `unverified_approval` when `CurrentInputs` succeeds with different inputs; and omits `approval` while returning `unverified_approval` (`UnverifiedApprovalItem` with a path-free operator recovery `reason`) when `PromotionTarget` is nil or `CurrentInputs` returns an error (recording the raw `CurrentInputs` error on the `controlplane.check_approval` span).
- **`decline_revision`** (`POST /changes/decline`)
  - **Input:** `change_id` (non-empty), `request_revision` (the waiting proposal's, 64 lowercase hex characters) → **Output:** `ChangeCardResult` (the change as `get_change` returns it)
  - Declines the revision the guide proposed beside a confirmed Change Card. Only the user may call it. The proposal is removed and the confirmed revision in force stays exactly as it was saved, with its answers, confirmation, approval, `stories` and design source; the file differs from before the proposal only in its saved time. A refused call leaves the change unchanged:

    | Answer | When |
    | --- | --- |
    | HTTP `400` | `change_id` is empty or `request_revision` is not 64 lowercase hex characters (refused by the generated request validation). |
    | `forbidden` (HTTP `403`) | The caller is not the user's browser session, or the change belongs to another owner. |
    | `not_found` (HTTP `404`) | No Change Card has this `change_id`. |
    | `conflict` (HTTP `409`) | No proposal waits, or `request_revision` does not name the waiting one, such as a proposal the guide has since replaced. |
- **`configure_model`** (`POST /cp1/model`)
  - **Input:** `provider?` (`anthropic` \| `openai`), `model_name?`, `gateway_addr?` → **Output:** `ModelConfigResult` (`provider?`, `model_name?`, `gateway_addr?`)
  - Selects, without restarting the Surla project server, the model that `CP3` live targets started afterwards receive in their environment (`SURLA_LIVE_MODEL_PROVIDER` and `SURLA_LIVE_MODEL_NAME`, or `SURLA_LIVE_MODEL_GATEWAY`); a live target already running keeps the model it started with. Proposing and revising Change Cards uses no model. Callers supply either `provider` and `model_name` together (the live target reads `ANTHROPIC_API_KEY` or `OPENAI_API_KEY` from the environment it inherits from the Surla project server, so API keys never cross MCP or HTTP requests) or `gateway_addr` alone (a plaintext gRPC `goa-ai` model gateway). HTTP rejects schema-invalid input, such as an unknown provider name, with `400`. The method returns `unprocessable` (HTTP `422`) for an invalid field combination or when the chosen provider's API key is missing from the Surla project server's environment.
  - The selection lasts for this server process. Restarting uses the API keys in the new process's environment again.
- **`get_model_connection`** (`GET /cp1/model`, the user only, not an MCP tool)
  - **Output:** `ModelConnection`, one of three cases named by `kind`: `none` (live targets start without a model setting), `direct` (`provider`, `model_name`), or `gateway` (`gateway_addr`).
  - Reports the choice live targets started afterwards receive: the choice the server made at startup from the API keys in its environment, until an accepted `configure_model` replaces it. It returns the choice only: no API key and no other environment value appears in its answer or its span. Verification runs do not use this choice; their evaluator suite runs with the server's own environment.
- **`verify_change`** (`POST /changes/{change_id}/verifications`)
  - **Input:** `change_id`, `request_revision`, `candidate_commit` → **Output:** `VerificationStatusResult`
  - Starts a Checkpoint 4 (`CP4`) verification run for `candidate_commit` when called by the owner of a `confirmed` Change Card at its current `request_revision`. The run is accepted into `<runs_dir>/<run_id>/`, carried out by a detached supervised process group (`internal/verifyrun`) that outlives the HTTP/MCP call and server restarts, and immediately returned with `state.type == "running"`. When verification is not configured on the control plane, it returns `not_implemented`.
  - Which tests run is decided at this call. With [stored verification settings](#surla-verification-and-surla-promotion) (`controlplane.Options.Verification`), the operator's evaluator runs and no build is needed. Without them (`controlplane.Options.WrittenTestRuns`), the user must have started a build of this `request_revision` and Surla must have accepted its tests (`get_build` shows `tests.state == "accepted"` with their `commit`). The run builds the candidate's programs from `candidate_commit` and runs the tests' generated entry point (`eval/gen/cmd/<service>-evals`) from the accepted commit. Both commits are read with git from the repository holding the workspace, never from its working files, and must have the layout `surla init` makes: `go.mod`, `eval/go.mod` for `<module>/eval`, `cmd/<service>` in the candidate commit and the generated entry point in the tests' commit.
  - Without stored settings, each refusal is HTTP `409` with a message that says what is missing and who acts, and ends with the step; through MCP the tool result is an error holding that message. Nothing is started for a refused call.

    | Refusal | When |
    | --- | --- |
    | `no_build` | The user has not built the revision in force (the user starts a build on the views page); the build is of another revision, which the message names; or the build was started without a test writer, while the project verified with stored settings (the user starts the build again). |
    | `tests_not_accepted` | Surla has not accepted the build's tests. The message names their state (assigned, reported, needs changes, could not run) and who acts next. |
    | `no_test_layout` | A commit lacks the layout `surla init` makes, naming the commit and what it lacks: the builder fixes a candidate commit, the test writer reports again for the tests' commit. Also an accepted tests' commit the repository no longer holds (the test writer reports again, or the user builds again), and a project folder outside any git repository (make the project folder a git repository and commit it). |
    | `conflict` | Besides an unconfirmed card or a stale `request_revision`: a `candidate_commit` the repository does not hold (commit it and try again), or a project folder replaced or removed after the server started (the user stops and starts Surla). |
  - Tests a separate agent wrote run outside Surla's sandbox, which isolates only the candidate's builds and processes. Their program therefore starts with only `PATH` and `TMPDIR`, a folder inside the run's own folder: no provider key and no other variable of the Surla project server. The operator's evaluator keeps the server's environment.
- **`get_verification`** (`GET /verifications/{run_id}`)
  - **Input:** `run_id` → **Output:** `VerificationStatusResult`
  - Reads the state of verification run `run_id` from `<runs_dir>/<run_id>/` (restricted to the principal recorded as the run's `owner` in `request.json`, independent of current Change Card state so runs remain readable after subsequent card edits or server restarts). The test writer may read only the runs of a change it holds an assignment for: the project has no stored evaluator and the change has a build started with a test writer. Returns `test_source`, recorded when the run started: `evaluator`, or `written_tests` with the tests' `commit` (the run's `suite_commit`) and the fixed `notice` "These tests were written by a separate agent on this machine and are not isolated from the builder." It also returns `accepted_at` (RFC3339 timestamp with sub-second precision when present) and the `state` union reporting `"running"` (`started_at`), `"completed"` (`suite_id`, `environment`, and `report` projected through `Report.ForBuilders()` so holdout scenario names, checks, and diagnostics never leave the evaluator), or `"not_completed"` (`kind`: `"execution_failed"` \| `"interrupted"`, `stage`: `"checkout"` \| `"build"` \| `"describe"` \| `"suite"` \| `"report"` \| `"supervise"`, and a builder-safe `summary` with full command output kept in `<runs_dir>/<run_id>/executor.log`).
- **`list_verifications`** (`GET /changes/{change_id}/verifications`)
  - **Input:** `change_id` → **Output:** `VerificationListResult` (`change_id`, `runs: VerificationStatusResult[]`)
  - Lists all saved verification runs in `<runs_dir>/` recorded for `change_id` and owned by the authenticated principal (allowed whether the card is currently `confirmed` or `needs_confirmation`, so runs from earlier revisions remain discoverable after `revise_change`), ordered newest first by `accepted_at` (`run_id` ascending on ties), each with its `test_source` and with each completed report projected through `Report.ForBuilders()`. The test writer may list only the runs of a change it holds an assignment for, as for `get_verification`.
- **`approve_merge`** (`POST /changes/approve`)
  - **Input:** `change_id`, `request_revision`, `candidate_commit`, `report_digest` → **Output:** `ChangeCardResult`
  - Records Checkpoint 5 (`CP5`) promotion approval on the owning principal's confirmed `ChangeCardResult` when `controlplane.Options.PromotionTarget` or `controlplane.Options.ResolvePromotionTarget` is bound (`surla promotion set <file>` followed by a server restart). It loads the full `verify.Report` for `report_digest` and the current `PromotionInputs` (`SuiteID`, `SuiteCommit`, `Environment`, measured candidate binary `Artifacts`) for `candidate_commit` from `PromotionTarget`, runs `verify.Approve`, re-verifies that the card still has `request_revision`, and saves and returns the updated `ChangeCardResult` with `approval` (`ApprovalItem`: `approved_by`, `report_digest`, `candidate_commit`, `suite_id`, `suite_commit`, `environment`, `artifacts`, `scenario_count`) in `Options.CardsDir`. Recording approval does not merge branches or deploy binaries. When no `PromotionTarget` is bound, `approve_merge` requests return `no promotion target is bound to this control plane`.
- **`preview_architecture`** (`POST /changes/preview`)
  - **Input:** `change_id`, `request_revision` → **Output:** `ArchitecturePreviewResult`
  - Reads the bound workspace's candidate module snapshot and evaluates it in a sandboxed subprocess at `CP2`, returning C4 `containers` and `edges`, `agent_policies`, and `tools` (`input_binding`). When the request includes a `progressToken`, it sends progress as evaluation starts and finishes. The result reports the design's declarations; it does not compare them with the Change Card's requirements. A tool's `requires_confirmation` declares a design-time `Confirmation` gate; it does not show that the application delivers an approval request to the right person or continues after the answer. [`examples/orders`](../examples/orders/README.md) implements and verifies a manager approving refunds.
- **`draft_override`** (`POST /sandbox/draft`)
  - **Input:** `target_kind` (`prompt_template` \| `compiled_rule`), `prompt_id?`, `new_content` → **Output:** `DraftOverrideResult`
  - On a live target started with `start_live_target`, the server sends the text to the target's prompt operation (see [Live target prompt contract](#live-target-prompt-contract)) and returns typed errors:
    - `prompt_not_found` (HTTP 404) and `invalid_prompt` (HTTP 422) mean the target rejected the update, so its text is unchanged;
    - `target_rejected_credential` (HTTP 502) means the target rejected the server's key, so nothing changed; start the target again;
    - `live_target_ended` (HTTP 409) means the target is no longer running; start it again;
    - `update_not_confirmed` (HTTP 502) means the target did not answer, so it may or may not have applied the text; keep the draft and send it again.
  - Surla changes prompts only in a live target it built and started itself, never in an application the user started or deployed. With no live target running, `prompt_template` requests return `no_live_target` (HTTP 409), which names `start_live_target` as the step that enables them; nothing changed. `compiled_rule` requests never need a target.
- **`start_live_target`** (`POST /live-target`)
  - **Input:** `candidate_commit` (a full commit ID) → **Output:** `LiveTargetResult` (`candidate_commit`, `url`, `message`)
  - The server builds the commit exactly as `CP4` builds a candidate, runs a test program's `--live` mode (see [the suite contract](#surla-verify)) and binds `draft_override` to the running candidate. `url` is the candidate's loopback origin, for trying its own API. With stored verification settings, the program is the operator's evaluator. Without them, it is the generated entry point of the test module in that same `candidate_commit`, the builder's own tree: no build or accepted tests are needed, and the test writer's code never runs in a live copy. The commit must have the layout `verify_change` describes; a commit without it, a commit the repository does not hold, a workspace outside any git repository, or a project folder replaced after the server started returns `live_target_failed` naming the missing piece. A server with neither returns `live_target_unavailable`.
  - The server runs at most one live target, so a second start stops the first before it starts the new one. A target runs until `stop_live_target`, its own exit, or the server's shutdown. The build continues if the caller disconnects, and is bounded by the configuration's `timeout`.
  - Starting a target first stops the one already running, which discards every prompt draft applied to it, even if the new build then fails. Prompt text survives only when it is committed.
  - Live targets get their own directories under `<cards-dir>/live`, which the cards directory's lock reserves for one Surla project server. If the server crashes, its executor kills the target's processes, and the directory stays. At startup the Surla project server names directories an earlier server using the same cards directory left; it does not check their processes or remove them.
  - Errors: `live_target_unavailable` (HTTP 501) when verification is not configured, and `live_target_failed` (HTTP 422) with a builder-safe summary of the step that failed.
- **`stop_live_target`** (`DELETE /live-target`)
  - **Output:** `StopLiveTargetResult` (`stopped`, `message`). Stops the running live target and unbinds `draft_override`. `stopped` is `false` when none was running.
  - A target's stop is attempted once. When its processes or directory cannot be confirmed stopped, `live_target_stop_failed` (HTTP 500) says to stop and start Surla, and a later stop reports the same failure.
- **`get_live_target`** (`GET /live-target`, the user and the builder)
  - **Output:** `LiveTargetStatus`, whose `live_target` is one of four cases named by `state`: `none`; `starting` (`candidate_commit`, `since`) while a start builds and starts the commit, which cannot be stopped until the start finishes; `running` (`candidate_commit`, `url`, `started_at`, `prompts`); or `not_confirmed_stopped` (`candidate_commit`, `message`, the server's sentence naming the step: stop and start Surla).
  - `prompts` has one entry per prompt whose draft the running target accepted (`prompt_id`, `version`, `accepted_at`), sorted by `prompt_id`; a later accepted draft of the same prompt replaces its entry when the target answers with another version, while the same version, such as the same text sent again, keeps the first `accepted_at`; a refused or unconfirmed draft leaves the entries as they were. Prompt text is never returned.
  - It changes and saves nothing and never waits for a start or a stop to finish: the answer lives exactly as long as the live target and the server.
- **`diff`** (`POST /diff`)
  - **Input:** `old` (`DesignSnapshot`), `new` (`DesignSnapshot`) → **Output:** `DiffResult`
  - Compares two `DesignSnapshot` modules by evaluating their generated Goa/Goa-AI artifacts, tool schemas, and `go.mod` directives.

### Builds and assignments

The user starts a build of the change's agreed revision. Surla saves that
revision's Story and design, prepares the working copy when a test writer is
needed, and registers the build's roles with their working folders. Each change
has one current build; building again replaces it and resets its assignments to
unread. A proposal waiting beside the agreed change is not built until the user
adopts it.

Projects with stored verification settings use that evaluator and add only the
builder. Other projects need the test layout made by `surla init`: Surla also
adds the test writer, opens its separate `.surla-tests` Git working copy on
`surla/tests`, and writes one required test entry per finished Story flow.
Draft flows require none. The full test-writer assignment supplies the saved
Story and design so the writer can align the public contract in its own `eval/`
module and implement the required methods there.

**Starting and recovering.** Before saving, Surla checks the owner, revision,
test source and prerequisites. It opens the copy and checks the revision again;
if that second check refuses the start, the reusable copy remains, but no new
build or agent is saved. After saving, Surla immediately registers the roles,
then writes the required tests. A generation failure caused by the copy becomes
`needs_changes`, with the finding and the step to correct it and report again.
An operational failure becomes `could_not_run`. The generation's verdict cannot
overwrite a report or a newer build saved while generation was running.

Accepted preparation continues if the page disconnects. Work on the copy has a
five-minute limit. Another build or a test check holding the project's turn
causes an immediate busy refusal, rather than a queued second build. After the
turn is released, team startup has its own 45-second limit. A busy, ambiguous or
unavailable window leaves the build and registered roles intact: follow the
returned step and call `start_team_agents`, or run `surla`. An already-open
Chorus launcher needs **Reload team** to show newly registered roles. A server
stop after saving leaves the saved assignment or verdict; start a build again
to finish preparation.

- **`start_build`** (`POST /changes/{change_id}/build`; HTTP only, bound project's user session only)
  - **Input:** `change_id`, `request_revision` → **Output:** `StartBuildResult` with `build`, `team` and `working_copy`.
  - `build` is the saved `StartedBuild`: `request_revision`, `started_at`, `in_force`, `builder`, `test_writer` and `required_test_count`. The count is derived from finished flows in its saved Story; zero states that no test of this change's Story was required. A count of required entries does not judge what the written tests assert.
  - `team.state` is `ready` (the existing team result, including its `step`), `window_busy` (step), `ambiguous_space` (count and step), or `unavailable` (step). A team warning is a successful saved build with unfinished team preparation.
  - `working_copy.state` is `not_used` with a stored evaluator; `at_project_commit` with its commit when the copy is new, already current or advanced; `not_contained` with the retained commit and a merge-then-build step when the tests branch is not contained in the project's branch; or `dirty` with the retained commit when tracked or untracked work must be preserved. These immediate outcomes are not saved as dialog or copy history on `get_build`.
  - Existing refusals are `not_found` (404), `forbidden` (403), revision `conflict` (409), and `unprocessable` (422) for finished flows whose required test names are empty or collide. No bound team returns `not_implemented` (501), with the step to start the project with `surla`. Shutdown or a limit reached before saving returns `unavailable` (503). The named prerequisite refusals below explain what is missing and end with the user's next step.

| Prerequisite refusal | HTTP status | Recovery |
| --- | --- | --- |
| `not_repository` | 409 | Create or select a Git repository. |
| `no_commit` | 409 | Make the repository's first commit. |
| `copy_folder_in_use` | 409 | Free the named working-copy folder. |
| `tests_branch_in_use` | 409 | Follow the library's step for the other working copy holding the tests branch. |
| `no_test_layout` | 409 | Store verification settings for a layout Surla does not know. |
| `git_identity_missing` | 409 | Configure the Git name and address used to record commits. |
| `working_copy_busy` | 409 | Start the build again after the current build or test check ends. |
| `sandbox_unavailable` | 501 | Use a system with Surla's candidate sandbox. |

**Judging reports.** Surla, not the test writer, decides whether the tests
count. Each `report_tests_finished` raises the build's report number and wakes
the server's *report checker*, which judges the test writer's working copy
against the Story saved with the build, without running any of the copy's code
outside the sandbox:

1. It writes the required tests into the copy again from the build's Story.
2. It checks that only files inside the test module changed since the copy's
   base, regenerates the generated folder in a private snapshot, builds the
   generated test program `./gen/cmd/<name>-evals` in the build sandbox
   (within 10 minutes), and lists its scenarios in the candidate sandbox
   (within 60 seconds). The whole judgment has 15 minutes.
3. Its verdict replaces `reported` and marks the test writer's assignment
   unread:
   - `needs_changes`: the copy has findings, such as a missing hook method,
     a file changed outside the test module, a scenario tagged `holdout`, a
     module that does not declare exactly one evaluation suite, or a build or
     listing that reached its limit;
   - `accepted`: the tests are committed on the copy's `surla/tests` branch,
     and the builder's assignment is marked unread too;
   - `could_not_run`: a cause the test writer cannot fix, with the user's
     step.

The check runs in the project's working-copy turn, one judgment at a time, so
a build start during a judgment is refused as busy. A newer report during a
judgment makes the server discard that judgment's verdict and judge the newer
report next. A report waits while the checker commits accepted tests, so it
keeps its number. Builds found reported when the checker starts are judged
then, and a build whose verdict could not be saved is judged again a minute
later, up to three times in a row; after that it stays `reported` until the
next report or the next server start. Findings and reasons contain no absolute path. A saved build has report
number 0 while its tests are assigned and at least 1 while they are reported.
The report checker runs for as long as the project server does; stopping the
server ends a judgment in progress without saving it, and the next server
judges the build again.

- **`get_build`** (`GET /changes/{change_id}/build`; user, guide, builder, test writer)
  - **Input:** `change_id` → **Output:** `ChangeBuild` (`change_id`, `build`).
  - `build.state` is `not_started` when no usable build exists, otherwise `started` with the saved build fields above. A build started with only a builder is usable while the server uses its stored evaluator. If those settings are removed, it has no test-writer copy: the read returns `not_started`, no assignment from that build is listed or returned, and the user builds again. This does not delete the saved record.
  - `in_force` stays true while a proposal waits beside the agreed revision, and becomes false when the user adopts or confirms another revision. An older usable build keeps its assignments until the user builds again; it is not rebuilt automatically.
  - `builder` reports whether its assignment was read. `test_writer` reports `tests`, `read` and `report`. Test states are `not_used`, `assigned`, `reported`, `needs_changes` (the `finding_count` of Surla's findings, never their text), `accepted` (the `commit`, and `not_judged`, always the sentence "Surla checked that these tests build into the generated test program and declare every required scenario; it did not judge what they assert."), or `could_not_run` (reason and step). Each verdict carries `checked_at`. `not_used` is derived from current stored verification settings, never saved as a verdict. `report` is the number of the test writer's latest report of the build: 0 until its first, and one more with each `report_tests_finished`.
- **`get_test_findings`** (`GET /changes/{change_id}/build/findings`; HTTP only, user session only)
  - **Input:** `change_id` → **Output:** `TestFindings` (`change_id`, `request_revision`, `started_at`, `report`, `checked_at`, `findings`)
  - Returns the text of Surla's findings when the build's tests are `needs_changes`, with the build they belong to (`request_revision` and `started_at`, as `get_build` reports them), the number of the `report` checked (0 when the problems were found as the build started, before any report) and when the verdict was reached, for the views page. `get_build` carries only their number. Returns `conflict` (HTTP `409`) in any other state, naming it, and `not_found` when no usable build exists. Reading changes nothing.
- **`list_builder_assignments`** (`GET /assignments/builder`) and **`get_builder_assignment`** (`GET /assignments/builder/{change_id}`; builder only)
  - **Output:** `BuilderAssignmentList` (`assignments`, most recently started first) or one `BuilderAssignment` (`change_id`, `request_revision`, `in_force`, `read`, `written_tests`). `written_tests.state` is `not_used`, `not_yet_accepted`, or `accepted` with its commit and verdict time.
  - The builder works in the control plane's active workspace, which may be the child project made by `init_project`. Reading an assignment marks it read; listing changes nothing. `not_found` means there is no usable build, and names the build-again step.
- **`list_test_writer_assignments`** (`GET /assignments/test-writer`) and **`get_test_writer_assignment`** (`GET /assignments/test-writer/{change_id}`; test writer only)
  - **Output:** the assignment list, or a full assignment with `in_force`, `progress`, `findings`, `working_folder`, `design_source`, `stories`, `required_tests`, `answers`, `examples` and `preserved_behaviors`. `findings` holds the text of Surla's findings when `progress` is `needs_changes`, as many as its `finding_count`, and is empty otherwise; the list carries only their number.
  - Only builds started with a test writer have its assignment. `working_folder` is the project's folder inside the linked working copy, not necessarily the repository checkout's root. Surla writes the test module's generated code, with the required tests, when the build starts and again at each report, so the test writer never regenerates it: `generate_code` is the builder's, and `surla gen` without the Story would drop the required tests. After changing the test module's design, the test writer reports, and Surla generates the module again. It checks its work with `go build ./...` in `eval/` of `working_folder`, which fails naming any required method not yet written, before `report_tests_finished`. The saved design and Story stay paired with the built revision after later changes.
  - `answers` pairs each question of the agreed revision with the option the user confirmed (`id`, `question`, `answer`), in question order; `examples` and `preserved_behaviors` are that revision's confirmation examples and preserved behaviors, exactly as confirmed. The build saves them when it starts, so a revision proposed or adopted later does not change them. The test writer may not read the change with `get_change`, so these fields are how its tests learn what the user agreed; its tests must hold the service to them as well as to the Story's flows. For example, when the user answered `without_tax` to "Does the limit include tax?", a test checks that a refund of an order of $50.00 before tax is instant. Surla's check of a report confirms the required tests, not what they assert.
  - `required_tests` holds one entry per finished flow (`story`, `flow_key`, `scenario`, `hook`): flow `refund-over-limit` requires scenario `refund_over_limit` and method `RefundOverLimit`. Unfinished flows require none; the test program must still build when the list is empty. Reading an assignment marks it read; listing changes nothing. No successful full assignment returns an empty working folder.
- **`report_tests_finished`** (`POST /assignments/test-writer/{change_id}/finished`; test writer only)
  - **Input:** `change_id`, `request_revision` → **Output:** `TestWriterAssignmentItem`.
  - Records `reported` for that assignment's revision from any state, raises `report` by one even when the tests are already `reported`, and clears an earlier verdict. It returns at once and wakes the report checker, whose verdict replaces `reported` (see **Judging reports** above). Returns `conflict` (409) for another revision and in a project that now verifies with its stored evaluator, where no test writer is used, and `not_found` when no usable test-writer assignment exists; a refused report changes nothing.

### Live target prompt contract

A live target serves one operation for `draft_override`. Surla calls it with a client it generates from its own description, [`design/targetprompt`](../design/targetprompt/design.go). The target declares the same method in its own Goa design and owns the implementation, so neither module depends on the other:

- `PUT /prompts/{prompt_id}` with body `{"template": "<Go text/template>"}` and header `X-Live-Target-Key: <key>` → `200` with `{"prompt_id": "...", "version": "..."}`. The version is an opaque identifier that is not blank and has at most 256 characters, such as `sha256:<64 hex digits>`; Surla treats any other answer as an update it cannot confirm.
- `401` (`unauthorized`) for a missing or wrong key, `404` (`unknown_prompt`) for an unregistered prompt, and `422` (`invalid_prompt`) for a template that does not render with the prompt's registered spec. The target renders the template before it saves it, so a rejected template leaves the text in use unchanged.
- The key is minted by the Surla project server for each `start_live_target`. It reaches the suite in `SURLA_LIVE_TARGET_KEY`, and the suite's `Live` function passes it to the candidate through `candidate.Spec.Env`. It lives as long as that target.
- The Surla project server also passes its active model configuration—from startup `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` auto-detection, or the most recent `configure_model` call—so a suite's `Live` function can plan with the same model: `SURLA_LIVE_MODEL_PROVIDER` and `SURLA_LIVE_MODEL_NAME` for a direct provider (the provider's key stays in the variable the Surla project server read it from, such as `ANTHROPIC_API_KEY`), or `SURLA_LIVE_MODEL_GATEWAY` for a model gateway. Nothing is set when the Surla project server has no model configured. Suites decide whether they need a model; `start_live_target` itself does not.
- The operator's evaluator receives the Surla project server's environment with these variables set. A test module from the candidate commit, which agents wrote, receives only `PATH`, `TMPDIR`, `SURLA_LIVE_TARGET_KEY`, the model variables above and the chosen direct provider's key, such as `ANTHROPIC_API_KEY`, and no other provider's key.
- The evaluator suite that `surla init` generates implements this contract. Its `override_prompt` reads the key from `SURLA_LIVE_TARGET_KEY` and replaces `<name>.helper.system`. Its `Live` function serves a direct provider through its own loopback model gateway, keeping the provider key in the suite process. It passes a literal loopback gateway address, such as `127.0.0.1:9090` or `[::1]:9090`, to the candidate directly. It reaches any other gateway, including a hostname such as `localhost`, through a byte-only forwarder it owns on a loopback port, which resolves and dials the address outside the sandbox, because the candidate sandbox allows only loopback connections and resolves no names. In every case the candidate's environment holds only the live key.
- [`examples/orders`](../examples/orders/README.md) implements this contract too, in its `prompts` service, and replaces `orders.refund.system`. Its evaluator suite's `Live` function starts the platform the way its scenarios do: simulated commerce and payments, a Temporal dev server, and the evaluator's scripted model, which needs no provider key and ignores the model configuration.

Changing this contract is a breaking change for every target that implements it.

### MCP Resources

- **`surla://golden-path`** (`text/markdown` via MCP `resources/read` on `POST /mcp`; JSON-encoded string on `GET /golden-path`)
  - Serves the embedded Golden Path architecture playbook ([`internal/goldenpath/GOLDEN_PATH.md`](../internal/goldenpath/GOLDEN_PATH.md)) for Goa and Goa-AI platforms, followed by a "Change Card decision views" section: the OpenUI decision components (generated from the views page's own component library into [`internal/goldenpath/decision_catalog.md`](../internal/goldenpath/decision_catalog.md)) that an analysis's optional `ui_composition` may use, and how a program refers to the analysis's ambiguities and examples by 1-based position.

### Views page

The **Build** section follows a change's saved summary. Confirm the change
before building. Its first build uses **Build this change**; an earlier build
offers **Build current agreed revision**. A current build emphasizes **View
team**, with a separate **Build again** question before replacement. Rebuilding
clears assignment reads and, when a test writer is used, its report and verdict.
A waiting proposal and unsaved edits keep their own decision controls primary;
building still uses the agreed saved revision. A known newer revision prevents
starting from an older editor. Starts can take a few minutes and continue after
the page closes.

The section shows saved assignment reads, test-writer progress and the required
Story test count, including zero. An accepted test-writer result does not mean
verification passed: the page shows the checker's explicit notice that it did
not judge the tests' assertions. When tests need changes, their findings load
through the user's separate read for that exact build and report; a failed
read offers Retry without discarding the saved build. Team and working-copy results from a start are dated
receipts of that action, not current health checks. A later successful **Start
agents** clears the earlier build's team receipt.

The **Team** view shows each registered role's terminal state, Chorus watcher
fact and assignment-read fact independently. **Start agents** prepares the team;
**Show pane** shows the selected role's existing pane in Herdr. Neither runs on
a refresh. Every view also shows roles that need you with their Show pane action.
The page follows build and Team changes through its existing waiting read.
Failed reads or an interrupted update chain retain the observations with a
last-read label. An ended session disables actions while preserving the view
and unsaved edits.

The Surla server serves the views page at `GET /views` on its numeric loopback address. Running `surla` opens it with a one-time link in the address fragment (`/views#link=<link>`), which a browser never sends to a server. The page is a fixed shell with no project data in it, built from [`ui/checkpoint/`](../ui/checkpoint/) into `internal/controlplane/checkpoint.html`. The server sends its policy as a `Content-Security-Policy` header, which a browser enforces before it runs the page:

| Directive | What the browser allows |
| --- | --- |
| `script-src 'sha256-<digest>'` | Only the page's one inline script, identified by the SHA-256 digest of its text that the build records. No other inline, injected, or loaded script runs, and nothing is evaluated from a string. |
| `style-src 'unsafe-inline'` | Inline styles of any kind: the page's style block and style attributes. No style sheet loads from anywhere. |
| `connect-src 'self'` | Requests only to the address the page was loaded from. |
| `default-src 'none'` | Nothing else: no images, fonts, frames, or other resources. |
| `frame-ancestors 'none'` | The page is never shown inside another page. |
| `form-action 'none'` | The page submits no form. |
| `base-uri 'none'` | The page cannot change its base address. |

On load, the page exchanges the link once through `POST /project/browser-session` for a session, removes the link from the address, and keeps the session only in the page's memory: never in browser storage or a cookie. It sends the session as `Authorization: Bearer <session>` with every request. Reloading or closing the tab ends the session, and a new tab needs a new link from `surla`. Without a link the page explains how to open it and sends no request. A link that was already used, or has expired, is reported as such. When the server answers `401` (as it does for every session after a restart), the page keeps everything the user typed visible and copyable and says to run `surla` again. Leaving the page with unsaved edits asks the browser to confirm.

An opt-in check, listed with the developer tests below, opens the page in a real headless Chrome or Chromium and confirms that the browser itself enforces this policy and the `frame-ancestors` rule, sends `Sec-Fetch-Site: same-origin` on the page's requests, keeps nothing in browser storage or cookies, ends the session on reload, and scrolls a long row of Story steps inside the Story stage without widening the page, shows the **Live target** view at a phone's width (375 CSS pixels) with only the chosen kind of model fields and no horizontal scroll, and that the server refuses requests that pages on other origins send.

The page sends only the operations listed in `PAGE_OPERATIONS` in [`ui/checkpoint/build.mjs`](../ui/checkpoint/build.mjs). The build generates their request types, address table and result validators from `gen/http/openapi3.json` into `ui/checkpoint/src/types.ts`, so the page has no hand-written copy of any request or result shape, and it refuses an answer that does not match its declared result.

- **Finding work:** the page opens on [`list_changes`](#mcp-tools), or directly on the change when there is only one. With no change it says that asking the guide to propose one makes it appear by itself.
- **Staying current:** the page keeps one `wait_for_changes` request open (`GET /change-updates?since=<version>`). The server answers as soon as the change list's `version` differs from `since`, or after a few seconds with the same version, and the page reads `list_changes` again only when the version changed. It reads the open card or its runs again only when their list entry changed. When the server does not answer, or announces a change but refuses the `list_changes` read that follows, the page says so in its status line, keeps every edit, and asks again after 5 seconds, doubling the wait after each further failure up to 30 seconds. Hiding the tab stops the open request; showing it resumes. The version also covers the live target's state, commit, start time and the prompt versions in force, so the **Live target** view follows a start, a draft, an end or a stop through this same request. It does not cover the model connection, which only the user changes: another tab of the same user shows a change the next time its **Live target** view opens. A new revision never replaces typed edits, the cursor or the focus: the page holds it beside unsaved edits (**Switch to Incoming Card**) and announces it once. `wait_for_changes` is HTTP-only and allowed only for the user's browser session; it is traced like every operation but never saved in the history `list_traces` reads. The reads that follow a change, like every read the page sends with `GET`, are not saved there when they succeed or are refused, only when they fail with an error; see [Operation diagnostics](#operation-diagnostics).
- **What the page reads and does:** for an open change, `get_change`; `preview_architecture` for the revision shown when the **Architecture** stage opens; `list_verifications` and `get_verification` in the **Proof** stage; `get_story_refresh` in the **Story** stage; `get_live_target` and `get_model_connection` in the **Live target** view; `list_traces` under **Traces**. **Confirm** sends `confirm_intent`, **Decline** sends `decline_revision`, and **Approve this result** sends `approve_merge` for a passed run, both for the exact revision shown, with the editor's stale-revision recovery (**Load Latest Card**). When the change list's `approval_available` is `false`, the page shows **Approval is unavailable on this server** with the step that enables it in place of **Approve this result**, before anything is sent. When the server still answers `approve_merge` with `not_implemented`, the page shows the same message with the server's reason.
- **A proposed revision:** when the guide revises an agreed change, its revision waits beside it as a proposal, and the change list marks the change **New revision proposed**. Without a reload, the page shows the proposal above the agreed revision under **Decisions & Approval**, with every part adopting it would confirm: whether the request and the summary changed (both texts when they did), the proposal's questions (a question the agreed revision did not ask is marked **New**), each example marked **New**, **Edited**, **Kept** or **Removed**, the preserved behaviors (new ones marked, dropped ones listed), the Story's flows it adds, removes or changes, named as the **Story** stage names them, and the design files it was made from that differ from the agreed revision's. **Adopt this revision**, the primary action, sends `confirm_intent` for the proposal's `request_revision` with the answers chosen there and is enabled once every question has an answer; **Decline** sends `decline_revision`. Answers chosen for the proposal survive the page reading the change again. While a proposal waits, refining the agreed revision is disabled with the reason. Adopting shows the earlier agreed revision as the previous one and keeps any unsaved edits made on it as preserved lines.
- **The Story:** the **Story** stage is available whether or not a change is open, and only reads. For the open change it draws the Story saved with the revision shown (the change's `stories`): each flow is a row of step cards, each card showing the actor's role, the step's summary, and whether the step starts the flow, is a step, or is an outcome. A flow the server reports as `complete` is marked **will be tested end to end**; any other flow is marked **draft: not tested** with the reasons its data gives (no step starts it, no step is an outcome, or its open questions, listed). Open questions show their prompt and suggested answers. A change saved without a Story says so. The confirm view states how many flows the change's Story has and how many are drafts, and links to the stage; drafts never block confirming.
- **The current Story draft:** the stage also shows the owner's current Story evaluation, read with `get_story_refresh`: none since the server started (ask the guide to refresh the Story), running, failed with the server's diagnostic, or succeeded with its Stories drawn as above. With a change open, a succeeded draft that differs in content from the change's Story is named as such, with the flows added, removed or changed, and the rule: the draft is not part of the change until the guide revises the change with it and the user confirms the new revision. An identical draft is called current. The change list's `version` also covers the evaluation's operation, state and snapshot digest, so the waiting read ends when an evaluation starts, finishes or is replaced, and the page reads `get_story_refresh` again then while it shows the stage, or when the stage opens next. The evaluation lives in server memory only: after a restart the stage shows that none has run.
- **The live target:** the **Live target** view is available whether or not a change is open. It says whether a live target is starting, running or could not be confirmed stopped, or that none runs and the builder agent starts one from a commit. For a running target it shows the commit, when it became ready, its address (a link only for a plain loopback `http` origin with a port, otherwise text), and each prompt a draft changed with the version in force and when it was accepted; drafts are discarded with the target and survive only when committed. **Stop live target** is offered while a target runs; it asks first, then sends `stop_live_target` once and shows the server's message until the target's state next changes. A target that could not be confirmed stopped is shown with the server's sentence (stop and start Surla) and no Stop, because a target's stop runs once and a retry cannot succeed. Below it, the view shows the model live targets started afterwards use and a form, opened on the kind of the current choice, whose **Apply** sends `configure_model` and shows its result or refusal, which names a missing key's variable; verification is not affected. What you type in the form stays while the page is open, across refreshes and other views.
- **Not shown on the page:** structural design comparisons (`diff`) and prompt text: the builder agent starts live targets and sends drafts (`start_live_target`, `draft_override`), and the page shows which prompts changed, never their text. The OpenUI decision view a guide may compose for a change (`ui_composition`) is an illustration, not the Story.

---

### Operation diagnostics

The Surla project server records safe summaries of authenticated, instrumented operations
without collector setup. On the views page, open **Traces** to inspect
recorded errors, switch to **All statuses** or **Without error status**, and
load older pages or refresh. Each page replaces the prior page. These reads
leave unsaved Change Card edits and the Save action available. If a read fails,
**Retry from newest** starts again without the failed cursor.

The control plane owns `<cards_dir>/traces.jsonl` under the same directory lock
as its Change Cards. It creates the file only when the first recorded span
finishes, saves no in-memory history, and reads the saved file after restart.
Opening an existing history rejects symlinks, non-regular files and permissions
other than `0600` before repairing an incomplete final append. Each safe record is at most 64 KiB;
operation names, summaries and attribute values can be shortened, as reported
by `shortened`. The private file contains a SHA-256 hash of the authenticated
principal for read isolation; neither the principal ID nor this hash appears
in API results or is added to exported spans. The hash is not anonymization;
keep the diagnostic file private.

The operator terminal prints one line with the safe summary, status description
and recorded exception messages when a `propose_change` or `revise_change`
operation span ends with an error status, such as a storage fault. This output
can contain private evidence; keep it operator-only. It stays quiet for expected
negative results, including an analysis `propose_change` or `revise_change`
refuses as `unprocessable`, whose span carries a code-authored summary without
error status.
Only recording spans created by this control plane for authenticated operations
enter its history. HTTP/MCP domain calls have `surla.request.<operation>` boundary
spans; expected domain or role refusals remain observations without error status,
while unexpected faults record errors. Child domain operations may appear
separately. Private history records the caller's actor ID and role without
changing exported SDK spans.

The history keeps what the agents and the server did, not what the views page
looked at. Reads by the user's page that succeed or are refused are exported
but not saved in this history. These are the reads the user's browser session
sends with `GET` (every `GET` route the user's session may call only reads; the
builder's and the test writer's assignment reads, which record that they were
read, are refused to it), such as the `list_changes`,
`get_change` and `get_live_target` reads the page repeats each time its
waiting read reports a change, or `list_traces` under **Traces**. A refusal,
such as `not_found` for a change that is gone or `forbidden`, is an expected
answer, not a failure, so a page that keeps asking does not fill the history
either. Such a read is saved only when it fails: when its span ends with an
error status or with a panic, which the span records as an exception event
without changing its status. The waiting read is never saved, whoever sends
it and even when it ends with an error, as it does when the page closes. Every
other call by the guide, the builder or the test writer, over HTTP or MCP,
every operation that changes something, and the user's reads sent with `POST`
(`preview_architecture`, `diff`, `read_check_output`) are saved.

Authentication and decoding failures, host or third-party spans,
unsampled spans, and separate verification, evaluator and check-helper processes
are absent. An unset status does not prove success.

To send full spans to an operator's OpenTelemetry collector, set
`OTEL_EXPORTER_OTLP_ENDPOINT` (the collector base URL) or
`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` (the complete HTTP trace-export URL) before
starting the Surla project server. The trace-specific variable takes precedence. The
standard OTLP/HTTP exporter handles its remaining environment settings. This
export retains original span fields, events and status without added identity
metadata. Export is independent of the safe caller view;
restrict access to the collector accordingly. With neither endpoint set,
Surla makes no collector requests. The check helper that runs each
`check_project` and `update_dependencies` call inherits these variables from
the Surla project server and exports its own spans the same way, as children of the
call's span; without an endpoint its spans are not kept anywhere. The Surla
project server owns its OpenTelemetry provider: it supplies the provider to the
control plane when it starts, and on shutdown it closes the control plane first,
so its work finishes while exporters remain active, then shuts the provider down.
The append-only file grows with completed operations; to discard its history,
stop Surla before removing `traces.jsonl`.

## CLI Reference

Running `surla help`, `surla -h`, or `surla --help` prints top-level usage to `stdout` and exits `0`. Running `surla help <command>` or `surla <command> -h` prints that subcommand's flags to `stderr` and exits `0`. Every subcommand rejects unexpected positional arguments (`unexpected arguments [...]`), so a stray non-flag token never causes Go's `flag` parser to silently ignore flags that follow it.

### `surla init`
Creates a Goa + Goa-AI + `goa.design/model` service in `<dir>` and a separate evaluator module in `<dir>/eval`. It resolves dependencies, generates each module's code inside Surla's sandbox with the same generator as [`surla gen`](#surla-gen) (the design code never runs on the host), checks both modules with `go vet ./...` and `go test ./...`, and prints the commands that commit the project and start Surla in it, followed by the operator tools `surla verify` and `surla approve`. Both module files start with the Go and library versions saved in the `surla` binary (`surla.GoMod()`). `surla init` builds one project tree in protected temporary storage outside the current working directory and destination parent, on the destination filesystem. It moves that completed tree to `<dir>` only after every step, including generation, has succeeded. A failure or interrupt leaves `<dir>` absent; publication is the final step and never replaces a directory, file, or symlink created meanwhile. Before generating the evaluator, `surla init` pins its exact Surla dependency with `go get github.com/goadesign/surla@<version>`; Go may select newer library versions required by that dependency.
- `-dir <path>` — Directory to create; it must not exist *(required)*. `surla init` never replaces an existing directory, file, or symlink, including one created while generation runs. Command logs, Go temporary files, and the unpublished tree stay in protected storage. A failed command keeps its log. A canceled or failed command removes its stage once its processes stop, or retains the stage and logs and reports their paths when cleanup cannot be confirmed. If Surla cannot find protected storage on the destination filesystem, it stops before building; choose a destination with room outside its parent for that storage.
- `-module <path>` — Go module path of the candidate service, such as `example.com/shop` *(required)*. Its last path element names the service and must be a lowercase Go identifier (`^[a-z][a-z0-9]*$`, excluding Go keywords and `main`).
- `-surla-version <version>` — Exact canonical semantic version (`golang.org/x/mod/semver`) of `github.com/goadesign/surla` required by `<dir>/eval/go.mod` (default: the build-info `Main.Version` stamped into the `surla` binary; required explicitly when running from a `(devel)` or `+dirty` build, and mutable labels such as `latest` or `main` are rejected).
- **Starter scope & visible-suite note:** The generated candidate (`cmd/<name>`) runs an in-memory Goa-AI `helper` agent (`RunPolicy`, `BindTo("price")`, and C4 `System`/`Container` model) connecting to `-model-gateway <host:port>`, registering `helper.SystemPrompt()` (`<name>.helper.system`, version `"v1"`) on each question's runtime and attaching `PromptRefs: []prompt.PromptRef{system.Ref}` on every model request. The generated `<dir>/eval/` module runs a scripted `goa-ai` model gateway and three starter scenarios, all tagged `"repair"`: `prices_tea` and `unknown_item`, checking the correlated `catalog.lookup_price` tool call and `ToolResultPart` exchange plus `pinned.Evidence("<name>")`, and `prompt_override_reaches_model`, checking that `override_prompt` rejects a wrong key and that the next model request carries the new prompt text. The suite's program, `eval/gen/cmd/<name>-evals`, is generated with the rest of `eval/gen/`: it builds the suite from the generated tests and the `eval/acceptance` package, so the author writes only that package. Because `eval/` lives inside the same repository as the starter code, none of its scenarios is hidden; scenarios that must stay hidden from builders belong in an evaluator repository that builder agents cannot read.

### `surla preview`
Reads a Go design module on disk into a symlink-safe snapshot (`go.mod`, `go.sum`, and non-test `.go` files; run `go mod tidy` first if `go.sum` is missing), evaluates it inside the sandboxed Goa/Goa-AI/Model compiler (`designeval.Evaluator`) at **0 LLM tokens**, and prints the indented JSON `designeval.Result` (`system_name`, `declarations`, C4 `containers` and `edges`, `agent_policies`, and `tools`) to `stdout`, which holds nothing else. After the JSON, it writes one summary line to `stderr`, such as `surla: "orders": 3 container(s), 2 boundary edge(s), 1 agent policy(s), 4 tool contract(s)`, with the system name quoted; the `preview_architecture` `banner` carries the same summary without the `surla: ` prefix. Needs no running server, no Change Card, and no LLM API key. SIGINT and SIGTERM cancel the evaluation. Before the command exits, it removes its compiler workspace and evaluation temporary directories, which it creates in protected storage outside the current working directory and inspected module, including when `TMPDIR` points inside either; a build whose processes cannot be confirmed stopped keeps it, and the error names it.
- `-dir <path>` — Root directory of the design module holding `go.mod`, `go.sum`, and `design/` (default `.`).

<a id="surla-serve"></a>
### `surla` and `surla stop`

Run `surla` in a project folder to start or attach to its server and open the
views page and prepare its guide team in Herdr. Before creating state or starting
anything, Surla requires `herdr-core`, `herdr-app`, Chorus v0.11.0 or newer, and
the platform browser opener. Missing programs name their install command; an
older Chorus names `chorus update -yes`. Install
[Herdr core](https://herdr.dev/core/) with
`curl -fsSL https://releases.herdr.dev/core/install.sh | sh`, the
[Herdr app](https://herdr.dev/) with
`curl -fsSL https://releases.herdr.dev/app/install.sh | sh`, and
[Chorus](https://github.com/goadesign/chorus) with
`curl -fsSL https://raw.githubusercontent.com/goadesign/chorus/main/scripts/install.sh | sh`.
The server runs in its own parentless
Herdr terminal, so closing the initiating terminal leaves it running.

After the server answers, Surla starts an absent Chorus service and Herdr window
in separate sessions without inherited output pipes. Each dependency has its
own bounded readiness check; timing out stops polling and leaves the user's
service or window running. Team preparation has a separate deadline. The server
owns the stable team name, per-role
handles, Herdr space, and complete atomic launcher roster. The roster lives at
`<HERDR_APP_HOME>/data/chorus-launcher/rosters/<space>.json` (Herdr's default
home is `~/.config/herdr-app`) and contains credential file paths, never tokens.
The server uses its login environment to resolve Herdr when preparing the team;
Chorus and Herdr need not be on that environment's PATH for the server to start
or for other management operations. The roster home also comes from the
server's login environment. It contains only roles whose working folders exist;
existing role entries require valid credential files. Each trusted role has at most
one agent. Surla registers build roles itself, preserving an existing role's
handle and credential while updating its saved working folder. A build using a
stored evaluator removes a former test writer and revokes its credential.
Missing working folders are omitted from the launcher and do not by themselves
open it. A role's running flag still reports its actual owned terminal, independently
of whether the role has a folder and is listed in the launcher.
Starting again reuses the guide and saved space. If a space was closed, the
server adopts exactly one local space matching its team name and physical
folder, creates one if none matches, and refuses duplicates with their count.
It removes only its own roster for a replaced space.

When any role listed in the roster is missing or its pane holds only an idle
shell, Surla focuses this project's space and opens the Chorus launcher so the
user can choose an agent and model. A running guide with a listed builder
whose pane is not running still opens the launcher. Surla reads open dialog
names before changing the window. An already open launcher stays open and keeps
its captured project. If it is for this project, click **Reload team** before
choosing agents; otherwise close it and run Surla again. Another
open dialog must be closed before retrying. If this project has no saved or
matching space and any dialog is open, a direct team-start request refuses
before creating a space or adding the guide. A build has already registered its
roles before requesting this step; those roles remain available for retry.
When every listed role is running,
no new dialog opens. The project server returns the user's next step and Surla
prints that text unchanged, then opens its views page. The guide works through the Story and change with the user; only the user confirms and approves
on the page. Starting a team does not require Git.

<a id="idle-shell"></a>

**Idle shell detection.** Surla treats a foreground program as an *idle shell*
when both conditions hold:

- Its executable basename, with leading dashes removed, is `sh`, `bash`, `zsh`,
  `fish`, `dash`, `ksh`, `csh`, `tcsh`, `nu`, `pwsh`, `xonsh`, or `elvish`, or is
  the basename of the user's own `$SHELL`, with leading dashes removed.
- Neither of the first two arguments after the program is `-c` or a
  single-dash flag group containing `c`, such as `-lic`.

A shell wrapper with such a command flag counts as running. A live pane with
unreported foreground information also counts as running.

The HTTP-only `POST /project/team` management operation (`ensure_team`) takes
no input. It returns `team`, `space`, `roles` (role, handle, running), and
`dialog` (`opened`, `already_open`, or `not_needed`), and a required `step` of
1–512 characters. The project's owner supplies the sentence: choose agents for
an opened dialog, click **Reload team** for an already-open one, or note that all
listed roles are running. Browser sessions and
agents cannot call it, and it is absent from MCP discovery. Its accepted work
has a bounded server lifetime independent of client disconnects. Named errors
include `ambiguous_space` with its numeric count, `window_busy`, and
`team_unavailable`. For those errors, the CLI uses fixed recovery text without
printing peer-authored error text. If the answering project server lacks this operation, Surla tells
you to run `surla stop`, then `surla`, to start the current server version.

<a id="team-supervision"></a>

**Team supervision.** The project server rebuilds this picture in memory from
Herdr and Chorus; it saves no supervision state. These HTTP-only operations
require the user's browser session. Guide, builder, test-writer and management
credentials cannot call them, and none appears in agent MCP discovery.

| Operation | Result or effect |
| --- | --- |
| `get_team` (`GET /team`) | Stable team name and registered roles in guide, builder, test-writer order. Before registration, the list is empty; reading creates nothing. |
| `show_team_pane` (`POST /team/pane`, `role`) | Show this project's space and focus only that role's freshly identified pane. Any open dialog returns `window_busy` before window changes. A missing or changed pane returns `pane_unavailable`. |
| `start_team_agents` (`POST /team/agents`) | Reuse the team-start workflow and result, including the open-dialog rule. `ambiguous_space` is an ordinary user error whose message gives the count and says to close duplicate Herdr spaces, then start the agents. |

Each role has required `role`, `handle`, `state`, `attention`, `watcher` and
`assignment` values. Terminal `state` is `not_started`, `ended`, `starting`,
`needs_you`, `working`, `waiting`, `error` or `unknown`. Attention is `none`,
`permission`, `question`, `auth`, `silent` or `unknown`. A running agent that
reports nothing for 60 seconds needs the user; this changes display state only.
A missing Herdr heartbeat or event for 45 seconds makes every role unknown
until fresh snapshots arrive. Watcher state is `held`, `gone` after 90 seconds
of known absence, or `unknown`.
Assignment state is `unread`, `nothing_owed` or `unknown`; the guide has none.
Failed assignment reads never imply that nothing is owed. A stale revision alone does not clear unread
assignments. The existing `wait_for_changes` version
also covers every public team fact, so no second waiting request is needed.
The views page does not draw these facts yet.

The executable currently leaves reminder delivery unavailable until positive
identification of the agent's own foreground program and exact text delivery to a status-reporting agent are verified. The reminder policy permits
only fixed text to an idle/done agent with positively reported foreground
arguments, a fresh ownership check and an unread assignment or a watcher gone
past the grace period. No reminder is eligible in the first 90 seconds after
server start. Unknown facts never authorize input. Missing foreground information
continues to count as running for pane safety, but cannot authorize a reminder.
Supervision never reads screen text or retains status messages or titles,
issues no explicit service-start command and never takes focus without a user action.

At startup, `ANTHROPIC_API_KEY` selects Anthropic with `claude-sonnet-4-6`; otherwise `OPENAI_API_KEY` selects OpenAI with `gpt-5.4-mini`.

A project is a folder, independent of Git. Surla resolves physical path aliases
and uses the nearest project record in that folder or its parents. Starting
inside a platform created by `init_project` finds its original project; the
control plane's existing workspace record restores the platform after restart.
Moving or renaming a folder creates a new, empty project.

Surla keeps one private state folder outside the workspace and enclosing working
tree, under `<UserConfigDir>/surla/operator/`. It contains the immutable operating
system account owner, one saved numeric `127.0.0.1` port, changes, runs, reports,
agent credentials, settings, and the running-server record. There are no
caller-selected identity, address, token, or storage paths. `surla` finds the
folder from the configuration folder of the shell where it runs and starts the
server with that folder. The server never looks the folder up itself: Herdr
starts it with the environment of Herdr's core, not the environment of your
shell, so a lookup there could find another folder. The server refuses a folder
that is not private to your account, holds no project created by `surla`, is
named for another project folder, lies inside the project folder, or is not
spelled as its physical path.

Port selection briefly binds and releases an available port; that check is a snapshot, and another process can take the port before the server binds it. If that happens, startup fails and retains the saved port and credentials.

A start lock serializes simultaneous commands. The existing card-store writer
lock determines whether a server owns the project: a held lock attaches only
after an authenticated project-identity response; a held lock with no answering
server reports the failure and starts nothing. A free lock makes an old running
record stale; a start removes it before launching. A held lock with no record
waits within the command's deadline and sends nothing. A taken saved port reports
an error and starts nothing.

When the server's Herdr terminal ends before the server answers, for example
because the server cannot read its saved settings or bind its port, Surla reads
what that terminal printed, then removes the terminal. The error says the server
stopped while starting and shows its last lines, where the server's own error
is: at most 20 lines and 4 KiB, keeping the end, with escape sequences and
control characters removed. For example:

```text
surla: the project server stopped while starting, and its last lines were:
  surla: project port is occupied: listen tcp 127.0.0.1:52011: bind: address already in use
```

If the terminal's output cannot be read or holds no printable text, the error
keeps the original failure and says the output could not be read. A server
whose terminal is still running but has not answered within the command's
deadline keeps its terminal, and the error names that Herdr terminal so you can
look at the server's output there.

`surla stop` requests authenticated ordered shutdown. Before its listener closes,
the server permanently revokes its management credential and withdraws its
running record. An authorized stop response finishes; owned work closes before
the store lock is released. Handled interrupt, termination, and hangup signals
attempt the same shutdown. An outright kill, lost holder, or host loss is recovered on the
next start from the free lock and stale record; it is not graceful shutdown.
Herdr's keep option replaces a lost program with a shell and is not a restart
mechanism.

Each server creates a fresh management credential in memory and publishes it
in its private running record only after binding its saved port. The CLI reads
that record for each management request to check the reported project identity,
stop the server, manage agent credentials, and mint a browser link. This limits
the credential's lifetime; it does not prove which program answered or encrypt
traffic. The CLI uses the literal local address without a proxy and refuses
redirects. Each management request, including a changed-credential retry, opens
a fresh connection after reading the credential; connections are never reused. A refused credential is retried once only if the record now contains
a different credential; otherwise the command says to run `surla` again. A
missing record is awaited within the command's deadline without network traffic.
Management cannot confirm changes or approve results. A live management
credential is never printed and is revoked before every listener close even if
record removal fails. The management value can reach another program only after
losing its authority; an echoed dead value is not treated as a secret.
Agent credentials survive restart and are removed with their agent. Agent connection settings and rosters name only their
own credential files, never the management credential.

The CLI opens only its exact saved views address with a 64-hex-digit link
fragment. It refuses any other destination without printing the supplied URL.
Management failures use CLI-owned wording and the received status, never text
from the TCP peer; malformed connections use a fixed connection-failure message.

The browser link expires after two minutes and can be exchanged once through
`POST /project/browser-session` for a random user session. The fixed `/views`
shell contains no project data. It removes the link fragment after exchange,
keeps the session only in page memory, and sends it only to the saved server
address. Reloading or closing the page ends access; run `surla` to reopen it.
Sessions are neither cookies nor browser storage and do not survive server
restart. Every response disables MIME sniffing, and every request checks the
exact saved Host and any Origin. Browser-session API calls require same-origin
browser metadata; native agent/CLI calls authenticate with their own credentials.

All processes run as the same operating system user. File permissions exclude
other accounts, not other programs under that account. A deliberate program can
read another credential, forge native request headers, or run `surla` to obtain a
browser link. These are separation-of-duties rules, not isolation from a
malicious agent running as the user. The single-use browser link is the only
credential passed in a launch argument; persistent credentials and sessions are
never placed in arguments, labels, or logs.

### `surla verification` and `surla promotion`

Each command accepts `set <file>`, `show`, or `remove`, and works while the server
is stopped; it uses local private-state ownership without needing a running
server or management credential. `set` validates an operator-named JSON file
outside the workspace and enclosing working tree before atomically storing a private copy. Unknown
fields and trailing data are refused, leaving prior settings intact. Relative
local repository and release paths are resolved against that file's directory.
`show` prints the stored settings; `remove` removes them. Changes apply at the
next server start, including changes made while the server runs.

Verification settings require `candidate_repo`, `candidate_module`, `binaries`
(binary name to package path), `suite_repo`, `suite_commit`, `suite_module`,
`suite_command`, and a positive duration `timeout`, such as `"30m"`. Surla derives
run and report storage from the project; `runs_dir` and `report_dir` are refused.
With stored settings, every change verifies with that evaluator. Without
them, each `verify_change` runs the tests Surla accepted for the change's
build, and each live copy runs the candidate commit's own test module; see
[`verify_change`](#mcp-tools--resources). Verification runs remain server-owned and
survive client disconnection and server restart.

Promotion settings require `release_dir`, `suite_id`, `suite_commit` (a full
40- or 64-character lowercase commit ID), `environment`, and distinct plain
artifact names in `binaries`. Reports always come from the project's report
store; `report_dir` is refused. The release directory must be outside the
workspace and enclosing working tree. Publish each complete
`<release_dir>/<candidate_commit>/` in one rename and do not edit it in place.
Approval rehashes its regular binaries through one directory handle and checks
the independent report. Missing, non-regular, or escaping files prevent approval;
changed inputs hide a stale approval, and unavailable inputs return the existing
`unverified_approval` recovery state. Approval does not merge or deploy code.

### `surla diff`
Reads two Go design modules into symlink-safe snapshots (`go.mod`, `go.sum`, and non-test `.go` files), evaluates both inside the sandboxed Goa/Goa-AI compiler at their own `go.mod` dependency versions, and prints the JSON `DiffResult` to `stdout`. Both evaluations share one build workspace, so the second reuses the dependencies the first compiled. SIGINT and SIGTERM cancel them, and the workspace is removed before the command exits, as for `surla preview`.
- `--old-dir <path>` — Root directory of the previous design module *(required)*.
- `--new-dir <path>` — Root directory of the candidate design module *(required)*.

### `surla gen`
Regenerates the `gen/` directory of a design module from its `design/`, inside Surla's sandbox, and replaces `gen/` in one step. The design code and its generators are compiled and run only in the sandbox; Surla writes their output.
- `-dir <path>` — Root directory of the design module holding `go.mod`, `go.sum`, and `design/` (default `.`).

When the design declares agents, Goa-AI also writes `AGENTS_QUICKSTART.md` at the module root, a guide to the design's agents and tools for coding agents. With `goa.design/goa-ai` `v0.90.1-0.20261110011046-f32bd628f2bf` or later, Surla replaces the guide's parts that name commands to run (its header, first-run steps, and codegen rules) with steps that use Surla's MCP tools, and replaces its evaluation section when the design declares no evaluation suite; Goa-AI's description of the design is unchanged. When the design declares evaluation suites, Goa-AI's evaluation section is kept, including its `goa example` and `go run ./cmd/<suite>-evals` instructions. A Goa-AI release before named sections writes its guide unchanged. The generation program reads declared suites through `goa.design/goa-ai/eval/expr`, as evaluation already does, so generation also needs a Goa-AI that has that package. `generate_code` writes the same guide.

For a Surla test module, generation also writes the suite's program at `gen/cmd/<service>-evals/main.go`. Three facts make a module a Surla test module, and all three are needed: its path is `<service module>/eval`; its design declares exactly one evaluation suite; and it has an `acceptance` package, which provides the five names the program calls: `Name`, `Hooks`, `New`, `Live` and `LiveThroughGateway`. Any other module gets no program, so a module that only shares the path keeps building. The program builds the suite from the generated tests and the `acceptance` package; a missing scenario method on `Hooks` therefore does not compile. A test writer who deletes the `acceptance` package gets no program, and the check of its report says so. Every run declares the environment `scripted-model`. Generation also adds a section naming those five names to `AGENTS_QUICKSTART.md`, and writes that guide with only this section when the design declares no agent. A hand-written `cmd/<service>-evals` from an earlier starter is left in place, but Surla never builds it; delete it.

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
- **[`design/`](../examples/orders/design/)**: Declares the public `orders` HTTP service (`chat`, `run_status`, `approve_refund`), the `prompts` service whose `override_prompt` follows the [live target prompt contract](#live-target-prompt-contract), internal `commerce` and `payments` services, the `goa-ai` `refund` agent (`Inject("tenant_id", "customer_id")`, `BoundedResult`, and `Confirmation`), and the C4 architecture model rendered to [`gen/structurizr/model.json`](../examples/orders/gen/structurizr/model.json).
- **[`AGENTS_QUICKSTART.md`](../examples/orders/AGENTS_QUICKSTART.md)**: Generated contextual map for coding agents, surfacing per-tool `BindTo` method links, server-injected fields, `BoundedResult` cursor guidance, `Confirmation` gates, and `gen/orders/agents/refund/specs/tool_schemas.json`.
- **[`internal/orders`](../examples/orders/internal/orders/)**: The refund rule (`StoreCreditLimitCents` in `refunds.go`): an order total up to `\$50.00`, including tax and shipping, is refunded as instant store credit; a larger total is refunded to the card after a manager approves. `PromptService` serves `PUT /prompts/{prompt_id}` only for the live key in `SURLA_LIVE_TARGET_KEY`, which Surla passes to a live copy it starts; a process started without that variable rejects every prompt change.
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

The views page's real-browser check runs only on request; CI compiles it but never runs it. It needs Chrome or Chromium and Node 22 or newer, evaluates a real Story with the Go toolchain, and ends its browser however the test ends; set `SURLA_TEST_BROWSER` to the browser's executable when it is not in a standard location:

```bash
go test -race -count=1 -tags browser ./internal/project
```

See [`examples/orders/README.md`](../examples/orders/README.md) for a step-by-step walkthrough including `surla verify` (default builder view vs. full operator report with `-builder-view=false`).

---

## Repository checks

The [CI workflow](../.github/workflows/ci.yml) runs each Go module's vet and short race tests in parallel with views page type checking and generated-file checks on pull requests and pushes to `main`. Each job has a five-minute limit. Run `go test -race -short ./...` in each of the three Go modules for the same short tests locally. Tests that compile candidate code in fresh sandboxes run without `-short` in the full suite.

The full suite runs nightly at 07:17 UTC and on a manual **Run workflow** request for **CI**. Its three complete Go-module suites, verifier, generation journey, starter journey, and orders acceptance suite run as separate parallel jobs. To request a pushed branch from the CLI, use `CI=true gh workflow run ci.yml --ref <branch> </dev/null`. These longer checks still need to pass when validating changes to candidate execution or starter generation; a passing short run does not prove those paths.

## Project Layout

```text
.
├── .github/
│   └── workflows/
│       └── ci.yml                # Parallel short checks per push; complete sandbox and native suites nightly or on request
├── design/
│   ├── design.go                 # Surla Goa & Goa-AI MCP 1.0.0 DSL contract
│   └── project.go                # Project management service: server identity, stop, agent credentials, browser link
├── cmd/
│   └── surla/
│       ├── main.go               # CLI entrypoint (`surla`, `surla init`, `surla stop`, `surla verification`, `surla promotion`, `surla preview`, `surla diff`, `surla gen`, `surla verify`, `surla approve`, and `surla version`)
├── docs/
│   ├── HOW_IT_WORKS.md           # The 5 checkpoints & independent verification
│   ├── ARCHITECTURE_PLAYBOOK.md  # Golden Path rules & Goa Design suite overview
│   ├── PRODUCT_DIRECTION.md      # Who Surla is for and the product decisions that follow
│   ├── STORY_TEST_DESIGN.md      # Engineering draft: Story flows and independently written tests
│   └── REFERENCE.md              # MCP tools/resources, CLI flags & repo layout
├── internal/
│   ├── controlplane/             # CP1–CP5 handlers, bearer auth, embedded views page, CP1 analysis rule checks
│   ├── designeval/               # Sandboxed Goa/Goa-AI/Model subprocess evaluator & C4 extractor
│   ├── diff/                     # Structural declaration, tool schema, and go.mod version diff engine
│   ├── goldenpath/               # Embedded GOLDEN_PATH.md playbook & decision component catalog served at surla://golden-path
│   ├── procgroup/                # Process-group lifecycle management for sandboxed child processes
│   ├── project/                  # One project's private state, owner identity, caller credentials, and browser sessions
│   ├── scaffold/                 # Starter project scaffolder (`surla init`) & embedded templates
│   ├── testwriter/               # The test writer's linked working copy, the sandboxed check of its tests against a Story, and the commit that records them
│   ├── verify/                   # Worktree verifier (`surla verify`), holdout redactor, and promotion check (`surla approve`)
│   └── verifyrun/                # Detached supervised process-group runner and run-directory store for CP4 verifications
├── ui/
│   └── checkpoint/               # React + OpenUI views page source; build.mjs writes checkpoint.html, its script digest, decision_catalog.md, and generated types
├── verify/
│   ├── candidate/                # macOS Seatbelt (`sandbox-exec`) build & runtime isolation jail (`Build`, `BuildTest`, `Pin`, `Exec`, `Output`)
│   └── suite/                    # Evaluator suite command runner (`suite.Main`, `suite.Command`, `suite.Declaration`)
├── examples/
│   ├── orders/                   # Reference Goa + Goa-AI + Temporal + SQLite order-refund platform
│   └── orders-eval/              # Isolated evaluator harness, simulators, and 5 acceptance scenarios
└── gen/                          # Generated Goa HTTP, JSON-RPC, and MCP server/client packages
```
