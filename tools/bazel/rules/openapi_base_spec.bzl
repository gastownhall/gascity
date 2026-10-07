"""The base side of the OpenAPI breaking-change gate (//cmd/openapi-breaking).

The gate diffs internal/api/openapi.json against the spec of the commit a
change is based on. That spec comes from git history, which no test action
can read, so this repository rule supplies it as an ordinary input:

  - GC_OPENAPI_BREAKING_BASE_SPEC set in the client environment (bazel.yml's
    unit lane on pull requests, `make openapi-breaking-check`): the file it
    names, written there with `git show <base>:internal/api/openapi.json`.
    A set value naming no file fails the fetch.
  - unset (pre-push, pushes to main, a plain `bazel test //...`): the
    committed spec itself, so the gate compares the spec with itself and
    passes. Those runs have no base to gate against.

@openapi_base_spec//:openapi.json is the base spec and :source.txt says which
of the two it is. Only the gate's test depends on this repository, so a new
base re-runs that one test and changes no other action key.
"""

_ENV = "GC_OPENAPI_BREAKING_BASE_SPEC"

def _openapi_base_spec_impl(rctx):
    path = rctx.getenv(_ENV)
    if path:
        spec = rctx.path(path)
        if not spec.exists:
            fail("%s=%s: no such file" % (_ENV, path))
        rctx.watch(spec)
        rctx.symlink(spec, "openapi.json")
        rctx.file("source.txt", "%s=%s\n" % (_ENV, path))
    else:
        rctx.symlink(rctx.path(rctx.attr.revision), "openapi.json")
        rctx.file("source.txt", "%s unset: the committed spec (no base to compare against)\n" % _ENV)
    rctx.file("BUILD.bazel", 'exports_files(["openapi.json", "source.txt"])\n')

openapi_base_spec = repository_rule(
    implementation = _openapi_base_spec_impl,
    attrs = {
        "revision": attr.label(
            mandatory = True,
            allow_single_file = True,
            doc = "The committed spec, the base when no base is configured.",
        ),
    },
    doc = "The OpenAPI breaking-change gate's base spec.",
)
