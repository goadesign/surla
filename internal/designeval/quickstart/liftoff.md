---

## 2. 🚀 The 3-Step Liftoff: Your First Agent Run

Surla runs Goa's generator in its sandbox and builds and tests this project in Linux containers, so use its MCP tools on the `surla serve` that manages this project rather than running `goa` or `go` commands on your machine:

1. After you edit `design/`, call `generate_code` (or run `surla gen`) to regenerate `gen/` and this guide.
2. Call `check_project` to build and test your uncommitted files. When its message reports a missing `go.sum` or `go.work.sum` entry or a `go.mod` update, call `update_dependencies` and check again. `read_check_output` returns the captured output.
3. Commit, then pass the commit to `start_live_target` to run it and to `verify_change` (or `surla verify`) to run its evaluator suite.

Surla does not run `goa example`. When this project contains the files it scaffolds, such as `cmd/<service>/main.go` and `internal/agents/`, they are yours to edit, and the next section explains what they do.

