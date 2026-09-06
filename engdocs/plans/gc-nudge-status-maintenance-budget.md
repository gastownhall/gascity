# Plan: Bound gc nudge status maintenance budget (`ga-7p2ahd`)

> Owner: `gascity/pm` - Created: 2026-07-03
> Source: reviewer follow-up from `ga-w1xseh` / `ga-gkc7yi`

## Goal

Keep `gc nudge status` responsive under a large nudge backlog by making the
status listing path use the same foreground maintenance budget pattern as the
already-reviewed enqueue and drain fixes.

The reviewed follow-up identified that `listQueuedNudges` and
`listQueuedNudgesForTarget` still choose an unbounded maintenance deadline
internally. That can make a foreground read-only status command spend
unbounded time doing best-effort nudge-queue maintenance. This is lower
urgency than the prompt hook drain path because status is not on a hot
per-prompt path, but it is the same failure class.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-7p2ahd.1` | Add regression coverage for bounded nudge status maintenance | `needs-tests` -> `gascity/validator` | none |
| `ga-7p2ahd.2` | Add nudge status compatibility coverage for budgeted maintenance | `needs-tests` -> `gascity/validator` | none |
| `ga-7p2ahd.3` | Bound gc nudge status listing maintenance to foreground budget | `ready-to-build` -> `gascity/builder` | `ga-7p2ahd.1`, `ga-7p2ahd.2` |

## Acceptance Summary

`ga-7p2ahd.1` is complete when focused `cmd/gc` regression coverage fails on
the current status-listing behavior because the listing helpers do not honor a
caller-supplied foreground maintenance deadline. The test must show requested
status results are still returned while skipped maintenance items remain queued
for a later pass.

`ga-7p2ahd.2` is complete when compatibility coverage proves status output and
target matching behavior remain unchanged, and poller or equivalent
non-foreground maintenance callers remain explicitly full-drain.

`ga-7p2ahd.3` is complete when the status listing helpers accept a
caller-supplied maintenance deadline instead of hardcoding the unbounded
deadline, `gc nudge status` passes the shared foreground budget, and
non-foreground callers pass the unbounded deadline explicitly.

## Dependency Graph

```text
ga-7p2ahd.1 -> ga-7p2ahd.3
ga-7p2ahd.2 -> ga-7p2ahd.3
```

The two validator slices can run in parallel. The builder slice stays blocked
until both validator beads finish.

## Notes For Downstream Agents

- Relevant history from PM triage:
  - `2771513fa` / `93be1b652`: foreground enqueue maintenance budget.
  - `695349397` / `a2a273dfe`: drain-claim maintenance budget and poller
    full-drain protection.
- The PM checkout did not contain every helper name from the reviewer note.
  Builder should first confirm the execution branch includes the foreground
  maintenance-budget baseline, or port the smallest already-reviewed slice
  needed before applying this status follow-up.
- Scope is limited to the `gc nudge status` listing path and its tests.

## Out Of Scope

- API, dashboard, schema, or OpenAPI changes.
- Broader nudge delivery behavior.
- New role-specific behavior or hardcoded role names.
- Reworking nudge queue storage beyond what is needed for the status-listing
  deadline plumbing.

## Verification Expectation

Validator and builder should document the focused `go test ./cmd/gc/ -run ...`
commands they use. Builder acceptance should include the validator tests plus
existing nudge drain or poller coverage that proves the foreground budget does
not cap the delivery path.
