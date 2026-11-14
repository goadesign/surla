---

## 8. 🧪 Evaluating Your Agents

This design declares no evaluation suites. Surla verifies a commit with an evaluator suite that is a separate Go module: an `eval/` module in the layout `surla init` creates, or the suite named by the server's verification settings the user stored with `surla verification set`. After an `eval/` module's design changes, the builder regenerates it with `generate_code` and `target: "eval"`, and the test writer reports to have Surla generate it again; from a shell that is not sandboxed, `surla gen -dir eval` does the same. `verify_change` keeps running the suite commit the server was configured with or saved when it created the project. Which suite commit the server runs is the user's choice, and the server reads it when Surla starts, so after you commit an edited suite, ask the user to make the server use that commit.

