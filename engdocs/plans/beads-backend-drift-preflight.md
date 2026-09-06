# Plan: beads backend drift preflight

> Owner: `gascity/pm` - Created: 2026-05-14
> Source design: `ga-g1v9yd` from `gascity/designer`
> Parent architecture: `ga-o92gf7`
> Decomposed into: 4 builder beads

## Context

Gas City needs a read-only operator preflight before any native beads store
path can activate. The current gascity workspace is the concrete failure
case: metadata and active bd context disagree, and the metadata project ID
does not match the database project ID. Without a typed preflight contract,
native store selection could be enabled over a drifted scope.

This plan turns the completed design into builder-ready work. There is no
remaining design task, and no validator-only task: each builder bead carries
its own TDD acceptance criteria.

Tracker import was a no-op in this session because no visible tracker skill
was installed.

## Children

| ID | Title | Routing label | Routes to | Depends on |
| --- | --- | --- | --- | --- |
| `ga-g1v9yd.1` | As an operator, I can trust redacted preflight diagnostics | `ready-to-build` | `gascity/builder` | - |
| `ga-g1v9yd.2` | As an operator, I can detect beads backend drift before native store activation | `ready-to-build` | `gascity/builder` | `ga-g1v9yd.1` |
| `ga-g1v9yd.3` | As an operator, I can run gc beads preflight in human or JSON mode | `ready-to-build` | `gascity/builder` | `ga-g1v9yd.2` |
| `ga-g1v9yd.4` | As a maintainer, I can verify preflight behavior with regression tests | `ready-to-build` | `gascity/builder` | `ga-g1v9yd.3` |

## Acceptance Rollup

The parent is complete when all four children are closed and the following
outcomes hold:

- `internal/beads/contract/` has typed preflight result, check result,
  verdict, fallback, repair-step, and redaction support.
- `PreflightChecker.Check()` evaluates provider contract, metadata backend,
  bd context agreement, identity match, and contract shape in the designed
  order.
- The checker remains read-only. It never rewrites metadata and never runs
  `bd doctor --fix`, `bd bootstrap`, or any hidden repair action.
- `gc beads preflight [--scope <path>] [--json] [--verbose]` exists, is
  non-interactive, and returns the designed exit codes: eligible=0,
  blocked=1, degraded=2, unable-to-run=3.
- Human output includes `[PASS]`, `[WARN]`, and `[FAIL]` text labels; JSON
  output is machine-readable and already redacted before serialization.
- Regression coverage includes the current gascity drift scenario, DSN
  redaction, skip-override remaining blocked, and unreadable-scope handling.

## Dependency Graph

```text
ga-g1v9yd.1
  -> ga-g1v9yd.2
      -> ga-g1v9yd.3
          -> ga-g1v9yd.4
```

`ga-l2souo.4` depends on `ga-g1v9yd.2` because the store factory needs the
preflight checker contract. `ga-l2souo.6` depends on `ga-g1v9yd.4` because
end-to-end native-store selection must agree with the finished preflight
regression suite.

## Routing Rationale

All child beads route to `gascity/builder` with `ready-to-build`. The design
already specifies operator UX, command shape, redaction rules, diagnostic
JSON, repair text, and guard tests. There is no unresolved UX or architecture
decision to send back upstream.

## Risks

- The active workspace currently has identity drift. Builder and PM bead
  mutations may need the recovery override until the workspace metadata is
  reconciled, but implementation code must not treat that override as a
  passing native-store condition.
- The redaction layer is load-bearing. Tests must assert that secrets are
  removed before JSON serialization, not only hidden in terminal rendering.
- Exit codes are an operator contract. Any future change requires a design
  update, not an incidental CLI tweak.

## Out of Scope

- Automated repair or a `--fix` flag.
- Postgres native store support.
- Embedded Dolt multi-writer safety.
- Store factory activation. That is decomposed under `ga-l2souo`.

## Validation Gates

- `go test ./internal/beads/contract/... -count=1`
- `go test ./cmd/gc/... ./internal/beads/contract/... -count=1`
- `go vet ./...`
- No hardcoded user role names in Go source.
- No untyped JSON wire additions.
