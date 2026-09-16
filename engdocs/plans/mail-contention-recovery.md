# Mail Contention Recovery Plan

Source beads: `ga-k2rrdv`, `ga-inf8lf`, `ga-bqldr7`
Architecture parent: `ga-31a6yv`
Priority: P1 for `ga-k2rrdv.1`, P2 for acute follow-ons, P3 for cache freshness follow-up

## Goal

Restore reliable `gc mail` behavior under Dolt contention so the mayor's
monitor hook is not blinded by slow inbox scans, repeated session topology
enumeration, or silent fallback to an equally slow local path.

## Work Packages

1. `ga-k2rrdv.1` - As an operator, I can list mail across aliases with one cached message scan
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Parent: `ga-k2rrdv`
   - Acceptance: `beadmail.Provider.Send` creates messages visible under
     a `beads.TierIssues` query with `AllowScan:true`; mail candidate
     enumeration uses one `TierIssues` scan plus in-memory route filtering
     instead of one `TierBoth` list per route; the API inbox path uses an
     optional multi-recipient provider call when supported; tests cover
     single recipient, multiple recipients, empty recipients, duplicate
     route aliases, and read-label filtering.

2. `ga-inf8lf.1` - As an operator, API mail reads reuse a thread-safe session route cache
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Parent: `ga-inf8lf`
   - Acceptance: `newMailProvider` constructs a cached beadmail provider
     for the API path; `NewCached` documentation covers command-scoped and
     long-lived API provider use, including cache lifetime and staleness;
     tests verify cached provider behavior and concurrent access safety;
     targeted package tests pass.

3. `ga-bqldr7.1` - As an operator, slow API mail reads produce a degraded hook notice instead of silent fallback
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Parent: `ga-bqldr7`
   - Acceptance: mail list/count API read sub-operations enforce a
     server-side deadline below the 10s client timeout and return typed
     503 details beginning `store_slow:` when the store is slow;
     `apiErrorFromResponse` maps those responses to non-fallbackable
     `storeSlowError` values exposed through `IsStoreSlowError`; `gc mail
     check --inject` emits the exact degraded notice and exits 0 on
     store-slow errors while non-inject mode surfaces an error and exits
     nonzero; tests cover parsing, fallback behavior, and inject output.

4. `ga-inf8lf.2` - As an operator, cached session routes refresh after session topology changes
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Parent: `ga-inf8lf`
   - Depends on: `ga-inf8lf.1`
   - Acceptance: long-lived API mail providers observe newly started and
     closed sessions without supervisor restart within a documented bounded
     refresh interval or explicit invalidation path; refresh remains
     concurrency-safe and does not restore per-request full session scans;
     tests cover new-session visibility, closed-session removal, and
     concurrent reads around refresh.

## Dependency Graph

`ga-k2rrdv.1`, `ga-inf8lf.1`, and `ga-bqldr7.1` can proceed in parallel
after their designer root beads close. `ga-inf8lf.2` is a follow-up and is
blocked by `ga-inf8lf.1`.

## Guardrails

- `ga-k2rrdv.1` is the highest ROI fix and should ship before broadening
  scope. It removes the direct per-route `TierBoth` scan that exceeds the
  10s client timeout under contention.
- Keep object-model logic in `internal/mail` and API projection behavior in
  `internal/api`; do not duplicate mail routing logic in `cmd/gc`.
- Keep HTTP wire paths typed and Huma-registered. Do not add hand-written
  JSON, `map[string]any`, or `json.RawMessage` wire types.
- `storeSlowError` must remain non-fallbackable for reads. Falling back to
  the local path recreates the same contention problem.
- The `--inject` degraded notice is a user-visible contract for hook
  prompts; preserve the exact text from `ga-bqldr7`.
- Cache freshness in `ga-inf8lf.2` must not block the acute cached-provider
  fix in `ga-inf8lf.1`.
