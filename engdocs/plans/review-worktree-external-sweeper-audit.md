# Plan: External `review-*` Worktree Sweeper Audit (`ga-dxqsqa`)

> Owner: `gascity/pm` | Created: 2026-07-28
> Source: architecture handoff `ga-dxqsqa`, derived from incident analysis
> `ga-gfcy8g`

## Why This Work Exists

Two review worktrees disappeared while their gates were running:

- `/var/tmp/review-ga-9x4z1g.1`
- `/var/tmp/review-ga-wnrgld`

The paths had different bead-derived names. The confirmed fixed-name collision
class therefore does not, by itself, explain these two incidents. The second
path also reappeared intact after the failure.

The architecture work in `ga-gfcy8g` already ruled out Gas City's three
in-repo worktree reapers, `gc-tmp-reaper.sh`, `clean-projects-artifacts`, and
the observed `systemd-tmpfiles-clean` schedule. It did not confirm the process
that removed or temporarily displaced either primary path.

An external service, script, cron job, or other same-user agent may sweep
`/var/tmp/review-*` by name pattern. That is a hypothesis, not a root cause.
The separate city-level bead `gm-nseac` tracks a pkill/EXIT-trap class that
could produce a similar symptom and must be checked for overlap.

## Goal

Determine whether a host- or city-level process can account for the two
primary disappearances, preserve evidence strong enough to support the
conclusion, and route only the follow-up justified by that evidence.

## Routing Decision

This is city-scoped work. The investigation begins outside the Gas City repo
and may cross system services, gc-management workflows, agent sessions, and
the city-level `gm-nseac` bead. The rig investigator's contract requires it to
escalate when the evidence trail leaves one rig, while `deep-investigator` is
the configured owner for cross-rig investigations. All three children
therefore use:

- Label: `needs-cross-rig-investigation`
- Metadata: `gc.routed_to=deep-investigator`
- Source: `source:actual-pm`

## Work Breakdown

| Bead | Outcome | Evidence gate |
| --- | --- | --- |
| `ga-dxqsqa.1` | Inventory every host- or city-level mutator of `/var/tmp/review-*` | Exact units, timers, scripts, hooks, prompts, commands, or retained transcript matches are cited; negative searches state their scope and time |
| `ga-dxqsqa.2` | Correlate both disappearances with runtime evidence | A non-destructive artifact tests the external sweeper, sibling cleanup/collision, and pkill/EXIT-trap alternatives without promoting an unsupported mechanism to root cause |
| `ga-dxqsqa.3` | Publish an evidence-backed disposition | The result is reconciled with `gm-nseac`, facts and unknowns are separated, and exactly one justified terminal route is chosen |

## Dependency Graph

```text
ga-dxqsqa.1  inventory host/city mutators
  -> ga-dxqsqa.2  correlate both incidents with runtime evidence
       -> ga-dxqsqa.3  reconcile gm-nseac and publish disposition
```

The inventory gates correlation so runtime evidence is evaluated against a
bounded candidate set rather than an assumed cause. The disposition waits for
both evidence slices so it cannot close the risk from search results alone.

## Acceptance Rollup

1. The audit identifies the scope, time, ownership, trigger, and target rule of
   every candidate host/city mutator it finds.
2. Previously ruled-out mechanisms are cited rather than re-investigated.
3. The two primary incident windows and session/process context are recovered
   as narrowly as retained evidence permits.
4. A durable, non-destructive runtime artifact either identifies the actor and
   action or records a bounded negative observation.
5. External name-pattern sweeping, sibling cleanup/collision, and
   pkill-triggered EXIT cleanup are each classified as supported, disproven, or
   still unconfirmed with citations.
6. The disappearance-and-reappearance of
   `/var/tmp/review-ga-wnrgld` is addressed explicitly.
7. `gm-nseac` is checked directly. Overlapping work is linked or folded instead
   of duplicated.
8. No root cause is declared unless evidence identifies the responsible actor
   and action.
9. The final disposition creates implementation or architecture work only when
   evidence warrants it; otherwise it records a ruled-out or explicitly
   unconfirmed result and the missing observation.
10. `ga-dxqsqa` and `ga-gfcy8g` receive the final disposition and durable
    artifact links.

## Safety And Scope

- The audit is read-only unless the investigator separately obtains authority
  for a controlled diagnostic action.
- It must not delete paths, kill processes, edit services, alter timers, or
  clean up current artifacts.
- The shared scratch-worktree naming fragment is separate work. Random suffixes
  close the confirmed fixed-name collision class but do not answer whether a
  cleaner matches the unchanged `review-` prefix.
- Absence of a current match is bounded negative evidence, not proof that no
  historical or intermittent sweeper exists.
- No speculative implementation bead should be filed merely to change the
  prefix or move scratch paths. Those options become actionable only if the
  evidence supports them.

## Tracker Import

No `tracker-to-beads` skill or supported external-tracker skill is materialized
in this PM session. The assignment and all children are native Gas City beads,
so tracker import is a no-op.
