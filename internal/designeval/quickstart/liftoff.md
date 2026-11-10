---

## 2. 🚀 The 3-Step Liftoff: Your First Agent Run

Surla runs Goa's generator in its sandbox and builds and tests this project in Linux containers, so use its MCP tools on the `surla serve` that manages this project rather than running `goa` or `go` commands on your machine:

1. After you edit `design/`, call `generate_code` (or run `surla gen`) to regenerate `gen/` and this guide.
2. Call `check_project` to build and test your uncommitted files. When its message reports a missing `go.sum` or `go.work.sum` entry or a `go.mod` update, call `update_dependencies` and check again. `read_check_output` returns the captured output.
3. Commit. When the server has verification configured (a committed project in the layout `surla init` creates, or `-verification-config`), pass the commit and your confirmed Change Card's `change_id` and `request_revision` to `verify_change` (or run `surla verify`) to run the evaluator suite against it. `start_live_target` runs a commit as a live target so `draft_override` can try prompt wording on it.

Surla does not run `goa example`. When this project contains the files it scaffolds, such as `cmd/<service>/main.go`, `internal/agents/<service>/bootstrap/bootstrap.go` and `internal/agents/<agent>/planner/planner.go`, they are yours to edit, and the rest of this part explains what they do.

