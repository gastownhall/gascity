# Release gate: keep adopted pool sessions in sync

**Verdict:** **PASS**

- Deploy bead: ga-dw37ir; build bead: ga-vixyn5.1; review bead: ga-yqwqd4.
- Reviewed source: `6e967701a685187bf220092c0737a4961f2820a0`.
- Deploy mode: remote; push remote: fork; planned isolated branch: `deploy/ga-dw37ir-gate`.
- gate_base: `f48cec1a6ecd253c745b6a353369b54ee56db74a`.
- Tested merge: `3ae02bcf88bc6baa5039abdeef37bcec5cbb92c3`; tree: `36227a0c0ad86afcaccc856d10cba8712fce5ca4`.
- Current `origin/main` at final conflict check: `0c5c1411006908867ffc6488d6ab50a31736b013`. Its movement from gate_base changed no candidate file, and `git merge-tree --write-tree origin/main 6e967701a685187bf220092c0737a4961f2820a0` exited 0 with tree `2c77820a946feb68af24a01af9475d5bd29bae6c`. Under the mayor's 2026-10-02 no-overlap ruling, the pinned full-suite evidence carries forward; PR merge-ref CI checks the current base.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-yqwqd4 is closed with verdict PASS on the exact reviewed source. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/session_beads.go` accepts only the canonical singleton pool step-aside name and reuses `poolRuntimeNameSuffix`. An existing named bead continues command and runtime metadata refresh even when a genuine reconfiguration cannot close it; duplicate creation remains gated. All five K88 regression tests passed. The four historical live beads need a post-merge, post-deploy `synced_at` spot-check by the operator; this is a deploy-time observation, not a pre-merge code change. PR #5638 must sequence after this fix. |
| 3 | Tests pass | PASS | The documented complete local suite ran on the materialized merge tree through the load gate and isolation wrapper: 40/40 jobs passed, `GATE_RUN_EXIT rc=0 state=complete`, 99,751 PASS / 0 FAIL / 335 SKIP test and subtest events from all 40 preserved job logs. The five diff-owned K88 tests each reported PASS by name and no FAIL or SKIP. The pinned policy lane and affected-package static lane passed. |
| 4 | No unresolved HIGH findings | PASS | Review ga-yqwqd4 reports no HIGH finding. Its one MINOR finding is a stale explanatory comment above an otherwise passing characterization test. |
| 5 | Final branch clean | PASS | The source checkout and materialized merge checkout are clean. The release gate record is the only planned deploy commit; verify the isolated branch is clean immediately after committing it. |
| 6 | Branch merges cleanly with main | PASS | The pinned gate-base merge-tree and materialized merge build/vet passed; the current `origin/main` merge-tree also exits 0, with no overlap between candidate files and the newer main commit. No self-rebase or source substitution. |
| 7 | Single feature theme | PASS | One named-session adoption and metadata-refresh fix in `cmd/gc/session_beads.go` plus its tests. Stack-aware ancestry scope check passed for ga-dw37ir and its build bead ga-vixyn5.1; no stack or denied path. |

## Criterion 3 evidence

- test_cmd: `load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOFLAGS=-v TMPDIR=/var/tmp make test-local-full-parallel LOCAL_TEST_JOBS=4`.
- test_cmd_scope: full-suite. `TESTING.md` names this target as the fast, process-backed, and integration composite. Runner reports 40 full jobs.
- test_counts: 99,751 PASS; 0 FAIL; 335 SKIP; 40/40 jobs passed.
- diff_tests_executed: `TestK88_SyncRefreshesCommandOnAdoptedPoolNamedBeadWithAssignedWork` PASS; `TestK88_Control_SyncRefreshesCommandWhenSessionNameMatchesSpec` PASS; `TestK88_Control_SyncRefreshesCommandOnPlainPoolBead` PASS; `TestK88_Observation_AdoptedPoolNamedBeadWithoutWorkIsClosedAsReconfigured` PASS; `TestK88_Artifact_AdoptionTickIsLastSyncWrite` PASS. No K88 FAIL or SKIP, including subtests.
- skip_justification: All 335 skips are outside the added or edited K88 test bodies. Preserved logs show platform-only Darwin/unsupported-OS cases, helper-process entry points, optional external/live canaries, ambient-cwd cases already disabled inside test binaries, and real-process tests skipped in the integration unit slice but exercised by the dedicated `cmd/gc` process shards in this same 40-job run. No skip condition was changed by this diff. The changed session-bead behavior is exercised by all five passing K88 tests.
- failure_attribution: none; no test or job failed.
- heavy_mode: none (`heavy-composite-gate.sh classify` on the exact gate base and reviewed source).
- ci_lane_run: n/a; the diff changes no CI job, matrix, timeout, or required-check list.
- waiver_ref: none.
- policy_lane: `fresh-tree-run.sh -- gate-base.sh run -- run-pinned-lint.sh -- make test-ci-policy` PASS, exit 0. `FRESH_TREE_RECORD` clean; `GATE_BASE_RECORD` pinned to gate_base; `GLT_RECORD` pin/resolved 2.12.0, mismatch false. Log: `/var/tmp/ga-dw37ir-policy.log`.
- static_lane: `fresh-tree-run.sh -- gate-base.sh run -- run-pinned-lint.sh -- make lint-affected fmt-check-changed LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=f48cec1a6ecd253c745b6a353369b54ee56db74a` PASS, exit 0; lint-affected selected `./cmd/gc`, 0 issues, format clean, pinned lint 2.12.0. Log: `/var/tmp/ga-dw37ir-static-pinned.log`. A first invocation with the default `LINT_CHANGED_REF=HEAD` selected no files and is not evidence.
- build_vet: `go build ./...` and `go vet ./...` PASS on the materialized merge, recorded before this run in the deploy bead's notes.
- test_bd: `GASCITY_TEST_BD version=v1.3.1 bin=/home/jaword/.local/bd-versions/v1.3.1/bd reports='bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)' ref_check=match:c1c4b642a origin=preexisting pin_source=/var/tmp/gc-merge-validate.ga-dw37ir.3IA5Pi/deps.env displaces='bd version 1.1.0 (0954be416)'`.
- run_dir: `/var/tmp/gc-heavy-gate/runs/ga-dw37ir.c3`; preserved 40 job logs: `/var/tmp/ga-dw37ir-test-logs`.
- load_threshold: 15; load_waited_seconds: 1530; load_wait_timed_out: 0; run_start_load: 14.93; run_max_load: 58.24; run_mean_load: 35.54; run_readings: 96. Wait samples: first 29.48, max 29.48, mean 22.19, 52 readings; read_errors: 0. High load during the run produced no test failure.
