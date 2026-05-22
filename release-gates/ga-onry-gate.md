# Release gate: ga-onry — alias-stickiness fix for terminal-ish named-session state (ga-qfgu)

**Bead:** ga-onry (review bead for builder bead ga-qfgu, architecture ga-ue1r)
**Source branch:** `builder/ga-qfgu-1` (fork)
**Source commits:**
- `a6ef3ea3` — `test(session): RED — alias-stickiness gating for stopped/failed-create (ga-qfgu)`
- `e13e2d9f` — `fix(session): release named-session alias on terminal-ish state (ga-qfgu)`

**Verdict:** **FAIL — superseded by merged PR #1579 (ga-ue1r)**

This is the second deployer evaluation of ga-onry. The first (2026-05-02, gate
commit `7540e3dc`) FAIL'd on duplicate-of-in-flight-PR-#1579 grounds and routed
the resolution decision (drop / supersede / cherry-pick test) to mayor. Mayor
did not respond. PR #1579 has since merged, forcing the disposition: drop as
duplicate.

## What changed since the prior gate

PR #1579 (`fix/release-named-alias-on-stopped`, single commit `9dd7cacc`)
**merged 2026-05-03T19:25:34Z** as merge commit `523c8b95` on `origin/main`.
That PR carried the same architecture (ga-ue1r) → builder spec (ga-qfgu) →
predicate fix that ga-onry's branch implements.

Verifying functional equivalence with current `origin/main`:

```
$ git diff origin/main...builder/ga-qfgu-1 -- cmd/gc/session_beads.go
```

The merge-base diff still shows the predicate change as if "new", because
`builder/ga-qfgu-1` was authored against `6b5d9121` and never rebased. But
`git show origin/main:cmd/gc/session_beads.go` confirms the **exact same
Q1/Q2/Q3 gate** is already in main:

- Q1 (HOLD) `state="stopped" + sleep_reason != ""` — present at
  `cmd/gc/session_beads.go:236-238` on main.
- Q2 (RELEASE) `state="stopped" + last_woke_at stale-or-missing` — present
  at `cmd/gc/session_beads.go:243-249` on main.
- Q3 (HOLD) race guard via `parseRFC3339Metadata(last_woke_at) <
  staleCreatingStateTimeout` — same constant, same precedent
  (`city_runtime.go:1530`).
- `state="failed-create"` always RELEASE — present at
  `cmd/gc/session_beads.go:251-256` on main.

Comment wording differs (e.g., "Any non-empty sleep_reason marks a
deliberate-sleep state" vs main's "Deliberate sleep markers (city-stop,
idle-timeout, drained, …) all signal …"). Behavior is identical.

## Marginal incremental coverage in ga-onry's branch

What `builder/ga-qfgu-1` adds beyond what shipped via #1579:

| File | ga-onry adds | Already on main |
|------|--------------|-----------------|
| `cmd/gc/session_beads_test.go` | `TestPreserveConfiguredNamedSessionBead` (15-row table covering all 9 deliberate-sleep `sleep_reason` variants individually) | `TestPreserveConfiguredNamedSessionBead_StateGate` (8-row table covering the same code paths with two representative `sleep_reason` values) |
| `cmd/gc/session_reconciler_test.go` | `TestSyncReconcileSessionBeads_StoppedNamedSessionReleasesAndReopens` (89-line end-to-end close→reopen continuity test) | `test/integration/gc_live_contract_test.go` got 60 lines of integration-tier coverage from PR #1579 |

Both gaps are **marginal at best**:
- The 9-vs-2 sleep-reason table-row count is testing literal values that
  fall through the same `if sleep_reason != ""` branch. The extra rows
  document the contract but don't exercise additional code paths.
- The unit-tier reconciler test asserts close→reopen at a different layer
  than the integration-tier contract test that #1579 shipped. Neither
  layer is missing — the integration test crosses the same boundary with
  more realistic plumbing.

Filing a follow-up to port the unit-tier reconciler test would be
defensible if anyone wants the coverage, but is not deployer-priority and
is **out of scope** for ga-onry.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `gascity/reviewer` recorded PASS in bead notes 2026-05-01 (RED→GREEN verified, all 17 cases passing on patched code). |
| 2 | Acceptance criteria met | PASS (in code) | Predicate at `cmd/gc/session_beads.go:240-258` on `builder/ga-qfgu-1` implements the Q1/Q2/Q3 + failed-create policy from the ga-qfgu spec. |
| 3 | Tests pass | NOT RUN | Did not proceed to test execution — branch is 214+ commits behind main and rebasing is wasted work given the supersession. |
| 4 | No HIGH-severity review findings open | PASS | One non-blocking informational note in reviewer feedback (comment wording about "atomically" at `session_beads.go:247-250`). 0 HIGH. |
| 5 | Final branch is clean | PASS | `builder/ga-qfgu-1` (fork) has only the two intended commits on top of `6b5d9121`. |
| 6 | Branch diverges cleanly from main | **FAIL** | `builder/ga-qfgu-1` is **214+ commits behind `origin/main`**. Cherry-picking onto current main would yield essentially zero net change to `cmd/gc/session_beads.go` (predicate logic already present; only comment-wording differences). The two commits cannot ship in their current form, and there is no implementation contribution left to ship. |
| 0 | Not duplicating merged work | **FAIL** | PR #1579 (commit `523c8b95`, merged 2026-05-03) already shipped the same ga-qfgu fix. |

## Disposition

- **Bead closed** (status=closed) with reason "superseded by PR #1579 (ga-ue1r)".
- **Branch `builder/ga-qfgu-1` left intact** on `fork`. No deletion from the deployer seat.
- **No follow-up bead filed** for the marginal test coverage gap (see "Marginal incremental coverage" above). If anyone wants the unit-tier `TestSyncReconcileSessionBeads_StoppedNamedSessionReleasesAndReopens` ported onto current main, they can file it.
- **Mayor mailed** with the resolution.

## Routing

Prior routing `gascity/deployer → mayor` (gate FAIL → mayor decision)
is closed by ground truth: PR #1579 merged, ga-onry superseded.
