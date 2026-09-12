# Plan: tmux server lifecycle ownership (`ga-yxnz9x`)

Source beads: `ga-h1aozp`, `ga-yxnz9x`
Created: 2026-05-30
Owner: `gascity/pm`
Priority: P2

## Goal

Keep operators attached to a city tmux socket during transient zero-session
churn while preserving clean `gc stop` teardown. The tmux provider should set
`exit-empty off` after the first successful session bind, and `gc stop` should
explicitly kill the server after all normal and orphan sessions have drained.

## Work Packages

1. `ga-yxnz9x.1` - Validator: lifecycle provider and stop-path tests
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Acceptance: failing tests cover `ConfigureServer`, `TeardownServer`,
     `sync.Once` idempotence, `cmdStopBody` teardown ordering after
     `stopOrphans()`, and safe skip behavior for non-lifecycle providers.

2. `ga-yxnz9x.2` - Builder: optional provider interface and tmux configuration
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-yxnz9x.1`
   - Acceptance: add `runtime.ServerLifecycleProvider` without changing the
     base `runtime.Provider`; implement it on `*tmux.Tmux` using existing
     `SetExitEmpty(false)` and `KillServer`; configure once after successful
     `NewSession*` calls; keep errors best-effort.

3. `ga-yxnz9x.3` - Builder: explicit stop teardown after orphan cleanup
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-yxnz9x.1`, `ga-yxnz9x.2`
   - Acceptance: `cmdStopBody` calls `TeardownServer()` after `stopOrphans()`
     and before bead-provider shutdown / final completion; `doStop` does not
     kill the server before orphan cleanup; teardown failures are reported on
     stderr without changing the stop exit code.

## Dependency Graph

- `ga-yxnz9x.1` blocks `ga-yxnz9x.2`.
- `ga-yxnz9x.1` and `ga-yxnz9x.2` block `ga-yxnz9x.3`.

## Guardrails

- Do not add server lifecycle methods to the base `runtime.Provider`.
- Do not add server lifecycle calls to the session reconciler.
- Use `sync.Once` for tmux configuration; do not use a boolean flag.
- Keep both lifecycle calls best-effort and non-fatal.
- Preserve orphan cleanup ordering: teardown belongs in `cmdStopBody` after
  `stopOrphans()`, not in `doStop()`.
- No OpenAPI, event payload registry, config schema, or hardcoded-role changes
  are expected.

## Validation

- Targeted validator tests fail before builder implementation and pass after.
- Relevant package tests for `internal/runtime/tmux` and `cmd/gc` pass.
- Full repo quality gates remain the builder/reviewer responsibility for the
  implementation beads.
