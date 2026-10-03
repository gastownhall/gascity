# Session restart handoff release gate

**Verdict:** **PASS**

Deploy bead: ga-ho591n. Reviewed source: `3d0cc65e8787625d40e8116e280f0a18a767a460`. Gate base: `680a7a47e0272800650f1a2131acde683979b42b`. Canonical merged commit: `e589680abbcf9bd1d5941ae8617af18b303b7d85`, tree: `ee3a681900a76f38f867ed38f374c1423c35a26c`.

| # | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS | PASS | Review bead ga-l2u9pp records `verdict: pass` on the exact reviewed source SHA, with style, security, spec, and acceptance findings read. |
| 2. Acceptance criteria | PASS | ga-2fpf9z's revised Done-when item 3 matches the reviewed fresh-wake behavior. After stopping a live runtime, both production paths record `state=asleep` in the same patch; the already-dead fall-through remains unchanged. The direct repro, next-tick fresh wake, and live-versus-dead restart tests all pass. |
| 3. Tests | PASS | The documented full 40-job local suite completed on the canonical merged tree. From retained shard logs: 55,633 root PASS, 0 FAIL, 236 SKIP; including subtests 99,947 PASS, 0 FAIL, 335 SKIP. All three diff-owned test roots passed by name in both process and integration shards. |
| 3a. Failure attribution | PASS | No failures occurred; no attribution or waiver is needed. |
| 3b. Policy/lint | PASS | Pinned policy composite passed on a fresh tree with gate base `680a7a47e0272800650f1a2131acde683979b42b`. Changed-path lint selected `./cmd/gc`; formatting, CI policy, dependency, core, native, event-export, routing, residency, and docs checks passed. golangci-lint pin and resolved version both 2.12.0. |
| 3c. CI configuration | PASS | The five-file diff changes no CI configuration. |
| 3d. Heavy package | PASS | `heavy-composite-gate.sh classify` reported `mode=none`. |
| 4. HIGH findings | PASS | Review ga-l2u9pp has no unresolved HIGH finding. |
| 5. Clean branch | PASS | Canonical merged scratch checkout is clean, `git diff --check` is clean, and the isolated deploy branch is clean after this gate commit. |
| 6. Main compatibility | PASS | `git merge-tree --write-tree` succeeded at the pinned base and matched the canonical materialized tree `ee3a681900a76f38f867ed38f374c1423c35a26c`. `go build ./...` and `go vet ./...` both exited 0 there. Main later advanced to `d140d3835ce089d73691c46a8a534aa229573920`; fresh merge-tree also succeeded at tree `0086cd97cace9dcbd8cc8307f4069589d1e7bb79`. None of the intervening main commits touches the five reviewed files. |
| 7. Single theme | PASS | Two production paths and three test files under `cmd/gc` address the same stopped-runtime session handoff defect. |

`test_cmd: GOFLAGS=-v GO_TEST_TIMEOUT=30m TMPDIR=/var/tmp/gotmp LOCAL_TEST_LOG_DIR=/var/tmp/ga-ho591n-regate.Bh0Yls/shards make test-local-full-parallel LOCAL_TEST_JOBS=4` through `gate-detached-run.sh`, `load-gate-run.sh` and `isolated-test-run.sh`.

`test_cmd_scope: full-suite`

`test_run_dir: /var/tmp/ga-ho591n-regate.Bh0Yls/full-suite-counts`

`test_checkout: /var/tmp/ga-ho591n-regate.Bh0Yls/shards (logs); /var/tmp/ga-ho591n-merge.pHHkg2 (canonical merged checkout)`

`test_bd: pinned v1.3.1, c1c4b642ac1c, match`

`test_counts: root PASS=55633 FAIL=0 SKIP=236; including subtests PASS=99947 FAIL=0 SKIP=335; 40 shard logs`

`diff_tests_executed: PASS TestPhantomReplacementSessionKeyRepro; PASS TestReconcileSessionBeads_FreshCycleWakesReliablyNextTick; PASS TestReconcileSessionBeads_RestartRequestSetsAsleepOnlyWhenLiveRuntimeKilled (both subtests). Each root passed in cmd/gc process and integration package shards.`

`skip_justification: 236 root skips are in tests untouched by this diff; zero skips among all 28 test roots in the three changed test files. The skipped roots are pre-existing fast-tier, platform, host-capability, and opt-in external integration cases. No changed test body, skip condition, or shared helper was skipped.`

`waiver_ref: none`

`load_threshold: 15`

`load_waited_seconds: 1350`

`load_wait_timed_out: 0`

`run_start_load: 14.78`

`run_max_load: 22.69`

`run_mean_load: 19.00`

`run_readings: 24`

`wait_first_load: 23.56`

`wait_max_load: 28.22`

`wait_mean_load: 22.00`

`wait_readings: 46`

The first fresh full-suite run also passed all 40 jobs but its default success cleanup removed per-test logs. The identical counted run above retained the logs and passed all 40 jobs. Policy log: `/var/tmp/ga-ho591n-regate.Bh0Yls/policy.log`.
