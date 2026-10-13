"""go_test_run: run an existing go_test once as a build action; go_test_replay_test replays it.

A test whose run leaves an output another target reads (the v2 simulator
corpus's coverage census, which a gate sums across all its ranges) cannot be
a test target: a test cannot read another test's outputs. go_test_run runs
the go_test's binary (the same binary, in the same configuration, so no extra
compile) as an action instead, with its runfiles, and records the run's log
and the outputs it was asked to write; its go_test_replay_test prints the
log.

A failing run fails the action (FAILED TO BUILD, with the log's tail), so no
failure is ever cached: a flake replayed from the cache would fail every
later run on the same inputs. A run that outlives timeout fails it too, its
Go test timeout's panic (which names the running test) printed. A passing
run is cached as an action: --cache_test_results=no and --runs_per_test do
not rerun it (--config=fresh does: GoTestRun=+no-cache, .bazelrc). Seeds fix
a simulator run's inputs, not its goroutines' interleavings, so a rerun may
take another path (mc-gfdi5).

  go_test_run:
    test     the go_test to run;
    args     the test binary's flags, e.g. "-test.run=^TestSimCorpus$";
    env      its environment, over the go_test's own;
    outs     env var -> file name: the run gets the file's absolute path in
             the variable, and the target's files are these outputs (an
             output the run did not write is left empty);
    timeout  seconds before the run is killed; the test binary's own
             -test.timeout fires 20s before it.

  go_test_replay_test:
    run      the go_test_run to replay: it prints the run's log.
"""

GoTestRunInfo = provider(
    doc = "A go_test_run's log.",
    fields = {"log": "the run's combined output"},
)

# The run. A Go test timeout (its panic names the running test, and the
# goroutines) or a kill fails the action with that block; any other failing
# exit fails it with the log's tail, the simulator cache's reconcile lines
# left out.
_RUN = """\
set -u
exe=$1 log=$2 timeout=$3
shift 3
export TEST_SRCDIR="$PWD/$exe.runfiles" TEST_TMPDIR="$PWD/$log.tmp"
export HOME="$TEST_TMPDIR" TMPDIR="$TEST_TMPDIR"
mkdir -p "$TEST_TMPDIR"
outs=$OUTS
unset OUTS
for out in $outs; do
  export "${out%%=*}=$PWD/${out#*=}"
done
rc=0
timeout -k 10 "$timeout" "$exe" "$@" >"$log" 2>&1 || rc=$?
rm -rf "$TEST_TMPDIR"
if grep -q '^panic: test timed out' "$log" || [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
  sed -n '/^panic: test timed out/,$p' "$log" | head -n 120 >&2
  echo "go_test_run: $exe $* outlived its timeout (${timeout}s)" >&2
  exit 1
fi
if [ "$rc" -ne 0 ]; then
  grep -v 'beads cache: reconciled' "$log" | tail -n 400 >&2
  echo "go_test_run: $exe $* failed (exit $rc)" >&2
  exit 1
fi
for out in $outs; do
  [ -e "${out#*=}" ] || : >"${out#*=}"
done
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
        command = _RUN,
        arguments = [exe.executable.path, log.path, str(ctx.attr.timeout)] + flags,
        tools = [exe],
        outputs = [log] + outs,
        env = env,
        mnemonic = "GoTestRun",
        progress_message = "Running %s once for %%{label}" % test.label,
    )
    return [
        DefaultInfo(files = depset(outs)),
        GoTestRunInfo(log = log),
    ]

go_test_run = rule(
    implementation = _go_test_run_impl,
    doc = "Runs an existing go_test once as an action, recording its log and outputs; a failing run fails the action.",
    attrs = {
        "test": attr.label(mandatory = True, executable = True, cfg = "target", doc = "The go_test to run."),
        "args": attr.string_list(doc = "The test binary's flags."),
        "env": attr.string_dict(doc = "Its environment, over the go_test's own."),
        "outs": attr.string_dict(doc = "Env var -> output file name; the run gets the file's absolute path."),
        "timeout": attr.int(default = 300, doc = "Seconds before the run is killed (and the build fails)."),
    },
)

def _go_test_replay_test_impl(ctx):
    run = ctx.attr.run[GoTestRunInfo]
    exe = ctx.actions.declare_file(ctx.label.name + ".sh")
    ctx.actions.write(
        output = exe,
        is_executable = True,
        content = "#!/bin/sh\ncat '%s'\n" % run.log.short_path,
    )
    return [DefaultInfo(executable = exe, runfiles = ctx.runfiles(files = [run.log]))]

go_test_replay_test = rule(
    implementation = _go_test_replay_test_impl,
    test = True,
    doc = "Replays a go_test_run: prints its log (a failing run failed the build).",
    attrs = {"run": attr.label(mandatory = True, providers = [GoTestRunInfo], doc = "The go_test_run to replay.")},
)
