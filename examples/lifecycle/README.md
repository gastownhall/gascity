# Lifecycle Example

Demonstrates a polecat/refinery pool: polecats create feature branches and
commit work; the refinery merges to main. All agents use bash scripts for
deterministic, reproducible demo recordings.

## Quickstart

### 1. Init from this example

```bash
gc init --from examples/lifecycle ~/demo-city
cd ~/demo-city
gc start
```

### 2. Add a demo repo as a rig

```bash
mkdir ~/demo-repo && cd ~/demo-repo && git init
cd ~/demo-city
gc rig add ~/demo-repo   # lifecycle pack is included automatically
```

### 3. Route a hello-world task to the polecat pool

```bash
gc sling demo-repo/polecat "Write hello world"
```

`gc sling` stamps `gc.routed_to=demo-repo/polecat` on the bead. On the
next patrol tick (<= 10 s) the controller runs `scale_check`, counts one
unassigned routed bead, and starts a polecat session. The polecat creates
a feature branch, commits a file, and hands off to the refinery.

> **Note:** bare `bd create "Write hello world"` does **not** trigger agent
> spawn — `gc sling` is the routing entry point. The polecat's work-finding
> loop filters by `gc.routed_to` metadata; beads without it are invisible
> to the pool.

### 4. Watch it run

```bash
bd show <bead-id> --watch
gc session list
```
