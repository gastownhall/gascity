---
title: Codebase Map
description: Key packages, ownership boundaries, and the fastest routes through the code.
---

## Core Paths

| Area | Start here | Why |
|---|---|---|
| CLI and controller | `cmd/gc/` | Most user-visible behavior is wired here |
| Runtime provider contract | `internal/runtime/runtime.go` | The provider interface is the lowest-level agent runtime seam |
| Config and packs | `internal/config/` | `city.toml`, pack composition, and override logic live here |
| Work store | `internal/beads/` | Tasks, molecules, waits, and mail all land on the store abstraction |
| Session lifecycle | `internal/session/` | Session identity, wait helpers, and session bead metadata |
| Orders | `internal/orders/` | Order parsing and scanner logic |
| Convergence | `internal/convergence/` | Iterative refinement loops, gates, and convergence metadata |
| API | `internal/api/` | HTTP resources used by dashboards and external clients |

## Common Change Paths

### Adding or changing a CLI command

1. Add or edit the command in `cmd/gc/`.
2. Update generated docs if config or CLI reference changed.
3. Add user-facing coverage in tests or docsync where appropriate.

### Changing runtime behavior

1. Start at `internal/runtime/runtime.go`.
2. Update the provider implementation package.
3. Check session reconciliation in `cmd/gc/` if wake, drain, or metadata
   semantics changed.

### Changing config behavior

1. Update `internal/config/config.go`.
2. Follow through `compose.go`, `pack.go`, and patch/override helpers.
3. Regenerate schema-backed reference docs if the schema changed.

### Changing docs or onboarding

1. Update the Mintlify nav in `docs/docs.json` if the IA changed.
2. Run `make check-docs`.
3. Keep tutorials and landing pages aligned with real commands and real files.
