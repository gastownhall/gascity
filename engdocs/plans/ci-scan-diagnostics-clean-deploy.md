# CI Scan Diagnostics Clean Deploy Recovery

Source PM bead: `ga-2l1g7m`
Related review bead: `ga-avsn13`
Reviewed commit: `278675e1c` on `builder/ga-omnkls`
Owner: `gascity/pm`
Created: 2026-06-20
Priority: P2

## Goal

Ship the reviewed CI scan-time fix and dispatcher diagnostics package as a
clean release unit, without bundling unrelated work from `builder/ga-omnkls`.

## Context

Reviewer passed commit `278675e1c` for the completed
`ga-pmhi6g.1.1`/`ga-pmhi6g.1.2`/`ga-pmhi6g.1.3` package:

- collect `GC_WORKFLOW_TRACE` in review-formula CI failure artifacts
- log ralph check-start/check-done duration in the control dispatcher trace
- replace review-formula full-store check scans with targeted bead queries

The deploy gate rejected `builder/ga-omnkls` for release-unit scope. The
branch would ship 13 commits across unrelated CI, extmsg/API/dashboard/docs,
dispatch-wake, testutil, timeout, and prior gate work, and it conflicts with
`origin/main` in `.github/workflows/ci.yml`, `Makefile`, and
`cmd/gc/dashboard/web/src/generated/index.ts`.

The recovery path is a fresh current-main branch containing only the reviewed
CI scan-time fix and dispatcher diagnostics package, followed by a deploy gate
against that branch.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-2l1g7m.1` | ready-to-build: prepare clean branch for CI scan-time fix + dispatcher diagnostics | `gascity/builder` | none |
| `ga-2l1g7m.2` | needs-deploy: gate clean CI scan-time fix + dispatcher diagnostics branch | `gascity/deployer` | `ga-2l1g7m.1` |

## Acceptance Focus

`ga-2l1g7m.1` is complete when builder records a pushed clean branch based on
current `origin/main`, with base commit, head commit, diff scope, and
verification evidence. The branch must include only the reviewed
`ga-pmhi6g.1.1`/`ga-pmhi6g.1.2`/`ga-pmhi6g.1.3` behavior and exclude unrelated
commits from `builder/ga-omnkls`.

`ga-2l1g7m.2` is complete when deployer gates the builder-provided clean
branch, confirms the effective diff remains single-package, opens a scoped PR
on PASS, and routes merge authority to mayor/mpr. On FAIL, deployer records the
exact failed criterion and routes back to PM.

## Dependency Graph

```text
ga-2l1g7m.1 -> ga-2l1g7m.2
```

The deploy retry waits for the clean branch because the previous failure was a
branch scope and origin/main conflict failure, not a rejection of the reviewed
CI scan-time fix itself.

## Out Of Scope

- Reusing `builder/ga-omnkls` as the deploy target.
- Shipping unrelated CI workflow timeout changes, extmsg/API/dashboard/docs
  changes, dispatch-wake work, testutil changes, or prior gate artifacts.
- Reopening architecture decisions from `ga-pmhi6g.1`.
- Merging any PR directly from a rig agent session.

## Tracker Import

No tracker-to-beads skill is installed in this PM worktree, so tracker import
is a no-op for this package.
