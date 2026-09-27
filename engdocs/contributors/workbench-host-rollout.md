# Workbench host rollout

One Workbench per Gas City host, packaged as a checksummed, upgradeable bundle.
No public Internet service is required.

## Package

```bash
scripts/export-workbench-bundle.sh [out-dir]
```

Builds a `workbench-<platform>.tar.gz` from the embedded SPA
(`internal/api/dashboardspa/dist`) plus a `MANIFEST`, and writes a sibling
`.sha256`. `WORKBENCH_PLATFORM` overrides the platform tag.

## Install / upgrade

```bash
scripts/install-workbench-bundle.sh <bundle.tar.gz> [dest]
```

1. **Verifies the sha256** and fails closed before writing anything.
2. Stages the payload and requires `workbench/index.html`.
3. Backs up the previous install to `DEST.rollback.<ts>`, then swaps atomically.
4. Runs `WORKBENCH_HEALTH_CMD <dest>` if set; on failure it **rolls back**.

It only writes the static SPA under `dest`. It never touches Gas City Sessions,
worktrees, agent state, or Beads data, and it **never replaces or alters the
`gc` binary** (including the Mac Graphviz `gc`).

## Control Centre vs standalone fallback

- The Control Centre Workbench reads Beads through the **typed Gas City
  REST/SSE API** — the default path.
- A standalone fallback may read `bd --json` only through a Beadbox-style stdio
  sidecar. In that mode **Beads remains the only durable work-state authority**;
  the Workbench stores no task state of its own.

## Verify

```bash
scripts/test-workbench-bundle.sh
```

Exercises export → install → health → upgrade-rollback, rejects a tampered
bundle before any write, and confirms a failing health check rolls back.
