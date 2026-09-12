# Plan: Seed Beads DB During gc init / gc rig add

**Source:** [gastownhall/gascity#1670](https://github.com/gastownhall/gascity/issues/1670)
**Priority:** P1 — blocks Tutorial 01 on clean install
**Root bead:** ga-6a84

## Problem

`gc init` and `gc rig add` write `.beads/config.yaml` with `issue_prefix` but never initialize the beads database. Any subsequent `bd` operation — including `gc sling` — fails:

```
gc sling: creating bead: bd create: exit status 1: {
  "error": "database not initialized: issue_prefix config is missing
   (run 'bd init --prefix <prefix>' for a new project, or 'bd bootstrap' to clone an existing remote)",
  "schema_version": 1
}
```

The YAML config is present and correct; the underlying DB is simply never created. This is a clean-install regression — Tutorial 01 is unusable for new users on gc 1.0.0.

## Root Cause

`gc init` and `gc rig add` write `.beads/config.yaml` but do not call `bd init --prefix <prefix>` (or the equivalent in-process library call) to seed the Dolt database. `bd config get issue_prefix` reports "not set" even though the YAML value is correct.

## Work Tree

| Bead | Title | Owner | Blocks |
|------|-------|-------|--------|
| ga-qm61ou | Test: regression test for clean-install tutorial path | validator | ga-sw7144 |
| ga-sw7144 | Fix: seed beads DB with issue_prefix during gc init and gc rig add | builder | — |

**Sequence:** validator writes a failing integration test → builder implements the fix → test passes.

## Acceptance Criteria

1. After `gc init ~/my-city --provider claude` + `gc rig add ~/my-project`:
   - `bd config get issue_prefix` returns the configured prefix (not "not set")
   - `gc sling my-project/claude "..."` succeeds without "database not initialized" error
2. Tutorial 01 (`docs/tutorials/01-cities-and-rigs.md`) passes end-to-end on a clean install
3. Integration test `TestCleanInstallTutorialPath` (authored by validator) is green

## Out of Scope

- Changes to `bd init` itself
- Changing the config schema or YAML format
- Other tutorial paths (Tutorial 02+) — only 01 is affected by this bug
