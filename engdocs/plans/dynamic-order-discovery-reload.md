# Plan: dynamic-order-discovery — controller picks up new/changed order files without a full restart

> **Status:** decomposing — 2026-05-12
> **Source beads (merged):**
> - `ga-8klwtl` (P2, BUG) — *controller doesn't pick up newly-added order
>   .toml files without restart.*
> - `ga-g6a57x` (P3, BUG) — *`gc reload` doesn't refresh order
>   definitions (cron orders need separate reload path).*
> **Decomposed into:** 1 builder bead (see Children below)

## Context

The two source bugs are the same root cause from different angles. PM
merged them into one slice because shipping one without the other leaves
the user-visible behavior incoherent.

### What `ga-8klwtl` saw

The user created `/home/jaword/projects/gc-management/orders/daily-mpr-sweep.toml`
at 2026-05-11 20:30 PDT with cron `*/30 * * * *`. Controller PID 3489025
had been running since 2026-05-11 07:52 PDT — well before the file
existed. After overnight:

- `gc order list` and `gc order show daily-mpr-sweep` BOTH show the
  new order (so the on-disk view of orders does include it).
- `gc order check` reports `cron: schedule not matched` (so the
  controller is at least *aware* of the cron expression).
- `gc order history daily-mpr-sweep` shows **no fires** despite ~26
  cron-match windows passing.
- `gc order run daily-mpr-sweep` works manually.

Smoking gun: the cron-firing loop appears to cache the order list at
controller startup. Newly-added `.toml` files are visible via CLI
inspection (probably via a filesystem rescan in the CLI path) but are
not added to the cron evaluation schedule until controller restart.

### What `ga-g6a57x` saw

`gc reload` is documented as *"Reload the current city's config without
restarting the city/controller."* Today the user added new order
`.toml` files and ran `gc reload`. Result: `"No config changes
detected."` followed by `"Reload request could not be accepted because
the controller is busy."` The reload watches `city.toml` only; it does
not watch `orders/`.

### Related observation

`mol-dog-stale-db` (`0 */4 * * *`, system-pack order existing well
before today) ALSO shows no fire history in `gc order history`. This
might be the same bug (the cron-firing loop is broken in general),
or it might be a separate bug (order ran but tracking bead is empty —
which is exactly what `ga-2uizkj` describes). Builder should treat
`mol-dog-stale-db`'s silent fires as a diagnostic during the fix:
if the cron loop is fixed but history is still empty for fires that
*did* happen, the symptom belongs to `ga-2uizkj`, not this slice.

### Why these merge

Either bug, fixed in isolation, leaves the contract incoherent:

- Fix `gc reload` to rescan orders but leave the cron loop's cache
  alone → reload says "OK" but new orders still don't fire.
- Make the cron loop watch the filesystem but leave `gc reload`
  alone → `gc reload` still says "No config changes detected"
  while the controller is, in fact, picking up changes silently.

The user-visible contract is: *"if I drop a new `.toml` under
`orders/`, the controller fires it on the next cron match."* Whether
that's achieved via fsnotify, periodic rescan, or an explicit reload
verb is a builder decision (see "Builder design notes" below), but
the contract has to be coherent across `gc order list`, `gc reload`,
and the cron-firing loop.

## Strategy

Single builder bead. The builder picks one of three implementation
shapes:

1. **Periodic rescan** — every controller tick (or a dedicated cadence
   like once per minute), rescan `orders/` and refresh the in-memory
   order set. Simplest. Costs O(files-in-orders/) per rescan; with
   <50 orders today that's fine.
2. **`fsnotify` watch** — push-driven rescan when `orders/` changes.
   Lower overhead, more code. Builds on `fsnotify/fsnotify` (already a
   transitive dep if any package uses it; verify).
3. **`gc reload` extension** — `gc reload` rescans BOTH `city.toml` AND
   `orders/`. Operator-driven, no background rescan. Lowest controller
   overhead, but the user has to remember to run `gc reload` after
   every file drop — easy to forget.

PM recommends **option 1 (periodic rescan)** as the default with a
minute-or-so cadence: it's the simplest, has no background-thread
lifecycle to manage, and matches the existing controller-tick loop
pattern. If the builder discovers there's already a controller tick
that's a natural attach point, even better.

Whichever mechanism the builder picks, `gc reload` MUST also trigger
an orders rescan as a side effect (current behavior: only rescans
`city.toml`). This makes the "I just dropped a file, fire it now"
operator gesture work and keeps the verb honest.

The controller-busy issue (`"Reload request could not be accepted
because the controller is busy"`) is a separate concern and outside
this slice. Builder may file a follow-up bead if it turns out to
block reload in normal operation — but in the ga-g6a57x repro it
appeared only after the controller had been up for many hours, which
suggests it's an unrelated reload-state-machine concern, not part of
the orders refresh path.

## Out of scope

- **`gc order reload` as a new dedicated subcommand.** Extending
  `gc reload` (or making rescan automatic) is sufficient; a new
  verb is not needed and would split the operator surface area.
- **Controller-busy diagnosis.** Outside this slice. File separately
  if it persists after this lands.
- **Tracking-bead empty-history for `mol-dog-stale-db`.** Belongs to
  `ga-2uizkj` (silent failures). Once that lands and this slice
  lands, the history pane will show real fires.

## Children

| ID            | Title                                                                                                          | Routing label    | Routes to         | Depends on |
|---------------|----------------------------------------------------------------------------------------------------------------|------------------|-------------------|------------|
| `ga-lqdcbh`   | feat(orders): controller rescans `orders/` so new/changed `.toml` files fire without restart; `gc reload` triggers it explicitly (ga-8klwtl + ga-g6a57x) | `ready-to-build` | `gascity/builder` | (none)     |

## Acceptance for the parent (ga-8klwtl + ga-g6a57x)

Met when the builder bead closes and all of the following hold:

- [ ] **The contract.** After dropping a new `.toml` under `orders/`
      with a cron trigger (e.g. `*/1 * * * *`), the controller fires
      it on the next cron-match window without controller restart.
      Empirical test: drop `orders/test-tick.toml` with
      `schedule = "*/1 * * * *"`, wait 90 seconds, `gc order history
      test-tick` shows ≥1 fire entry.
- [ ] **`gc reload` honesty.** Running `gc reload` after dropping a
      new order file:
      - Returns success (not "No config changes detected" when the
        orders set has changed).
      - Forces an orders rescan as a side effect so the new order
        fires on its next cron match.
      - Existing `city.toml`-only reload behavior is preserved.
- [ ] **`gc reload` quiet path.** When neither `city.toml` nor the
      `orders/` tree has changed, `gc reload` still reports "No
      config changes detected" (must not become spammy).
- [ ] **No regression on existing orders.** `mol-dog-stale-db`,
      `dolt-health`, `daily-standup-*`, etc., continue to fire on
      their existing schedules.
- [ ] **Deletion is handled.** If a `.toml` is removed from
      `orders/`, the controller stops scheduling it (with a log
      entry). Builder may choose to require an explicit
      `gc reload` for deletions if periodic rescan makes deletion
      handling awkward — but document the choice in the bead
      close-out.
- [ ] **Tests.** Unit test for the rescan path (mock filesystem
      with add / change / remove). Integration test (with build tag)
      that adds a `.toml` to a live controller and asserts it fires
      within one cron window.
- [ ] **No new `--config` switches or env vars.** This is a fix for
      already-broken behavior, not a new feature.

## Builder design notes

- Order discovery code lives at
  `/home/jaword/projects/gascity/internal/orders/{discovery,scanner,filenames,override}.go`.
  Reload command at `/home/jaword/projects/gascity/cmd/gc/cmd_reload.go`.
  The CLI's `gc order list` apparently does its own scan (since it
  surfaces new files immediately) — this is the existing rescan
  primitive to lift into the controller.
- `internal/orders/scanner.go` is the right level to plug a rescan
  function; current call sites suggest it's already invoked at
  startup. If so, an additional invocation from the controller tick
  is plumbing, not new logic.
- **Avoid global mutable state**. The current order set in the
  controller is probably a map field on a controller struct; protect
  swaps with a mutex or use atomic.Value. Don't introduce a singleton.
- **Don't watch from inside the cron-firing loop.** Watch (or rescan)
  from the controller-tick path, then publish a swap into the
  cron-firing loop's order set. This keeps the cron-firing loop free
  of filesystem syscalls.
- **`mol-dog-stale-db` is currently silent in `gc order history`** —
  builder MUST verify after the fix lands that this order does, in
  fact, fire every 4 hours and shows up in history. If it still
  doesn't show fires, that's evidence the silent-failure bug
  (`ga-2uizkj`) is dominating and the cron-loop is actually fine.
  Note this in the bead close-out either way.

## Verification (for the PM, post-merge)

1. Drop `orders/pm-test-tick.toml` with `schedule = "*/1 * * * *"`
   and `exec = "/bin/true"`. Wait 90 seconds. `gc order history
   pm-test-tick` shows entries. Remove the file. Wait another 90
   seconds. No further history entries.
2. `gc order history daily-mpr-sweep` over 24h shows ~48 fires.
3. `gc order history mol-dog-stale-db` over 24h shows 6 fires
   (every 4h cron).
