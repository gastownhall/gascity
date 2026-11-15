# Resource test-target census release gate — ga-26lie46

**Verdict:** **PASS**

Disposition: gate PASS on 2026-10-29. Publish only from deploy/ga-26lie46-gate with this record committed; merge authority remains mpr.

Reviewed source: `0f814df14b836f576915b6e4b01428a03eefc8b3`. Reviewer ga-1vbp998: PASS. Build ga-mnwyz2 carries only the two census commits under original work ga-4ag4p2.1. Source branch builder/ga-mnwyz2 is provenance only.

Gate base: `4a4921a60f684602f423a0ddcc882d54fdbf2ebe`. Synthetic merge: `dfd3e35b0a135a65850fc660f17a7a3c7a7cc7a0`; tree `141feab79b0592755d64992ec42d298d00ca9c01`. Deploy mode: remote; base repository gastownhall/gascity. Preflight commit/pulls returned no PR for the source. No source rebase or reset.

merge_state: clean (origin/main@4a4921a60f684602f423a0ddcc882d54fdbf2ebe)
gate_base_kind: current-tip
merge_validation: clean merged tree built, vetted, and tested here; merge authority verifies the actual merged result and current CI at merge time.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Review ga-1vbp998 resolves to the exact source; no review carryover or reviewer-added source commit. |
| 2 | Acceptance | PASS | Four owned scanner fixtures and repository ledger/count/documentation checks passed; mapping below. |
| 3 | Full CI-equivalent tests | PASS | Five fresh remote phases and all 16 native-floor steps rc0; 133512 PASS / 0 FAIL / 373 SKIP top-level Go result lines; all four owned tests PASS. |
| 4 | High-severity findings | PASS | Review has zero unresolved HIGH/blocker/major findings. |
| 5 | Clean branch | PASS | Source and merge scratch clean; publication enforces a clean isolated branch after committing only this gate record. |
| 6 | Merge/build/vet | PASS | Clean merge above; full Bazel build //... and full make vet/nogo rc0; original capacity refusal retained. |
| 7 | One feature theme | PASS | One resource-census dimension with its fixtures, policy, generated counts, and documentation. No CI job or test target added. |

## Acceptance evidence

1. ResourceTestTarget and ScopeBuildTargets are registered. Both fixture increments exercise the nonzero scope selection.
2. TestScanBuildTargetGoTestLineIncrementsCount, TestScanBuildTargetWorkflowRunLineIncrementsCount, TestScanBuildTargetCountsMultipleFilesAndLines, and TestScanBuildTargetSkipsCommentedAndNonRunGoTestText each PASS by name. YAML is structurally restricted to run fields; Makefile matches require recipe tabs. Literal matching intentionally does not evaluate shell/Make expressions.
3. TestRepositoryLedgerMatchesCensusAndDocumentation PASS checks the policy/TOML mirror and ownership fields, including the new row.
4. TestRepositoryCountsFileIsCanonicalAndDerivesEveryCountedRow PASS verifies 47 invocations/4 files from the real merged repository: Makefile 38, ci.yml 2, fork-verify.yml 5, nightly.yml 2. The full suite exceeds the acceptance clause's former fast-tier coverage.
5. TESTING.md gains one generated ledger row through the existing render path; repository render comparison and docsync PASS. No render-code change.

## Fast lane and build

policy_lane: PASS — pinned format check; make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks; 13 required Bazel policy/docs/format targets. Nogo also ran across the full build/vet and suite.
drift_lane: PASS — make bazel-sync plus empty git diff/status; seven schema/spec/client/bundle/lockfile targets and OpenAPI breaking check. Each fast lane ran in its own fresh view of the gate merge, with the pinned base supplied through gate-base.sh.
ci_lane_run: n/a — this change reads Makefile/workflow files but edits no CI job, matrix, timeout or required-check list.
heavy_mode: none
failure_attribution: none
waiver_ref: none

## Criterion 3

test_cmd: gate-criterion3.sh, detached as c3-run, remote own-code path. Phases are make check (Bazel //...), acceptance, integration-packages, full integration excluding registered latency tests, and the separate latency leg, followed by the canonical local floor.
test_cmd_scope: full-suite
test_exec_mode: remote-exec (BEP-derived)
test_exec_bep: 499 results, all remote; cached_remotely=0 cached_locally=0 retried=0 other_strategy=0 window_ok=true; all terminal verdicts PASS.
farm_test_seconds: 11195.888 (sum of farm execution time; not elapsed time)

| Phase | Target/shard result | BEP results |
|---|---|---|
| Unit/policy | 289/289 PASS | 375 |
| Acceptance | 7/7 PASS | 24 |
| Integration packages | 36/36 PASS | 85 |
| Full integration | 14/14 shards PASS | 14 |
| Latency | 1/1 target PASS | 1 |

Farm top-level Go result lines: 133491 PASS / 0 FAIL / 359 SKIP. Counts are result lines per log, not distinct tests, and exclude indented subtests. Farm skip inventory includes 71 environment-absence rows and 288 other rows. Every selected environment-absence parent was rerun in the native floor. Final farm-plus-floor counts: 133512 PASS / 0 FAIL / 373 SKIP, over 515 logs. Floor alone: 21 PASS / 0 FAIL / 14 SKIP over 16 logs, plus fixed checks below; these counts are already included in the final union. All four diff-owned tests PASS in the resourcecensus unit log; none SKIP/FAIL/MISSING.

latency_leg: measured, complete, first probe clear; both registered budget tests PASS. Main integration phase ran neither latency test.
rbe_state: rc marker present, kill switch absent, certificate valid through 2027-01-09; candidate and base worker-env pins match.
admission: remote floor25GiB/cap2; admitted57.736GiB after0.001s, no limit override. Native floor retains canonical threshold15/max1800s soft load wait.
fallback_reason: none
owned_topup: [] — no additional owned-test run needed; four fresh farm PASS results
floor_topup: complete; all 13 package groups plus Darwin compile, push-gate-lock self-test (55/55), and local-concurrency self-test (25/25) rc0. Pinned beads own-cache test PASS. Raw native logs and their hashes retained; no test reruns.
skip_inventory: retained at c3-run/evidence/remote/skip-inventory.json and floor-topup.json with per-test reasons. Remaining skips are unchanged platform/opt-in/live-tool conditions (replay/coverage inputs unset, herdr/br unavailable or live opt-in unset, no ambient tmux binding server, optional PostgreSQL), intentional helper/negative-control paths, and documented policy deferrals. They are outside resourcecensus and cannot be reached by this scanner-only diff. Native floor recovered host-dependent locale, namespace, Dolt/metadata, mail and systemd coverage; every owned census test PASS with no SKIP/FAIL/MISSING.
test_log_dir: /var/tmp/deploy-ga-26lie46-2nb40lot/c3-run/evidence/remote/logs; native floor logs adjacent and retained.

LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=17.14 run_max_load=23.55 run_mean_load=21.85 run_readings=7 wait_first_load=70.85 wait_max_load=73.95 wait_mean_load=38.12 wait_readings=61 read_errors=0

load_threshold: 15; load_waited_seconds: 1800; load_wait_timed_out: 1; run_start_load: 17.14; run_max_load: 23.55; run_mean_load: 21.85; run_readings: 7. Canonical ordinary soft wait proceeded on timeout; no threshold/limit change and no failure attributed to load. The earlier load1 snapshot was informational and is not substituted for these five-minute run statistics.

## Integrity and retained setup history

Evidence root: `/var/tmp/deploy-ga-26lie46-2nb40lot`. C6_VERIFIED.json, FAST_VERIFIED.json, FARM_INDEPENDENT_AUDIT.json, RESUME_FARM_AUDIT.json and FINAL_C3_VERIFIED.json retain the checks. All 42 fast and 998 farm raw artifacts were independently rehashed on resume; C6 build/vet hashes remain unchanged. Existing detached runner/watcher were resumed, never duplicated.

Build PASS preceded a remote disk-admission timeout in which vet never ran. Supported owner-only cache release recovered space, and vet-only continuation completed on the same merge. The first fast-lane wrapper later refused the ordinary40GiB floor before any command ran; only unrun lanes resumed after capacity recovered. Refusals/logs remain retained; no failed test was rerun into a PASS. ICU74 check OK, rootless Podman socket configured, pinned test bd v1.3.2 resolved from deps.env. No testcontainer image pin exists in this test tree.

The exact-equality 47/4 ratchet can change if another PR changes invocation lines before merge. All policy expiry dates, including the new row, are 2026-10-31; results here are as of 2026-10-29. These are recorded constraints for the merge authority.

Publication setup disclosure: the deployer initially forced the gate-record commit hook to remote execution without checking the source-only worker-env pin. The source pin is older than the merged tree/base pin, leaving two hook actions queued. The owned publication service was stopped before the docsync test executed, before a commit/push, and without a test verdict. The full gate above is unchanged. Publication continues on the same reviewed source/isolated branch using supported local Bazel hooks under ordinary local admission/load limits; no source re-pin, rebase, hook bypass or gate rerun.

## Publication pre-push attribution

The normal local pre-push run on `4560711a4cbb981b8c1861a09d0be94cb8568293` reported two failing targets, then was stopped to prevent the generic dry-run fallback from repeating a failed test. Its partial output is not claimed green. 158 raw hook logs were copied and hashed before cleanup; cancellation diagnostics from interrupted steps are retained separately from the two completed failing-target reports.

failure_attribution (publication hook only):
- TestWaitTurnEnded/never_ends -> ga-vkhfnj; the 100ms snapshot deadline loses the last-read state under whole-suite load.
- TestRenderOnceRefusesAFIFOParentWithoutBlocking -> ga-vkhfnj; the 10s completion guard expires under whole-suite load.
- TestWriteTokenFileKeepsGroup -> ga-bwvl64t; known local sandbox supplementary-group chown EINVAL.

Clauses 1–4: all three failure sites are mechanically UNCHANGED with zero shared hunks; both trackers were opened and predate this run (ga-vkhfnj 2026-08-29, ga-bwvl64t 2026-10-24), and this deploy's first sightings were appended/read back. Clause3(a) MECHANISM lands: the complete failing test-binary import closures (19 module packages for t3bridge,48 for tokenrender) contain none of the changed non-test package inputs; the only changed production Go package is resourcecensus. No package/path overlap, so clause4 passes. No inconclusive inference, no waiver and no failed candidate rerun. This is the standing pre-push exact-head exception in non-diff-owned-gate-failure.md.tmpl; the final publication head is rebound after this record is committed, with only gate Markdown changed. It does not replace or stitch the completed full criterion3 evidence above.
