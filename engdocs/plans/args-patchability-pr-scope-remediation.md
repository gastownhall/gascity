# Args Patchability PR Scope Remediation

Owner: `gascity/pm`
Created: 2026-06-09
Source beads: `ga-q7y8lr`, `ga-1xxum0`, `ga-qfr6ri`
PR: https://github.com/gastownhall/gascity/pull/3256

## Goal

Unblock deploy for the args patchability fix by restoring a single-feature
release scope. The accepted feature is `ga-qfr6ri` acceptance path A: make
`args` patchable through `[[rigs.patches]]` and `[[patches.agent]]` with
documented full-replace semantics.

## Context

The reviewer passed commit `3acd0198e8ac0f3b5dade9dcf49d6f10029af543` on
`builder/ga-0mhj1r`, and CI plus targeted tests passed. The deploy gate failed
criterion 7 because the same PR also contains an independently shippable,
unused exported overlay lint helper in `internal/overlay/lint.go` and
`internal/overlay/lint_test.go`.

The deploy unblock is not a new architecture decision. The release package
must either remove that helper from PR #3256 or split it into its own separately
scoped work before deploy.

## Work Packages

| Bead | Route | Label | Acceptance focus |
|------|-------|-------|------------------|
| `ga-q7y8lr.1` | `gascity/builder` | `ready-to-build` | Update PR #3256 or its branch so the deploy candidate contains only the args patchability feature, docs, generated artifacts, and tests required for `ga-qfr6ri` acceptance path A. |
| `ga-q7y8lr.2` | `gascity/deployer` | `needs-deploy` | After builder hands off a focused, reviewed candidate, rerun the standard deploy gate and route the merge request only if the gate passes. |

## Dependency Graph

The deploy rerun depends on the builder scope fix. No design work is required,
and no validator-owned test authoring is required for the deploy unblock.

## Acceptance Notes

The builder package must preserve the already reviewed args behavior:

- `AgentOverride.Args` and `AgentPatch.Args` support nil keep-existing,
  empty-slice clear, and populated-slice full-replace behavior.
- The field-sync and apply coverage tests continue to cover the new args
  fields.
- OpenAPI, generated client, schema, and config documentation remain in sync
  if the focused diff requires those artifacts.
- `internal/overlay/lint.go` and `internal/overlay/lint_test.go` are absent
  from the deploy candidate unless they are tied to a separate accepted bead.

The deploy package must record gate evidence and report the PR URL and result
back to mayor. Merge authority remains operator, mayor, or mpr only.
