# Plan: converge rig-scope rejection Option B (`ga-6qnps`)

> PM owner: `gascity/pm`
> Source: `ga-6qnps`
> Origin: architecture and designer handoff from `ga-6dy55`

## Goal

`gc converge` must fail fast when an operator explicitly requests rig scope
with `--rig` or `GC_RIG`. Until the larger rig-aware convergence feature ships,
convergence root beads remain city-scoped and rig-bound work should be reached
through city-scoped formulas whose wisps target rig agents.

## Context

This is the immediate P2 fix. It is intentionally CLI-only and must not touch
controller convergence files. The source design corrects one architecture note:
`retry` does not use `convergeSocketCmd`, so its check belongs in the retry
`RunE` path.

Because the source includes a completed design handoff, all child beads are
build-ready and routed to `gascity/builder`.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-6qnps.1` | Add converge rig-scope rejection regression coverage | `ready-to-build` -> `gascity/builder` | none |
| `ga-6qnps.2` | Implement explicit converge rig-scope rejection | `ready-to-build` -> `gascity/builder` | `ga-6qnps.1` |
| `ga-6qnps.3` | Verify P2 converge rig-scope fix and guardrails | `ready-to-build` -> `gascity/builder` | `ga-6qnps.2` |

## Acceptance: `ga-6qnps.1`

1. Tests cover `create --rig` returning the canonical unsupported-rig error
   before socket or store work.
2. Tests cover `list` with `GC_RIG` set returning the canonical unsupported-rig
   error before store work.
3. Tests cover the approve path through `convergeSocketCmd` with `rigFlag` set
   returning the canonical unsupported-rig error before dialing.
4. Tests cover `retry --rig` in retry's own `RunE` path.
5. Existing convergence tests remain runnable without broad fixture rewrites.

## Acceptance: `ga-6qnps.2`

1. All 8 converge subcommands reject `rigFlag` or `GC_RIG` with the actual
   subcommand name in the message.
2. Checks run before `resolveCity`, `openCityStore`, `sendConvergenceRequest`,
   or socket dialing.
3. `create`, `status`, `list`, `test-gate`, and `retry` perform their checks in
   their own `RunE` paths.
4. `approve`, `iterate`, and `stop` perform the check in `convergeSocketCmd`.
5. `convergence_tick.go`, `convergence_store.go`, and `city_runtime.go` are not
   changed in this P2 slice.

## Acceptance: `ga-6qnps.3`

1. Focused converge command tests pass.
2. `go test ./...` passes, or unrelated pre-existing failures are documented
   with exact failing packages.
3. `go vet ./...` passes, or unrelated pre-existing failures are documented
   with exact output.
4. Git diff confirms the slice stayed CLI-only and left controller convergence
   files untouched.
5. The root bead can be closed with a concise implementation summary.

## Dependency Graph

`ga-6qnps.1` -> `ga-6qnps.2` -> `ga-6qnps.3`

The tests come first so the CLI behavior is pinned before implementation. The
final verification bead is the blocker for the larger rig-aware convergence
feature.

## Out Of Scope

- Rig-store convergence loop creation.
- Controller handler maps or multi-store tick logic.
- New flags on approve, iterate, stop, status, or test-gate.
- Any rewrite of formula or wisp execution semantics.

## Risk

The main risk is checking inferred rig context instead of only explicit rig
surfaces. Builders must check `rigFlag` and `os.Getenv("GC_RIG")`, not
`resolveContext().RigName`, to avoid false positives when a user runs commands
from a registered rig directory.
