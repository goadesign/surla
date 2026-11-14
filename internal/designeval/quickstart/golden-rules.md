---

## 10. 📜 The Golden Rules: Working with Codegen

* ✍️ **Design First:** Always make changes in your `design/*.go` files.
* 🔄 **Regenerate:** The builder calls `generate_code` to apply design changes. When a build runs with a test writer (without stored verification settings), the `eval/` test module belongs to the test writer, which reports to have Surla generate it again, or runs `surla gen -dir eval` from a shell that is not sandboxed; outside such a build, the builder regenerates `eval/` with `generate_code` and `target: "eval"`.
* 🧪 **Check Before You Commit:** The builder calls `check_project` to build and test the project's uncommitted files.
* 🚫 **Hands Off `gen/`:** Never edit the `gen/` directory by hand. Your changes will be overwritten!

