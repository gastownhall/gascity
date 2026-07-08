---
title: Runtime-partial discipline
description: Treating a failed runtime-liveness observation as "I could not tell" instead of "nothing is running", mirroring the store-partial guard.
---

The bead-store side already distinguishes a partial/failed read from a real
"no rows" answer: `storeQueryPartial` threads through the session reconciler and
gates every destructive arm (close-as-orphaned, drain-ack stop, pending-create
rollback) so a degraded store never causes a healthy session to be torn down
(`cmd/gc/session_reconciler.go`, search `storeQueryPartial`).

The runtime side had no equivalent. A tmux-liveness observation that FAILED
(server briefly unreachable) was indistinguishable from the fact "no sessions
exist", so a brief blip drove the reconciler to drain/close healthy pool slots.

## Landed (this PR)

- `runtime.ErrRuntimeUnavailable` sentinel in `internal/runtime/runtime.go` —
  the runtime-side analogue of a partial store read. Callers dispatch on it with
  `errors.Is`.
- `internal/runtime/tmux/state_cache.go` `tmuxFetcher.FetchState`: an
  unreachable server (`ErrNoServer`) now returns `ErrRuntimeUnavailable`
  (wrapping the original cause) instead of an empty *success*. `refresh()`
  therefore preserves the cache's last-known-good until the existing `staleTTL`
  cliff, so a brief outage no longer collapses `IsRunning` to false. This is the
  highest-leverage single point on the **liveness** path: the reconciler's
  `IsRunning` / `ObserveLiveness` reads all flow through `StateCache`, so
  protecting the observation source shields that whole path at once, bounded by
  `staleTTL` (30s default). The wrapped error still satisfies `isNoServerError`,
  so the ~20 existing `ErrNoServer` absorbers are unaffected.

### Still exposed: the `ListRunning` sites

The `FetchState` fix shields the `StateCache.IsRunning` liveness path, but NOT
`Provider.ListRunning` (via `Tmux.ListSessions`), which still returns
`(nil, nil)` on `ErrNoServer` — an empty *success*, not a partial signal. Three
reconciler-facing sites call `ListRunning` destructively on that empty result:

- `cmd/gc/city_runtime.go:960` — pool `on_death` hooks. On a full tmux outage
  every pool slot vanishes from the empty listing at once, so the tick fires the
  user's `on_death` command for EVERY pool slot: a false death storm.
- `cmd/gc/city_runtime.go:1899` — provider swap on config reload.
- `cmd/gc/city_runtime.go:3466` / `:3478` — shutdown (and the force-shutdown
  late-async-start re-list) session listing.

All four `IsPartialListError` guards at those call sites already exist (verified
in-tree), but none fires today because `ListRunning` returns a nil error on
`ErrNoServer`. The clean completion path is doc-only from here: emit the
EXISTING `PartialListError` from `Provider.ListRunning` / `Tmux.ListSessions` on
`ErrNoServer` (arm 6 below), which activates all four guards with no new
plumbing — the `on_death` storm is arm 4.

### Bounded behavior change (maintainer, please confirm)

Genuine session ends evict from the cache immediately via `Stop()` /
`EvictSession`, so they are NOT masked. The one residual: an **externally**
killed **last** session (killed outside `Stop`, which also makes tmux exit-empty
and return `ErrNoServer`) is reported running from last-known-good for up to
`staleTTL` before the cliff clears it. This is the intended trade — a bounded
cleanup delay in an edge case, versus draining every pool slot on a blip — but
it is a real behavior change and is called out here for explicit sign-off.

## Follow-up arms (not yet threaded)

Even with the source-level fix, each of these destructive arms should read a
`runtimeQueryPartial` signal and defer, mirroring the `storeQueryPartial`
branches, for the window AFTER `staleTTL` (when the cache legitimately goes
empty but the runtime is still just unreachable). Do them one at a time, each
with a `beadReconcileTick`-level test that asserts the arm defers under a
partial runtime observation:

1. **state_cache staleTTL cliff** — after `staleTTL`, `currentState()` returns an
   empty snapshot (`state_cache.go` ~line 148). Expose a `Degraded()`/partial
   status so consumers can distinguish "empty because unreachable" from "empty
   because idle", instead of silently reporting all-not-running.
2. **heal-to-asleep slot-free** — `cmd/gc/session_reconciler.go` heal path
   (`healStateWithRollback`, ~line 1692): a `!providerAlive` observation drives a
   running session toward asleep/closed. Gate with `!runtimeQueryPartial`.
3. **orphan close / drain-advance false-complete** — the `!desired` orphan branch
   (`session_reconciler.go` ~1537) and drain completion: an empty/negative
   observation must not advance a drain to "complete" or close a pool bead as
   orphaned when the runtime query was partial.
4. **on_death storm** — the death handler that fires when a session is observed
   gone: suppress the death cascade when the observation was runtime-partial.
5. **pre-start orphan fail-open** — `cmd/gc/session_wake.go` (~line 552,
   `if err != nil { running = false }`): a failed reachability probe currently
   falls open to "not running"; it should treat `ErrRuntimeUnavailable` as
   partial and defer.
6. **`Tmux.ListSessions` / `Tmux.HasSession`** (`internal/runtime/tmux/tmux.go`
   ~993-1018): these still return `nil,nil` / `false,nil` on `ErrNoServer` for
   their (tmux-internal) callers. They are not on the reconciler liveness path
   (that path is `list-panes` via `FetchState`), so they were left alone here;
   surface `ErrRuntimeUnavailable` from them too for consistency once a consumer
   needs it, auditing each internal caller to preserve today's absorb behavior.

The plumbing to get a per-tick `runtimeQueryPartial` to the reconciler arms
(optional provider interface via type-assert, like `LivenessObserver`, plus a
`Liveness.RuntimePartial` field) is the shared prerequisite for 1-5; it is the
load-bearing design step and should be reviewed on its own before the arms are
converted.
