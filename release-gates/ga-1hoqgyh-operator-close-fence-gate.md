# Operator close revision-fence release gate

**Verdict:** **PASS**

- Bead: `ga-1hoqgyh`; build `ga-pp5v22v`; review `ga-e8iau08` (round 3 PASS).
- Evaluated: 2026-10-25T18:27:11.085004+00:00.
- Deploy mode: remote; isolated branch: `deploy/ga-1hoqgyh-gate`.
- Reviewed source: `09d36d63a7f9fea745ca63ff045491325c736f94` (resolved as a commit locally; source branch is provenance only).
- Frozen main: `ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083`.
- Tested synthetic merge: `9a1de17a174c492f57a793b40e47bfad190f5576`; tree `0a6d4c2aa2e7972796c66fbe31038c388f0f9de1`.
- Evidence root: `/var/tmp/deploy-ga-1hoqgyh-regate.jtthvksr`; original prior-run evidence remains at `/var/tmp/deploy-ga-1hoqgyh.He5GxFkQ` and receives no credit here.

| # | Criterion | Result and evidence |
|---|-----------|---------------------|
| 1 | Review PASS present | PASS — round-3 reviewer PASS binds to the exact resolved source above. No review carryover or reviewer-added tests. |
| 2 | Acceptance criteria met | PASS — both operator-close regressions and the unchanged controller-bound witness PASS by name in this fresh suite. Code uses eight attempts for operator close and keeps three for controller close and pending-create rollback; exported option docs and unique SESSION-START-021 row are present. The builder's optional 60-run farm probe was not run. |
| 3 | Full-scope tests pass | PASS — canonical five-phase forced remote gate, all 16 native-floor jobs, retained by-name verification and skip audit complete. `126495` PASS / `0` FAIL / `370` SKIP top-level events, including native-floor events once. |
| 4 | No high-severity findings open | PASS — zero blocking/HIGH findings; the review's four non-blocking advisories remain documented in the bead. |
| 5 | Final branch clean | PASS — reviewed source and tested scratch were clean before adding this gate record. Publication requires a clean status after the normal gate-record commit. |
| 6 | Merge state recorded; builds and vets | PASS — clean merge into pinned main; independent `git merge-tree --write-tree` tree equals the normal-hook scratch merge. Full `bazel build //...` and `make vet`/all-target nogo passed; active `.githooks` verified. |
| 7 | Single feature theme | PASS — all five changed paths are under `internal/session/` and implement or verify operator-close revision-fence retries. Ancestry citation and denylist helpers PASS; no stack. |

```text
merge_state: clean (origin/main@ff7194cf9ccf)
merge_validation: merge result built and vetted here; later main movement is governed by merge-time checks
gate_base_kind: current-tip
gate_base: ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083
heavy_mode: none
ci_lane_run: n/a (no CI-config change)
waiver_ref: none
failure_attribution: none (zero failures)
```

The prior landing wait cleared only after the shipped seat-hook remedy `ga-yu01v6x` / tracker `ga-bqsrp4o` landed as `5c28b43e2f0f7baf37914b939175df255ce9b65e`, independently verified as an ancestor of this frozen main. This is a complete fresh gate, including the native floor.

## Test execution evidence

`test_cmd`: `gate-criterion3.sh --repo <tested scratch> --base <frozen main> --bead-json <persisted live bead> --owned <two owned tests plus controller-bound acceptance witness> --run-dir <fresh evidence directory>`, started through `gate-detached-run.sh` as a systemd user service. Exact paths and arguments are retained in `c3/cmd.argv`, `c3/evidence/` and the phase logs.

- `test_cmd_scope: full-suite`
- `test_exec_mode: remote-exec` (derived from BEP; native-floor top-ups are recorded separately).
- `fallback_reason: none`; `criterion3_valid: true`.
- `farm_test_seconds: 9901.978`.
- `admission`: remote class, 25 GiB floor, cap 2, 56.754 GiB free, 0.001 s wait. No local-heavy admission or host-load wait applies to remote phases.
- `rbe_state`: generated rc present, kill switch absent, certificate valid through Jan 9 2027; tested/main worker-env pins equal `sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45`.
- `test_exec_bep`: every result terminal, strategy remote, first attempt, start inside its phase window; no cached results, local fallback or retries.
- `test_log_dir`: `/var/tmp/deploy-ga-1hoqgyh-regate.jtthvksr/c3/evidence/remote/logs`; native originals in `remote/floor-logs`. Native logs are copied into the former and are not counted twice.
- Retention: 994 original Bazel log/XML artifacts, 67540597 bytes, all SHA256-verified. Per-job logs, BEP, native logs and manifests remain retained.

| Phase | Result | Result records | Remote executed | Cached | Retried |
|-------|--------|---------------:|----------------:|-------:|--------:|
| unit | PASS | 373 | 373 | 0 | 0 |
| acceptance | PASS | 24 | 24 | 0 | 0 |
| integration-packages | PASS | 85 | 85 | 0 | 0 |
| integration-full | PASS | 14 | 14 | 0 | 0 |
| integration-latency | PASS | 1 | 1 | 0 | 0 |

The unit phase runs `make check` (287/287 targets); acceptance is 7/7 targets; integration-packages is 36/36 targets. Full integration covers all 14 shards except the two registered latency names, which run in their own subsequent measured phase. All five phase commands force `--nocache_test_results` and `--noremote_local_fallback`:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-1hoqgyh-regate.jtthvksr/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-1hoqgyh-regate.jtthvksr/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-1hoqgyh-regate.jtthvksr/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-1hoqgyh-regate.jtthvksr/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-1hoqgyh-regate.jtthvksr/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

`diff_tests_executed` (owned set derived mechanically with `diff-owned-tests.py`; both tests are newly added):

- `TestOperatorCloseOutlastsAControllerTickBurst`: PASS in unit and integration-packages.
- `TestOperatorCloseStillBoundsAWriterThatWinsEveryFence`: PASS in unit and integration-packages.
- Acceptance witness, unchanged and not diff-owned: `TestCloseBoundsRepeatedAtomicRevisionConflicts`: PASS in both phases.

`owned_topup: []` — no owned test missing, skipped or failed.

`latency_leg`: complete, first attempt, farm probe clear after 20 s (load1 48.52, unchanged threshold 50); both `TestGcBdLatencyBudget` and `TestGcSeatHookLatencyBudget` PASS. Neither ran in the main integration leg. Retained `latency-leg.json` has the probe, measurements and per-name verdicts.

`floor_topup`: complete, 16/16 commands exit 0: every environment-absence package selected by the canonical inventory, beadstest, Darwin filesystem compile, push-gate-lock self-test and local-concurrency self-test. Native top-level events are 21 PASS / 0 FAIL / 14 SKIP, already included in the overall counts.

```text
load_threshold: 15
load_waited_seconds: 0
load_wait_timed_out: false
run_start_load: 11.30
run_max_load: 12.91
run_mean_load: 10.22
run_readings: 42
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.30 run_max_load=12.91 run_mean_load=10.22 run_readings=42 wait_first_load=11.30 wait_max_load=11.30 wait_mean_load=11.30 wait_readings=1 read_errors=0
```

These host-load measurements cover the native floor; the remote admission does not wait on host load.

## Skip inventory and ownership audit

`skip_inventory`: 356 remote inventory entries, including subtests; their exact names, reasons and logs are in `remote/skip-inventory.json`. All 271 distinct inspected source sites report UNCHANGED and shared_hunks=0. The only new test file contains the two owned tests, both PASS; its added helpers are called only by those tests. No owned SKIP/FAIL exists.

The retained audit groups the remote entries as 130 lane/process helpers, 30 tracked existing skip, 47 explicit opt-in/fixture, 64 optional resource absent, 40 platform/privilege, 45 provider capability. Of the remote inventory entries, 145 have an exact-name PASS elsewhere in this run; these are lane or environment differences with retained execution evidence. Other skips are controlled by unchanged platform/privilege checks, subprocess-helper entry points, optional fixture/experiment inputs, explicit quarantines or adapter capability guards. The operator-close attempt bound changes none of those inputs or guards. The blank crash-injection skip reason was independently recovered from its raw log and source: `ga-vnycm2.27`, interrupted abort-scope metadata; that site is also UNCHANGED.

Every environment-absence package was attempted in the required native floor. The 14 remaining native top-level SKIPs are inventoried: replay/simulation inputs unset, live herdr journeys disabled by the native lane, `br` unavailable, and binding tests requiring an enclosing tmux session. They are not owned by this diff and their guards are unchanged. Runtime setup used the isolation wrapper (Gas City reports PASS-THROUGH on its pristine scratch), a working rootless podman socket and the pinned Dolt image; recorded native ICU74 hashes check OK. No overlay, library-path injection or custom tool shim was used.

## Fast lanes and acceptance source checks

`policy_lane: PASS` — pinned golangci-lint 2.12.0 (`resolved=2.12.0`, mismatch=false) format check, full nogo, hook ownership, release-ignore/routed-row/split-topology/residency/core/module/event-export/version guards and the native dependency surface policy. Seven explicit Bazel policy targets passed; cache credit here is limited to fast checks, never criterion-3 test execution.

`drift_lane: PASS` — CI-discovered `make bazel-sync` followed by `git diff --exit-code` and empty tracked/untracked status; seven generated schema/client/bundle/lockfile targets passed. Each lane ran in its own fresh view and left the canonical scratch unchanged. The exact lane commands/results are in `fast-result.json`.

```text
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-1hoqgyh-regate.jtthvksr/merge-scratch head=9a1de17a174c tree=0a6d4c2aa2e7 dir=/var/tmp/gc-fresh-tree.6NA2wq modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge-scratch,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.6NA2wq
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-1hoqgyh-regate.jtthvksr/merge-scratch head=9a1de17a174c tree=0a6d4c2aa2e7 dir=/var/tmp/gc-fresh-tree.jS0bFD modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge-scratch,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.jS0bFD
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-1hoqgyh-regate.jtthvksr/merge-scratch head=9a1de17a174c tree=0a6d4c2aa2e7 dir=/var/tmp/gc-fresh-tree.SG5QL4 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge-scratch,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=ff7194cf9ccf4c1bd2dfcfcad3c8666af7aeb083 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.SG5QL4
```

Acceptance source review: `CommitAsOperator` selects the existing eight-attempt operator bound; `CommitClose` defaults to the unchanged three-attempt terminal bound; rollback explicitly retains that bound. The atomic retry rechecks its guard after each lost fence. The requirement row is unique, and exported-option/commit docs describe the behavior. No new endpoint, config, role or storage capability is introduced.
