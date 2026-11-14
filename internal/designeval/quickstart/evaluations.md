---

## 8. 🧪 Evaluating Your Agents

This design declares no evaluation suites. Surla verifies a commit with an evaluator suite that is a separate Go module: an `eval/` module in the layout `surla init` creates, or the suite named by the server's verification settings the user stored with `surla verification set`. After an `eval/` module's design changes, the builder regenerates it with `generate_code` and `target: "eval"`, and the test writer reports to have Surla generate it again; from a shell that is not sandboxed, `surla gen -dir eval` does the same. Without stored verification settings, `verify_change` runs the tests Surla accepted for the user's build of the change, at the commit where it accepted them. With stored verification settings, it runs the suite commit named in them, which the server reads when Surla starts, so after you commit an edited suite, ask the user to store that commit with `surla verification set` and to run `surla stop`, then `surla`.

