---
title: Operate GitHub Actions Red-Streak Monitors
description: Mental model, enrollment recipe, forbidden assumptions, and investigation playbook for the generic GitHub Actions red-streak episode mechanism.
---

This runbook is for contributors enrolling a scheduled GitHub Actions
workflow into red-streak tracking, and for whoever picks up an open
`ci-nightly-red-streak` episode bead. It covers the generic mechanism only —
`[[github.run_monitor]]` config, `gc github runs evaluate`, and the episode
beads it creates. You're in the right place if a bead with label
`ci-nightly-red-streak` landed on your hook, or if you need to point the
mechanism at a new workflow.

> **Rules of thumb** — skip to [Investigate an open episode](#investigate-an-open-episode)
> if you have a bead in hand right now. Come back for the mental model after.

## Mental model

One `[[github.run_monitor]]` config entry watches one scheduled workflow.
`gc github runs evaluate` polls that workflow's run history from GitHub,
walks it newest-to-oldest, and asks one question: has the monitor's own
`aggregate_job` failed for at least `threshold` consecutive runs? Crossing
the threshold creates exactly one durable episode bead per monitor; staying
below it, or recovering, never does anything destructive — recovery is
recorded as evidence, but a human always closes the episode.

```
[[github.run_monitor]] config entries
        |
        v
gc github runs evaluate  --monitor <name> [--dry-run]
        |
        v
GitHub run history for owner/repo@workflow_file
        |
        v
EvaluateHistory: filter to `event`, filter to after `activated_after`,
walk newest -> oldest counting consecutive aggregate_job conclusions
        |
        +-- red streak >= threshold, no open episode  -->  create episode bead
        |                                                   (labels, route, rig: all from config)
        +-- open episode already exists                -->  update it (streak depth,
        |                                                    or recovery evidence)
        +-- below threshold, nothing open               -->  no bead action
```

Nothing here is specific to any one workflow, repository, or role. The
routing target, the rig that owns the episode bead, the label, and the
priority are all supplied by the config entry — the monitor itself contains
no hardcoded role name.

### Why the aggregate job, not the run's own conclusion

`EvaluateHistory` always resolves the conclusion of the job named by
`aggregate_job`, and never reads the workflow run's own top-level
conclusion. This is load-bearing, not a simplification: a run's headline
conclusion can disagree with its own aggregate job's verdict (a workflow can
report `cancelled` at the run level while its aggregate job clearly recorded
`failure`, and vice versa). Reading the run-level conclusion would silently
miss real streaks or manufacture false ones. If you're ever tempted to "just
check `Run.Conclusion`" while extending this package, don't — read the named
job's own conclusion, exactly as `resolveAggregateConclusion` in
[`internal/redstreak/history.go`](https://github.com/gastownhall/gascity/blob/main/internal/redstreak/history.go)
already does.

### Config fields

| Field | Required | Default | Meaning |
|-------|----------|---------|---------|
| `name` | yes | | Stable monitor identity used by episode metadata and diagnostics. |
| `owner` / `repo` | yes | | The GitHub repository whose runs are polled. |
| `workflow_file` | yes | | Workflow file name (e.g. `nightly.yml`). |
| `aggregate_job` | yes | | The job name whose own conclusion is the authoritative verdict for a run. |
| `activated_after` | yes | | RFC3339 timestamp; runs started at or before this are ignored. |
| `threshold` | no | 3 | Consecutive red aggregate runs required before an episode is created. |
| `rig` | yes | | The Gas City rig that owns episode beads for this monitor. |
| `route` | yes | | Operator-supplied route target for episode beads (e.g. `gascity/investigator`). |
| `priority` | no | P1 | Episode bead priority. |
| `event` | no | schedule | Workflow trigger event to filter runs by. |
| `notify` | no | | Session/mail recipients for episode notifications. |

Full reference: [GitHubRunMonitor config fields](/reference/config#githubrunmonitor).

## Enroll a new monitor

Add one `[[github.run_monitor]]` entry per workflow. This example enrolls
two real monitors exactly as configured for this repository's own Mac
Regression and Nightly workflows:

```toml
[[github.run_monitor]]
name = "mac-regression-red-streak"
owner = "gastownhall"
repo = "gascity"
workflow_file = "mac-regression.yml"
aggregate_job = "Mac regression summary"
activated_after = "2026-07-02T00:00:00Z"
threshold = 3
rig = "gascity"
route = "gascity/investigator"
priority = "P1"

[[github.run_monitor]]
name = "nightly-red-streak"
owner = "gastownhall"
repo = "gascity"
workflow_file = "nightly.yml"
aggregate_job = "Nightly summary"
activated_after = "2026-07-26T00:00:00Z"
threshold = 3
rig = "gascity"
route = "gascity/investigator"
priority = "P1"
```

Three things to get right when enrolling a new monitor:

1. **`aggregate_job` must already exist as a real job name** in the target
   workflow file. If the workflow doesn't yet have a single job that stands
   for the whole run's pass/fail verdict, add one first — enrolling before
   the job exists guarantees a data-contract error on every evaluation (see
   [Do not read a DataContractError as a tracker bug](#do-not-read-a-datacontracterror-as-a-tracker-bug)).
2. **`activated_after` must be on or after the aggregate job's own rollout
   date**, not just the monitor's own enrollment date. If the aggregate job
   didn't exist yet in runs after your chosen boundary, those runs will
   report zero matches for the job name and trip a false data-contract
   error instead of being cleanly skipped.
3. **`route` must be something the city can actually route work to.** The
   mechanism does not validate that a route target exists or is staffed; it
   only writes whatever string you give it.

Trigger an evaluation with `gc github runs evaluate`. Use `--monitor <name>`
to evaluate one monitor in isolation, and `--dry-run` to preview without
mutating any episode bead.

## What NOT to do

### Do not close an episode before recovery is confirmed

Episodes never auto-close — recovery is recorded as metadata on the still-open
bead, but closing is always a human decision. Closing early is not merely
premature: a closed episode is invisible to the mechanism's own lookup for
"the current open episode for this monitor" ([`firstOpenRedStreakEpisode`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/cmd_github_runs.go)
only considers non-closed beads). If the workflow goes red again after an
early close, the next evaluation creates a **new** episode bead rather than
reopening the old one, splitting one incident's history across two beads.

### Do not assume editing `route` re-routes an already-open episode

`gc.routed_to` is written only when an episode is created; every later
update to that episode leaves it untouched. Changing `route` in config only
affects episodes created after the change. To move an already-open episode,
use the hand-edit escape hatch below.

### Do not backdate `activated_after` earlier than the aggregate job's own rollout

Runs from before the aggregate job existed have no job by that name. If
`activated_after` lets those runs into the evaluated window, they report
zero matches for `aggregate_job` and the monitor reports a data-contract
error instead of cleanly ignoring pre-rollout history.

### Do not read a DataContractError as a tracker bug

A data-contract error means `aggregate_job` matched zero or more than one
job in some evaluated run — never that the red-streak tracker itself is
broken. The fix belongs in the workflow YAML (rename/dedupe the job) or in
the monitor's own config (fix `aggregate_job`/`activated_after`), not in
`internal/redstreak`.

## Sanctioned escape hatches

### Re-route a stuck episode

```bash
bd update <episode-id> --set-metadata gc.routed_to=<new-route>
```

Safe to do at any time: `updateRedStreakEpisode` never rewrites
`gc.routed_to` on its own, so a hand-edit sticks for the rest of the
episode's lifetime.

### Recover from closing an episode too early

```bash
bd reopen <episode-id> --reason "streak resumed, continuing prior incident"
```

Reopen before the next `gc github runs evaluate` tick runs. A reopened
episode is visible to `firstOpenRedStreakEpisode` again, so the next tick
updates it in place instead of creating a duplicate. If a duplicate was
already created before you reopened the original, close the duplicate and
cross-reference it from the reopened original.

### Preview an evaluation without mutating anything

```bash
gc github runs evaluate --monitor <name> --dry-run
```

### Evaluate just one monitor

```bash
gc github runs evaluate --monitor <name>
```

## Investigate an open episode

1. **Read the episode's own metadata and description for the source run.**
   `bd show <episode-id> --json` — the metadata carries
   `redstreak.workflow_file`, `redstreak.run_id`, `redstreak.first_run_id`,
   `redstreak.first_run_url`, `redstreak.aggregate_conclusion`, and
   `redstreak.consecutive_count`; the free-text description carries the
   "Latest run" and "First run" URLs.
2. **Open the linked run and inspect the aggregate job specifically** —
   not just the run's headline status, which (per [Why the aggregate job](#why-the-aggregate-job-not-the-runs-own-conclusion))
   can disagree with it.
3. **Fix the underlying workflow issue** the normal way for your
   repository. This mechanism only detects and tracks streaks; it has no
   opinion on the fix.
4. **Interpret recovery evidence before closing.** Once the aggregate job
   has gone green for `threshold` consecutive runs, the still-open episode
   gains `redstreak.recovery_ready=true` and `redstreak.recovery_seen_at`
   (RFC3339). This is evidence for a human to review, never a signal the
   mechanism acts on by itself.
5. **Close only after you can point at green runs, not just at the
   metadata.** Confirm the streak yourself in the GitHub Actions UI, then:
   ```bash
   bd close <episode-id> --reason "confirmed N consecutive green runs of <workflow>, see <run URL>"
   ```
6. **Validate recurrence behavior the first time you close one of these.**
   Re-run `gc github runs evaluate --monitor <name> --dry-run` and confirm
   it reports no new episode while the workflow stays green. If the
   workflow goes red again later, expect a **new** episode bead unless you
   proactively `bd reopen` the closed one first (see the escape hatch
   above).

## References

- Code: [`internal/redstreak/history.go`](https://github.com/gastownhall/gascity/blob/main/internal/redstreak/history.go) — `EvaluateHistory`, the aggregate-job streak walk.
- Code: [`internal/redstreak/types.go`](https://github.com/gastownhall/gascity/blob/main/internal/redstreak/types.go) — `Run`, `Job`, `Evaluation`, `DataContractError`.
- Code: [`cmd/gc/cmd_github_runs.go`](https://github.com/gastownhall/gascity/blob/main/cmd/gc/cmd_github_runs.go) — `gc github runs evaluate`, episode create/update/routing.
- Code: [`internal/config/github_run_monitor.go`](https://github.com/gastownhall/gascity/blob/main/internal/config/github_run_monitor.go) — `GitHubRunMonitor` schema, defaults, validation.
- Reference: [GitHubRunMonitor config fields](/reference/config#githubrunmonitor).
- Reference: [gc github runs evaluate CLI](/reference/cli#gc-github-runs-evaluate).
