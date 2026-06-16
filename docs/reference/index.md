---
title: Overview
description: Every Gas City reference page — generated docs, hand-maintained references, specifications, and internals.
---

Reference material answers "what exactly is this?" — commands, config keys,
APIs, formats, and contracts, stated precisely. Tutorials and guides teach;
these pages are for looking things up.

## Generated Docs

Regenerated from code by `go run ./cmd/genschema` and `go run ./cmd/genspec`
(the pre-commit hook keeps them in sync) — edit the Go sources, never these
pages.

| Page | Covers |
|---|---|
| [CLI Reference](/reference/cli) | Every `gc` command, flag, and example |
| [Gas City Configuration](/reference/config) | The `city.toml` schema |
| [Supervisor REST API](/reference/api) | The typed HTTP + SSE control plane |
| [gc events Output Formats](/reference/events) | Exact output formats emitted by `gc events` |
| [Schemas](/reference/schema) | Downloadable machine-readable schema artifacts (OpenAPI, events, city/pack schemas) |

## Hand-Maintained References

| Page | Covers |
|---|---|
| [Gas Town → Gas City Command Map](/reference/gastown-command-map) | The closest `gc`/`bd` equivalent for each `gt` command |
| [System Packs](/reference/system-packs) | Built-in packs bundled with `gc` |
| [Command Execution Trust Boundaries](/reference/trust-boundaries) | Which component runs what, and with whose authority |
| [Exec Session Provider](/reference/exec-session-provider) | The `exec` session runtime provider contract |
| [Exec Beads Provider](/reference/exec-beads-provider) | The `exec` beads backend contract |
| [Tmux Agent Slice](/reference/tmux-agent-slice) | `GC_AGENT_SLICE` systemd scoping for tmux panes |

## Sibling Sections

- **[Specifications](/reference/specs/index)** — authoritative, normative
  specs for file formats and contracts (packs, formulas v1, formulas v2).
- **[Internal](/reference/internal/index)** — internals-grade reference for
  operators and contributors.
