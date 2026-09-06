# Plan: review-formulas dispatch speedup

> PM owner: `gascity/pm`
> Sources: designer beads `ga-rncghg`, `ga-o1exvf`;
> architecture root `ga-v3i9mi`

## Current Status

This plan is historical as of 2026-06-20. Do not use it to route new
`gc hook --wait` or timeout-revert work.

Operator decision `ga-rncghg.2.1` superseded Track B: PR #3599 was closed,
`ga-rncghg.2` is abandoned, and `gc hook --wait` is not part of the active
review-formulas slowdown exit path. The timeout bumps from #3590 and #3593
stay in place unless a Track-C-justified revert has explicit operator
confirmation.

Use `engdocs/plans/review-formulas-ci-slowdown-current-plan.md` for the active
`ga-pmhi6g.1` handoff.

## Goal

Turn the completed dispatch-speedup designs into builder-ready work that
reduces review-formula integration latency and removes temporary timeout
bumps once the real fixes are verified.

The package has four builder beads. Mayor amendment `gm-wisp-m5q6sam`
places Track A on hold pending nudge-latency evidence; Tracks B and C proceed
now. Track B is a standalone `gc hook --wait` feature with its own tests.

- Track A sends work-ready nudges from the controller when assigned work beads
  become newly ready. This track is deferred until investigator evidence shows
  nudge delivery beats the existing roughly 2.5s worker poll; if delivery is
  supervisor-patrol-gated, Track A is dropped.
- Track B adds true-blocking `gc hook --wait` and updates the graph-dispatch
  test worker to wait on dispatch signals instead of idle polling.
- Track C makes managed-Dolt `gc init` wait for Dolt readiness before
  continuing.
- Closeout reverts temporary review-formula timeout bumps after Tracks B and C
  have landed with timing evidence and Track A has an explicit mayor or
  investigator decision: reauthorized and landed, or dropped.

## Work Breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-rncghg.1` | As a worker, I get nudged when assigned work becomes ready | held; resume to `gascity/builder` only if reauthorized | `deferred`, `on-hold`, `needs-investigation` |
| `ga-rncghg.2` | As an operator, gc hook can truly block until work is ready | `gascity/builder` | `ready-to-build` |
| `ga-o1exvf.1` | As a city initializer, managed Dolt is ready before gc init continues | `gascity/builder` | `ready-to-build` |
| `ga-rncghg.3` | As a maintainer, temporary review-formula timeout bumps are reverted | `gascity/builder` | `ready-to-build` |

## Dependency Graph

```text
ga-rncghg.1 (Track A controller work-ready nudges; held)
ga-rncghg.2 (Track B standalone gc hook wait feature)
ga-o1exvf.1 (Track C managed-Dolt init robustness)
  -> ga-rncghg.3 (timeout bump reverts + red-CI closeout)
```

Tracks B and C are independent implementation PRs and are not blocked on Track
A. The closeout waits for B and C, then confirms the Track A decision before
reverting temporary timeout bumps. If Track A is reauthorized, closeout waits
for it to land; if Track A is dropped, closeout proceeds with B+C timing
evidence.

## Acceptance Summary

`ga-rncghg.1` is on hold. It may only resume when investigator evidence shows
nudge delivery beats the existing roughly 2.5s worker poll. If nudge delivery is
gated on the supervisor patrol or otherwise slower than that poll, Track A is
dropped rather than built.

`ga-rncghg.2` is complete when `gc hook --wait <duration>` checks for work
before blocking, performs a true blocking wait on an event source such as the
city wake socket, inotify, long-poll, or a blocking query, preserves non-wait
exit-code behavior, handles missing sockets and invalid durations cleanly,
documents the flag in help, updates `test/agents/graph-dispatch.sh`, and ships
as its own feature PR with its own tests. It must not be implemented as a
fast-poll loop or repeatedly respawn the `gc` binary while idle. Its timing
evidence is standalone; combined Track A timing is not a prerequisite while
Track A is held.

`ga-o1exvf.1` is complete when managed-Dolt `gc init` waits for the port file
and TCP readiness before city table initialization, exits with clear stderr and
no partial city state on timeout or process death, skips the wait for
non-managed Dolt, simplifies `initCityWithManagedDoltRecovery`, and verifies
the retry tail is gone through repeated init or relevant integration runs. It
ships separately from Tracks A and B and is surfaced to mayor for merge.

`ga-rncghg.3` is complete when the temporary timeout bumps from #3590 and
#3593 are reverted after Tracks B and C are proven sufficient and Track A has
an explicit final decision, review-formula timing evidence shows comfortable
restored-budget margin, and red-CI tracker beads `ga-okw7ls`, `ga-svvlfb`,
`ga-2bpndt`, and `ga-o7k0r9` are closed or updated with their remaining blocker.
The backoff-cap path from #3597 / `ga-acyrjd` is not used as the closeout
strategy; the operator ruled it out because it would thrash real user machines.

## Handoff Notes

- No additional design bead is needed; both source beads are labeled
  `source:actual-designer`.
- Track A is not currently routed to builder. Resume it only on mayor or
  investigator reauthorization.
- Tracks B, C, and closeout route to `gascity/builder`; there is no route back
  to design.
- Builder should not add role-specific Go logic or new persisted dispatch state.
- Builder should keep Track B a true blocking wait, not a short sleep loop.
- Backoff-cap work from #3597 / `ga-acyrjd` is off the table.

## Risks

The main risk is treating the timeout-revert closeout as optional. It is part
of the architecture done-when, not cleanup. The second risk is accidentally
building Track A before the latency investigation resolves; a patrol-gated
nudge would be slower than the existing poll and would regress dispatch. The
third risk is that standalone `gc hook --wait` could become a fast-poll loop;
that would reintroduce CPU, battery, and fan cost for real users. The ruled-out
backoff-cap path must stay closed even if it looks like a quick timing win.
