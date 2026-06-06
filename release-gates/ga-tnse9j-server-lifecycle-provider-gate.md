# Release Gate: ServerLifecycleProvider and tmux Server Lifecycle

Date: 2026-06-01
Deploy bead: ga-cv6483
Source review bead: ga-r3c4az
PR: https://github.com/gastownhall/gascity/pull/2774
Branch: builder/ga-yxnz9x.2-server-lifecycle
Reviewed PR head before this gate refresh: 1682958f5dd16df177969958101bae380c5c0736
Feature code commit: 7e20af490
Base checked: origin/main b2b659d421ace115fcc47575b202c0a4541ad75a

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses the deployer prompt criteria and the source bead acceptance checklist.

## Scope

The release diff from current `origin/main` contains one runtime/tmux lifecycle provider slice plus this release gate:

| Path | Status | Purpose |
|---|---:|---|
| `internal/runtime/runtime.go` | M | Adds optional `runtime.ServerLifecycleProvider`. |
| `internal/runtime/tmux/adapter.go` | M | Exposes the optional lifecycle provider capability from the tmux provider adapter. |
| `internal/runtime/tmux/tmux.go` | M | Configures tmux server `exit-empty off` after successful session creation and delegates teardown to socket-scoped `KillServer`. |
| `internal/runtime/tmux/lifecycle_test.go` | A | Covers configure, idempotence, teardown delegation, and already-gone teardown success. |
| `release-gates/ga-tnse9j-server-lifecycle-provider-gate.md` | A/M | Release gate evidence, refreshed for `ga-cv6483`. |

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-r3c4az` shows a closed review bead with `REVIEW VERDICT: PASS` for PR #2774 and commit `1682958f5`. |
| 2 | Acceptance criteria met | PASS | Code adds an optional `ServerLifecycleProvider` extension without changing the base `Provider` interface; tmux implements `ConfigureServer` and `TeardownServer`; `ConfigureServer` uses `sync.Once`, calls `SetExitEmpty(false)`, and is invoked best-effort after successful session creation. |
| 3 | Tests pass | PASS | Targeted runtime/tmux tests passed; `go vet ./...` passed; `make test-fast-parallel` passed with `All fast jobs passed`. |
| 4 | No high-severity review findings open | PASS | Review notes list informational findings only; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Detached PR-head worktree was clean before gate refresh; final clean status is verified after committing this refreshed gate. |
| 6 | Branch diverges cleanly from main | PASS | `git fetch origin main` refreshed current base; `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `69a03f03f2c17085d4a0dac00fa5edf02b11ad2a`; `git diff --check origin/main...HEAD` exited 0. |
| 7 | Single feature theme | PASS | Commit set is one runtime/tmux server lifecycle capability slice under `internal/runtime` and `internal/runtime/tmux`. |

## Acceptance Evidence

| Source criterion | Result | Evidence |
|---|---|---|
| Add `runtime.ServerLifecycleProvider` as an optional extension interface with exported doc comments. | PASS | `internal/runtime/runtime.go` defines exported `ServerLifecycleProvider` with method comments. |
| Do not add server lifecycle methods to the base `runtime.Provider` interface. | PASS | `Provider` remains unchanged; lifecycle methods live only on the optional extension. |
| Implement tmux server configuration and teardown using existing primitives. | PASS | `ConfigureServer` calls `SetExitEmpty(false)` and `TeardownServer` calls `KillServer()`. |
| Make server configuration idempotent with `sync.Once`, not a bool flag. | PASS | `Tmux` includes `configureOnce sync.Once`; lifecycle tests verify one `set-option -g exit-empty off` call across repeated `ConfigureServer` calls. |
| Trigger configuration after successful new-session creation and keep it best-effort. | PASS | `NewSession`, `NewSessionWithCommand`, and `NewSessionWithCommandAndEnv` call `_ = t.ConfigureServer()` only after successful `t.run(...)`. |
| Preserve non-tmux provider compatibility. | PASS | `go test -count=1 ./internal/runtime/tmux ./internal/runtime` and `go vet ./...` passed. |
| Avoid new hardcoded role names in Go source. | PASS | Added diff across changed runtime/tmux files contains no added `mayor`, `deacon`, or `polecat` role-name strings. |
| Preserve tmux safety. | PASS | Teardown delegates to existing socket-scoped `KillServer`; no bare default-server cleanup path is introduced. |

## Test Evidence

- PASS: `go test -count=1 ./internal/runtime/tmux ./internal/runtime`
- PASS: `go vet ./...`
- PASS: `make test-fast-parallel`
- PASS: `.githooks/pre-commit` is active via `core.hooksPath=.githooks`; commit hook runs `go test ./test/docsync`.

Gate result: PASS.
