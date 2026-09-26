# Plan: retire-dolt — append `[orders] skip` to `city.toml` (ga-37bc9z final slice)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-37bc9z` (closed) — *retire Dolt sql-server
> now that all rigs are on Postgres.*
> **Designer spec:** `ga-08mm01` — design body is the architect's §3.1
> verbatim, designer's confirmation pass against the current tree, and
> a small optional integration-test pin.
> **Sibling work landed:** `ga-9h05hk` (slice 2/5 of `ga-nw4z6`, live-
> session SHOW PROCESSLIST probe + fail-closed) — this is the P0 fix
> that makes `gc dolt cleanup` safe for the one-time operator stop.
> **Decomposed into:** 1 builder bead (see Children below)

## Context

The architect (`ga-37bc9z`) closed out the retire-dolt design with a
single config-only city-tree change: append an `[orders] skip = [...]`
block to `/home/jaword/projects/gc-management/city.toml` naming all
8 dolt-pack orders. The order scanner (`internal/orders/scanner.go:85`)
filters skipped names at config load, so once the block lands and the
controller reloads, `dolt-health` stops respawning the ~840 MB dolt
sql-server.

The designer (`ga-08mm01`) confirmed every pin still holds against the
current tree:

- No existing `[orders]` block in `city.toml` — append is conflict-free.
- All 8 dolt orders still live under `.gc/system/packs/dolt/orders/`.
- `[[orders.overrides]] enabled = false` is **not** the right mechanism
  (dispatcher gap, see ga-37bc9z §4.1). Use `[orders] skip` only.
- `gc dolt cleanup` is now safe for the one-time stop because slice 2/5
  of `ga-nw4z6` (`ga-9h05hk`) shipped the live-session SHOW PROCESSLIST
  cross-check 2026-05-11.

The designer also added a small optional integration test
(`TestCityTomlSkipsAllDoltOrders`, `//go:build integration`) that
asserts the parsed `city.toml` skip list matches the 8 expected names.
This is a nice-to-have — the builder MAY drop it if the project
rejects integration tests against the live `city.toml`. The architect's
§8 manual verification is the authoritative acceptance path.

## Why a single builder bead

Per the design's §9 estimate: **~12 LOC city.toml edit + ~60 LOC
optional test, 1 PR. Smallest design slice in the queue.** The work
is one file (`city.toml`) plus one optional test file. There is no
internal coupling that would split into multiple beads, and the
operator's one-time stop is intentionally NOT scripted (architect §10
guardrail: "Single config file change").

## Children

| ID            | Title                                                                                          | Routing label    | Routes to         | Depends on            |
|---------------|------------------------------------------------------------------------------------------------|------------------|-------------------|-----------------------|
| `ga-jj70gx`   | feat(city.toml): append `[orders] skip` block retiring all 8 dolt-pack orders (ga-37bc9z)       | `ready-to-build` | `gascity/builder` | (none; design closed) |

## Acceptance for the parent (ga-08mm01)

Met when the builder bead closes and all of the following hold (mirrors
ga-08mm01's acceptance list verbatim):

- [ ] `/home/jaword/projects/gc-management/city.toml` has the §3.1
      `[orders] skip = [...]` block appended verbatim (inline comment
      preserved, grouping preserved: `dolt-*` first, `mol-dog-*` second).
- [ ] `gc order list 2>&1 | grep -i dolt` produces no output after a
      reload.
- [ ] EITHER `TestCityTomlSkipsAllDoltOrders` exists under
      `//go:build integration` and passes, OR the operator's manual §8
      verification from `ga-37bc9z` ran successfully and is logged as a
      follow-up comment on the builder bead.
- [ ] `gc bd list --status in_progress` still works against all 5 rigs
      (gm_beads, gascity_ga, projectwrenunity, mcdclient_mc, beads_be —
      all on Postgres).
- [ ] No edits in `.gc/system/packs/dolt/` (upstream-synced).
- [ ] No `[[orders.overrides]] enabled = false` blocks added anywhere.
- [ ] No new `packs/dolt-override/orders/` files.

## Notes for the builder

- **Read `ga-08mm01` in full before any edit.** The TOML block is
  pinned byte-for-byte in §1, including the multi-line inline comment
  above `[orders]`. The grouping (`dolt-*` daemon-maintenance orders
  first, `mol-dog-*` dog-formula orders second) is intentional — do
  not alphabetise.
- **Verify the 8 order names at impl time.** Run
  `ls .gc/system/packs/dolt/orders/` and confirm each of the 8 names
  in the skip list corresponds to a real order file basename
  (without the `.toml` extension). If upstream sync has added or
  removed an order, update the skip list and the test in lockstep
  and call it out in the PR description.
- **Do NOT commit the operator's one-time stop procedure.** The
  procedure in design §2 is for the operator to run manually after
  the commit lands. The builder commits only the `city.toml` change
  (and optionally the test).
- **Reload command name.** The design says
  "`gc city reload` or whatever the in-place reload command is in
  this build." Builder confirms the current command name at impl time
  and mentions it in the PR description if it has shifted.
- **Optional integration test placement.** If the builder elects to
  ship `TestCityTomlSkipsAllDoltOrders`, the design recommends
  `cmd/gc/cmd_city_smoke_test.go` (or wherever the project's
  city-level smoke tests live). Verify the `filepath.Abs("../..")`
  resolution is correct for the chosen home and adjust if needed.
- **Trailing comma policy.** The pinned block includes a trailing
  comma after `"mol-dog-stale-db"`. Go's TOML parser accepts it; if
  the project's `gofmt`-equivalent toml linting rejects, drop it.

## Out of scope (deferred follow-ups)

Per design §"Out of scope" — **do NOT bundle these into the builder
bead**. They are independent of the city.toml edit and should be filed
as fresh `bd create` calls AFTER this slice merges:

1. Cleanup on-disk dolt data dir (`.beads/dolt/`).
2. Remove stale `/home/jaword/projects/gc-management/gascity/.beads/metadata.json`
   (backend=dolt).
3. Close the SDK `[[orders.overrides]] enabled = false` dispatcher gap
   (`cmd/gc/order_dispatch.go:143`).

## Refs

- Design: `ga-08mm01` (PM closes after decomposition)
- Parent architecture: `ga-37bc9z` (closed)
- Sibling P0 fix that makes the operator stop safe:
  `ga-9h05hk` (slice 2/5 of `ga-nw4z6`, closed 2026-05-11)
- Designer handoff mail: `gm-m96pw2`
