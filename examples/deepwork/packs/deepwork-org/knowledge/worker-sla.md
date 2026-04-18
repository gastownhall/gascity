# Worker SLA — Deterministic Accountability System

Workers (gt-docker, future GTs) must follow these rules. Violations are tracked
automatically and trigger escalation.

## Claim-to-PR Pipeline (Enforced)

```
Issue assigned (gt-status:pending)
    |
    v  Worker claims (gt-status:claimed) -- clock starts
    |
    |  <= 24 hours: first commit must appear on branch
    |  <= 48 hours: PR must be submitted targeting dev
    |
    v  PR submitted (gt-status:done, needs-review label)
    |
    v  Review + merge by Mayor
```

**If 24h passes with no commit:** Issue auto-unclaimed, escalation comment posted.
**If 48h passes with no PR:** Issue reassigned, worker gets SLA violation.
**After 2 violations:** Worker loses assignment priority.

## Deacon Enforcement (Automated)

The `deacon-worker-sla.sh` script runs every 30 minutes and:

1. Lists all `gt-status:claimed` issues across repos
2. Checks `claimed_at` timestamp (from issue event timeline)
3. If claimed > 24h ago with no branch activity:
   - Posts comment: "SLA WARNING: No activity in 24h. Issue will be unclaimed in 6h."
4. If claimed > 30h ago with no PR:
   - Removes `gt-status:claimed`, adds `gt-status:pending`
   - Posts comment: "SLA VIOLATION: Unclaimed due to inactivity. Reassigning."
   - Sends mesh mail to Mayor with violation report
5. Tracks violations per worker in DoltHub `worker_sla` table

## Closed Issue = Full Stop

When the Mayor closes an issue:
- ALL work on that issue STOPS immediately
- Any open branch for it should be abandoned or deleted
- Any cron job polling for it must be removed
- Continued commits to a closed issue = automatic SLA violation

This is non-negotiable. The Mayor's close is final.

## Cron Hygiene

- Every cron job must be registered in `.gt-mesh/cron-registry.yaml`
- Unregistered crons found during audit are killed immediately
- When an epic/issue is deprioritized, all associated crons die with it
- Workers must not create crons without Mayor approval

## Deprioritization Protocol

When the Mayor deprioritizes work:
1. Issues are closed with "DEPRIORITIZED" comment
2. All crons related to that work are removed from registry
3. Mesh mail sent to affected workers: "STOP work on X"
4. Workers have 1 sync cycle (2 min) to acknowledge
5. Any commit after deprioritization = SLA violation

## Worker Scorecard

Tracked per worker, reported weekly:
- PRs submitted
- PRs merged
- SLA violations
- Average claim-to-PR time
- Issues reassigned due to inactivity

Workers below minimum delivery (3 PRs/week) get flagged.
Workers with 3+ violations in a month get decommissioned.
