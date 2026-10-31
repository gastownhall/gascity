# Release gate: idle on-demand progress-stall recycle (ga-e1m89o)

**Verdict:** **PASS**

The gated source is `520f66cc9a3fa81a874d9bc11a6b9ce08be190aa`. The release gate tested the canonical merge of that source with pinned `origin/main` `adc49b764646bbcf35edd380a197bf5daa3941ba`: merge commit `9915721ee323917a80598b1f48406c877abb47a4`, tree `4d493812578dbf44eed59f40bf292f2f230ead9b`. Main advanced during the run to `2feddeb321f4d11923553176677c01254e367a0c`; a final merge-tree check against that tip is also clean. The suite result refers to the pinned tree, not the later main tip.

| # | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS | PASS | Reviewer bead `ga-fwtt3j` records `verdict: pass` on the exact source SHA above. No review carryover is needed. |
| 2. Acceptance criteria | PASS | The claim-less recycler exempts an idle named `on_demand` seat only when named, routed, pool, ready-wait, and open assigned work are absent. A seat with demand still recycles. Unreadable open work skips destructive recycle with a diagnostic. The exemption has a typed trace reason. Five new regression tests cover idle/no-demand, no reset, no spurious re-wake, open assigned work, and routed demand. Post-deploy live observation is explicitly deferred until the code is deployed. |
| 3. Tests pass | PASS | The documented full-scope `make test-local-full-parallel` ran all 40 jobs on the canonical merge: 40/40 passed. Root test lines: 55,847 PASS / 0 FAIL / 236 SKIP. Including subtests: 100,667 PASS / 0 FAIL / 335 SKIP. All five diff-owned test roots each reported PASS in both process and integration package lanes, with no owned SKIP. `TestTutorial01` PASSed in the required `cmd-gc-process-1-of-6` lane. Pinned test bd v1.3.1 and the rootless Podman socket with cached Dolt images were verified. Logs: `/var/tmp/ga-e1m89o-regate-20261003/shards/` and `/var/tmp/gc-heavy-gate/runs/ga-e1m89o.c3-20261003/units/`. |
| 3b. Policy/lint | PASS | Fresh-tree, pinned-base run `/var/tmp/gc-heavy-gate/runs/ga-e1m89o.policy-20261003` exited 0. `lint-affected` selected `./cmd/gc`, 0 issues, golangci-lint pin/resolved 2.12.0. Formatting, CI policy, gomod, core and native dependency, event export, routed and split test rows, residency, docs, and native DoltLite checks passed. FRESH_TREE_RECORD modified=0 untracked=0 ignored=0; GATE_BASE_RECORD pinned the base above. |
| 3c. CI config | PASS | No CI job, matrix, timeout, or required-check config changed. |
| 3d. Heavy package | PASS | `heavy-composite-gate.sh classify` returned `mode=none`. |
| 4. High review findings | PASS | Reviewer recorded no high security, style, or spec findings. |
| 5. Clean final tree | PASS | Canonical merge scratch and the assigned role worktree were clean at gate completion, before this untracked gate record was created. |
| 6. Clean merge and build | PASS | `git merge-tree --write-tree` exited 0; scratch HEAD tree matched the expected tree. `go build ./...` and `go vet ./...` passed on the canonical merge. Final current-main merge-tree also exited 0. |
| 7. One feature theme | PASS | Candidate diff contains only claim-less progress-stall recycling for idle named `on_demand` sessions and its tests/trace reason. |

`test_cmd_scope: full-suite`
`test_cmd: load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- env LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m GOFLAGS=-v TMPDIR=/var/tmp/gotmp DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 make test-local-full-parallel`
`diff_tests_executed: TestReconcileSessionBeads_ProgressStallSkipsIdleOnDemandSeatWithoutDemand PASS; TestReconcileSessionBeads_IdleOnDemandSeatWithoutRecycleCommitsNoReset PASS; TestReconcileSessionBeads_RecycledSeatWithoutResetCommitStaysAsleep PASS; TestReconcileSessionBeads_ProgressStallRecyclesIdleOnDemandSeatWithOpenAssignedWork PASS; TestReconcileSessionBeads_ProgressStallRecyclesIdleOnDemandSeatWithRoutedDemand PASS` (each in process and integration package lanes)
`skip_justification: no diff-owned skips; the remaining skips are existing fast-unit, optional environment, and lane-specific cases, while the process and integration lanes exercised the changed cmd/gc tests and TestTutorial01`
`failure_attribution: none — 0 FAIL`
`waiver_ref: none`
`load_threshold: 15; load_waited_seconds: 811; load_wait_timed_out: 0; run_start_load: 14.97; run_max_load: 32.60; run_mean_load: 22.96; run_readings: 90; wait_first_load: 23.16; wait_max_load: 23.53; wait_mean_load: 19.05; wait_readings: 28; read_errors: 0`
