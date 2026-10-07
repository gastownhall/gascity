# Observation commands must not start managed Dolt

PM root: `ga-4v9kuu` (P2, `source:actual-architect`). Origin: operator incident `ga-hbyk8b`, [issue #7170](https://github.com/gastownhall/gascity/issues/7170). This artifact completes requirements and decomposition; it does not establish a runtime fix.

## Goal and product decisions

An operator inspecting a stopped or struggling city must not add managed-Dolt lifecycle load. Observation uses existing connection state and never starts, recovers, restarts, or stops a managed server or its scope watchdog. This applies throughout execution: starting a server and stopping it before exit still violates the requirement.

On a stopped city, commands report unavailable store-backed information. `gc doctor` continues independent checks and identifies the store checks it omitted. On a running city, observations may read the existing server without invoking its health/recovery ladder. Unavailable information must remain distinguishable from an empty healthy inbox or work queue, including partial cached responses.

The explicit observation list is:

| Entry | Boundary |
|---|---|
| `gc status` | Inspection, including its existing API and direct-store paths |
| `gc doctor` | Inspection without `--fix`; independent checks still run |
| `gc hook` | Discovery without `--claim` or `--drain-ack` |
| `gc mail check`, `inbox`, `peek` | Include `check --inject`; preserve unread state |
| Nudge-poller observations | The observation stages; the full delivery loop can mutate work |
| Read-only work-query observations | SDK-owned store preparation and observation; arbitrary user shell side effects are outside this guarantee |

Mutating operations such as hook claims, doctor fixes, mail reads/sends, nudge delivery, sling, bead writes, and order execution retain their separate contracts. No runtime heuristic should guess which operation the user intended.

## Evidence and existing work

The status-triggered Dolt restart is an operator report in #7170, not a PM reproduction. The architect identified existing nonrecovering environment construction and several default recovering call sites, but did not establish every command's path. PM confirmed on source base `1570a81aff811ace497ce8a3da15f7dfb97bdbfd` that hook claims are explicit and that ordinary discovery is a distinct invocation. A nonrecovering environment helper is evidence about one layer only: proxied/subprocess providers can still start infrastructure downstream.

The nudge-poller shutdown fix is already on main: [PR #7264](https://github.com/gastownhall/gascity/pull/7264), merge `9d1f0159f3c1821af479ed85134be8cd3c8a4ca7`, merged 2026-10-07 at 03:18:25Z. Preserve it; do not create a second #6857 implementation.

[Doctor PR #4827](https://github.com/gastownhall/gascity/pull/4827) remains open as of PM intake. PM read head `3d3176a270a00d7a43a892589be949829da483fd`: its guard snapshots server presence, permits the existing checks to start a server, then attempts cleanup on release. It addresses the survivor problem from [issue #4685](https://github.com/gastownhall/gascity/issues/4685); that mechanism alone does not satisfy never-start. The architect must compare overlap before landing. Adoption, replacement, and contributor communication remain the mayor's decisions. Investigation and unrelated paths do not wait for that PR to merge.

`ga-6jzn19` owns the controller's managed-Dolt failure state. Coordinate the common observation/recovery invariant with its architect; do not absorb its health ladder, cooldown, mutex, or recovery-budget work. Neither scope blocks the other.

## Work packages and dependency graph

| Bead | Routing label / target | Deliverable | Prerequisite |
|---|---|---|---|
| `ga-0w1f0t` | `needs-architecture` / architect | Runtime evidence, command/path matrix, unavailable-state and output/exit contracts, shared policy and contributor-PR comparison | None |
| `ga-7id4lg` | `needs-tests` / validator, then builder on the same bead | Meaningful RED, runtime fix, GREEN, regression guard, owning contributor invariant and end-to-end evidence | `ga-0w1f0t` |
| `ga-05pltw` | `ready-to-build` / builder | Existing operator guidance matching the verified behavior | Both preceding beads |

Every child carries `discovered-from:ga-4v9kuu`; the root traces to the operator request. Blocking edges encode the table. The test and implementation scope is one open bead: validator finishes its own molecule, releases its claim, relabels, and slings that same bead to builder. There is no duplicate implementation child.

## Acceptance and verification

1. The architect runs at least one listed command on a named current source-built SHA against an isolated stopped, unregistered city. Record invocation, state, output, exit status, scoped process identities/counts before and after, and lifecycle/spawn evidence during execution. Record already-compliant paths honestly; a source read is not a reproduced failure.
2. The command matrix covers city/rig federation, API/cache and direct fallback paths, native and subprocess opens, and proxied scopes where applicable. Every unverified entry is named before tests/build proceed. Stopped, healthy, unreachable, stale-publication and concurrent legitimate ownership cases have settled contracts, using existing command deadlines.
3. Tests establish zero managed server/watchdog starts or recoveries during an observation, plus unchanged relevant process identities/counts afterward. Existing healthy reads work; unavailable results remain truthful; no observation stops someone else's server. Tests must detect transient starts that a final count would miss.
4. A regression guard fails if an agreed observation path gains a recovering environment or another startup path, including indirectly through a helper. Demonstrate sensitivity with a controlled mutation. Keep meaningful runtime tests; call-count assertions alone do not establish the user behavior.
5. The runtime PR updates its owning `AGENTS.md`/contributor invariant, preserves the existing nudge-stop fix, and records the applicable unit, process, integration, race and repository checks. PR evidence covers the agreed list. Use `Refs #7170`, never `Closes #7170`; the pending mayor/operator issue-link ruling remains authoritative.
6. Operator documentation follows `gascity-docs`, uses verified stopped and healthy examples, and updates existing pages. Edit generated references at their source if needed. Before publishing behavior claims, verify the implementation is on current main or explicitly coordinate joint shipment; a closed build bead alone does not prove landing.

## Risks and scope limits

- A no-recovery flag can still leave indirect provider autostart reachable. Inspect the whole command path and prove behavior at the process boundary.
- Global process counts are noisy on a live factory. Use an isolated owned city, identified processes, and a known test socket; never stop the default tmux server or live factory.
- Existing recovery helpers are shared with mutating/controller paths. The architect owns boundary choices; implementation must avoid changing unrelated lifecycle policy.
- Doctor's unavailable-store behavior can affect exit/JSON compatibility. Settle those details in the prerequisite instead of treating skipped diagnostics as healthy results.
- Contributor overlap is a coordination requirement, not permission for a builder to adopt or supersede PR #4827.

Cold-start/PSI gates, start-loop policy, controller failure-state fixes, new APIs/events/dashboard surfaces, external issue comments, and deployment are outside this plan. Tracker import was a no-op: no tracker skill is installed in the session's skill set. PM completion closes the requirements root after this exact artifact is committed and verified; reproduction and implementation remain downstream work.
