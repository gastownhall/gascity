# Changelog

All notable changes to Gas City will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- Managed Dolt config now emits listener backlog and connection-timeout keys.
  Existing managed cities may see a `dolt-config` doctor warning until
  `gc dolt restart` or the next managed server start regenerates
  `dolt-config.yaml`.
- In bead-backed pool reconciliation, `scale_check` output is now documented
  and enforced as additive new-session demand. Assigned work is resumed
  separately; custom checks that previously returned total desired sessions
  should return only new unassigned demand.

## [1.0.0] - 2026-04-21

First stable release. Between `v0.15.1` and `v1.0.0` the project received 610
commits across 1,273 files (+303,902 / −46,437) from the core team and 12
community contributors. See the GitHub release page for the full narrative.

### Added

- `gc reload [path]` — structured live config reload. Failures keep the previous
  runtime config active instead of silently degrading.
- `gc prime --strict` — turns silent prompt/agent fallback paths into explicit
  CLI failures for debugging.
- `rig adopt` — adopt existing rigs without a full rebuild.
- Provider-native MCP projection for Claude, Codex, and Gemini, with multi-layer
  catalog resolution and projected-only `gc mcp list`.
- Per-agent `append_fragments` so prompt layering is configurable through the
  supported config and migration paths.
- Wave 1 pass over orders and dispatch runtime — store resolution, dispatch
  surfaces, rig-aware execution, and verifier coverage.

### Changed

- **Session model unified.** Declarative `[[agent]]` policy/config is now
  cleanly separated from runtime session identity; session beads are the
  canonical runtime projection.
- **Pack V2 is the active layout.** Bundled packs use `[imports.<name>]`;
  builtin formulas, prompts, hooks, and orders come from the builtin `core`
  pack. V1-era city-local seeding is retired.
- `gc init` is back on the pack-first scaffold contract. Agent and named
  sessions belong in `pack.toml`; machine-local identity stays in
  `.gc/site.toml`; `city.toml` keeps workspace/provider state.
- `gc import install` is now the explicit bootstrap path for importable packs.
- `gc session logs --tail N` returns the last `N` entries (matches Unix `tail`
  convention) instead of the old compaction-oriented behavior.
- Supervisor API migrated to Huma/OpenAPI; Go client regenerated; dashboard SPA
  restored.
- Order "gates" renamed to **triggers**.

### Fixed

- Startup proofs for hook-enabled providers — correct startup prompt delivery,
  no duplicate `SessionStart` hook context, no replay of startup prompts on
  resumed sessions.
- Managed Dolt hardening: recovery, transient failures, health probes,
  runtime-state validation, and late-cycle macOS portability fixes (start-lock
  FD inheritance, path canonicalization, `lsof` reachability, PID confirmation,
  portable `sed` parsing).
- Pack V2 tmux startup regression where large prompt launches could silently
  fall back to the known-broken inline path.
- Custom provider option defaults now fail early instead of silently degrading.
- Beads storage core quality pass — cache recovery, close-all fallback
  semantics, watchdog reconciliation cadence, dirty-cache fallback reads.
- Long tail of session lifecycle, wake-budget, and pool identity fixes.

[Unreleased]: https://github.com/gastownhall/gascity/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/gastownhall/gascity/releases/tag/v1.0.0
