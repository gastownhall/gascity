# Release gate: idle pool members wake for shared-template work

- Deploy bead: `ga-z1axa6`
- Build bead: `ga-8vz95k.6`
- Review bead: `ga-ofn9oc`
- Reviewed commit: `6db4ec51ddde4adebfc7ad96b59ccfdc7ce7317a`
- Branch tip at gate time: `77d36f78ab24499aa01e9ce9aadb044db787588b` (merge of `origin/main`; contributes no diff of its own)
- Base checked: `origin/main@3d268c5849ca2ef49392c6e4adb68e9a10d0b59d`
- Gate result: **BLOCKED — criterion 3 cannot be certified against the current base**

The previous revision of this file certified `f60f2f936dda31d798bc6e1e4e2dd5c57944b481`,
which predates the drained/dependency-only exclusion in
`sessionAssigneePoolTemplateMatches`. This revision re-runs every criterion that
is verifiable against the current base and records, without substitution, the
one that is not.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-ofn9oc` is closed with verdict `pass`. The verdict was recorded against `f60f2f9`; `6db4ec5` narrows the same surface (excludes drained and dependency-only slots) and is strictly more conservative than what was reviewed. |
| 2 | Acceptance criteria met | **PASS** | Ready open work assigned to a pool template wakes every eligible configured pool member. Blocked, deferred, terminal, and otherwise-not-ready work does not. In-progress work still requires a concrete holder identity. Membership comes from typed `pool_managed` metadata; numeric suffixes, configured named sessions, manual sessions, members of other pools, drained slots, and dependency-only slots cannot impersonate membership. |
| 3 | Tests pass | **NOT CERTIFIED — base build break blocks the required lane** | The `cmd/gc` test package does not compile at `origin/main@3d268c58`, so the required `cmd/gc process` lane cannot run on this branch. See "Criterion 3: blocked" below for the reproduction on a clean `origin/main` worktree and for the diff-owned test results obtained with the offending base-owned file set aside. |
| 4 | No high-severity review findings open | **PASS** | Reviewer reported no security, style, or specification findings and no unresolved HIGH findings. |
| 5 | Final branch is clean | **PASS** | `git diff --check origin/main...HEAD` passed. `gofmt -l cmd/gc` reported no files. This gate file is the only deploy-only addition and is committed on the isolated branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching `origin/main@3d268c5849ca2ef49392c6e4adb68e9a10d0b59d`, `git merge-tree --write-tree origin/main HEAD` completed without conflict and produced tree `cc0c6e09281035cc74484df8ec6b0b47acb41155`. The same simulation against `6db4ec51ddde4adebfc7ad96b59ccfdc7ce7317a` produces the identical tree. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The four code-bearing commits and five changed files form one awake-set behavior change: distinguish pool-template serviceability for ready work from concrete ownership of claimed work. Changed files: `cmd/gc/compute_awake_bridge.go`, `cmd/gc/compute_awake_set.go`, `cmd/gc/compute_awake_set_pool_template_claim_test.go`, `cmd/gc/session_beads.go`, `cmd/gc/session_reconciler.go`. |

## Criterion 3: blocked

`engdocs/contributors/release-gate-criteria-conventions.md` requires this
criterion to name the CI jobs `ci-required` gates merge on for the changed
paths. This diff is entirely under `cmd/gc/**`, so the `cmd_gc_process` filter
applies and `make test-cmd-gc-process[-parallel]` (or the CI `cmd/gc process`
job) is the required lane. That lane cannot run, because the `cmd/gc` test
package does not build at the current base:

```text
vet: cmd/gc/pool_session_name_route_assignee_liveness_test.go:63:2:
  not enough arguments in call to releaseOrphanedPoolAssignments
```

- **Not diff-owned.** `cmd/gc/pool_session_name.go` and
  `cmd/gc/pool_session_name_route_assignee_liveness_test.go` are byte-identical
  between this branch and `origin/main` (`git diff origin/main HEAD -- <paths>`
  is empty). Neither is in this PR's diff.
- **Reproduces without this branch.** A detached worktree at
  `origin/main@3d268c58` fails `go vet ./cmd/gc/` with the same error.
- **Mechanism.** A semantic merge conflict between two independently green PRs:
  `30ed29b97` (#6265) added the test file calling `releaseOrphanedPoolAssignments`
  with ten arguments, and `3d268c584` (#6261) added the eleventh `recordPhase`
  parameter. Neither commit references the other.
- **Why it is not attributable under the raw-failure rule.** The existing
  attribution clauses require the failing test's file/package not to overlap the
  candidate files. This is a build failure in the same Go package as the diff,
  and it prevents the diff-owned tests from executing at all in an unmodified
  checkout. It blocks the evidence rather than sitting alongside it, so it is
  recorded as blocking, not waived.

Re-certify this criterion once `origin/main` builds again. No change to this
branch can unblock it.

### Diff-owned test results (partial evidence, not a substitute for criterion 3)

Obtained by temporarily moving the base-owned
`cmd/gc/pool_session_name_route_assignee_liveness_test.go` aside and restoring
it immediately afterward. Nothing in the committed tree was modified.

```text
go test ./cmd/gc/ -run 'TestAwakeSetPool|TestAwakeSetReadyPool|TestBuildAwakeInputFromReconcilerCarriesConfiguredPoolMembership' -count=1
```

- `test_cmd_scope: package-scoped, GC_FAST_UNIT unset` — does **not** reach
  `TestTutorial01` and therefore does not satisfy criterion 3 on its own.
- `test_counts: 7 PASS / 0 FAIL / 0 SKIP` (`ok github.com/gastownhall/gascity/cmd/gc 0.861s`)
- `diff_tests_executed:`
  - `TestAwakeSetReadyPoolTemplateAssignmentWakesEligibleMember` — PASS
  - `TestAwakeSetPoolTemplateAssignmentRequiresReadyDemand` — PASS
  - `TestAwakeSetPoolInProgressOwnershipRemainsConcrete` — PASS
  - `TestAwakeSetPoolTemplateServiceabilityRequiresConfiguredMembership` — PASS
  - `TestBuildAwakeInputFromReconcilerCarriesConfiguredPoolMembership` — PASS
  - `TestAwakeSetPoolTemplateAssignmentWakesAllEligibleMembers` — PASS (multi-member fan-out)
  - `TestAwakeSetPoolTemplateAssignmentFillsScaleSlotPerMember` — PASS (`countAssignedScaleSlots` interaction)
- `waiver_ref: none` for diff-owned tests
- `skip_justification:` no test added or modified by this diff skipped.

## Additional required lanes

Re-run against `origin/main@3d268c58` unless noted:

- `make test-ci-policy` — **PASS** (5 + 15 unittest cases, `scripts/cipolicy`, `scripts/prwatchdog`, and the four static-scope tests in `./scripts`)
- `go build ./cmd/gc/` — **PASS**
- `gofmt -l cmd/gc` — **PASS**, no files reported
- `git diff --check origin/main...HEAD` — **PASS**
- `go vet ./cmd/gc/...` — **PASS only with the base-owned broken test file set aside**; fails on an unmodified checkout for the reason in "Criterion 3: blocked"
- `go vet ./...` (whole module) — **NOT RUN**, blocked by the same base build break
- `lint-affected` / `fmt-check-changed` — **NOT RE-RUN** at this base

## Acceptance audit

- The reconciler projects typed `session.Info.PoolManaged` into the pure awake-set input.
- Ready/open template-assigned work can wake any eligible member of that configured pool, and the fan-out to all eligible members is now pinned by test.
- In-progress work cannot match by template and remains bound to bead ID, runtime session name, or named-session identity.
- Existing readiness/blocker filtering runs before matching, so not-ready work creates no wake demand.
- Named sessions, manual sessions, numeric-suffix lookalikes, members of another pool, drained members, and dependency-only members are explicitly covered and rejected.
- The change introduces no role-name logic, wire/API change, dependency, or migration.

## Known gap carried forward

`countAssignedScaleSlots` calls `sessionHasAssignedWork` per member, so one
template-assigned ready bead now reports a filled scale slot for every member of
that pool. Every affected member is already awake via `assigned-work`, so no
stranding scenario is reachable and the observable contract is unchanged —
`TestAwakeSetPoolTemplateAssignmentFillsScaleSlotPerMember` records that
contract. The counter nonetheless measures something narrower than its name
implies; this is documented, not fixed, in this change.
