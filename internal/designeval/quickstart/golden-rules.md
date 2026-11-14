---

## 10. 📜 The Golden Rules: Working with Codegen

* ✍️ **Design First:** Always make changes in your `design/*.go` files.
* 🔄 **Regenerate:** The builder calls `generate_code` (with `target: "eval"` for the `eval/` test module) to apply design changes; the test writer reports to have Surla generate its test module again, or runs `surla gen -dir eval` from a shell that is not sandboxed.
* 🧪 **Check Before You Commit:** Call `check_project` to build and test your uncommitted files.
* 🚫 **Hands Off `gen/`:** Never edit the `gen/` directory by hand. Your changes will be overwritten!

