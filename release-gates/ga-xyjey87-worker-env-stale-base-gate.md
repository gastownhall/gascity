# Worker environment stale-base release gate

**Verdict:** **PASS**

Deploy bead: ga-xyjey87. Build lineage: ga-s9mdfm / ga-6kdidhx. Review: ga-d2tzyvx.
Reviewed source: fee292391d2fac3fc88ab3384815bd757aef530d.
Deploy branch: deploy/ga-xyjey87-gate.
PR: https://github.com/gastownhall/gascity/pull/8366.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Independent reviewer PASS binds to the exact source. reviewer-added-tests.sh independently returns reviewer-tests files=scripts/rbe_worker_env_drift_test.go for 3cab89576431ebfd85795ef8ce76b5981c13172a -> fee292391d2fac3fc88ab3384815bd757aef530d, build bead ga-s9mdfm. |
| 2 | Acceptance criteria met | PASS | Three-way pin decision, unchanged legacy skip, failure on unreadable base, empty missing file, workflow wiring, docs and resource census independently checked. The three changed tests pass by name; the real PR-event lane also passes. Details below. |
| 3 | CI-equivalent tests and policy pass | PASS | All five fresh farm phases, required native floor, format, nogo/vet, shell guards and generated-file drift pass. The unchanged complexity-baseline failure is attributed with same-run pinned-base proof to ga-q7p6is; its literal command remains red. No waiver. |
| 4 | No open high-severity finding | PASS | Zero unresolved HIGH findings; reviewer advisories a-f are explicitly non-blocking. |
| 5 | Final branch clean | PASS | No implementation edits by deployer. Only this gate record is added; final git status is verified clean after its commit and before publication. |
| 6 | Merge state and build/vet | PASS | Clean canonical merge on the immutable base below; its tree equals git merge-tree output. gc builds, and full-tree nogo/vet passes. |
| 7 | Single feature theme | PASS | One CI worker-env stale-base behavior, with its tests, documentation and resource ledger; no unrelated feature. |

Acceptance evidence:

- TestRBEWorkerEnvDriftPreflight PASS: stale remote fails with its cure; stale non-remote runs; actual pin movement keeps the skip; drift precedence and legacy two-argument behavior retain coverage.
- TestBazelLaneWorkerEnvStepStaleBase PASS, including all seven scenarios and the reviewer-added absent-file case. Unreadable BASE_SHA fails; absent file at an existing commit is empty.
- TestBazelMultiLaneWorkerEnvPreflight PASS: BASE_SHA and third argument wired only to the lane. Coverage/two-argument callers, skip gates, mode/environment keys and fresh-merge remain unchanged.
- Expected-red T2 re-executed at f6f2c3ed58e48eb91a0233bbb0023c9458949cc1: stale remote reports step error <nil>, want failure true, with pin-moved=true/measure=true and shell exit0. The same named test passes in the full fresh source run. Before/after output is in the PR body, with no Closes line.
- bash -n tools/rbe/worker-env-drift PASS. TESTING.md/tool documentation matches the behavior; census checks pass.

merge_state: clean (origin/main@37bd09351ac80604606366872b4efeed63b624b8)
gate_base: 37bd09351ac80604606366872b4efeed63b624b8
gate_base_kind: current-tip
merge_validation: merge result built and vetted here; merge-time freshness remains the merge authority responsibility.
scratch_merge_tree: 5985aaf2f44f7abded7765b7701b4dff1096369c
deploy_mode: remote
heavy_mode: none
waiver_ref: none

Build/vet: bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc; make vet NOGO_BUILD_FLAGS="--keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback" -> PASS.
Policy: full-tree make lint/nogo and check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows, check-residency-boundary -> PASS.
Format: pinned golangci-lint 2.14.0, resolved 2.14.0, mismatch=false; make fmt-check -> PASS.
Drift: make bazel-sync, then git diff --exit-code -> PASS, no generated drift. Fast lanes run in separate fresh views; scratch merge is unchanged.

policy_attribution: make complexity-check -> ga-q7p6is; positive BASE reproduction (clause 3b mirror / proof d), no scanned-path overlap, tracker predates this run.
Candidate/base literal check both rc2; exactly 74 byte-identical finding rows, sha256 2ac4f0a851ec91ed7aae01aa2dcf2f7c69be041f395fc1625349e8e338f43642.
Pinned gocyclo v0.6.0. make complexity-diff against the exact same base returns rc0 / no threshold changes. The scanner excludes test files and internal/testpolicy; this diff changes no scanned production path. No baseline edit or waiver.

Process-tier coverage: TestTutorial01 PASS in the retained integration-packages-aa6cf5c138f0b61b866a.log; the full process-tier test ran, rather than only its fast-tier SKIP. This names the coverage required for internal/** paths by the release-gate conventions.
ci_lane_run: PASS, first real pull_request run https://github.com/gastownhall/gascity/actions/runs/40396599218, attempt1, exact reviewed source.
CI base: 1036b2cb20c15f0a6532fbd04fbb7d63ae42e2b3 (resolved in object store). All four matrix lanes pass worker-env preflight, Set up Bazel and their actual test step. Raw preflight shows matching checkout/default pins, pin-moved=false measure=false. Entire workflow concludes success.

test_cmd_scope: full-suite
test_cmd:
    test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-xyjey87-round3.4p5nxud6/criterion3-ready/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
    test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-xyjey87-round3.4p5nxud6/criterion3-ready/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
    test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-xyjey87-round3.4p5nxud6/criterion3-ready/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
    test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-xyjey87-round3.4p5nxud6/criterion3-ready/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
    test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-ga-xyjey87-round3.4p5nxud6/criterion3-ready/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec

test_counts: {"FAIL": 0, "PASS": 126844, "SKIP": 372}
Native Go-only counts: PASS=21 FAIL=0 SKIP=14; shell self-tests: push-gate-lock55PASS/0FAIL, local-concurrency25PASS/0FAIL. Remaining native skip reasons retained in native-skip-inventory.json.
test_exec_mode: remote-exec
fallback_reason: None
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 24, "results": 24, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 85, "results": 85, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 373, "results": 373, "retried": 0, "window_ok": true}}
farm_test_seconds: 10824.918
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 46.14026641845703, "waited_seconds": 0.002}

diff_tests_executed:
  TestBazelLaneWorkerEnvStepStaleBase PASS (unit-81d8a9af827e251f6b12.log)
  TestBazelMultiLaneWorkerEnvPreflight PASS (unit-271bc77ad6715453dfe1.log)
  TestRBEWorkerEnvDriftPreflight PASS (unit-81d8a9af827e251f6b12.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name

skip_inventory: {"counts_by_reason_class": {"environment-absence": 71, "other": 287}, "top_level_counts_by_reason_class": {"environment-absence": 42, "other": 215}}
Full skip inventory: /var/tmp/gc-ga-xyjey87-round3.4p5nxud6/criterion3-ready/evidence/remote/skip-inventory.json.
Skip justification: unchanged provider-capability checks, fast-tier process tests exercised in integration, absent PostgreSQL DSN, optional br tool and pre-journal binary, and platform/group observations. The required native topups cover the computed environment-absence selection; remaining non-owned skips retain their actual printed reasons. This CI worker-env diff changes none of those paths. All three changed parent tests and their scenarios execute and PASS without a skip or failure.
owned_topup: []
floor_topup: complete=true, steps=16; all commands rc0. Includes beadstest, Darwin compile, runner self-tests and environment-absence package topups.
test_bd: GASCITY_TEST_BD version=v1.3.2 bin=/home/jaword/.local/bd-versions/v1.3.2/bd reports='bd version 1.3.2 (9b65b9e19: HEAD@9b65b9e198ac)' ref_check=match:9b65b9e19 origin=preexisting pin_source=/var/tmp/gc-merge-validate.XaKqE9/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
test_log_dir: /var/tmp/gc-ga-xyjey87-round3.4p5nxud6/criterion3-ready/evidence/remote/logs
native_test_log_dir: /var/tmp/gc-ga-xyjey87-round3.4p5nxud6/criterion3-ready/evidence/remote/floor-logs
latency_leg: complete=true, verdict=measured, attempts=1; TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS under probe clear.

load_threshold: 15
load_waited_seconds: 1801
load_wait_timed_out: 1
run_start_load: 45.59
run_max_load: 65.78
run_mean_load: 59.84
run_readings: 12
Native floor load receipt: threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=45.59 run_max_load=65.78 run_mean_load=59.84 run_readings=12 wait_first_load=30.66 wait_max_load=83.27 wait_mean_load=59.71 wait_readings=61 read_errors=0

Fresh lane receipts:
    FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.XaKqE9 head=b3404ea31ee5 tree=5985aaf2f44f dir=/var/tmp/gc-fresh-tree.tCViZ5 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.XaKqE9,bazel-out,bazel-testlogs
    GLT_RECORD: {"golangci_lint":{"pin":"2.14.0","resolved":"2.14.0","mismatch":false}}
    FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.XaKqE9 head=b3404ea31ee5 tree=5985aaf2f44f dir=/var/tmp/gc-fresh-tree.OIPZJK modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.XaKqE9,bazel-out,bazel-testlogs
    GATE_BASE_RECORD base=37bd09351ac80604606366872b4efeed63b624b8 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.OIPZJK
    FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.XaKqE9 head=b3404ea31ee5 tree=5985aaf2f44f dir=/var/tmp/gc-fresh-tree.3uNniJ modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.XaKqE9,bazel-out,bazel-testlogs
    FRESH_TREE_RECORD from=/var/tmp/gc-merge-validate.XaKqE9 head=b3404ea31ee5 tree=5985aaf2f44f dir=/var/tmp/gc-fresh-tree.xpGQ2E modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-validate.XaKqE9,bazel-out,bazel-testlogs
    GATE_BASE_RECORD base=37bd09351ac80604606366872b4efeed63b624b8 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.xpGQ2E

Evidence directory: /var/tmp/gc-ga-xyjey87-round3.4p5nxud6
Farm raw artifacts: {"bytes": 67669966, "files": 994, "native_steps": 16, "remote_results": 497, "sha256_verified": true}
All retained raw log/XML hashes independently re-verified; native logs retained. No force-push, source rewrite or merge executed.

Round history: both earlier FAIL records are preserved in commits 9820416eb9fe807bca8ad5aadd7ea392c1fa608b (caller-placement failure) and 73626f78b3c226edd8225c69064d4e3000684e91 (attributed unrelated cleanup, incomplete coverage). Mayor disposition gm-wisp-bw9gmn2 / c3403c8d authorized ONE complete fresh round3 on the same reviewed source, with a binding no-fourth-round stop if another unrelated failure cuts it short. All build, fast-lane, farm and native evidence here belongs to round3, wholly under /var/tmp outside any city; no old result was stitched or counted.
Setup receipt: the first round3 invocation was refused by rbe-rc-check.py before bazel test because the caller had made the remote evidence directory0755. It has zero BEP/test records and zero farm seconds and is retained untouched under criterion3/. Corrected0700 directories use the separate criterion3-ready/ run for this complete suite; no test verdict was replayed or discarded.
