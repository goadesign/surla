---

## 8. 🧪 Evaluating Your Agents

This design declares no evaluation suites. Surla verifies a commit with an evaluator suite that is a separate Goa module: an `eval/` module in the layout `surla init` creates, or the suite named by the server's `-verification-config`. After you edit an `eval/` module's design, regenerate it with `generate_code` and `target: "eval"` (or `surla gen -dir eval`). `verify_change` runs the suite commit that `surla serve` pinned when it started or created the project; to run an edited suite, commit it and restart `surla serve`, or pass that commit to `surla verify -suite-commit`.

