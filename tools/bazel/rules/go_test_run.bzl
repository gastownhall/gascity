"""go_test_run: run an existing go_test once as a build action; go_test_replay_test replays it.

A test whose run leaves an output another target reads (the v2 simulator
corpus's coverage census, which a gate sums across all its ranges) cannot be
a test target: a test cannot read another test's outputs. go_test_run runs
the go_test's binary (the same binary, in the same configuration, so no extra
compile) as an action instead, with its runfiles, and records the run's log,
its exit status and the outputs it was asked to write. The run's status is
recorded, not raised: a failing run is still a built target, and its
go_test_replay_test fails with the run's log as its own. A run that outlives
timeout is killed and fails the build (a hang never stalls it).

The run is an action, so it caches as one: `--cache_test_results=no` does not
rerun it, and a recorded failure replays until an input changes. That fits
only a deterministic test (the simulator's seeds are).

  go_test_run:
    test     the go_test to run;
    args     the test binary's flags, e.g. "-test.run=^TestSimCorpus$";
    env      its environment, over the go_test's own;
    outs     env var -> file name: the run gets the file's absolute path in
             the variable, and the target's files are these outputs (an
             output the run did not write is left empty);
    timeout  seconds before the run is killed.

  go_test_replay_test:
    run      the go_test_run to replay: it prints the run's log and exits
             with the run's status.
"""

GoTestRunInfo = provider(
    doc = "A go_test_run's log and exit status.",
    fields = {"log": "the run's combined output", "status": "the run's exit status, a decimal line"},
)

# The run: a killed run (timeout's 124, or the KILL's 137) fails the action
# with its log's tail; any other exit is recorded in the status file.
_RUN = """\
set -u
exe=$1 log=$2 status=$3 timeout=$4
shift 4
export TEST_SRCDIR="$PWD/$exe.runfiles" TEST_TMPDIR="$PWD/$log.tmp"
export HOME="$TEST_TMPDIR" TMPDIR="$TEST_TMPDIR"
mkdir -p "$TEST_TMPDIR"
for out in $OUTS; do
  export "${out%%=*}=$PWD/${out#*=}"
done
rc=0
timeout -k 10 "$timeout" "$exe" "$@" >"$log" 2>&1 || rc=$?
rm -rf "$TEST_TMPDIR"
if [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; then
  tail -n 200 "$log" >&2
  echo "go_test_run: $exe $* outlived ${timeout}s" >&2
  exit 1
fi
for out in $OUTS; do
  [ -e "${out#*=}" ] || : >"${out#*=}"
done
echo "$rc" >"$status"
"""

def _go_test_run_impl(ctx):
    test = ctx.attr.test
    exe = test[DefaultInfo].files_to_run
    if exe.executable == None:
        fail("%s is not executable" % test.label)
    log = ctx.actions.declare_file(ctx.label.name + ".log")
    status = ctx.actions.declare_file(ctx.label.name + ".status")
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

    ctx.actions.run_shell(
        command = _RUN,
        arguments = [exe.executable.path, log.path, status.path, str(ctx.attr.timeout)] + ctx.attr.args,
        tools = [exe],
        outputs = [log, status] + outs,
        env = env,
        mnemonic = "GoTestRun",
        progress_message = "Running %s once for %%{label}" % test.label,
    )
    return [
        DefaultInfo(files = depset(outs)),
        GoTestRunInfo(log = log, status = status),
    ]

go_test_run = rule(
    implementation = _go_test_run_impl,
    doc = "Runs an existing go_test once as an action, recording its log, status and outputs.",
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
        content = "#!/bin/sh\ncat '%s'\nexit \"$(cat '%s')\"\n" % (run.log.short_path, run.status.short_path),
    )
    return [DefaultInfo(executable = exe, runfiles = ctx.runfiles(files = [run.log, run.status]))]

go_test_replay_test = rule(
    implementation = _go_test_replay_test_impl,
    test = True,
    doc = "Replays a go_test_run: prints its log and exits with its status.",
    attrs = {"run": attr.label(mandatory = True, providers = [GoTestRunInfo], doc = "The go_test_run to replay.")},
)
