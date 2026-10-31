"""pnpm_lock_in_sync_test: a pnpm-lock.yaml stays the translation of its package-lock.json.

Each npm workspace Bazel builds keeps package-lock.json (local `npm ci`) as
its source of truth and commits pnpm-lock.yaml, `pnpm import`'s translation,
for MODULE.bazel's npm_translate_lock. Declare this test beside the lockfiles.
"""

load("@rules_python//python:py_test.bzl", "py_test")

def pnpm_lock_in_sync_test(name, manifests, rules_ts_typescript = False, **kwargs):
    """Fails when pnpm-lock.yaml and package-lock.json resolve different trees.

    Args:
      name: test name.
      manifests: every workspace package.json (root and members) and
        pnpm-workspace.yaml, if any; the lockfiles in this package are added.
      rules_ts_typescript: also require MODULE.bazel's rules_ts
        typescript.deps to pin the TypeScript the lockfile resolves.
      **kwargs: passed to py_test.
    """
    data = [
        "package-lock.json",
        "pnpm-lock.yaml",
    ] + manifests
    args = [native.package_name()]
    if rules_ts_typescript:
        data.append("//:MODULE.bazel")
        args.append("--rules-ts-typescript")
    py_test(
        name = name,
        srcs = ["//tools/bazel/rules:pnpm_lock_in_sync_test.py"],
        main = "//tools/bazel/rules:pnpm_lock_in_sync_test.py",
        args = args,
        data = data,
        **kwargs
    )
