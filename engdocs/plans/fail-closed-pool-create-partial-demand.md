# Plan: fail-closed pool create gate for partial demand reads

> Owner: `gascity/pm` - Created: 2026-06-23
> Sources: designer handoff `ga-4qbgqf`; architecture root `ga-01yukx`

## Goal

Turn the completed architecture and design review into builder-ready work
packages that stop pool session over-spawn when demand reads are partial.

The incident-critical path is the coupled A/B/C fix:

- block fresh pool creates when the affected template has a partial demand
  read, without blocking reuse or resume paths;
- narrow partial-retained wake counts to confirmed alive capacity plus valid
  in-flight create claims;
- unblock stale creating-bead rollback so expired create claims clear and stop
  inflating desired pool capacity.

Fix D and Fix E are tracked as separate follow-up slices. They must not delay
the incident-critical A/B/C path.

## Work breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-4qbgqf.1` | Add fail-closed partial demand pool create regression tests | `gascity/builder` | `ready-to-build` |
| `ga-4qbgqf.2` | Build partial-demand create gate for pool session planning | `gascity/builder` | `ready-to-build` |
| `ga-4qbgqf.3` | Build partial-retention narrowing and stale-create rollback path | `gascity/builder` | `ready-to-build` |
| `ga-4qbgqf.4` | Build cold-wake singleton eligibility guard follow-up | `gascity/builder` | `ready-to-build` |
| `ga-4qbgqf.5` | Build provider-health registry gate follow-up | `gascity/builder` | `ready-to-build` |

## Dependency graph

```text
ga-4qbgqf.1
  -> blocks ga-4qbgqf.2
  -> blocks ga-4qbgqf.3

ga-4qbgqf.2
  -> blocks ga-4qbgqf.4
  -> blocks ga-4qbgqf.5

ga-4qbgqf.3
  -> blocks ga-4qbgqf.4
  -> blocks ga-4qbgqf.5
```

`ga-4qbgqf.2` and `ga-4qbgqf.3` are both part of the coupled A/B/C fix and
should land together unless the builder finds a smaller independently safe
sequence. `ga-4qbgqf.4` and `ga-4qbgqf.5` are intentionally blocked on both
core implementation beads so they remain follow-ups.

## Acceptance summary

### `ga-4qbgqf.1`

1. A failing regression proves a partial pool demand read sets
   `DesiredStateResult.PoolScaleCheckPartialTemplates[template] == true`.
2. The same regression proves no new pool session create plan or new desired
   session appears when the affected template has no reusable alive capacity.
3. Active and awake sessions for the affected template remain desired and are
   not drained.
4. `state=creating` with `pending_create_claim=true` remains retained as
   in-flight capacity after the retainable narrowing.
5. Stale creating beads roll back within one reconciler tick after the narrow
   alive-on-partial guard allows rollback.
6. Existing partial-read tests continue to pass.
7. `go test ./...` and `go vet ./...` pass.

### `ga-4qbgqf.2`

1. `agentBuildParams` carries `poolScaleCheckPartialTemplates` as a
   package-private field documented as assigned after `evaluatePendingPoolsMap`.
2. `buildDesiredState` assigns the field before pool desired session
   realization can create sessions.
3. `selectOrPlanPoolSessionBead` refuses only the fresh-create path when the
   affected template is partial; reusable and resume paths remain eligible.
4. The fresh-create refusal releases the reserved slot before returning.
5. The sentinel error text is
   `pool session create skipped: demand read partial`.
6. `realizePoolDesiredSessions` logs/skips the sentinel distinctly with
   `(partial demand read, fresh create blocked)`.
7. The `selectOrCreatePoolSessionBead` wrapper path propagates or skips the
   sentinel without swallowing unrelated errors.
8. `go test ./...` and `go vet ./...` pass.

### `ga-4qbgqf.3`

1. `scaleCheckPartialSessionRetainable` counts `active` and `awake` sessions
   directly.
2. The `isPendingPoolCreate` fallback is preserved for valid in-flight creates.
3. `state=creating` and `state=start-pending` no longer count solely because
   of state.
4. `discoverSessionBeadsWithRoots` uses a narrower alive-on-partial guard for
   stale-create rollback blockers.
5. Broader non-create preservation behavior remains unchanged.
6. Tests prove stale creating beads stop inflating retained count after
   rollback, while fresh `pending_create_claim=true` creates remain retained.
7. `go test ./...` and `go vet ./...` pass.

### `ga-4qbgqf.4`

1. Starts only after the A/B/C fix beads close and a verification note confirms
   stable fail-closed create behavior.
2. The guard is limited to the approved cold-wake probe tier and does not
   silently constrain general uncapped pool behavior.
3. Tests cover guarded/opt-in behavior and existing multi-session pool
   compatibility.
4. Lands in a PR separate from A/B/C.
5. `go test ./...` and `go vet ./...` pass.

### `ga-4qbgqf.5`

1. Starts only after the A/B/C fix beads close.
2. Implements the lower-urgency provider-health registry gate as a separate
   Fix E slice.
3. Tests cover healthy providers being allowed and unhealthy/unavailable
   providers being blocked at the intended boundary.
4. Lands separately from A/B/C and separately from Fix D unless a maintainer
   explicitly combines the follow-up work.
5. `go test ./...` and `go vet ./...` pass.

## Handoff notes

- Tracker import was a no-op: no `tracker-to-beads` command or sibling tracker
  skill is installed in this worktree.
- No additional design bead is needed; `ga-4qbgqf` is the completed designer
  review.
- The A/B/C fix must not gate at `realizePoolDesiredSessions` entry because
  that would block resume requests with proven assigned work.
- Do not change `scaleCheckPartialSessionPreservable` unless a failing test
  proves the contract changed. Preservation and retained wake count are
  separate concepts in this design.
- If `ga-4qbgqf.4` cannot stay within the approved cold-wake probe constraint,
  stop and create a `needs-architecture` escalation before broadening behavior.

## Risks

- Blocking reuse/resume during partial reads would strand legitimate work; the
  fresh-create guard must sit after reuse paths.
- Counting stale `creating` or `start-pending` sessions as retained wake demand
  can recreate the over-spawn ratchet.
- Merging Fix D or Fix E with A/B/C could slow the incident-critical path and
  mix defense-in-depth work with the production runaway fix.
