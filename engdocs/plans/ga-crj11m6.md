# Split-storage reconciliation after upstream S4b

Producer: `ga-crj11m6`  
Requested by: mayor, message `gm-wisp-lpkiuiw`  
Original plans: `ga-c6sekn4`, `ga-zspenhc`  
Examined main: `7472f2967c2f80481146693c2a69fc61451d52e6`  
Date: 2026-11-01 UTC / 2026-10-31 Pacific

The migration product outcome now has an upstream implementation. The October 26
packages describe a different architecture and should not consume another
validator/build/gate cycle. Ready-query compatibility remains a real blocker;
its implementation waits on one current-main architecture decision.

## Evidence and material differences

[PR #8486](https://github.com/gastownhall/gascity/pull/8486) merged at
`16777d7880f3bc7734bf49e76fc88d158ecbefaa` on October 29. Its squash is an ancestor
of the examined main. S4a (#8447), routing/placement slices #8360/#8488, and the
split-projection gate #8335 are also represented in main's history.

Current `gc storage migrate --from-rigs [--rig X] --fleet-stopped` imports rig
graph/infrastructure rows into the city binding after city convergence. It
preserves IDs, records rig scope, verifies copied rows/edges, writes per-rig
manifests/backups, clears copies with resumable state, and supplies per-rig
preflight/status/doctor census. The current runbook documents the commands,
refusals, suspended/unreachable rigs and crash recovery.

The old C1-C3 contract required graph to remain rig-local, one combined source
extent and different backup/edge semantics. Main instead moves graph rows,
runs per rig and deliberately refuses/drops/records particular boundary edges.
Retirement here means the old implementation specification is superseded; it
does not mean its original acceptance matrix passed or its source commits landed.
S4c reverse and S4d staging/benchmark are explicitly separate in #8486. No
parallel reverse or benchmark build is created by this reconciliation.

`cmd/gc/cmd_ready.go` still registers no `--label`, `--label-any` or `--rig`.
The current split-storage runbook deliberately refuses the wider bd-ready
surface and points callers to `--metadata-field`. A split city refuses
`gc bd ready` by topology. The mayor's October 30 inventory found 94/96 query
lines using `gc bd ... ready`, 92 with labels; the other two used raw bd. These
are dated observations, to be re-inventoried before a conversion.

## Package dispositions

| Existing record | Disposition and reason |
| --- | --- |
| C1 `ga-c6sekn4.3` | Retire the old enumerator/preflight contract. Main's rig census and importer provide the product path under the current upstream model. |
| C2 `ga-c6sekn4.4` | Retire the old combined carry/proof/clear/backup contract. Main provides per-rig verified import and resumable backup/clear; the old matrix is not claimed equivalent. |
| C3 `ga-c6sekn4.5` | Retire the old status/recovery/runbook member. Current status, doctor, import retry and runbook supply the operator path; the unmeasured scale target belongs to the separate upstream staging/benchmark work. |
| Fixture build `ga-c6sekn4.2` | Already closed as build/review handoff. Preserve its exact source history; mark this package retired, without rewriting it as shipped. |
| Fixture deploy `ga-pnbot2y` | Cancel as obsolete. Preserve the old gate evidence and source. No rerun, PR, waiver or PASS is requested. |
| Fixture-main condition `ga-zumsvdr` | Retire the prerequisite after native decision guards are read back on all R scopes. This does not claim the old fixture landed. |
| Migration rollup `ga-si8uwhb` | Retire the unused rollup; never arm or create a replacement migration PR. |
| Migration-main condition `ga-qn2fdl5` | Retire the old-rollup condition, naming #8486 as upstream coverage. No claim that the old rollup landed. |
| R1-R4 `ga-zspenhc.1`–`.4` | Keep open, natively blocked on `ga-735syud`. Their old `Build:` paragraphs are historical until the ruling is applied. Reuse or retire these same scopes after that decision. |
| Ready rollup `ga-mr6z8uh` | Keep open/inert behind the same decision and its existing member guards; do not arm it. |

## One remaining architecture decision

`ga-735syud` revises the authoritative ruling `ga-e61klgk`: add label/scope
selectors to `gc ready`, or convert pack queries to its supported metadata/route
contract. The architect must compare current source and resolved pack inventory,
preserve required selection/order/limit/exclusion/claim semantics, and map each
existing R scope and deployment gate to keep, shrink or retire. PM chooses no
new technical contract here.

The architect records a revision on the existing ruling, then reroutes this
same open decision/application bead to PM. It stays open until PM applies the
ruling to the existing R scopes and pack guards. That ordering prevents an
architect handoff from accidentally releasing old implementation notes.

## Dependency and operational guards

Before the obsolete fixture condition closes, same-store `blocks` edges from
all four R scopes, the ready rollup and `ga-a2vew07` to `ga-735syud` must be
read back. The legacy fixture condition then ceases to be their release signal.

HQ pack work `gm-0qegy3` remains blocked by HQ `gm-cyic3i`; those foreign IDs
are provenance pointers in this rig, not substitute cross-store blockers.
`ga-21lx6fz`/HQ `gm-cyic3i` still require an accepted deployed contract and
unsplit parity. `ga-p0gntwc` still requires the actual pack landing and observed
dispatch. Their exact flag-specific exits must be revised after the ruling,
not silently satisfied by a newer binary. `ga-a2vew07` remains open behind the
remaining operational guards and the new decision.

No split is enabled. No live storage census/rehearsal, configuration edit,
database/schema change, installation, restart, migration or cutover was run by
PM. Operator initiative `gm-f42vxh` and its journal retain ownership of rollout,
rehearsal and turn-off readiness; the missing reverse slice is not waived here.

## Handoff and validation

The validator is told to retire only the three obsolete migration scopes and
their own attached workflows. The deployer is told to cancel only the fixture
gate and preserve its source/evidence; other conditions and gates are unchanged.
The architect receives the one decision bead. Mayor receives a verified account
of which records closed, which remain open and the one remaining decision.

This is a planning reconciliation, not a code review. Evidence consists of main
ancestry, primary PR readback, current source/runbook comparison, native edge
readbacks and final bead/mail verification. No old acceptance-suite PASS or
live-data safety result is inferred from it.
