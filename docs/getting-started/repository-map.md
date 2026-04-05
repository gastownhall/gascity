---
title: Repository Map
description: Where the main subsystems live in the Gas City repository.
---

## Top-Level Layout

| Path | What it contains |
|---|---|
| `cmd/gc/` | CLI entrypoints, controller wiring, runtime assembly, and command handlers |
| `internal/runtime/` | Runtime provider abstraction plus tmux, subprocess, exec, ACP, K8s, and hybrid implementations |
| `internal/config/` | `city.toml` schema, validation, composition, packs, patches, and override resolution |
| `internal/beads/` | Store abstraction and provider implementations used for work, mail, molecules, and waits |
| `internal/session/` | Session bead metadata, wait lifecycle helpers, and session identity utilities |
| `internal/orders/` | Order parsing and scanning for periodic dispatch |
| `internal/convergence/` | Bounded iterative refinement loops and gate handling |
| `internal/api/` | HTTP API handlers and resource views |
| `docs/` | Mintlify docs content, architecture docs, design docs, and archive |
| `examples/` | Example cities, packs, formulas, and reference topologies |
| `contrib/` | Helper scripts, Dockerfiles, and integration support assets |
| `test/` | Integration and support test packages |

## Where to Start

- CLI behavior: start in `cmd/gc/`, then follow the command-specific helper it
  calls.
- Runtime/provider work: start in `internal/runtime/runtime.go` and the
  provider package you are changing.
- Config and pack behavior: start in `internal/config/config.go`,
  `internal/config/compose.go`, and `internal/config/pack.go`.
- Work routing and molecule creation: start in `cmd/gc/cmd_sling.go` and
  `internal/beads/`.
- Supervisor, sessions, and wake/sleep behavior: start in `cmd/gc/`,
  `internal/session/`, and `internal/runtime/`.

For a contributor-oriented package walkthrough, continue to
the Codebase Map (`engdocs/contributors/codebase-map`).
