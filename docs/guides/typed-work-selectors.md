---
title: "Typed Work Selectors"
description: Declare portable Beads-backed demand, discovery, and claim predicates for custom packs.
---

Typed work selectors let a pack describe its Beads work queue in TOML instead
of duplicating shell snippets in controller demand checks, hooks, and worker
prompts. A selector is read by:

- `scale_check_query`, which the controller counts for new session demand.
- `work_selector`, which `gc work count`, `gc work next`, `gc work claim`, and
  typed `gc hook` use for worker discovery.

Use both fields together when an on-demand agent should scale from the same
predicate that workers use to inspect and claim work.

## Agent Configuration

```toml
# agents/triager/agent.toml
scope = "rig"
lifecycle = "one_shot"
start_command = "gc work claim --agent {{.Agent}} --json"
min_active_sessions = 0
max_active_sessions = 3

[scale_check_query]
status = "open"
type = "task"
unassigned = true
sort = "created_asc"

[scale_check_query.metadata]
"gc.routed_to" = "{{.Rig}}/{{.Agent}}"

[work_selector]
status = "open"
type = "task"
unassigned = true
sort = "created_asc"

[work_selector.metadata]
"gc.routed_to" = "{{.Rig}}/{{.Agent}}"
```

The two selectors must match after normalization. Gas City rejects a config
where `scale_check_query` and `work_selector` diverge, because otherwise the
controller could start sessions for work the worker cannot see or claim.

## Try It

The public example pack in `examples/work-selectors` includes the same
selector pattern.

```bash
gc init --from examples/work-selectors ~/gc-work-selectors-demo
cd ~/gc-work-selectors-demo
mkdir -p ~/gc-work-selectors-rig
gc rig add ~/gc-work-selectors-rig --name demo

bd create "Review the demo task" \
  --type task \
  --metadata '{"gc.routed_to":"demo/work-selectors.triager"}'

gc work count --agent demo/work-selectors.triager --json
gc work next --agent demo/work-selectors.triager --json
gc work claim --agent demo/work-selectors.triager \
  --assignee demo-worker \
  --set-metadata claimed_by=demo-worker \
  --json
```

`gc work claim` returns the claimed bead and updates it to `in_progress` with
the chosen assignee. If another worker wins first, the command exits non-zero
instead of returning duplicate work.

## Selector Fields

Selectors can filter by status, issue type, excluded type, label, assignee,
unassigned state, parent, metadata, dependency, tier, and sort order. The
default status is `open`; the default tier is regular issues; the default sort
is `created_asc`.

Use `tier = "wisps"` or `tier = "both"` only for ephemeral work that is safe to
consume from the wisp table. Assigned `in_progress` wisps remain visible to the
controller's assigned-work demand so claimed ephemeral work does not create
duplicate replacement sessions.

## Migration Notes

- Keep existing `scale_check` and `work_query` until the equivalent selector
  has been tested with `gc work count` and `gc work next`.
- Do not combine `scale_check` with `scale_check_query`, or `work_query` with
  `work_selector`, on the same agent.
- Prefer metadata routing such as `gc.routed_to={{.Rig}}/{{.Agent}}` for
  custom packs. It keeps selector predicates portable across rig names.
