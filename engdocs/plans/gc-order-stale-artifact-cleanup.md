# Plan: gc-order-stale-artifact-cleanup — remove deprecated dir-based and nested orders artifacts

> **Status:** decomposing — 2026-05-12
> **Source bead:** `ga-b8ecsu` (P4, BUG) — *[gc order] nested
> orders/orders/beads-health/order.toml + deprecated dir-based path
> are file-system artifacts that should be cleaned up.*
> **Decomposed into:** 1 builder bead (see Children below)

## Context

Two suspicious paths exist under `orders/` in the gc-management repo:

1. **`orders/beads-health/order.toml`** — directory-based order layout.
   Triggers a deprecation warning on *every* `gc` command:
   ```
   warning: deprecated order path .../orders/beads-health/order.toml; rename to orders/beads-health.toml
   ```
   The destination (`orders/beads-health.toml`) would shadow the system-pack
   canonical version at `.gc/system/packs/core/orders/beads-health.toml`,
   so the right fix is removal, not renaming.

2. **`orders/orders/beads-health/order.toml`** — bizarre nested path
   (`orders/orders/`), almost certainly produced by a buggy import or
   migration. Same file shape as #1. Source unknown.

The canonical `beads-health` order lives in the system pack (`source:
.gc/system/packs/core/orders/beads-health.toml`). Both local copies are
stale artifacts with no active purpose.

## Strategy

Simple deletion — no code change required. Single `ready-to-build` bead
for the builder to execute.

## Children

| ID         | Title                                                                                                       | Routing label    | Routes to         | Depends on |
|------------|-------------------------------------------------------------------------------------------------------------|------------------|-------------------|------------|
| `ga-yt8ouk` | chore(orders): remove stale orders/beads-health/ dir-based artifact and orders/orders/ nested path (ga-b8ecsu) | `ready-to-build` | `gascity/builder` | (none)     |

(ID filled in by `bd create`; see the matching commit for the final bead-E value.)

## Acceptance for the parent (ga-b8ecsu)

Met when the builder bead closes and all of the following hold:

- [ ] `orders/beads-health/` directory is gone from the repo.
- [ ] `orders/orders/` directory is gone from the repo.
- [ ] The system-pack canonical order remains: `gc order list | grep beads-health`
      shows source `.gc/system/packs/core/orders/beads-health.toml`.
- [ ] No deprecation warning appears when running `gc status` or `gc order list`.
- [ ] `go test ./...` passes.
- [ ] The removals are committed.

## Builder notes

- Use `rm -rf orders/beads-health orders/orders` (non-interactive, per CLAUDE.md).
- Verify before touching: `ls orders/beads-health/` shows `order.toml`; `ls orders/orders/` shows `beads-health/`.
- After removal: run `gc order list` and confirm beads-health is still present (from system pack, not local copy).
- This is a ~3-minute task; no architecture decisions needed.
