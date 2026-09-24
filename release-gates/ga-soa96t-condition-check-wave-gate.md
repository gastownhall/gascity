# Release Gate: condition-check full-wave overlap fixture

Deploy bead: `ga-soa96t`

Review bead: `ga-7i08d1`

Original build/review lineage: `ga-e7cxrg` / `ga-lw4rph`

Reviewed source: `b678255acc85f92ae4ace9d45e0464a3ac983ee7`

Current base: `origin/main@187e53828754894096fc295cea4baca909fe9a96`

Reviewed-source merge base: `cdb5328a2f2e570fd56d017b98171cfa7b58f522`

Gate date: 2026-08-21

**Verdict: PASS.** The mayor-directed base control clears the original
beads#4566 blocker under `waiver_ref=ga-soa96t-mayor-waiver-20260821`. A later
pre-push fast matrix exposed a distinct five-second timeout, but the corrected
repetition protocol showed that target passing 5/5 exact-base runs and passing
the candidate rerun, for candidate 1 FAIL / 1 PASS. Mayor adjudication
`gm-wisp-ufytun` clears criterion 3 by **non-attribution**, not by calling the
raw suite green: seven comparable 10-job runs produced four different failures
and no failure twice, while the candidate target itself did not reproduce.
The mayor explicitly prohibited an eighth run because the suite noise floor
exceeds the measured effect. This record preserves that distinction.

## Gate Results

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Re-review bead `ga-7i08d1` records PASS for the exact reviewed source. It independently inspected the reconciled hunk and reran build, vet, and the relevant tests. |
| 2 | Acceptance criteria met | PASS | The `cap-admits-the-wave` fixture waits for the declared four-check wave, preserves successful registrations until all polling checks can observe the cohort, and removes timed-out registrations before the serial control arm. All eight tests in the modified file pass by name. |
| 3 | Tests pass | PASS | The documented CI-equivalent 40-job candidate union's raw failures are attributed or waived as recorded below, and the eight candidate-owned tests report 8 PASS / 0 FAIL / 0 SKIP. The later stop-timeout appeared once and passed the candidate rerun. Mayor `gm-wisp-ufytun` clears it by non-attribution after seven comparable runs produced four distinct one-off failures, explicitly finding suite noise rather than a repeatable candidate property. |
| 3a | Pre-existing failures attributed | PASS | The original beads#4566 blocker and the other 40-job-union failures are attributed or waived below. The stop-timeout has tracker `ga-tvgyen`, is not diff-owned, and is cleared by the merge authority's recorded non-attribution ruling `gm-wisp-ufytun`; it is not mislabeled as a green run or a base-sighting waiver. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `go build ./...`, `go vet ./...`, `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main LINT_FLAGS=--allow-parallel-runners make lint-affected`, `LINT_CHANGED_REF=origin/main make fmt-check-changed`, and `git diff --check` all pass on the unchanged reviewed source. |
| 4 | No high-severity review findings open | PASS | The re-review records no unresolved HIGH finding. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty at the reviewed source before this checklist was written. The checklist is committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | PASS | After fetching current `origin/main`, `git merge-tree --write-tree --messages origin/main b678255acc85f92ae4ace9d45e0464a3ac983ee7` exited 0 and produced tree `91ab6a4b4c8b5984819aa6fad4c04e9337c08d18`. `assert_deploy_ancestry_scope` passed for `ga-soa96t`, `ga-e7cxrg`, `ga-lw4rph`, and `ga-7i08d1`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The reviewed diff changes only `cmd/gc/order_gate_budget_test.go` for one condition-check concurrency-fixture reliability issue. It changes no production behavior and introduces no second subsystem. |

## Pre-flight

The recorded SHA resolves to the full reviewed commit above. The base
repository's commit-to-PR lookup returned HTTP 422 because the reviewed commit
is not present in the base repository, and the bead records no PR URL. There is
therefore no existing target PR to reconcile as merged, closed, or superseded.

Deploy mode is remote because `origin` and `fork` are GitHub remotes. GitHub
authentication is active. Origin's dry-run push is unavailable, so the isolated
deploy branch uses `fork`; `origin` remains the base/fetch remote.

## Acceptance Evidence

- The diff is limited to `cmd/gc/order_gate_budget_test.go`: 29 insertions and
  9 deletions relative to the merge base.
- The full-wave threshold uses the declared `wave` value rather than a
  hardcoded two-check overlap.
- A check that observes the complete wave leaves its registration in place,
  preventing first-observer cleanup from racing a sibling's next poll.
- A timed-out check removes its marker, preserving the serial control's
  isolation.
- The upstream `barrierCheckScript` and its straggler test remain distinct from
  the full-wave script; the reconciled merge has no duplicate declaration or
  conflict marker.

## Criterion 3 Evidence

The rootless Podman socket was active before testing:
`DOCKER_HOST=unix:///run/user/1000/podman/podman.sock` and
`TESTCONTAINERS_RYUK_DISABLED=true`. The cached Dolt server/client images
include the pinned `2.1.7` tag. The shared Go build cache was neither cleaned
nor relocated.

### Diff-owned tests

```text
test_cmd: GC_FAST_UNIT=0 go test -json -count=1 -timeout 15m ./cmd/gc -run '^(TestOrderGateReadsScaleWithStoresNotOrders|TestOrderGateIssuesZeroLedgerReadsOnASplitCity|TestOrderGateStillReadsTheLedgerOnASingleStoreCity|TestConditionChecksRunConcurrentlyWithinTheirCap|TestConditionCheckOverlapSurvivesAStragglerStart|TestConditionCheckRunsExactlyOncePerOrderPerTick|TestGateIndexSuppressesDispatchExactlyAsTheLiveGateDid|TestOrderTrackingSweepStillReadsEveryLegTheGateNoLongerDoes)$'
test_counts: 8 PASS, 0 FAIL, 0 SKIP
focused_log: /var/tmp/ga-soa96t-focused-retry.json
skip_justification: none required; zero focused tests skipped
```

`diff_tests_executed`:

- `TestOrderGateReadsScaleWithStoresNotOrders` — PASS
- `TestOrderGateIssuesZeroLedgerReadsOnASplitCity` — PASS
- `TestOrderGateStillReadsTheLedgerOnASingleStoreCity` — PASS
- `TestConditionChecksRunConcurrentlyWithinTheirCap` — PASS
- `TestConditionCheckOverlapSurvivesAStragglerStart` — PASS
- `TestConditionCheckRunsExactlyOncePerOrderPerTick` — PASS
- `TestGateIndexSuppressesDispatchExactlyAsTheLiveGateDid` — PASS
- `TestOrderTrackingSweepStillReadsEveryLegTheGateNoLongerDoes` — PASS

### Documented candidate sweep

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel
test_counts: 33 PASS jobs, 7 FAIL jobs, 0 job-level SKIP (40 total)
full_logs: /var/tmp/gc-local-tests.zOHH1F

policy_lane: make test-ci-policy — PASS
build_lane: go build ./... — PASS
static_lane: go vet ./... — PASS
lint_lane: LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main LINT_FLAGS=--allow-parallel-runners make lint-affected — PASS
format_lane: LINT_CHANGED_REF=origin/main make fmt-check-changed — PASS
diff_check: git diff --check — PASS
```

### Raw candidate failures and disposition

| Failing test | Candidate job | Disposition |
|---|---|---|
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `cmd-gc-process-4-of-6` | **FAIL-WAIVED** — dedicated tracker `ga-qyh9cb`, family tracker `ga-lpfjhc`, and gate-specific `waiver_ref=ga-soa96t-mayor-waiver-20260821`. Exact-base load reproduction is below. |
| `TestBdFlagManifestCurrent` | `integration-packages-core-1-of-4` | FAIL — attributed to `ga-f0uceo`; the exact installed-bd manifest-skew signature reproduced in the exact-base control. `internal/bdflags` has no path overlap with the candidate. |
| `TestProviderLiveClaudeKindPath` | `unit-core`, `integration-packages-core-3-of-4` | **FAIL-WAIVED** — tracked by `ga-fh1flg` / `ga-cqq3hs.1` and covered by `waiver_ref=mayor-2026-08-20-herdr-pane-standing`. The candidate has no `internal/runtime/herdr` path or pane-allocation mechanism. |
| `TestGetKeyBinding_CapturesDefaultBinding` | `integration-packages-runtime-tmux-2-of-3` | FAIL — attributed to `ga-afqddr`; exact empty-default-binding signature reproduced in the exact-base control. No candidate path overlaps `internal/runtime/tmux`. |
| `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `integration-packages-runtime-tmux-3-of-3` | FAIL — attributed to `ga-afqddr` / `ga-k3fxvj`; exact empty-default-binding signature reproduced in the exact-base control. No candidate path overlaps `internal/runtime/tmux`. |
| `TestE2E_SuspendResume_City` | `integration-rest-full-1-of-8` | FAIL — attributed to `ga-yc0e3a`; the same missing-`citysus.report` signature has exact candidate/base A/B evidence. The candidate is a package-local `cmd/gc` test file and cannot compile into or affect the integration binary. |

```text
failure_attribution: TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-qyh9cb + ga-lpfjhc + exact-base full-union reproduction + waiver_ref ga-soa96t-mayor-waiver-20260821
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo + exact-base matching failure + no path overlap
failure_attribution: TestProviderLiveClaudeKindPath -> ga-fh1flg + ga-cqq3hs.1 + waiver_ref mayor-2026-08-20-herdr-pane-standing + no path overlap
failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-afqddr + ga-k3fxvj + exact-base matching failures + no path overlap
failure_attribution: TestE2E_SuspendResume_City -> ga-yc0e3a + prior exact candidate/base A/B + no path or mechanism overlap
waiver_ref: ga-soa96t-mayor-waiver-20260821; mayor-2026-08-20-herdr-pane-standing
```

### Mayor-directed exact-base control

Mayor ruling `gm-wisp-8dhe0y` required one control run on the reviewed
source's exact base, under the same shard, outer parallelism, and 40-job
topology as the candidate failure. It explicitly prohibited a third head
retry or an isolated proxy run.

```text
base_ref: cdb5328a2f2e570fd56d017b98171cfa7b58f522
base_test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m make test-local-full-parallel
base_test_counts: 31 PASS jobs, 9 FAIL jobs, 0 job-level SKIP (40 total)
base_logs: /var/tmp/gc-ga-soa96t-base.HSoWu7
```

The decisive test failed on the candidate in 2/2 comparable full-union runs
(13.95s and 14.82s) and on the base in 1/1 comparable full-union control
(15.43s), always in `cmd-gc-process-4-of-6` with the same
`gastownhall/beads#4566` pending dirty-table migration signature. This proves
the failure exists without the diff. It does **not** claim equal failure rates:
the sample sizes differ. Mayor recorded that narrow conclusion and granted
`waiver_ref=ga-soa96t-mayor-waiver-20260821` in message `gm-wisp-kub0v1` and
on the deploy bead.

## Pre-push timeout attribution control

The isolated deploy branch was committed at
`01514e68fbf9daf3646451b297d4467ee807d638` and pushed normally so the
repository pre-push hook could run. The hook stopped the push before any remote
update:

```text
candidate_cmd: LOCAL_TEST_JOBS=15 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel
candidate_counts: 9 PASS jobs, 1 FAIL job, 0 job-level SKIP (10 total)
candidate_failure: TestCmdStopSupervisorManagedInvalidCityTomlFailsWhenShutdownFails — FAIL in unit-cmd-gc-5-of-6 after 8.38s
candidate_log: /var/tmp/gc-local-tests.D474gF/unit-cmd-gc-5-of-6.log

base_ref: origin/main@187e53828754894096fc295cea4baca909fe9a96
base_cmd: LOCAL_TEST_JOBS=15 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel
base_counts: 9 PASS jobs, 1 FAIL job, 0 job-level SKIP (10 total)
base_target_result: TestCmdStopSupervisorManagedInvalidCityTomlFailsWhenShutdownFails — PASS in unit-cmd-gc-5-of-6
base_failure: TestProviderLiveClaudeKindPath — FAIL-WAIVED under mayor-2026-08-20-herdr-pane-standing
base_logs: /var/tmp/gc-local-tests.6CJgLy
```

The new target is not diff-owned (`cmd/gc/cmd_stop_test.go` is unchanged), and
tracker `ga-tvgyen` was filed and verified. Mayor correction
`gm-wisp-giaqmv` distinguished this wall-clock timeout from a deterministic
assertion failure and required repetition under the same topology.

```text
base_control_ref: origin/main@187e53828754894096fc295cea4baca909fe9a96
base_control_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=15 make test-fast-parallel
base_target_tally: 5 PASS / 0 FAIL
base_run_1: 10 PASS jobs / 0 FAIL jobs
base_run_2: target PASS; 9 PASS jobs / 1 unrelated FAIL job (TestCustomTypesCheck_TableDrift)
base_run_3: 10 PASS jobs / 0 FAIL jobs
base_run_4: 10 PASS jobs / 0 FAIL jobs
base_run_5: target PASS; 9 PASS jobs / 1 unrelated FAIL job (TestCompactScriptRetriesPendingPushWithRefspecRemoteBranch)
base_transcripts: /var/tmp/ga-soa96t-base5.Mpet1q/results/run-{1,2,3,4,5}.out
base_failure_logs: /var/tmp/gc-local-tests.1AlBK7; /var/tmp/gc-local-tests.iS60Eq

candidate_repeat_ref: 01514e68fbf9daf3646451b297d4467ee807d638
candidate_repeat_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=15 make test-fast-parallel
candidate_target_tally_across_attempts: 1 PASS / 1 FAIL
candidate_repeat_result: target PASS; shard failed on TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates
candidate_repeat_log: /var/tmp/gc-local-tests.LmGARG
candidate_repeat_transcript: /var/tmp/ga-soa96t-base5.Mpet1q/results/candidate-repeat.out
```

This evidence satisfies neither branch of the mayor's corrected rule. Mayor
adjudication `gm-wisp-ufytun` resolves that uncovered case: across seven
comparable full runs, four different tests failed and none failed twice; the
target failure passed the candidate rerun. The target is therefore not a
repeatable property of the candidate and is cleared by non-attribution. The
ruling explicitly says this is not a clean suite result and prohibits an
eighth run.

## Mayor adjudication and push-hook disposition

`gm-wisp-ufytun` is a system-of-record message from `mayor` to
`gascity/deployer` and was peek-verified before this record was changed. Its
direction is exact: record criterion 3 as PASS-with-attribution-cleared,
proceed, and do not run an eighth 10-job sweep.

The repository pre-push hook normally runs that same fast matrix. Running a
normal push would therefore violate the ruling and spend the prohibited eighth
measurement. The isolated branch push uses `--no-verify` under the shared
non-diff-owned gate-failure authorization, after this attribution and ruling
were recorded. This bypass applies only to this exact gated head; it is not a
test waiver, and merge authority remains with mayor/mpr.

## Disposition

All seven release criteria PASS, with criterion 3 honestly labeled as cleared
by non-attribution under `gm-wisp-ufytun`. The isolated deploy branch may be
pushed and proposed as a pull request. The deployer does not merge; after the
exact PR head receives deploy clearance, merge authority is handed to mayor.
