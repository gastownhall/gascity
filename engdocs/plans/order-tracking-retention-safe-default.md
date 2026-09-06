# Order Tracking Retention Safe Default

Source PM beads: `ga-tjp87g`, `ga-0op21n`, `ga-wwqr33`
Parent architecture bead: `ga-58rpni`
Owner: `gascity/pm`
Created: 2026-06-07
Priority: P1 for Gas City controller/operator work; P2 for upstream bd coordination

## Goal

Closed order-tracking beads should not accumulate indefinitely in cities that
do not run the gastown maintenance pack. The controller must prune old closed
order-tracking beads safely, operators must see clear advisory signals when
backlog exists, and bd maintainers need a separate index request for the label
scan cost that remains even after the backlog is bounded.

## Context

The architecture bead `ga-58rpni` established that the existing 7d retention
default is safe only when paired with the hard retain-10-per-order floor. The
bug is that the default is not exercised automatically unless a maintenance
order is installed. The designer handoff split the work into:

- `ga-tjp87g`: controller retention watchdog and bounded sweep variant.
- `ga-0op21n`: startup warning, `gc doctor` advisory, and config comment fix.
- `ga-wwqr33`: upstream bd index recommendation for label-table scans.

The index track corrects an earlier assumption: label-filtered bead queries use
`labels` and `wisp_labels` table subqueries, not `LOWER(title) LIKE`.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-tjp87g.1` | As an operator, closed order-tracking beads are pruned automatically by the controller | `ready-to-build` -> `gascity/builder` | none |
| `ga-0op21n.1` | As an operator, order-tracking retention health is visible at startup and in gc doctor | `ready-to-build` -> `gascity/builder` | `ga-tjp87g.1` |
| `ga-wwqr33.1` | As a maintainer, bd maintainers receive an actionable index request for order-tracking label scans | `ready-to-build` -> `gascity/builder` | none |

## Acceptance: `ga-tjp87g.1`

The builder bead is complete when:

1. Tests are written or updated first for the bounded cross-store retention
   sweep, watchdog interval guard, deletion-budget behavior, and stderr
   reporting.
2. The bounded sweep deletes at most 100 closed order-tracking beads total
   across all stores per call.
3. Budget exhaustion returns the partial deleted count with nil error.
4. The existing retain-10-per-order floor and TTL cutoff remain enforced by the
   sweep path.
5. `CityRuntime` runs order-tracking retention at most once every 15 minutes,
   resolves policy from config, uses all order-tracking sweep stores, reports
   store/sweep errors, and reports a pruned count when deletions occur.
6. `dispatchOrders` runs stale open recovery first, closed-bead retention
   second, nudge/mail cleanup third, then order dispatch.
7. Focused `cmd/gc` tests pass, then `go test ./...` and `go vet ./...` are
   clean.

## Acceptance: `ga-0op21n.1`

The builder bead is complete when:

1. Tests are written or updated first for the startup count threshold, capped
   count formatting, non-blocking count errors, doctor thresholds, and doctor
   registration.
2. `gc start` performs a bounded closed order-tracking count after the existing
   orphaned-tracking startup sweep using the already-open sweep store.
3. Startup warns only when the bounded result exceeds 100 and startup continues
   if the count query fails.
4. Startup warning wording matches `ga-0op21n`, including the retention
   watchdog behavior, config hint, and `gc order sweep-tracking` manual cleanup
   hint.
5. `gc doctor` includes an advisory-only `order-tracking-retention` check:
   OK with count below 500, Warning at or above threshold, Warning for
   store/list errors, `CanFix=false`, and `WarmupEligible=false`.
6. `BeadPolicyConfig.DeleteAfterClose` documentation reflects the
   policy-specific 7d `order_tracking` default and does not add a misleading
   field-level schema default.
7. This bead ships after `ga-tjp87g.1`, so operator messaging is true when it
   is exposed.

## Acceptance: `ga-wwqr33.1`

The builder bead is complete when:

1. An upstream `gastownhall/beads` issue or PR is prepared with the corrected
   query shape: label filters use `labels` and `wisp_labels` table subqueries.
2. The request identifies `hq.labels(label, issue_id)` and
   `hq.wisp_labels(label, issue_id)` as the critical covering indexes.
3. Secondary recommendations are included for `hq.issues(status)`,
   `hq.issues(status, created_at)`, `hq.wisps(status)`, and
   `hq.wisps(status, ttl_ts)`.
4. The request explains why switching Gas City to `gc.order_run_name` metadata
   filtering would regress performance because JSON path predicates are not
   indexed without generated columns.
5. The request references `ga-wwqr33` and notes that `ga-tjp87g.1` reduces
   accumulation but does not remove steady-state label-scan cost.
6. No Gas City query restructuring is included for this bead.
7. The upstream URL or PR URL is recorded in bead notes and mailed to mayor.

## Dependency Graph

`ga-tjp87g.1` -> `ga-0op21n.1`

The startup warning and doctor advisory depend on the watchdog implementation
so the operator-facing message is accurate at ship time. The upstream bd index
coordination can proceed independently and must not block the Gas City watchdog.

## Out Of Scope

- Removing or weakening the retain-10-per-order ledger floor.
- Adding a field-level `jsonschema` default for all `BeadPolicyConfig`
  instances.
- Replacing label filters with metadata JSON filters in Gas City.
- Requiring the gastown maintenance pack for basic SDK retention safety.
- Changing `gc order sweep-tracking` manual cleanup semantics.

## Risk

The critical risk is inaccurate operator signaling: warnings must not claim
automatic pruning unless the watchdog is present. The dependency from
`ga-tjp87g.1` to `ga-0op21n.1` keeps that ordering explicit. The second risk is
scope bleed from the bd index analysis back into Gas City query code; the index
bead is intentionally framed as upstream coordination only.
