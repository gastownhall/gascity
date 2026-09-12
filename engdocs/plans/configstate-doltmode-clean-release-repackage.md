# ConfigState DoltMode Clean Release Repackage

Owner: `gascity/pm`
Created: 2026-06-25
Root bead: `ga-m3up75`
Source review bead: `ga-vlidgj`
Source implementation beads: `ga-yqn5py.1.1`, `ga-yqn5py.1.2`

## Goal

Recover the failed ConfigState.DoltMode deploy gate by replacing the
contaminated release branch with a clean origin/main-based release candidate
for the reviewed canonical config contract change only.

## Context

The deploy gate for `ga-m3up75` failed before tests ran. The gate artifact
records failures for:

- Criterion 3, tests pass: not run because release scope failed first.
- Criterion 6, branch diverges cleanly from main: conflict in
  `cmd/gc/build_desired_state.go`.
- Criterion 7, single feature theme: the branch bundled the reviewed
  `internal/beads/contract` DoltMode work with older `cmd/gc` pool/session
  desired-state changes and prior release-gate files.

Gate evidence:
`/tmp/gascity-deploy-ga-m3up75-1782347939/release-gates/ga-m3up75-configstate-doltmode-gate.md`.

Tracker import was a no-op because no tracker companion skill or command was
present in this PM worktree.

## Work Packages

| Bead | Route | Label | Acceptance focus |
| --- | --- | --- | --- |
| `ga-m3up75.1` | `gascity/builder` | `ready-to-build` | Produce a fresh origin/main-based branch containing only the reviewed ConfigState.DoltMode contract scope from `ga-yqn5py.1.1` and `ga-yqn5py.1.2`; record branch, head, base, diff scope, and focused contract test evidence. |
| `ga-m3up75.2` | `gascity/reviewer` | `needs-review` | Review the clean candidate from `ga-m3up75.1`; confirm it faithfully preserves the reviewed behavior from `ga-vlidgj` and excludes unrelated `cmd/gc` and release-gate artifacts. |
| `ga-m3up75.3` | `gascity/deployer` | `needs-deploy` | Run the standard deploy gate on the reviewed clean branch only; open a scoped PR and route merge request to mayor/mpr on PASS; do not merge from an agent session. |

## Dependency Graph

`ga-m3up75.1` -> `ga-m3up75.2` -> `ga-m3up75.3`

`ga-m3up75.3` also directly depends on `ga-m3up75.1` so deployer can verify
the exact branch and SHA recorded by builder.

## Acceptance Notes

The builder handoff must prove the candidate is clean:

- Based on current `origin/main`.
- Diff limited to `internal/beads/contract` behavior needed for
  `ConfigState.DoltMode` and `EnsureCanonicalConfig`.
- No unrelated `cmd/gc/agent_build_params.go`,
  `cmd/gc/build_desired_state.go`, `cmd/gc/build_desired_state_test.go`, or
  prior `release-gates/*.md` changes unless those are already on `origin/main`.
- `git merge-tree --write-tree origin/main HEAD` reports no conflicts.

The reviewer handoff must provide a PASS/FAIL for the clean candidate, not the
old contaminated branch.

The deployer handoff must run only after reviewer PASS, record gate evidence,
open a ConfigState.DoltMode-only PR on PASS, and route merge authority to
mayor/mpr.

## Out Of Scope

- Shipping the older `cmd/gc` pool/session desired-state changes.
- Reusing `builder/ga-4qbgqf.2-partial-demand-create-gate` as the deploy
  candidate.
- PM-authored implementation, tests, PR approval, or PR merge.
