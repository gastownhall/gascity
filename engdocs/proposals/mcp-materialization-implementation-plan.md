---
title: "MCP Materialization — Implementation Plan"
---

Companion to `engdocs/proposals/mcp-materialization.md`. Breaks MCP
projection into reviewable slices that each end at a natural `/review-pr`
boundary.

## Slice 1: Foundations

Goal: create the neutral MCP model, validator, and effective resolver before
touching runtime delivery.

Files:

- `internal/materialize/mcp.go` or similar new package for:
  - neutral server structs
  - parser and filename/name validation
  - template expansion
  - relative command resolution
  - canonical normalization
  - effective shared/agent-local catalog resolution
- `cmd/gc/cmd_mcp.go`
- `cmd/gc/cmd_mcp_test.go`
- new unit tests for schema rules, duplicate detection, precedence, and
  normalization

Scope:

- discover shared + agent-local MCP definitions
- resolve precedence across city, rig, explicit imports, implicit imports, and
  bootstrap
- surface projected-only `gc mcp list` semantics at the model level
- no provider writes yet

Review boundary:

- run `/review-pr` on the cumulative diff once the model, resolver, and CLI
  behavior compile and tests pass

## Slice 2: Provider Emitters and File Ownership

Goal: project the canonical model into provider-native surfaces with atomic
writes and ownership semantics.

Files:

- MCP materialization package emitters for:
  - Claude `.mcp.json`
  - Gemini `.gemini/settings.json`
  - Codex `.codex/config.toml`
- shared file-write/lock helper in `internal/fsys` or nearby if needed
- new tests for:
  - atomic `0600` writes
  - adoption backup behavior
  - semantic preservation of sibling Gemini/Codex settings
  - empty-set cleanup
  - managed-subtree replacement

Scope:

- provider-family selection via `ResolvedProvider.Kind`
- per-target OS-level locking
- adoption snapshot + marker
- semantic rewrite of JSON/TOML sibling content
- diagnostics redaction rules for errors from the emitter layer

Review boundary:

- run `/review-pr` once the emitters are wired and covered by unit tests, before
  integrating them into startup/session flows

## Slice 3: Runtime Integration

Goal: hook provider projection into the same stage-1/stage-2 lifecycle as
skills.

Files:

- hidden command such as `cmd/gc/cmd_internal_project_mcp.go`
- `cmd/gc/build_desired_state.go`
- `cmd/gc/skill_integration.go` or a sibling MCP integration file
- session/runtime gating sites
- supervisor/start integration points
- tests for:
  - supported vs unsupported runtime/workdir topologies
  - stage-2 pre-start injection ordering
  - shared-target conflict handling
  - last-good-state preservation and unhealthy-target behavior

Scope:

- eager stage-1 reconcile
- stage-2 per-session projection
- serialized writer use from both controller and pre-start command
- startup failure semantics
- claimant-aware cleanup

Review boundary:

- run `/review-pr` once runtime integration is complete and the feature works
  end-to-end in unit/integration coverage

## Slice 4: Drift, Doctor, and Acceptance Coverage

Goal: complete the operational contract.

Files:

- drift/fingerprint integration in desired-state/runtime config
- `internal/doctor` MCP checks
- acceptance/integration tests for:
  - provider projection
  - adoption backup
  - redaction
  - shared-target conflicts
  - cleanup when claims disappear
  - unsupported runtimes/providers
  - Codex repo-local acceptance gate

Scope:

- restart on projected MCP drift
- report-only doctor checks with actionable context
- target-specific `gc mcp list` output
- high-signal migration/adoption warnings

Review boundary:

- run `/review-pr` before PR creation

## Docs and Cleanup

After Slice 4, update docs that still describe MCP as list-only:

- `docs/reference/cli.md`
- `docs/packv2/doc-agent-v2.md`
- `docs/packv2/doc-pack-v2.md`
- `docs/guides/migrating-to-pack-vnext.md`
- any conformance/skew docs that still mention deferred projection

These doc updates can merge with the final slice unless the code review
suggests splitting them.
