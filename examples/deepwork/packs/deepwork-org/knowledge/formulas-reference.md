# Formulas Reference — Workflow Templates

Formulas define multi-step workflows as TOML files. Instantiated as molecules with steps that agents execute in order.

## Active Formulas

### mol-polecat-base (v1)
Base polecat lifecycle — never used directly, extended by variants.
Steps: load-context → workspace-setup (placeholder) → preflight-tests → implement → self-review

### mol-polecat-commit (v1)
Extends mol-polecat-base. Direct-commit to base branch, no feature branch.
Steps: ...base steps... → commit-and-push (3-retry push with rebase)
Use case: Simple setups without merge review.

### mol-do-work (v1)
Minimal: read bead → implement → close. No git branching.
Use case: Demos, simple single-agent workflows.

### mol-scoped-work (v2)
DAG-based workflow with explicit scope beads, worktree lifecycle, fail-fast.
Steps: load-context → body scope → workspace-setup → preflight → implement → self-review → submit → cleanup-worktree
Use case: Gas City prototype. Most sophisticated formula.

### cooking / pancakes (v1)
Demo formulas for testing the molecule system.

## Missing Formulas (from reference implementation)

These exist in the gascity reference but not in our town:

### mol-polecat-work (v7)
Feature-branch + refinery handoff variant. The production polecat workflow:
- Creates bead-scoped worktree with feature branch
- Pushes branch to Gitea
- Hands off to refinery for merge review
- NOT a direct commit like mol-polecat-commit

### mol-deacon-patrol (v12)
Deacon patrol loop:
- Check inbox, orphan process cleanup, health scan
- Utility agent health, Dolt health, system diagnostics
- Pour next iteration with exponential backoff

### mol-witness-patrol (v7)
Witness patrol loop:
- Check inbox, recover orphaned beads
- Check polecat health, check refinery queue
- Pour next iteration

### mol-refinery-patrol (v1)
Merge queue processor:
- Find work, rebase, run tests, handle failures
- Merge/push (supports direct and PR strategies)

### mol-idea-to-plan (v2)
Full planning pipeline:
- Draft PRD, 6 parallel review legs, human gate
- 6 design explorations, 3 PRD alignment rounds
- Convert to bead DAG

### mol-shutdown-dance (v1)
3-attempt interrogation (60s/120s/240s) — pardon or execute stuck agents.

## Exec Orders (reference has, we don't)

The reference implementation has "exec orders" — shell scripts run by the controller on cooldown without LLM involvement:

| Order | Interval | Script |
|-------|----------|--------|
| gate-sweep | 30s | Evaluate timer/condition gates |
| orphan-sweep | 5m | Reset beads assigned to dead agents |
| cross-rig-deps | 5m | Convert satisfied cross-rig blocks |
| spawn-storm-detect | 5m | Detect crash-looping beads |
| jsonl-export | 15m | Export Dolt to JSONL git archive |
| reaper | 30m | Reap stale wisps/issues |
| wisp-compact | 1h | TTL-based ephemeral bead cleanup |
| prune-branches | 6h | Clean stale gc/* branches |

We handle some of these via plugins (which require deacon + LLM), but exec orders are more efficient.
