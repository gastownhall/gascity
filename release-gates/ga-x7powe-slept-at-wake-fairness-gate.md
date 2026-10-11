# Release gate: `slept_at` wake-fairness fallback

- Deploy bead: `ga-x7powe`
- Build bead: `ga-mh0vxm.1`
- Review bead: `ga-ck1716`
- Reviewed source: `6c20370b8f3e944847411f2b5658b3416709346f`
- Base checked: `origin/main@76d37b9b9de05c172a00a5c7604aae6a9996ea12`
- Decision: **PASS**

`docs/PROJECT_MANIFEST.md` is not present in this worktree. This record uses
the seven release criteria in the deployer contract and the repository's
documented commands in `TESTING.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-ck1716` is closed with an explicit PASS for the exact reviewed source. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | Wake fairness now reads `last_woke_at`, then `slept_at`, then `CreatedAt`; drain acknowledgement stamps `slept_at` while clearing `last_woke_at`; records with neither timestamp retain the creation-time fallback. The existing `slept_at` metadata key is projected through `session.Info`, with no migration, new persistence key, or parallel read surface. |
| 3 | Tests pass | PASS | The documented 40-job full suite produced 48,016 PASS / 4 attributed FAIL / 209 SKIP test events. Every modified test passed by name. All four raw failures are non-diff-owned, tracked, mechanism-attributed, and path-disjoint as recorded below. The required policy lane passed; supplemental vet and formatting lanes passed, and the affected-lint lane's eight pre-existing diagnostics are attributed below. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no security, style, or correctness blocker. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty after testing and before adding this gate record. `git diff --check origin/main...6c20370b...` passes. |
| 6 | Branch diverges cleanly from main | PASS | After the final base refresh, `git merge-tree --write-tree origin/main 6c20370b...` succeeded and produced tree `80c249986924848426db9732eab23a475c769ba1`. `assert_deploy_ancestry_scope` passed for `ga-x7powe` and its build bead `ga-mh0vxm.1`. No self-rebase was required. |
| 7 | Single feature theme | PASS | Two commits change one coupled subsystem: session lifecycle timestamps and the wake-fairness ordering that consumes them. |

## Acceptance evidence

- `session.Info` projects the already-persisted `slept_at` value through the
  shared metadata codec.
- `wakeFairnessTime` uses a valid `LastWokeAt` first, a valid `SleptAt` second,
  `CreatedAt` third, and the zero time only when all three are unavailable.
- `SleepPatch` and `AcknowledgeDrainPatch` both stamp the sleep time while
  clearing the prior wake time. The reconciler supplies its UTC clock value to
  drain acknowledgement.
- The characterization test covers sleep, drain acknowledgement, malformed and
  missing timestamps, and same-tick stable ordering.
- No new metadata key, storage migration, API shape, or CI configuration is
  introduced.

## Test evidence

`test_cmd_scope: full-suite`

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
GC_TEST_NO_SLICE=1 \
GOFLAGS=-v \
LOCAL_TEST_LOG_DIR=/var/tmp/ga-x7powe-full.ttrhxW \
make test-local-full-parallel
```

- `test_counts: 48016 PASS, 4 attributed FAIL, 209 SKIP`
- `job_counts: 36 green, 4 raw-failure, 0 skipped (40 total)`
- `diff_tests_executed: 9 unique modified-test surfaces PASS, 0 FAIL, 0 SKIP`
  - `TestWakeFairnessInfoTwinCharacterization`
  - `TestInfoApplyPatchMatchesReprojection`
  - `TestInfoMarkClosedMatchesReprojection`
  - `TestInfoApplyPatchDoesNotMutateReceiver`
  - `TestInfoCodecProjectionParity`
  - `TestInfoCodecKeysMatchProjectedList`
  - `TestLifecycleTransitionPatchesSetCompleteMetadata`
  - `TestDrainCompletionPatchesClearStopPendingReason`
  - `TestAcknowledgeDrainPatchClearsStaleStateReasonOnApply`
- `skip_justification:` all 209 SKIPs are unchanged conditional cases for
  platform-specific behavior, optional live providers/infrastructure, helper
  subprocesses, permissions, or explicit opt-in integration contracts. No
  diff-owned test skipped, and the changed session paths executed in both unit
  and integration package lanes.
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI configuration change)`

### Raw failures and attribution

| Raw result | Tracker | Attribution |
|---|---|---|
| FAIL: `TestBdFlagManifestCurrent` | `ga-f0uceo` | Clause 3(a) mechanism: the installed `bd` exposes flags absent from the checked-in manifest. The tracker predates this run; the candidate changes neither `internal/bdflags` nor the installed binary. This occurrence was appended and read back. |
| FAIL: `TestProviderLiveClaudeKindPath` | `ga-iepsvr` | Clause 3(a) mechanism: the external herdr start returned `agent_pane_busy` because shared pane `w1:p1` was not an available shell. The tracker predates this run; the candidate changes neither `internal/runtime/herdr` nor pane state. This occurrence was appended and read back. |
| FAIL: `TestSweep_ReapsRealDoltDataDirAfterSIGKILL` | `ga-cp7r41` | Clause 3(a) mechanism: the external `dolt init` setup process was killed before the sweep scenario began. The candidate changes neither the test, `internal/doltorphan`, nor the Dolt binary. The tracker was created and verified during this discovering run under the landed-mechanism escape. |
| FAIL: `TestGCLiveContract_BeadsAndEvents` | `ga-xfpjr0` | Clause 3(a) mechanism: city startup failed before session behavior ran because `dolt_schemas` changed concurrently during beads schema migration. The candidate changes neither this test nor the beads/Dolt migration path. The tracker was created and verified during this discovering run under the landed-mechanism escape. |

Every raw failure satisfies: (i) the failing test is not diff-owned; (ii) an
open root-condition tracker exists; (iii) the mechanism proof above identifies
an external or unchanged source; and (iv) the failing and mechanism paths do
not overlap the diff. The candidate modifies existing tests but adds no test
target or declared test-load census entry.

```text
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | installed-bd manifest drift; no path overlap
failure_attribution: TestProviderLiveClaudeKindPath -> ga-iepsvr | shared herdr pane busy; no path overlap
failure_attribution: TestSweep_ReapsRealDoltDataDirAfterSIGKILL -> ga-cp7r41 | external dolt init killed before scenario; no path overlap
failure_attribution: TestGCLiveContract_BeadsAndEvents -> ga-xfpjr0 | concurrent dolt_schemas mutation during startup; no path overlap
inconclusive-guard: n/a — mechanism proofs landed; reachable_production_code not relied upon; added_test_load=no
```

## Policy and static lanes

- `policy_lane: make test-ci-policy — PASS`
- `format_lane: make fmt-check-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main — PASS`
- `static_lane: make vet — PASS`
- `affected_lint_lane: make lint-affected LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main — 8 raw diagnostics, all attributed`
  - Four gofumpt findings in unchanged `cmd/gc` files -> `ga-d3m213`.
  - Three SA1019 findings in unchanged `cmd/gc` tests -> `ga-t88402`.
  - One SA4006 finding in unchanged `internal/api` code -> `ga-egkp4x`.

```text
policy_attribution: gofumpt unchanged cmd/gc files -> ga-d3m213 | exact files and diagnostics exist on origin/main
policy_attribution: SA1019 unchanged cmd/gc tests -> ga-t88402 | exact files and diagnostics exist on origin/main
policy_attribution: SA4006 unchanged internal/api file -> ga-egkp4x | exact file and diagnostic exists on origin/main
```

## Pre-flight and commands

GitHub's commit-to-PR lookup returned no PR for the reviewed source after the
final `origin/main` refresh, and the reviewed SHA is not an ancestor of main.
Normal isolated-branch deployment applies.

```text
git fetch origin main
gh api repos/gastownhall/gascity/commits/6c20370b8f3e944847411f2b5658b3416709346f/pulls
git merge-tree --write-tree origin/main 6c20370b8f3e944847411f2b5658b3416709346f
assert_deploy_ancestry_scope origin/main 6c20370b8f3e944847411f2b5658b3416709346f ga-x7powe ga-mh0vxm.1
git diff --check origin/main...6c20370b8f3e944847411f2b5658b3416709346f
make test-local-full-parallel
make test-ci-policy
make lint-affected LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main
make fmt-check-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main
make vet
```
