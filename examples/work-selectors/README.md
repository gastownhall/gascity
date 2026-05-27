# Typed Work Selectors Example

This example demonstrates a public custom pack that uses
`scale_check_query`, `work_selector`, and `gc work claim` instead of shell-local
Beads queue logic.

## Quickstart

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
gc work claim --agent demo/work-selectors.triager --assignee demo-worker --json
```

The `triager` agent's controller demand selector and worker selector are the
same predicate. The controller counts open, unassigned tasks routed to the
rig-qualified agent identity, and the worker claims the same first matching
task.
