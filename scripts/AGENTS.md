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
