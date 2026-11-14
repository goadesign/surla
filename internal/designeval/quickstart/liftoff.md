---

## 2. 🚀 The 3-Step Liftoff: Your First Agent Run

Surla runs Goa's generator in its sandbox and builds and tests this project in Linux containers, so use its MCP tools on the Surla project server that manages this project rather than running `goa` or `go` commands on your machine:

1. After you edit `design/`, regenerate `gen/` and this guide: the builder calls `generate_code`, with `target: "eval"` for the `eval/` test module, and the test writer, which works only in that test module, reports with `report_tests_finished` to have Surla generate it again. From a shell that is not sandboxed, `surla gen -dir <module>`, such as `surla gen -dir eval`, also regenerates a module.
2. Call `check_project` to build and test your uncommitted files. When its message reports a missing `go.sum` or `go.work.sum` entry or a `go.mod` update, call `update_dependencies` and check again. `read_check_output` returns the captured output.
3. Commit. When the server has verification configured (a project in the layout `surla init` creates, committed when the Surla project server started or created by `init_project`, or verification settings the user stored with `surla verification set`), `verify_change` runs the evaluator suite against the commit: pass the commit as `candidate_commit` with the `change_id` and `request_revision` of the confirmed Change Card (the guide proposes the card with `propose_change`, and the user confirms it on the views page). It returns a `run_id`; call `get_verification` with it to read the run's state and result. `start_live_target` takes the confirmed change's `change_id` and a `candidate_commit`, so `draft_override` can try prompt wording on that live copy. Without stored verification settings, both tools use the tests Surla accepted for the change's current build; choose a model in the Live target view before starting the target. With stored settings, the operator's evaluator starts it without a build or accepted test-writer tests.

Surla does not run `goa example`. When this project contains the files it scaffolds, such as `cmd/<service>/main.go`, `internal/agents/<service>/bootstrap/bootstrap.go` and `internal/agents/<agent>/planner/planner.go`, they are yours to edit, and the rest of this part explains what they do.

