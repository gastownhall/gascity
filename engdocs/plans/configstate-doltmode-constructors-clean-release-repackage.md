# ConfigState DoltMode Constructors Clean Release Repackage

Owner: `gascity/pm`
Created: 2026-06-25
Root bead: `ga-t2x8dv`
Source review bead: `ga-94h3qv`
Source implementation bead: `ga-yqn5py.2.1`

## Goal

Recover the failed deploy gate for the ConfigState constructor DoltMode
server-mode slice by replacing the contaminated release branch with a clean
origin/main-based candidate for the reviewed `ga-yqn5py.2.1` behavior only.

## Context

The deploy gate for `ga-t2x8dv` failed before deployer reran tests. The gate
artifact records failures for:

- Criterion 3, tests pass: not rerun because release scope failed first.
- Criterion 6, branch diverges cleanly from main: `git merge-tree` reported a
  content conflict in `cmd/gc/build_desired_state.go`.
- Criterion 7, single feature theme: the branch bundled unrelated
  pool/supervisor commits and prior release-gate files with the reviewed
  ConfigState constructor DoltMode slice.

Gate evidence:
`/home/jaword/projects/gc-management/.gc/worktrees/gascity/deploy-ga-t2x8dv-gate/release-gates/ga-t2x8dv-doltmode-configstate-constructors-gate.md`.

Tracker import was a no-op because no tracker companion command was present in
this PM worktree.

## Work Packages

| Bead | Route | Label | Acceptance focus |
| --- | --- | --- | --- |
| `ga-t2x8dv.1` | `gascity/builder` | `ready-to-build` | Produce a fresh origin/main-based branch containing only the reviewed ConfigState constructor DoltMode behavior from `ga-yqn5py.2.1`; record branch, base SHA, head SHA, diff scope, merge-tree result, focused tests, `make test`, and `go vet ./...` evidence. |
| `ga-t2x8dv.2` | `gascity/reviewer` | `needs-review` | Review the clean candidate from `ga-t2x8dv.1`; confirm it preserves the accepted behavior from `ga-94h3qv` and excludes unrelated pool/supervisor/release-gate scope. |
| `ga-t2x8dv.3` | `gascity/deployer` | `needs-deploy` | Run the standard deploy gate on the reviewed clean branch only; open a scoped PR and route merge authority to mayor/mpr on PASS; do not merge from an agent session. |
| `ga-t2x8dv.4` | `gascity/validator` | `needs-tests` | Non-blocking follow-up for the `ga-94h3qv` coverage note: add direct unit coverage for the four ConfigState constructor DoltMode defaults without changing the clean deploy candidate scope. |

## Dependency Graph

`ga-t2x8dv.1` -> `ga-t2x8dv.2` -> `ga-t2x8dv.3`

`ga-t2x8dv.3` also directly depends on `ga-t2x8dv.1` so deployer can verify
the exact branch and SHA recorded by builder.

`ga-t2x8dv.4` is intentionally non-blocking. It should use its own branch and
normal review path, and it must not be bundled into the clean deploy candidate
unless PM explicitly retargets it.

## Acceptance Notes

The builder handoff must prove the candidate is clean:

- Based on current `origin/main`.
- Preserves only the reviewed `DoltMode: "server"` constructor behavior for
  managed city, explicit rig, inherited rig, and requested rig endpoint
  ConfigState paths.
- Excludes unrelated `cmd/gc/agent_build_params.go`,
  `cmd/gc/build_desired_state.go`, `cmd/gc/build_desired_state_test.go`,
  pool/supervisor changes, and prior `release-gates/*.md` files unless those
  are already on `origin/main`.
- `git merge-tree --write-tree origin/main HEAD` exits 0.

The reviewer handoff must provide a PASS/FAIL for the clean candidate, not the
old contaminated branch.

The deployer handoff must run only after reviewer PASS, record gate evidence,
open a ConfigState-constructor-DoltMode-only PR on PASS, and route merge
authority to mayor/mpr.

## Out Of Scope

- Shipping unrelated pool/supervisor commits.
- Reusing `builder/ga-yqn5py.2.1-dolt-mode-constructors` as the deploy
  candidate while it remains contaminated.
- Bundling the direct constructor unit-test follow-up into the clean release
  candidate without explicit PM retargeting.
- PM-authored implementation, tests, PR approval, or PR merge.
