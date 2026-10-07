# scripts — change guide

## Hermetic Git test config is mirrored

`Makefile`'s `TEST_ENV` and the nested `env -i` wrappers in
`scripts/test-local-parallel`, `scripts/test-go-test-shard`, and
`scripts/test-integration-shard` must all pin `GIT_CONFIG_NOSYSTEM=1` and
`GIT_CONFIG_GLOBAL=/dev/null`. Updating only the Makefile is insufficient
because each nested runner rebuilds the environment and would otherwise
restore user Git configuration through the preserved `HOME`.

## Git hooks chain to beads

Each `.githooks` hook forwards to `.githooks/lib/beads-chain.sh`. Adding a
hook that beads manages means adding its `.githooks` counterpart too —
`TestGitHooksCoverEveryBeadsManagedHook` in `scripts/` fails otherwise. Hook
ownership is explained in `CONTRIBUTING.md` ("Git hook ownership").

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
