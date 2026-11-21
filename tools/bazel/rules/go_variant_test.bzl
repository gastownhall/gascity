"""go_variant_test: run an existing go_test under build tags or extra environment.

A Go package's suite sometimes has to run more than one way: compiled under a
build tag (-tags productmetrics_testhook), or against a pinned tool named in
the environment. Each way is a test target of its own, so it gates, caches
and shards on its own, but none of them should restate the package's srcs
and deps: gazelle owns those on the package's go_test.

go_variant_test wraps that go_test instead. It runs the same test binary
(built in the same configuration unless gotags is set, so env-only variants
add no compile actions), with the wrapped test's runfiles, plus:

  env      test environment, merged over the wrapped test's own env;
           values expand $(rootpath ...) and friends over data. Tools the
           code under test runs by name go on PATH through internal/testenv's
           GC_TEST_TOOL_PATHS (.bazelrc pins the test PATH itself);
  data     extra runfiles, beside the wrapped test's own;
  args     (the common test attribute) test binary flags, e.g.
           "-test.run=^TestDoltlite"; the wrapped test's own args are not
           carried over;
  gotags   build tags: the wrapped test and its whole Go dependency graph
           are built with --@rules_go//go/config:tags set to these, as
           `go test -tags` builds them, while its data (a //cmd/gc binary,
           say) keeps the untagged build. The package's BUILD file needs a
           matching `# gazelle:build_tags` directive so gazelle lists the
           tagged files in srcs (rules_go drops them again in every
           configuration without the tag).

Sharding is the test binary's own: rules_go test binaries honor
TEST_TOTAL_SHARDS / TEST_SHARD_INDEX, so shard_count works as on a go_test.
"""

_TAGS = "@rules_go//go/config:tags"

# rules_go's own go_transition records the pre-transition value here, and its
# non_go_transition (on every go_test/go_binary data edge) restores it, so a
# tagged test's data is still the untagged build rather than a second compile
# of its whole graph. Do the same bookkeeping.
_ORIGINAL_TAGS = "@rules_go//go/private/rules:original_tags"

def _gotags_transition_impl(settings, attr):
    tags = settings[_TAGS]
    original = settings[_ORIGINAL_TAGS]
    if attr.gotags:
        want = sorted({t: None for t in attr.gotags}.keys())
        if want != tags:
            if original:
                fail("go_variant_test gotags cannot nest inside another Go tags transition")
            original = json.encode(tags)
            tags = want
    return {_TAGS: tags, _ORIGINAL_TAGS: original}

_gotags_transition = transition(
    implementation = _gotags_transition_impl,
    inputs = [_TAGS, _ORIGINAL_TAGS],
    outputs = [_TAGS, _ORIGINAL_TAGS],
)

def _single(attr_value):
    # An attribute with a 1:1 transition is a list of one on some Bazel
    # versions and a target on others.
    if type(attr_value) == "list":
        if len(attr_value) != 1:
            fail("expected exactly one test target, got %d" % len(attr_value))
        return attr_value[0]
    return attr_value

def _go_variant_test_impl(ctx):
    test = _single(ctx.attr.test)
    test_exe = test[DefaultInfo].files_to_run.executable
    if test_exe == None:
        fail("%s is not executable" % test.label)

    # A symlink, not a launcher script: rules_go test binaries find their
    # package directory (and chdir there) through TEST_SRCDIR and the
    # package path compiled into them, not through argv[0].
    exe = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.symlink(output = exe, target_file = test_exe, is_executable = True)

    runfiles = ctx.runfiles(files = [test_exe] + ctx.files.data)
    runfiles = runfiles.merge(test[DefaultInfo].default_runfiles)
    for d in ctx.attr.data:
        runfiles = runfiles.merge(d[DefaultInfo].default_runfiles)

    env = {}
    inherited = []
    if RunEnvironmentInfo in test:
        env.update(test[RunEnvironmentInfo].environment)
        inherited = test[RunEnvironmentInfo].inherited_environment
    for key, value in ctx.attr.env.items():
        env[key] = ctx.expand_location(value, ctx.attr.data)

    providers = [
        DefaultInfo(executable = exe, runfiles = runfiles),
        RunEnvironmentInfo(environment = env, inherited_environment = inherited),
    ]
    if InstrumentedFilesInfo in test:
        providers.append(test[InstrumentedFilesInfo])
    return providers

go_variant_test = rule(
    implementation = _go_variant_test_impl,
    test = True,
    doc = "Runs an existing go_test with build tags, extra env, data and args.",
    attrs = {
        "test": attr.label(
            mandatory = True,
            cfg = _gotags_transition,
            doc = "The go_test to run.",
        ),
        "env": attr.string_dict(doc = "Extra test environment ($(rootpath) expands over data)."),
        "data": attr.label_list(
            allow_files = True,
            doc = "Extra runfiles, beside the wrapped test's own.",
        ),
        "gotags": attr.string_list(doc = "Build tags for the wrapped test and its Go deps."),
    },
)
