# Plan: designer handoff batch for nudge sweep, patrol idle, fan-out, and doctor branch check

> PM owner: `gascity/pm`
> Sources: `ga-9zqt5o`, `ga-la6yf7`, `ga-paqwas`, `ga-iq13bl`
> Origin: designer handoff from `gascity/designer`, 2026-06-06

## Goal

Move four completed designer handoffs into builder-ready implementation work.
Because each source bead is labeled `source:actual-designer`, no work routes
back to design. Each child bead is labeled `ready-to-build` with
`gc.routed_to=gascity/builder`.

## Context

The designer completed UX and operator-flow specs plus diagrams for:

- `ga-9zqt5o`: `nudge-mail-sweep` order for consumed nudge and read mail bead GC.
- `ga-la6yf7`: patrol idle-and-exit pattern replacing agent sleep loops with
  clean exit plus existing idle sleep policy.
- `ga-paqwas`: canonical graph.v2 fan-out guidance and `gc validate` warning
  for legacy `gc.output_json`.
- `ga-iq13bl`: `gc doctor` `rig-root-branch` advisory check, including warmup.

PM resolved the designer open questions as follows:

- `nudge-mail-sweep`: manual run should be discoverable through the existing
  order command surface; logs are sufficient for v1, and no new event type is
  required unless an existing order telemetry contract already demands one.
- Patrol idle: use the existing `[session_sleep]` / `sleep_after_idle` policy
  surface with a 300s noninteractive effective policy. Keep the plain text
  `IDLE: no work, exiting turn` signal for v1.
- Fan-out: use `engdocs/drain-fanout.md` as the canonical path. The validation
  warning should link there; no inline replacement snippet is required for v1.
- Doctor branch check: standardize advisory output for this check through the
  existing `SeverityAdvisory` rendering contract. Do not add `gc doctor --fix`
  support in this slice.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-9zqt5o.1` | As an operator, stale nudge and read mail beads are swept safely | `ready-to-build` -> `gascity/builder` | none |
| `ga-9zqt5o.2` | As an operator, I can run and observe nudge-mail-sweep | `ready-to-build` -> `gascity/builder` | `ga-9zqt5o.1` |
| `ga-la6yf7.1` | As a patrol formula author, idle steps exit cleanly instead of sleeping | `ready-to-build` -> `gascity/builder` | none |
| `ga-la6yf7.2` | As an operator, patrol idle sessions use sleep-after-idle policy | `ready-to-build` -> `gascity/builder` | `ga-la6yf7.1` |
| `ga-paqwas.1` | As a formula author, I have a canonical drain fan-out guide | `ready-to-build` -> `gascity/builder` | none |
| `ga-paqwas.2` | As a formula author, gc validate warns on gc.output_json in graph.v2 | `ready-to-build` -> `gascity/builder` | `ga-paqwas.1` |
| `ga-iq13bl.1` | As a city operator, gc doctor detects rigs on the wrong branch | `ready-to-build` -> `gascity/builder` | none |
| `ga-iq13bl.2` | As a city operator, advisory doctor output is clear during doctor and warmup | `ready-to-build` -> `gascity/builder` | `ga-iq13bl.1` |

## Acceptance Summary

`ga-9zqt5o.1` is complete when the core sweep safely selects only stale,
non-live nudge beads and open read mail beads, observes 10m/60m default TTLs,
enforces a 50-bead total budget, records terminal nudge metadata before close,
continues past per-bead close conflicts, and has focused tests for those cases.

`ga-9zqt5o.2` is complete when operators can run and dry-run the sweep manually,
the controller can run the same sweep automatically without any user-configured
role, output matches the designer copy, and tests cover CLI and automatic paths.

`ga-la6yf7.1` is complete when all shipped patrol formulas that sleep between
cycles are inventoried and updated to emit exactly `IDLE: no work, exiting turn`,
stop immediately, and avoid `sleep`, `ScheduleWakeup`, or `Monitor` idle loops
while preserving required next-wisp behavior.

`ga-la6yf7.2` is complete when patrol agents have an effective 300s
noninteractive idle-sleep policy through existing config layering, `idle_timeout`
precedence is checked, and verification demonstrates controller-managed sleep or
wake-on-nudge without polling or growing context.

`ga-paqwas.1` is complete when `engdocs/drain-fanout.md` exists with a quick
reference, when-to-use guidance, drain field table, minimal graph.v2 TOML
example, legacy `gc.output_json` caveat, and FAQ.

`ga-paqwas.2` is complete when `gc validate` emits a non-fatal warning for
`gc.output_json` in graph.v2 formulas only, preserves exit-code semantics, links
to `engdocs/drain-fanout.md`, and tests warning/no-warning cases plus warning
count summary.

`ga-iq13bl.1` is complete when `gc doctor` evaluates each non-suspended rig's
current branch against its default branch, warns advisory on mismatch, reports
dirty-tree impact, handles missing git/non-repo paths gracefully, and tests the
designer cases.

`ga-iq13bl.2` is complete when advisory severity renders with a text-only
advisory indicator in doctor/warmup output, `rig-root-branch` is registered for
doctor and startup warmup with `WarmupEligible=true` and `CanFix=false`, and
blocking warning/error semantics remain distinct.

## Dependency Graph

`ga-9zqt5o.1` -> `ga-9zqt5o.2`

`ga-la6yf7.1` -> `ga-la6yf7.2`

`ga-paqwas.1` -> `ga-paqwas.2`

`ga-iq13bl.1` -> `ga-iq13bl.2`

## Out Of Scope

- New dashboard/API surfaces for these items.
- New event-bus payload types unless an existing order telemetry contract
  already requires one.
- `gc doctor --fix` for the branch check.
- Forced migration of existing `gc.output_json` callers.
- New config schema for patrol idle sleep.

## Risk

The highest risks are accidental behavior changes in controller-owned loops:
order dispatch must stay controller-driven, patrol idle must not reintroduce a
model polling loop, and doctor warmup must not turn advisory branch drift into a
blocking startup failure. Builder should keep tests close to the existing command
and domain surfaces and escalate if any slice appears to require a new primitive
or role-specific Go behavior.
