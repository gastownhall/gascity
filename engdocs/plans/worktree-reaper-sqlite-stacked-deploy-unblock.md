# Worktree Reaper And SQLite Stacked Deploy Unblock

Source PM beads: `ga-3u2e95`, `ga-mtgtv7`
Owner: `gascity/pm`
Created: 2026-06-03
Priority: P2

## Goal

Unblock deployment of two reviewed branches without opening a multi-theme PR.

The reviewed deploy branches are stacked:

- `builder/ga-plhh3l-worktree-reap-events` at `6a6a4c964` stacks on the
  coordstore RSS guard work in PR #2998.
- `builder/ga-apo4sa-remove-cgo-sqlite` at `396be53ce` stacks on the
  worktree reaper event branch, which itself stacks on PR #2998.

PR #2998 was checked during PM intake on 2026-06-03 and is still open:
https://github.com/gastownhall/gascity/pull/2998

Tracker import was a no-op because this worktree has no visible
`tracker-to-beads` or sibling tracker skill installed.

## Release Path

Architecture decision `ga-4q6sgc.1` approved a split release topology on
2026-06-03. The stacked PR #3004 branch is replaced by two independent deploy
units, both based directly on current `origin/main`. Neither unit waits for
PR #2998.

1. Deploy Unit B, the SQLite CGO migration, first because it has no expected
   conflicts.
2. Deploy Unit A, the worktree reaper typed-events/runtime unit, after Unit B
   is in review or in parallel if deployer capacity allows.
3. Do not reuse PR #3004. It represented the stacked branch state and should
   be closed or left superseded in favor of two fresh PRs from clean branches.

## PR #3004 Update

A later hook check surfaced `ga-4q6sgc`, a newer deploy failure for PR #3004
at `717935724` on `builder/ga-apo4sa-remove-cgo-sqlite`.

Current GitHub state checked on 2026-06-03 before the architecture decision:

- PR #2998 is open against `main`.
- PR #3004 is open against `builder/ga-pzgem-soak-mem-ceiling`, not `main`.

The deploy gate for `ga-4q6sgc` failed because the PR #3004 stack bundles
independent themes and conflicts in `internal/config/config.go` after the
deployer simulated current `origin/main` plus PR #2998 plus PR #3004.

PM filed `ga-4q6sgc.1` to architecture for a release-topology decision before
creating any builder/validator/deployer split work. Older deploy bead
`ga-k15skh` is superseded by `ga-4q6sgc` because it points at an earlier
commit on the same branch.

Architecture resolved the decision as follows:

- Unit B: source `ga-apo4sa`; branch `builder/ga-apo4sa-clean`; cherry-pick
  `396be53ce`, `cb4d5140a`, `717935724`; allowed paths `internal/beads/`,
  `go.mod`, `go.sum`; excluded paths `internal/benchmarks/`,
  `internal/events/`, `internal/config/`, `cmd/gc/`.
- Unit A: sources `ga-plhh3l`, `ga-xxsd7k`, `ga-xha30e`; branch
  `builder/ga-reaper-events-runtime`; cherry-pick `6a6a4c964`,
  `6b7ed6be9`, `8e421922b`, `9836428c0`, `74a87442b`; allowed paths
  `internal/events/`, `internal/config/`, `cmd/gc/`,
  `docs/schema/openapi.json`, and
  `cmd/gc/dashboard/web/src/generated/`; excluded paths `internal/beads/`
  and `go.mod`.
- Unit A has one expected conflict in `internal/config/config.go`: keep main's
  stage-bead-policy fields and add `AutoReapClosedBeadWorktrees` after
  `AutoRestartOnDrift`.

## Work Packages

| Bead | Route | Label | Acceptance focus |
|------|-------|-------|------------------|
| `ga-3u2e95.1` | Closed | Superseded | PR #2998 is not a prerequisite for either split unit. |
| `ga-3u2e95.2` | `gascity/deployer` | `needs-deploy` | Deploy Unit A from current `origin/main` using all 5 worktree reaper commits; resolve the expected `internal/config/config.go` conflict per `ga-4q6sgc.1`. |
| `ga-mtgtv7.1` | Closed | Superseded | Typed-event-before-SQLite coordination is unnecessary because the units are independent. |
| `ga-mtgtv7.2` | `gascity/deployer` | `needs-deploy` | Deploy Unit B from current `origin/main` using all 3 SQLite commits; verify CGO-free build and exclude non-SQLite paths. |
| `ga-4q6sgc.1` | Closed | Decision complete | Source architecture decision for this split. |

## Dependency Graph

No dependency edge remains between Unit A and Unit B. `ga-3u2e95.1` and
`ga-mtgtv7.1` are superseded coordination gates, and the two live deploy beads
are independent.

## Handoff

The original reviewer deploy beads are closed as decomposed into the revised
deploy packages above. The deployer should not retry the stacked PR #3004
branch directly.

PM revised `ga-3u2e95.2` and `ga-mtgtv7.2`, removed their obsolete blockers,
and routed both to `gascity/deployer`. Each deploy child must record gate
evidence, PR URL, and merge-request handoff to `mayor/mpr` on PASS, or exact
gate failure evidence on FAIL.
