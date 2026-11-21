# internal/orders

Order runs: the tracking-bead lifecycle (CreateRun, SetOutcome, CloseRun), the
sweep readers, and the dispatch-deadline hold.

## Dispatch lifetime invariant

Every dispatch — the controller fire path in `cmd/gc/order_dispatch.go` and
manual `gc order run` in `cmd/gc/cmd_order.go` — resolves its effective timeout
ONCE (`newOrderRunDeadline`), persists the absolute deadline on the tracking
bead at creation (`RunOpts.Deadline` → `beadmeta.OrderRunDeadlineMetadataKey`,
UTC RFC3339Nano; a zero value omits the key), and executes against that
instant. The deferred tracking close runs on the OUTER dispatch context, so its
bounded close retries survive the run's own expiry.

Every recovery sweep — the stale watchdog, `gc order sweep-tracking`, and the
boot orphan sweep — holds an unfinished run until its persisted deadline plus
`runCompletionGrace` (one minute; `deadlineHolds`): closing the single-flight
marker mid-run allows duplicate dispatch. A run with a terminal outcome never
holds (only its close recovery remains). A missing stamp never holds. A
present-but-empty, malformed or zero stamp never holds either, and is diagnosed
per bead through `runtimeHelpersLogf`: corruption loses its deadline protection
but must not jam the store, so the bead falls back to the ordinary age rules.

The sweep readers take the sweep instant explicitly
(`StaleOpenRuns(cutoff, now)`, `StaleNotClosedRuns(cutoff, now)`,
`OrphanedOpenRuns(now)`); tests pass synthetic instants, production passes
`time.Now()`.
