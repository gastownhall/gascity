# Release Gate: condition-check full-wave overlap fixture

Deploy bead: `ga-soa96t`

Review bead: `ga-7i08d1`

Original build/review lineage: `ga-e7cxrg` / `ga-lw4rph`

Reviewed source: `b678255acc85f92ae4ace9d45e0464a3ac983ee7`

Current base: `origin/main@187e53828754894096fc295cea4baca909fe9a96`

Reviewed-source merge base: `cdb5328a2f2e570fd56d017b98171cfa7b58f522`

Gate date: 2026-08-21

**Verdict: PASS.** The candidate-owned tests, required policy lane, build,
vet, affected lint, formatting, conflict check, and scope check pass. The
required 40-job candidate union retains its raw failures in this record. Every
failure is either attributed to a tracked pre-existing failure with no path or
mechanism overlap, or is explicitly marked FAIL-WAIVED under a recorded mayor
authorization. In particular,
`TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` remains
FAIL-WAIVED under `waiver_ref=ga-soa96t-mayor-waiver-20260821`; the exact
failure reproduced on the reviewed source's base under the same 40-job
topology. Nothing in this record rewrites a raw failure to green.

## Gate Results

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Re-review bead `ga-7i08d1` records PASS for the exact reviewed source. It independently inspected the reconciled hunk and reran build, vet, and the relevant tests. |
| 2 | Acceptance criteria met | PASS | The `cap-admits-the-wave` fixture waits for the declared four-check wave, preserves successful registrations until all polling checks can observe the cohort, and removes timed-out registrations before the serial control arm. All eight tests in the modified file pass by name. |
| 3 | Tests pass | PASS | The documented CI-equivalent 40-job candidate union ran with the process-backed `cmd/gc` lane enabled and reported 33 PASS jobs / 7 FAIL jobs / 0 job-level SKIP. The six failing test names are preserved below: three are attributed with tracker plus base/no-overlap evidence, one uses prior exact candidate/base A/B evidence, and two failure classes are explicitly FAIL-WAIVED. The eight candidate-owned tests report 8 PASS / 0 FAIL / 0 SKIP. |
| 3a | Pre-existing failures attributed | PASS | The beads#4566 blocker reproduced on the exact base under identical load and is covered for this gate by mayor waiver `ga-soa96t-mayor-waiver-20260821`. `TestBdFlagManifestCurrent` and both tmux binding tests reproduced on the exact base with matching signatures. `TestE2E_SuspendResume_City` is tracked with prior exact candidate/base A/B evidence and cannot be reached from the candidate's package-local `_test.go` file. `TestProviderLiveClaudeKindPath` is covered by `waiver_ref=mayor-2026-08-20-herdr-pane-standing`. |
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

## Disposition

The gate passes with all raw failures visible, tracked, and attributed or
explicitly waived. No failure was retried into green. The isolated deploy
branch may be pushed and proposed for review; merge authority remains with
mayor/mpr.
