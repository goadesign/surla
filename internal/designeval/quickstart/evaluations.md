---

## 8. 🧪 Evaluating Your Agents

This design declares no evaluation suites. Surla verifies a commit with an evaluator suite that is a separate Goa module: the project's `eval/` module when it has one, or the suite named by the server's `-verification-config`. After you edit the `eval/` module's design, regenerate it with `generate_code` and `target: "eval"` (or `surla gen -dir eval`). Run the suite against a commit with `verify_change` (or `surla verify`), which runs it in Surla's sandbox.

