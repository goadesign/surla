---

## 10. 📜 The Golden Rules: Working with Codegen

* ✍️ **Design First:** Always make changes in your `design/*.go` files.
* 🔄 **Regenerate:** Call `generate_code` (or run `surla gen`) to apply your changes.
* 🧪 **Check Before You Commit:** Call `check_project` to build and test your uncommitted files.
* 🚫 **Hands Off `gen/`:** Never edit the `gen/` directory by hand. Your changes will be overwritten!

