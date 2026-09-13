# Release gate: idle pool members wake for shared-template work

- Deploy bead: `ga-z1axa6`
- Build bead: `ga-8vz95k.6`
- Review bead: `ga-ofn9oc`
- Reviewed commit: `ebc77ee31442ba2a44d28c9561d1e92983811f2d`
- Branch tip at gate time: `ebc77ee31442ba2a44d28c9561d1e92983811f2d`
- Base checked: `origin/main@a718dd6894109ee20a894c8975a6e13c237bbdbd`
- Gate result: **PASS**

The previous revision of this file recorded criterion 3 as blocked: the `cmd/gc`
test package did not compile at the then-current base `3d268c58`, so the
required `cmd/gc process` lane could not run. That base has moved. `origin/main`
now carries the eleven-argument `releaseOrphanedPoolAssignments` call site, the
package builds, and the lane runs. This revision re-runs every criterion against
`a718dd68` and certifies criterion 3 on its own evidence rather than on a
substitute.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-ofn9oc` is closed with verdict `pass`. The verdict was recorded against `f60f2f9`; `6db4ec5` narrows the same surface (excludes drained and dependency-only slots) and is strictly more conservative than what was reviewed. The only change since is added test coverage (`9a171b7c9`, `ebc77ee31`), which alters no production behavior. |
| 2 | Acceptance criteria met | **PASS** | Ready open work assigned to a pool template wakes every eligible configured pool member. Blocked, deferred, terminal, and otherwise-not-ready work does not. In-progress work still requires a concrete holder identity. Membership comes from typed `pool_managed` metadata; numeric suffixes, configured named sessions, manual sessions, members of other pools, drained slots, and dependency-only slots cannot impersonate membership. |
| 3 | Tests pass | **PASS** | The required `cmd/gc process` lane ran green at this tip: `make test-cmd-gc-process-parallel` over six shards under `GC_FAST_UNIT=0`, including `TestTutorial01` (executed in shard 6). See "Criterion 3: required lane" below for the shard results and for the one transient shard-4 failure that was retried green and attributed. |
| 4 | No high-severity review findings open | **PASS** | Reviewer reported no security, style, or specification findings and no unresolved HIGH findings. |
| 5 | Final branch is clean | **PASS** | `git diff --check origin/main...HEAD` passed. `gofmt -l cmd/gc` reported no files. This gate file is the only deploy-only addition and is committed on the isolated branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching `origin/main@a718dd6894109ee20a894c8975a6e13c237bbdbd`, `git merge-tree --write-tree origin/main HEAD` completed without conflict and produced tree `7acce120ba073afddec608b8d58efdee2479ebff`. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The six code-bearing commits and six changed files form one awake-set behavior change: distinguish pool-template serviceability for ready work from concrete ownership of claimed work. Changed files: `cmd/gc/compute_awake_bridge.go`, `cmd/gc/compute_awake_set.go`, `cmd/gc/compute_awake_set_pool_template_claim_test.go`, `cmd/gc/session_beads.go`, `cmd/gc/session_reconciler.go`, and this gate file. |

## Criterion 3: required lane

`engdocs/contributors/release-gate-criteria-conventions.md` requires this
criterion to name the CI jobs `ci-required` gates merge on for the changed
paths. This diff is entirely under `cmd/gc/**`, so the `cmd_gc_process` filter
applies and `make test-cmd-gc-process[-parallel]` (or the CI `cmd/gc process`
job) is the required lane. That lane now runs, and it ran here:

```text
make test-cmd-gc-process-parallel     # CMD_GC_PROCESS_TOTAL=6, GC_FAST_UNIT=0
```

- `test_cmd_scope: full ./cmd/gc package, sharded 6 ways, GC_FAST_UNIT=0` —
  reaches `TestTutorial01`, which the convention names as the coverage every
  default-tier entry point skips. `TestTutorial01` executed in shard 6, which
  reported `ok`.
- `shard_results:` shards 1, 2, 3, 5, and 6 reported `ok` on the first run.
  Shard 4 failed once and was retried green (`make test-cmd-gc-process-shard
  CMD_GC_PROCESS_SHARD=4 CMD_GC_PROCESS_TOTAL=6` — `ok ... 68.067s`).
- `transient_failure_attribution:` the single shard-4 failure was
  `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`
  (`cmd/gc/cmd_bd_test.go`), which is not in this diff. It failed inside
  `bd init` with the shared-Dolt refusal `refusing to auto-apply 13 pending
  schema migrations to a shared server database (v53 -> v66)` — the host-wide
  condition `AGENTS.md` documents, whose sanctioned fix is a one-time
  `bd migrate schema` by a designated migrator, not a code change. The test
  passes at this tip in isolation and passes at `origin/main@a718dd68` in
  isolation, so it is environmental and not attributable to this branch.

### Diff-owned test results

```text
go test ./cmd/gc/ -run 'TestAwakeSetPool|TestAwakeSetReadyPool|TestBuildAwakeInputFromReconcilerCarriesConfiguredPoolMembership' -count=1
```

- `test_counts: 8 PASS / 0 FAIL / 0 SKIP` (`ok github.com/gastownhall/gascity/cmd/gc 1.264s`)
- `diff_tests_executed:`
  - `TestAwakeSetReadyPoolTemplateAssignmentWakesEligibleMember` — PASS
  - `TestAwakeSetPoolTemplateAssignmentRequiresReadyDemand` — PASS
  - `TestAwakeSetPoolInProgressOwnershipRemainsConcrete` — PASS
  - `TestAwakeSetPoolTemplateServiceabilityRequiresConfiguredMembership` — PASS
  - `TestBuildAwakeInputFromReconcilerCarriesConfiguredPoolMembership` — PASS
  - `TestAwakeSetPoolTemplateAssignmentWakesAllEligibleMembers` — PASS (multi-member fan-out)
  - `TestAwakeSetPoolTemplateAssignmentFillsScaleSlotPerMember` — PASS (`countAssignedScaleSlots` interaction)
  - `TestAwakeSetPoolTemplateClaimCollapsesFanOutToHolder` — PASS (post-claim collapse to the concrete holder)
- `waiver_ref: none` for diff-owned tests
- `skip_justification:` no test added or modified by this diff skipped.

## Additional required lanes

Re-run against `origin/main@a718dd68` at tip `ebc77ee3`:

- `make test-ci-policy` — **PASS** (5 + 15 unittest cases, `scripts/cipolicy`, `scripts/prwatchdog`, and the four static-scope tests in `./scripts`)
- `go build ./cmd/gc/` — **PASS**
- `gofmt -l cmd/gc` — **PASS**, no files reported
- `git diff --check origin/main...HEAD` — **PASS**
- `go vet ./cmd/gc/...` — **PASS**
- `go vet ./...` (whole module) — **PASS**
- `lint-affected` — **PASS**, 0 issues (`LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main`, selected `./cmd/gc`)
- `fmt-check-changed` — **PASS** (same scope/ref)

Not verified here: the hosted CI result for `CI / required`, `Check`, and
`cmd/gc process / shard 1..12`, and the Mac aggregate. Those are observable only
on the pushed head and must be confirmed there. The reviewer recorded that the
Mac aggregate stays red on an `internal/pathutil` symlink test that is
base-owned; that observation is carried forward here unverified and, if it
holds, must not gate this branch.

## Acceptance audit

- The reconciler projects typed `session.Info.PoolManaged` into the pure awake-set input.
- Ready/open template-assigned work can wake any eligible member of that
  configured pool. In practice the ready-by-assignee probe
  (`readyAssignedWorkAssignees`) never enumerates a bare template, so the
  beads that reach this path are open assigned molecule and workflow roots
  (`isOpenAssignedMoleculeWork`), not plain template-assigned tasks. The
  fan-out to all eligible members is pinned by test.
- In-progress work cannot match by template and remains bound to bead ID, runtime session name, or named-session identity.
- Existing readiness/blocker filtering runs before matching, so not-ready work creates no wake demand.
- Named sessions, manual sessions, numeric-suffix lookalikes, members of another pool, drained members, and dependency-only members are explicitly covered and rejected.
- Once a member claims the bead the assignment is a concrete identity on an
  in-progress bead, so the fan-out collapses to that holder and the members
  that lost the claim race report no assigned work.
- The change introduces no role-name logic, wire/API change, dependency, or migration.

## Known gap carried forward

`countAssignedScaleSlots` calls `sessionHasAssignedWork` per member, so one
template-assigned ready bead now reports a filled scale slot for every member of
that pool. Every affected member is already awake via `assigned-work`, so no
stranding scenario is reachable and the observable contract is unchanged —
`TestAwakeSetPoolTemplateAssignmentFillsScaleSlotPerMember` records that
contract. The counter nonetheless measures something narrower than its name
implies; this is documented, not fixed, in this change.
