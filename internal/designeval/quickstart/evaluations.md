---

## 8. 🧪 Evaluating Your Agents

This design declares no evaluation suites. Surla verifies a commit with an evaluator suite that is a separate Go module: an `eval/` module in the layout `surla init` creates, or the suite named by the server's `-verification-config`. After you edit an `eval/` module's design, regenerate it with `generate_code` and `target: "eval"` (or `surla gen -dir eval`). `verify_change` keeps running the suite commit the server was configured with or saved when it created the project. Which suite commit the server runs is the operator's choice, so after you commit an edited suite, ask the operator to make the server use that commit.

