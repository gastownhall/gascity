# Plan: shared order scanning refactor (`ga-gse1pe`)

> PM owner: `gascity/pm`
> Source: `ga-gse1pe`
> Origin: cleanup follow-up from `ga-hlhxo7`

## Goal

Order discovery should have one shared behavior contract across the user-facing
order commands, the dispatcher, and the `order-firing-current` doctor check.
The cleanup removes duplicated scanning logic while preserving current
semantics for city orders, rig-scoped orders, overrides, and filtering.

## Context

`ga-hlhxo7` added the `order-firing-current` doctor check. To keep that PR
focused, it duplicated the minimal order-scanning path from `cmd/gc`: city
layers, rig-exclusive layers, order overrides, enabled filtering, and manual
filtering for auto-dispatch. The source design allowed that shortcut if the
refactor scope-crept and asked for a follow-up cleanup bead.

Architecture docs keep `internal/orders` as the domain package for order
parsing, scanning, and trigger evaluation. `cmd/gc` is a projection over that
domain for CLI and dispatcher behavior. The cleanup must not make
`internal/doctor` import `cmd/gc`.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-gse1pe.1` | As a maintainer, I have regression tests for shared order scanning behavior | `needs-tests` -> `gascity/validator` | none |
| `ga-gse1pe.2` | As a maintainer, order consumers share one order scanning path | `ready-to-build` -> `gascity/builder` | `ga-gse1pe.1` |

## Acceptance: `ga-gse1pe.1`

The validator bead is complete when:

1. Tests or a test-focused patch pin shared order discovery across city order
   roots and rig-exclusive formula layers without double-scanning city orders.
2. Tests cover city order overrides, including `enabled=false` disabling an
   otherwise discovered order.
3. Tests cover the auto-dispatch view excluding manual-trigger orders while
   list/check style consumers can still observe manual orders where appropriate.
4. The tests live at the lowest practical layer and fail if the shared scanning
   behavior regresses.
5. Focused test commands are documented for builder handoff.

## Acceptance: `ga-gse1pe.2`

The builder bead is complete when:

1. `cmd/gc` order dispatch/list/check paths and `internal/doctor`
   `order-firing-current` use one shared order scanning implementation or
   shared lower-level helper rather than duplicated scanning logic.
2. `internal/doctor` does not import `cmd/gc`, and no package dependency points
   upward across the existing layering.
3. City-level order roots, rig-exclusive order layers, order overrides,
   `enabled=false` filtering, skip filtering, and `Rig` stamping behave the
   same as before.
4. Manual-trigger orders remain excluded from auto-dispatch while user-facing
   list/check behavior remains appropriate for manual orders.
5. The validator tests from `ga-gse1pe.1` pass, along with focused order and
   doctor tests.
6. `go test ./cmd/gc ./internal/orders/... ./internal/doctor/...` passes, and
   `go vet ./...` is clean.

## Dependency Graph

`ga-gse1pe.1` -> `ga-gse1pe.2`

The validator bead goes first so the behavior contract is visible before the
refactor. The builder bead should not expand order semantics; it pays down
duplication only.

## Out Of Scope

- New order trigger behavior or schedule parsing changes.
- New dashboard/API surfaces for orders.
- Reworking formula loading beyond what is necessary to share the existing
  order scanning path.
- Changing the `order-firing-current` status thresholds from `ga-hlhxo7`.

## Risk

The main risk is accidentally changing which orders are visible to a consumer:
manual orders should stay out of auto-dispatch, rig-exclusive layers should not
double-count city orders, and overrides must still apply consistently. The
validator bead is intentionally first to make those regressions concrete.
