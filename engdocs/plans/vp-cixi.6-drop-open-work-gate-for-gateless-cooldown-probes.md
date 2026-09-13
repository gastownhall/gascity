# vp-cixi.6 — Drop the open-work gate for gate-less cooldown probes

Bead: vp-cixi.6 (child of EPIC vp-cixi). Root cause (GAP D from vp-cixi.5):
`provider-health-probe` is a pure cooldown probe that tracks NO beads, yet the
dispatcher runs two open-work gates for it per tick (`hasOpenTracking` then
`hasOpenWork`), each issuing `bd list` / `bd query` reads against Dolt and
bounded by `orderGateTimeout` (8s). On store slowness the gate times out and
`gateFailClosed` SKIPS the order every cycle → the provider-health cache goes
stale → fail-closed provider health → failover can't pick claude2/anything.
Confirmed live: 60–90+min gaps between probe runs despite a 10m interval.

A/B/C (PR #357) shipped a mitigation (hysteresis + wider TTL) that absorbs the
skips. This plan is the deeper fix in gc-core (gascity): let an order OPT OUT
of the open-work gates entirely, since they are meaningless for orders that
consume no bead work. Same class as the decision-sweep rig-enum starvation.

## Approach

Add an order-level opt-out flag `no_work_gate` (Go `Order.NoWorkGate`). When
`true`, the dispatcher skips BOTH open-work gates (`hasOpenTracking` and
`hasOpenWork`) for that order — no `gateOpenWorkBounded` call, no Dolt reads,
no fail-closed skip, no gate-timeout backoff. The order still respects its own
trigger (cooldown interval) and per-order exec timeout; single-flight for these
orders is naturally bounded by the cooldown interval + the synchronous
tracking-bead the dispatcher creates before launch (which still happens).

Why a new flag, not reusing `Idempotent`:
- `Idempotent` changes semantics to fail-OPEN on timeout (may double-dispatch).
  A gate-less probe should not even *enter* the gate — it must not depend on a
  Dolt read completing inside 8s, and it should never emit
  `order.gate_timeout_fail_open`. The two properties ("safe to re-run" vs
  "consumes no bead work") are distinct; conflating them would make a probe's
  dispatch contingent on store health, which is exactly the bug.

**Layer split (2026-07-05 re-plan, after the executor flagged the cross-layer
blocker in T-003):** the gc-core mechanism — flag + decode + dispatcher
gate-skip + end-to-end regression — all ships from **gascity** (T-001/T-002,
done green; T-003 parse test + T-004 docs remain, both gascity-self-contained).
Activating the flag on the live probe is a **separate deployed-pack-layer
edit** (`packs/voxist-city/orders/provider-health-probe.toml`, which is NOT a
file tracked in gascity or any voxist rig repo — see S-1). Per the
one-bead-per-plan / plan-ends-at-the-boundary rule, that activation is sling
S-1, not a micro-task. The gc-core PR is valuable and mergeable on its own:
it ships the opt-out mechanism + tests; the pack flip turns it on for the one
order that needs it today.

## GDPR data-flow impact

No data-flow impact. The change alters *dispatch scheduling* (which gate
checks run for an order), not what data the order reads, processes, or
persists. `provider-health-probe` already runs unchanged; `NoWorkGate` only
removes the redundant open-work gate reads (which themselves only read bead
*status*, not personal data) from its dispatch path. No new PII is accessed,
stored, transmitted, or retained. No data subject, retention period, or
export path is affected.

## MDR Class I traceability

No-op. This change is entirely outside the voxmemo→voxist-api clinical
documentation pipeline (chain-of-evidence from microphone to exported
clinical note). It touches gc-core dispatch scheduling only; no clinical
recording, transcription, or documentation artifact is created, modified, or
re-routed. The heading is retained per the writing-plan discipline so an
auditor sees the explicit consideration.

## Micro-tasks (TDD, red/green/refactor/commit-on-green)

### T-001 — Order model: add `NoWorkGate` field + TOML decode + validation guard
- **acceptance**: `TestOrderNoWorkGateParsed` — an order TOML with
  `no_work_gate = true` decodes to `Order.NoWorkGate == true`; default is
  `false`. Also assert `Validate` accepts it (no extra constraint).
- **files**: `internal/orders/order.go` (struct field + `orderDecode` +
  `normalized()`), test in `internal/orders/order_test.go`.
- commit: `feat(orders): T-001 add Order.NoWorkGate opt-out flag — green at TestOrderNoWorkGateParsed`

### T-002 — Dispatcher: skip both gates when `NoWorkGate`
- **acceptance**: `TestOrderDispatchNoWorkGateSkipsGatesUnderStoreDelay` —
  with `orderGateTimeout` shortened and a store whose gate queries sleep past
  it, an order with `NoWorkGate: true` STILL dispatches (creates a tracking
  bead) and records ZERO gate query calls; a plain order is skipped as before.
- **files**: `cmd/gc/order_dispatch.go` (guard the two `gateOpenWorkBounded`
  call sites + the `gateBackoffActive` short-circuit so backoff is irrelevant
  when the gate never runs).
- commit: `fix(dispatch): T-002 skip open-work gates for NoWorkGate orders — green at TestOrderDispatchNoWorkGateSkipsGatesUnderStoreDelay`

### T-003 — Parse test: provider-health-probe-shaped order opts out (gascity-self-contained)
- **acceptance**: `TestProviderHealthProbeOrderOptsOutOfWorkGate` — a TOML
  literal shaped like the shipped provider-health-probe order (cooldown
  trigger, 10m interval, real `exec` + 120s timeout) WITH `no_work_gate = true`
  parses via `orders.Parse` to `Order.NoWorkGate == true` and passes
  `Validate`; the same TOML WITHOUT the flag parses to `false`. Mirrors the
  `TestOrderNoWorkGateParsed` house style (inline TOML literal → `Parse`,
  no cross-tree file reach).
- **files**: test only — `internal/orders/order_test.go`. No source change
  (the field/decode landed in T-001).
- **why not edit the shipped pack here**: the live
  `packs/voxist-city/orders/provider-health-probe.toml` is a *deployed
  city pack-store artifact*, not a file tracked in the gascity repo or in any
  voxist rig repo reachable from this session (voxist-platform/api/web have
  zero matching tracked files; voxist-city itself is not a git repo; the only
  local copy is `.pr337-fetch/`). Reaching across into that layer from a
  gascity test would be machine-specific and break CI. The pack-flag flip is
  filed as the cross-layer sling below (S-1) and is NOT a micro-task in this
  plan — per the one-bead-per-plan / plan-ends-at-the-boundary rule.
- commit: `test(orders): T-003 provider-health-probe-shaped order opts out of work gate — green at TestProviderHealthProbeOrderOptsOutOfWorkGate`

### T-004 — Docs: order-author guide note for `no_work_gate`
- **acceptance**: a docs note exists describing when to set `no_work_gate`
  (pure probes/sweeps that track no beads) and the warning that it disables
  single-flight protection, so the order must be self-idempotent or
  interval-bounded. The note names provider-health-probe as the canonical
  example and cross-references sling S-1 (the pack flip that activates it).
- **files**: `docs/tutorials/07-orders.md` (nearest order-author reference —
  `docs/guides/orders.md` does not exist; the base-`Order` TOML fields are
  documented in the orders tutorial's "Duplicate prevention" section, next to
  the sibling `idempotent` flag, which is the correct conceptual neighborhood
  for the third gate-behavior option).
- commit: `docs(orders): T-004 document no_work_gate opt-out`

## Cross-layer slings (plan ends at the boundary)

The gc-core mechanism (T-001/T-002) ships from gascity. Activating it for the
live probe requires flipping the flag on the deployed pack — a different layer
(pack store, not a git repo here). That is sling S-1, NOT a micro-task.

### S-1 — Flip `no_work_gate = true` on the deployed provider-health-probe pack
- **owning layer**: deployed city pack store —
  `packs/voxist-city/orders/provider-health-probe.toml` (the order TOML that
  sits next to `packs/voxist-city/bin/provider-health-probe`; see vp-cixi.5
  GAP D for the path). This is a city/pack-layer config edit, not a rig-repo
  code edit; the bead should be filed in whichever rig owns the deployed
  pack tree and routed to that rig's executor.
- **change**: add `no_work_gate = true` to the `[order]` table (one line),
  alongside the existing `trigger = "cooldown"` / `interval = "10m"` /
  `timeout = "120s"`.
- **acceptance**: after deploy, `provider-health-probe` dispatches on its
  10m cooldown even when the Dolt store is slow enough to time out the
  open-work gate for other orders (the #2893 starvation this whole bead
  fixes). Verifiable in `supervisor.log`: no more
  `open-work gate for provider-health-probe timed out ... skipping this order`
  lines, and provider-health cache refresh gaps return to ~10m.
- **sling-ready bead text**:
  ```
  Title: pack: set no_work_gate=true on provider-health-probe (activates vp-cixi.6)
  Body: Flip `no_work_gate = true` on the [order] table of the deployed
  packs/voxist-city/orders/provider-health-probe.toml. This activates the
  gc-core NoWorkGate opt-out (vp-cixi.6, PR <fill>) so the probe stops being
  skipped every cycle when the Dolt store is slow (#2893 dispatch starvation
  -> stale provider-health cache -> fail-closed health -> failover can't pick
  claude2). Depends on vp-cixi.6 merging first (the flag is a no-op without
  the gc-core mechanism). One-line config edit; verify via supervisor.log
  (no more gate-timeout-skip lines for provider-health-probe; cache refresh
  gaps back to ~10m). Same pack layer as the vc-flh.5 probe impl.
  Labels: gc.pack, provider-health, failover. Blocks: none. Blocked-by: vp-cixi.6.
  ```

## Status

- [x] T-001 — Order model: add NoWorkGate field + TOML decode + validation guard   ✅ green at 415429c91
- [x] T-002 — Dispatcher: skip both gates when NoWorkGate   ✅ green at TestOrderDispatchNoWorkGateSkipsGatesUnderStoreDelay (6612e768c)
- [x] T-003 — Parse test: provider-health-probe-shaped order opts out (gascity-self-contained)   ✅ green at TestProviderHealthProbeOrderOptsOutOfWorkGate (bd0ff6b40)
- [x] T-004 — Docs: order-author guide note for `no_work_gate`   ✅ green at docs(orders): T-004 document no_work_gate opt-out (45a3548fa)
- [ ] S-1 (sling, NOT a micro-task) — Flip `no_work_gate = true` on the deployed pack   ← cross-layer, separate bead (file after PR merges)

**Re-plan note (2026-07-05):** original T-003 reached across into the deployed
pack layer (a non-repo artifact) and was unexecutable from gascity. T-003 is
now a gascity-self-contained parse test (house style: inline TOML → `Parse`);
the pack-flag flip moved to sling S-1. T-001/T-002 are unchanged and green.
The gc-core PR (T-001/T-002/T-003/T-004) is mergeable independently of S-1.

**Completion note (2026-07-05):** all four gc-core micro-tasks are green on
branch `gc/vp-cixi.6`. T-003 was a parse-only test (the field/decode landed in
T-001), so it went green on first run — no source change needed, matching the
re-planned "test only" scope. T-004 landed in `docs/tutorials/07-orders.md`
(no `docs/guides/orders.md` exists; the base-`Order` TOML fields live in the
orders tutorial, and the "Duplicate prevention" section is the correct home
next to the `idempotent` sibling). The pack activation (S-1) remains a
separate cross-layer bead to file once this PR merges.
