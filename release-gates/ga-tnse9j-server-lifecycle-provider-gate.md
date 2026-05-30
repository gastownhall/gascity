# Release Gate: ServerLifecycleProvider and tmux server configuration

- Deploy bead: `ga-tnse9j`
- Source bead: `ga-yxnz9x.2`
- Review bead: `ga-7mz8rt`
- Branch: `builder/ga-yxnz9x.2-server-lifecycle`
- Remote branch: `origin/builder/ga-yxnz9x.2-server-lifecycle`
- Reviewed commit: `3e4ac7dca feat(runtime): add tmux server lifecycle provider`
- Note: `docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses the deployer role criteria and the source bead acceptance checklist.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `bd show ga-7mz8rt` shows a closed review bead with `REVIEW VERDICT: PASS` for commit `3e4ac7dca`. |
| 2 | Acceptance criteria met | PASS | Checked source bead criteria against code and tests; details below. |
| 3 | Tests pass | PASS | `go test -count=1 ./internal/runtime/tmux ./internal/runtime` passed; `go vet ./...` exited 0; `make test-fast-parallel` completed with `All fast jobs passed`. |
| 4 | No high-severity review findings open | PASS | Review notes list informational findings only; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | Branch was clean before gate creation; deployer rechecked clean status after committing this gate before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `65ac617b144e34ac8c1981485b8eccf9898a5873`. |
| 7 | Single feature theme | PASS | Commit set is one runtime/tmux lifecycle provider slice touching `internal/runtime` and `internal/runtime/tmux` only. |

## Acceptance Evidence

| Source criterion | Result | Evidence |
|---|---|---|
| Add `runtime.ServerLifecycleProvider` as an optional extension interface in `internal/runtime/runtime.go` with exported doc comments. | PASS | `internal/runtime/runtime.go` defines exported `ServerLifecycleProvider` with doc comments for the interface and both methods. |
| Do not add `ConfigureServer` or `TeardownServer` to the base `runtime.Provider` interface. | PASS | The base `Provider` interface remains unchanged; server lifecycle is a separate optional interface. |
| Implement `ConfigureServer` and `TeardownServer` on `*tmux.Tmux` using existing `SetExitEmpty(false)` and `KillServer` primitives. | PASS | `internal/runtime/tmux/tmux.go` implements `ConfigureServer` via `SetExitEmpty(false)` and `TeardownServer` via `KillServer()`. |
| Add a `sync.Once`-backed `configureOnce` field on `*Tmux`; do not use a bool flag. | PASS | `internal/runtime/tmux/tmux.go` adds `configureOnce sync.Once`; no bool flag is introduced for server configuration state. |
| `ConfigureServer` is triggered after successful new-session creation in each `NewSession*` variant and is best-effort. | PASS | `NewSession`, `NewSessionWithCommand`, and `NewSessionWithCommandAndEnv` call `_ = t.ConfigureServer()` after successful `t.run(...)`. |
| Non-tmux providers continue to compile unchanged. | PASS | `go test -count=1 ./internal/runtime` and `go vet ./...` passed; only tmux provider asserts the optional interface. |
| No role names are introduced in Go source. | PASS | `git diff origin/main...HEAD -- internal/runtime/runtime.go internal/runtime/tmux/adapter.go internal/runtime/tmux/tmux.go` contains no added role-name strings. |
| Targeted validator tests and relevant existing tmux/runtime tests pass. | PASS | `go test -count=1 ./internal/runtime/tmux ./internal/runtime` passed. |

## Reviewer Notes

- This PR adds a provider capability surface only; `runtime.Provider` itself remains unchanged.
- `ConfigureServer` is best-effort at session creation and uses `sync.Once`, so a first-call failure is not retried on that `*Tmux` instance.
- `TeardownServer` delegates to socket-scoped tmux execution through the existing `KillServer` path.
- The `cmd/gc stop` teardown wiring remains out of scope for this bead and is tracked by the dependent stop-path bead.
