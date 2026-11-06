# scripts — change guide

## Hermetic Git test config is mirrored

`Makefile`'s `TEST_ENV` and the nested `env -i` wrappers in
`scripts/test-local-parallel`, `scripts/test-go-test-shard`, and
`scripts/test-integration-shard` must all pin `GIT_CONFIG_NOSYSTEM=1` and
`GIT_CONFIG_GLOBAL=/dev/null`. Updating only the Makefile is insufficient
because each nested runner rebuilds the environment and would otherwise
restore user Git configuration through the preserved `HOME`.

## Test-env pane shell is pinned

The same four allowlists pin `SHELL=/bin/sh` and never forward the caller's
`SHELL`. A test that opens a tmux or herdr pane runs that shell in it, and the
invoking user's zsh under a fresh `HOME` (a release gate, CI) opens its new-user
wizard, which swallows the typed text and fails the test by timeout (ga-1wilql,
ga-sux0ij). Change all four together: `TestRunnerTestEnvsPinPaneShell` in
`scripts/git_test_env_test.go` fails when one drifts.
`test/acceptance/helpers/env.go` builds its own environment and still forwards
`SHELL`: its panes start with an explicit command, which tmux runs as
`$SHELL -c`, so they never reach the wizard.

## Git hooks chain to beads

Each `.githooks` hook forwards to `.githooks/lib/beads-chain.sh`. Adding a
hook that beads manages means adding its `.githooks` counterpart too —
`TestGitHooksCoverEveryBeadsManagedHook` in `scripts/` fails otherwise. Hook
ownership is explained in `CONTRIBUTING.md` ("Git hook ownership").

The pre-push Go-change scan must consume the complete diff and fail if the
comparison cannot be read. An early-exiting `grep -q` under `pipefail` can make
a large diff look empty when Git receives SIGPIPE, silently skipping the suite.
A remote tip missing from the local object store is refused first with a
`git fetch <remote> <ref>` remedy: Git still runs the hook for a non-forced push
over an unfetched tip and prints its own "fetch first" hint only if the hook
passes. A forced push over an unfetched tip is refused too, until that fetch.
The remedy names the ref because a plain `git fetch <remote>` can skip the tip
(a single-branch clone, a URL push, a moved tag), and it strips credentials
from a push URL. `githooks_pre_push_stdin_test.go` covers large diffs,
unfetched remote tips, credentialed push URLs and unreadable comparisons.

## Make targets run Bazel; `-go` twins are the escape hatch

`make test`, `check`, `check-all`, `check-docs`, `test-acceptance` and
`test-integration` run the `bazel test` commands `bazel.yml`'s lanes run.
Each has a plain-`go test` twin named `<target>-go`. A workflow that runs a
Go-native suite calls the `-go` name explicitly (`TestWorkflowsNameTheirTestEngine`
in `scripts/` fails on a workflow calling a bazel-backed primary name). A new
test runner gets a Bazel target first; a `go test` recipe is an extra, never
the only route. `TestMakePrimaryTargetsRunBazel` in `scripts/` pins this.

`.githooks/lib/push-suite.sh` runs `bazel test //...` at push time. Its
`make test-fast-parallel` fallback prints a banner with the reason, because
that suite is not what CI enforces; `GC_PREPUSH_SUITE=go` opts in explicitly.
`auto` also falls back when a loader probe says the host cannot resolve the
ICU runtime `MODULE.bazel`'s sysroot pins, or cannot say. The docs side of
`.githooks/pre-commit` makes the same choice between `make check-docs` and
`make check-docs-go` (`GC_PRECOMMIT_DOCS=auto|bazel|go`), again behind a
banner. One shared decision, `.githooks/lib/local-runtime.sh`, serves both
hooks; it derives the soname list from a single ICU-major constant, and
`TestBazelLocalRuntimePinMatchesProbe` fails when that constant, the
`MODULE.bazel` pin and `engdocs/bazel-quickstart.md` disagree. Only `auto`
probes: an explicit mode is never replaced.

## The tmux test inventory is derived, never counted

The `internal/runtime/tmux` integration inventory is `scripts/runtime-tmux-tests.manifest`
(every test name, in discovery order) plus `scripts/runtime-tmux-integration-files.manifest`
(the `_test.go` files that compile only with `-tags integration`, one basename
per line, ascending). Both are admin-gated through CODEOWNERS `/scripts/`.
`scripts/runtime_tmux_manifest_test.go` and `scripts/runtime_tmux_lane_test.go`
derive their expectations from the source tree, so never type an inventory
count (total, default-lane, integration-only, or per-shard) into a test: two
PRs that each add a test would conflict on it. Adding a test costs manifest
lines only; a new integration-only test file costs one sorted record line.
The shard rule is `i % shard_total == shard_index - 1` in
`scripts/test-go-test-shard` (`TestRuntimeTmuxIntegrationShardUsesCheckedManifestOnLinux`
restates it and runs each shard through the real script).
