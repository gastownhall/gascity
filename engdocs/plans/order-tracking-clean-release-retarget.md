# Order Tracking Clean Release Retarget

Source PM bead: `ga-2aejra`
Related work: `ga-tjp87g.1`, `ga-0op21n.1`, `ga-f0efcs`, `ga-ny8cxu`
Owner: `gascity/pm`
Created: 2026-06-07
Priority: P2

## Goal

Ship the order-tracking retention watchdog and visibility work as clean release
units, without bundling them into PR #3194's independent Makefile/CI timeout
branch.

## Context

The first deploy gate for `ga-2aejra` failed criterion 7, single feature theme.
The source branch `builder/ga-uzijoh-mac-ci-timeout-fix` is already open as PR
#3194 for the unrelated CI timeout fix and also contains order-tracking
retention commits. Deployer correctly opened no PR and pushed no branch from
that gate.

Builder has already created clean branches:

- `builder/ga-tjp87g-order-retention`, tip `0bb03cfb1`, for the controller
  retention watchdog and bounded sweep.
- `builder/ga-0op21n-order-tracking-visibility`, tip `3ccdcd3d4`, for startup
  warning and `gc doctor` visibility. This branch includes the watchdog commit
  as context because `ga-0op21n.1` depends on `ga-tjp87g.1`.

## Work Packages

| Bead | Title | Routing | Dependencies |
| --- | --- | --- | --- |
| `ga-f0efcs` | needs-deploy: order-tracking retention watchdog | `gascity/deployer` | none |
| `ga-ny8cxu` | Review: order-tracking startup warning and gc doctor check | `gascity/reviewer` | none |
| `ga-2aejra` | needs-deploy: order-tracking retention advisories | `gascity/deployer` | `ga-f0efcs`, `ga-ny8cxu` |

## Acceptance: `ga-2aejra`

The deploy handoff is complete when:

1. The deployer does not use `builder/ga-uzijoh-mac-ci-timeout-fix` or PR
   #3194 for order-tracking retention work.
2. `ga-ny8cxu` is closed with reviewer PASS for the clean visibility branch.
3. `ga-f0efcs` has completed the clean watchdog deploy path or the deployer
   records why the visibility deploy can safely proceed as a single clean
   stacked release unit.
4. The release gate runs against `builder/ga-0op21n-order-tracking-visibility`
   or a branch derived from it that contains only order-tracking retention
   work.
5. Any PR opened from this path has a title and body scoped to
   order-tracking retention visibility, not Makefile/CI timeout behavior.
6. On PASS, the deployer routes the merge request to mayor/mpr and records the
   PR URL plus gate artifact on `ga-2aejra`.
7. On FAIL, the deployer records the gate artifact on `ga-2aejra` and routes it
   back to PM without opening a PR.

## Dependency Graph

`ga-f0efcs` -> `ga-2aejra`

`ga-ny8cxu` -> `ga-2aejra`

The watchdog deploy is tracked separately from the visibility deploy because
the visibility messaging depends on the watchdog behavior being present. The
review of the clean visibility branch must happen before deploy because the
previous reviewer PASS was on the contaminated branch.

## Out Of Scope

- Changing implementation code.
- Re-reviewing PR #3194.
- Merging any PR directly from an agent session.
- Reopening already-closed builder beads.

## Risk

The main risk is accidentally shipping retention changes through the existing
CI timeout PR. The corrected route makes PR #3194 explicitly out of scope and
blocks the visibility deploy on the clean review path.
