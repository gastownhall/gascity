"""go_test_run: run an existing go_test once as a build action.

A test whose run leaves an output another target reads (the v2 simulator
corpus's coverage census, which a gate sums across all its ranges) cannot be
a test target: a test cannot read another test's outputs. go_test_run runs
the go_test's binary (the same binary, in the same configuration, so no extra
compile) as an action instead (go_test_run.sh), with its runfiles, and
records the run's log and the outputs it was asked to write. The test that
reads the outputs pulls the runs into `bazel test`.

A failing run fails the action (FAILED TO BUILD, with the failing tests'
summary lines and the log's tail), so no failure is ever cached: a flake
replayed from the cache would fail every later run on the same inputs. A run
that outlives timeout fails it too, its Go test timeout's panic (which names
the running test) printed. A passing run is cached as an action:
--cache_test_results=no and --runs_per_test do not rerun it (--config=fresh
does: GoTestRun=+no-cache, .bazelrc). A simulator range is a function of
its seeds (TestSimDeterministic, mc-gfdi5), so a cached pass is the pass a
rerun would make.

    test     the go_test to run;
    args     the test binary's flags, e.g. "-test.run=^TestSimCorpus$";
    env      its environment, over the go_test's own;
    outs     env var -> file name: the run gets the file's absolute path in
             the variable, and the target's files are these outputs (an
             output the run did not write is left empty);
    timeout  seconds before the run is killed; the test binary's own
             -test.timeout fires 20s before it.
"""

def _go_test_run_impl(ctx):
    test = ctx.attr.test
    exe = test[DefaultInfo].files_to_run
    if exe.executable == None:
        fail("%s is not executable" % test.label)
    if ctx.attr.timeout <= 30:
        fail("timeout must leave the test binary's own -test.timeout (20s earlier) room")
    log = ctx.actions.declare_file(ctx.label.name + ".log")
    outs, pairs = [], []
    for var, name in sorted(ctx.attr.outs.items()):
        out = ctx.actions.declare_file(name)
        outs.append(out)
        pairs.append("%s=%s" % (var, out.path))

    env = {}
    if RunEnvironmentInfo in test:
        env.update(test[RunEnvironmentInfo].environment)
    env.update(ctx.attr.env)
    env["TEST_WORKSPACE"] = ctx.workspace_name
    env["OUTS"] = " ".join(pairs)

    flags = ["-test.timeout=%ds" % (ctx.attr.timeout - 20)] + ctx.attr.args
    ctx.actions.run_shell(
        command = 'exec bash "$@"',
        arguments = [ctx.file._runner.path, exe.executable.path, log.path, str(ctx.attr.timeout)] + flags,
        inputs = [ctx.file._runner],
        tools = [exe],
        outputs = [log] + outs,
        env = env,
        mnemonic = "GoTestRun",
        progress_message = "Running %s once for %%{label}" % test.label,
    )
    return [DefaultInfo(files = depset(outs)), OutputGroupInfo(log = depset([log]))]

go_test_run = rule(
    implementation = _go_test_run_impl,
    doc = "Runs an existing go_test once as an action, recording its log and outputs; a failing run fails the action.",
    attrs = {
        "test": attr.label(mandatory = True, executable = True, cfg = "target", doc = "The go_test to run."),
        "args": attr.string_list(doc = "The test binary's flags."),
        "env": attr.string_dict(doc = "Its environment, over the go_test's own."),
        "outs": attr.string_dict(doc = "Env var -> output file name; the run gets the file's absolute path."),
        "timeout": attr.int(default = 300, doc = "Seconds before the run is killed (and the build fails)."),
        "_runner": attr.label(default = Label("//tools/bazel/rules:go_test_run.sh"), allow_single_file = True),
    },
)
