---
title: "Removing multi Agent Config"
---

Gas City no longer supports `multi = true` on agents.

## What to change

1. Remove `multi = true` from the agent definition in `city.toml`.
2. Create interactive sessions from that template with:

```bash
gc session new <template>
```

## Old multi-instance beads

If you previously used `gc session new` (formerly `gc agent start`) with the old multi-instance model,
your bead store may still contain open beads with labels like:

- `multi:<template>`
- `instance:<name>`
- `state:running` or `state:stopped`

Those beads are no longer used by Gas City. If you want to clean them up,
close them manually.

Example:

```bash
bd list --json \
  | jq -r '.[] | select(any(.labels[]?; startswith("multi:"))) | .id' \
  | xargs -r -n1 bd close
```

If you never used multi-instance agents before, no cleanup is required.
