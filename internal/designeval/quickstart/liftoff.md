---

## 2. 🚀 The 3-Step Liftoff: Your First Agent Run

Surla runs Goa's generator in its sandbox and builds and tests this project in Linux containers, so use its MCP tools on the Surla project server that manages this project rather than running `goa` or `go` commands on your machine:

1. After you edit `design/`, regenerate `gen/` and this guide: the builder calls `generate_code`, with `target: "eval"` for the `eval/` test module, and the test writer, which works only in that test module, reports with `report_tests_finished` to have Surla generate it again. From a shell that is not sandboxed, `surla gen -dir <module>`, such as `surla gen -dir eval`, also regenerates a module.
2. The builder calls `check_project` to build and test the project's uncommitted files. When its message reports a missing `go.sum` or `go.work.sum` entry or a `go.mod` update, the builder calls `update_dependencies` and checks again; `read_check_output` returns the captured output.
3. The builder commits, then calls `verify_change` with the commit as `candidate_commit` and the `change_id` and `request_revision` of the confirmed Change Card (the guide proposes the card with `propose_change`, and the user confirms it on the views page). The run uses the tests Surla accepted for the user's build of that revision, so `verify_change` is refused until the user has built the revision and Surla has accepted the test writer's tests, as the builder's assignment shows; with verification settings the user stored with `surla verification set`, it runs their evaluator suite instead and needs no build. It returns a `run_id`; `get_verification` reads the run's state and result. The builder can also call `start_live_target` to run a commit as a live target, so that `draft_override` can try prompt wording on it; without stored verification settings, that commit needs the test layout `surla init` creates.

Surla does not run `goa example`. When this project contains the files it scaffolds, such as `cmd/<service>/main.go`, `internal/agents/<service>/bootstrap/bootstrap.go` and `internal/agents/<agent>/planner/planner.go`, they are yours to edit, and the rest of this part explains what they do.

