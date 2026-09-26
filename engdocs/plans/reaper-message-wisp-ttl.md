# Reaper Message Wisp TTL Plan

Source bead: `ga-6qed8`
Priority: P1

## Goal

Break the maintenance reaper self-escalation loop by adding TTL handling
for ephemeral message wisps and excluding those wisps from the anomaly
threshold that currently triggers more escalation mail.

## Work Packages

1. `ga-6qed8.1` — Tests: reaper covers message-wisp TTL and non-message alert count
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Acceptance: cover `GC_REAPER_MSG_WISP_AGE`, `closed_at=created_at`,
     dry-run safety, and the exact compound alert-count exclusion.

2. `ga-6qed8.2` — As an operator, reaper closes stale ephemeral message wisps by TTL
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-6qed8.1`
   - Acceptance: add `MSG_WISP_AGE`, `MSG_WISP_AGE_H`,
     `DB_MSG_WISPS_CLOSED`, and Step 1a before Step 2 purge with the
     dry-run guard and counter updates.

3. `ga-6qed8.3` — As an operator, reaper alerts exclude ephemeral message wisps and expose closure counts
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-6qed8.2`
   - Acceptance: change Step 5 to count non-message open wisps using the
     compound exclusion, add `msg_wisps_closed` to the Dolt commit
     message, and document the underscore-only quarantine DB convention.

4. `ga-6qed8.4` — Verify: reaper fix clears message-wisp backlog without self-escalation
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Depends on: `ga-6qed8.3`
   - Acceptance: run dry-run and post-fix verification, record backlog
     clearance counters, confirm no mayor mail from message-wisp noise,
     and confirm the dotted orphan database operation was handled or
     explicitly deferred by the maintainer.

## Dependency Graph

`ga-6qed8.1` blocks `ga-6qed8.2`, which blocks `ga-6qed8.3`, which
blocks `ga-6qed8.4`.

## Operational Risk

The one-time operation
`DROP DATABASE \`mcdclient.broken-20260519-0837\`;` is not builder scope.
It requires maintainer confirmation, exact-name verification, and a
low-traffic execution window. PM has flagged this to mayor separately.

## Guardrails

- Step 1a must run before Step 2 purge.
- `closed_at=created_at` is intentional.
- `MSG_WISP_AGE` defaults to `PURGE_AGE`, not `MAX_AGE`.
- Keep the dry-run guard around Step 1a.
- Use `NOT (issue_type='message' AND ephemeral=1)` for the alert count.
- Do not modify `wisp-compact.sh` or the 30-minute order interval.
