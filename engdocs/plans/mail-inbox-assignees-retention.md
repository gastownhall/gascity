# Mail Inbox Assignees And Retention Plan

Source bead: `ga-2znrco`
Architecture parents: `ga-n27ytv`, `ga-3h8dgp`
Design reviewer: `gascity/designer`
Priority: P2

## Goal

Make mail inbox reads scale with the recipient's message set instead of all
open message wisps, and bound historical read-message growth through
configurable production retention.

## Work Packages

1. `ga-2znrco.1` - Builder: add the ListQuery.Assignees contract
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `internal/beads.ListQuery` has `Assignees []string` with
     godoc documenting mutual exclusivity with `Assignee`;
     `ListQuery.Validate` returns an error when both are set; `HasFilter`
     treats `Assignees` as a filter adjacent to `Assignee`; `Matches`
     checks `Assignees` immediately after `Assignee`; tests cover matching
     any listed assignee and the mutual-exclusivity error.

2. `ga-2znrco.2` - Builder: index ListQuery.Assignees in HQStore
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-2znrco.1`
   - Acceptance: `hqTierIndex` candidate selection unions per-route
     assignee index sets when `q.Assignees` is set; an empty union is
     represented as an empty set so intersections return zero candidates
     instead of a full scan; tests cover multi-route hits and routes with
     zero messages returning an empty result, not all open messages; no
     untyped wire or API changes are introduced.

3. `ga-2znrco.3` - Builder: route beadmail inbox through Assignees
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-2znrco.1`, `ga-2znrco.2`
   - Acceptance: `BdStore` maps one `Assignees` value to `--assignee` and
     internally enables `AllowScan` for multi-route fallback before Go
     filtering; `listEphemeral` follows the same single-versus-multi
     behavior; `messageCandidatesAll` sets `q.Assignees=routes` and leaves
     `AllowScan=false` when routes are present; no-route calls remain
     explicit scans; `TestInboxUsesSingleBothTierMessageScanAcrossRoutes`
     asserts `Assignees` equals routes, `Assignee` is empty, `AllowScan` is
     false, `TierBoth` and `Live` remain set, and exactly one `store.List`
     call occurs; the `matchesRecipientRoute` defense-in-depth comment is
     present.

4. `ga-2znrco.4` - Builder: wire mail retention_ttl config
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: `[mail] retention_ttl` is parsed as a Go `time.Duration`
     and wired into HQStore options; zero disables retention for backward
     compatibility; `gc city init` generated `city.toml` includes a
     commented `[mail] retention_ttl` example explaining that `0` disables
     retention, `168h` means 7 days, and `7d` is not a valid Go duration;
     invalid duration values return contextual errors.

5. `ga-2znrco.5` - Builder: purge read message wisps by retention TTL
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-2znrco.4`
   - Acceptance: `HQStore.PurgeExpired`, under the existing store lock and
     `ttlInterval` schedule, deletes only wisp beads where `Type` is
     `message`, `mail.read` equals `true`, and `CreatedAt` is older than
     mail retention TTL; unread or unset `mail.read` messages are never
     purged; retention is disabled at TTL zero; the log line is emitted only
     when count is greater than zero and follows
     `hqstore: purged N read message wisps (retention_ttl=<ttl>)`; tests
     cover old read messages purged, unread messages preserved, zero TTL
     disabled, and log behavior.

## Dependency Graph

- `ga-2znrco.1` blocks `ga-2znrco.2` and `ga-2znrco.3`.
- `ga-2znrco.2` blocks `ga-2znrco.3`.
- `ga-2znrco.4` blocks `ga-2znrco.5`.

## Guardrails

- `Assignee` and `Assignees` are mutually exclusive; reject contradictory
  queries with an error, not a panic.
- Do not remove `Live:true` from the inbox query.
- Do not add multi-value `--assignee` behavior to the bd CLI invocation;
  `BdStore` multi-route behavior is an internal scan-and-filter fallback.
- Preserve the HQStore empty-union guard so zero matching routes do not
  degrade into a full scan.
- Never reap unread messages; require strict `mail.read == "true"`.
- Retention runs inside existing `PurgeExpired` maintenance; no new
  goroutine or scheduler.
