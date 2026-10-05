# Release gate: fleet-wide prompt delivery budget doctor check

**Verdict:** **PASS**

Date: 2026-10-04 UTC  
Deploy bead: `ga-vahqmh`  
Reviewed source: `8b91840f53beaaf9fe375d52a51f2caf3c4f8d4c`  
Gate base: `9b19defec38070498387c598b9f1dba5a0b03e37`  
Candidate merge commit: `2939a624e7674cfdbf22437b13dad692c542a61a`  
Candidate merge tree: `fc72c9869f7e5471320dd2d49d3cd314432e9890`

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-4ho07z` closed with `verdict: pass` for the exact reviewed source. |
| 2 | Acceptance criteria met | PASS | Registered, observational `prompt-delivery-budget` doctor check surveys runnable city and rig agents through launch prompt rendering and the shared `promptDelivery` decision. It reports OK, Warning for nudge fallback or rendering trouble, and Error for unsupported oversized argv delivery. Findings include agent, runtime, modes, byte counts and limits without prompt text. It is non-fixing and starts no session. Reviewer verified the acceptance tests and tracked two nonblocking assertion gaps in `ga-u4dq0h`. |
| 3 | Tests pass | PASS (one attributed failure) | The complete 40-job suite finished: 39 jobs passed, one job failed in an unchanged test during external Dolt fixture setup. All 20 diff-owned tests passed by name. The failure is attributed under criterion 3a to pre-existing tracker `ga-cp7r41` with all four clauses documented below. |
| 4 | No high-severity review findings open | PASS | Reviewer reported no blocking or security finding. The two test assertion gaps are P4 in `ga-u4dq0h`. |
| 5 | Final candidate tree clean | PASS | The materialized merge worktree remained clean after build, vet and the suite. Policy and Bazel drift lanes ran in separate fresh tree views with zero modified, untracked or ignored files; the deploy branch is checked clean before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree` succeeded at pinned base `9b19defec38070498387c598b9f1dba5a0b03e37` (tree `fc72c9869f7e5471320dd2d49d3cd314432e9890`), where `go build ./...` and `go vet ./...` passed. Main advanced during the suite. After fetching its then-current tip `9bedc0980d48555d49b195b8ae39a92707b3cbc8`, a final merge-tree recheck also succeeded (tree `f85fb40b82a5ebb269d3ee59c630837a2fb3dc8f`). |
| 7 | Single feature theme | PASS | Eight changed files, all in `cmd/gc`, implement one doctor prompt delivery budget check. |

## Criterion 3 evidence

- `test_cmd`: `make test-local-full-parallel LOCAL_TEST_JOBS=4`, via `load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh` from the materialized merge tree.
- `test_cmd_scope`: full-suite (documented 40-job local target).
- `test_counts`: 102,087 PASS / 1 FAIL / 336 SKIP terminal test lines; 39/40 suite jobs passed, one failed. Full run record: `/var/tmp/gc-heavy-gate/runs/ga-vahqmh.c3v.20261004` (completed, wrapper rc 2 from the one test failure).
- `diff_tests_executed`: 20/20 diff-owned tests identified by `diff-owned-tests.py` against the pinned base and merge head reported PASS by name in the full-suite logs; 0 FAIL, 0 SKIP. The changed doctor-check-name golden consumer also ran through the cmd/gc shards.
- `skip_justification`: All 336 SKIP lines are from tests outside the diff-owned set, including platform-specific, environment-specific and helper cases in the full-scope suite. Every diff-owned test has a PASS terminal line, so no changed behavior is hidden by a SKIP.
- `waiver_ref`: none; gascity has no waiver path.
- `heavy_mode`: none (`heavy-composite-gate.sh classify` returned `mode=none`).
- `ci_lane_run`: n/a; this diff changes no CI job, matrix, timeout or required-check list.
- `policy_lane`: PASS in isolated fresh tree on the candidate commit; format, CI policy, pinned lint 2.12.0, core/native checks, spec sync. Run record: `/var/tmp/gc-heavy-gate/runs/ga-vahqmh.policy.20261004b` (1/1 complete, 0 failed). `GLT_RECORD` pin=resolved=2.12.0, mismatch=false. `FRESH_TREE_RECORD` reports candidate head and no prior modified, untracked or ignored files.
- `drift_lane`: PASS; `make bazel-sync` followed by `git diff --exit-code` in an isolated fresh tree, record `/var/tmp/gc-heavy-gate/runs/ga-vahqmh.drift.20261004b` (1/1 complete, 0 failed). `FRESH_TREE_RECORD` head=2939a624e767, modified=0, untracked=0, ignored=0.
- `test_bd`: Pinned v1.3.1 (`c1c4b642a`) via `deps.env`; the isolation wrapper confirmed the ref match. The full-suite harness uses nested `env -i` wrappers for shard jobs.
- `load_threshold`: 15; `load_waited_seconds`: 1801; `load_wait_timed_out`: 1; `run_start_load`: 29.45; `run_max_load`: 67.02; `run_mean_load`: 42.81; `run_readings`: 123. Wait readings: first 24.39, max 46.12, mean 33.51, count 61; read errors 0. The ordinary gate ran after its bounded wait timed out, as specified.

### Diff-owned test evidence

Full-suite shard logs: `/var/tmp/deploy-ga-vahqmh.20261004/shards-verbose/`. Each row below was found as a terminal `--- PASS:` line in that run; no diff-owned FAIL or SKIP line was found.

| Test | PASS log |
|---|---|
| `TestPromptDelivery` | `PASS@cmd-gc-process-2-of-6.log PASS@integration-packages-cmd-gc-5-of-6.log` |
| `TestPromptDeliveryBudgetCheck_ACPSession_BypassesSizeGuard` | `PASS@cmd-gc-process-5-of-6.log PASS@integration-packages-cmd-gc-2-of-6.log` |
| `TestPromptDeliveryBudgetCheck_CanFixIsFalse` | `PASS@cmd-gc-process-6-of-6.log PASS@integration-packages-cmd-gc-3-of-6.log` |
| `TestPromptDeliveryBudgetCheck_MultiRig_PackDirsScopedPerAgent` | `PASS@cmd-gc-process-2-of-6.log PASS@integration-packages-cmd-gc-5-of-6.log` |
| `TestPromptDeliveryBudgetCheck_MultipleAgents_WorstCaseWins` | `PASS@cmd-gc-process-3-of-6.log PASS@integration-packages-cmd-gc-6-of-6.log` |
| `TestPromptDeliveryBudgetCheck_NilConfig` | `PASS@cmd-gc-process-3-of-6.log PASS@integration-packages-cmd-gc-6-of-6.log` |
| `TestPromptDeliveryBudgetCheck_NoAgents` | `PASS@cmd-gc-process-4-of-6.log PASS@integration-packages-cmd-gc-1-of-6.log` |
| `TestPromptDeliveryBudgetCheck_NoPromptLeakageInDetails` | `PASS@cmd-gc-process-1-of-6.log PASS@integration-packages-cmd-gc-4-of-6.log` |
| `TestPromptDeliveryBudgetCheck_OversizedQuoted_NudgeFallbackRuntime` | `PASS@cmd-gc-process-4-of-6.log PASS@integration-packages-cmd-gc-1-of-6.log` |
| `TestPromptDeliveryBudgetCheck_OversizedQuoted_UnsupportedRuntime` | `PASS@cmd-gc-process-2-of-6.log PASS@integration-packages-cmd-gc-5-of-6.log` |
| `TestPromptDeliveryBudgetCheck_OversizedRaw_NudgeFallbackRuntime` | `PASS@cmd-gc-process-3-of-6.log PASS@integration-packages-cmd-gc-6-of-6.log` |
| `TestPromptDeliveryBudgetCheck_OversizedRaw_UnsupportedRuntime` | `PASS@cmd-gc-process-1-of-6.log PASS@integration-packages-cmd-gc-4-of-6.log` |
| `TestPromptDeliveryBudgetCheck_PromptModeNone_BypassesSizeGuard` | `PASS@cmd-gc-process-6-of-6.log PASS@integration-packages-cmd-gc-3-of-6.log` |
| `TestPromptDeliveryBudgetCheck_RegisteredInDoctorChecks` | `PASS@cmd-gc-process-5-of-6.log PASS@integration-packages-cmd-gc-2-of-6.log` |
| `TestPromptDeliveryBudgetCheck_RenderError` | `PASS@cmd-gc-process-2-of-6.log PASS@integration-packages-cmd-gc-5-of-6.log` |
| `TestPromptDeliveryBudgetCheck_SafePrompt_QuotedThreshold` | `PASS@cmd-gc-process-6-of-6.log PASS@integration-packages-cmd-gc-3-of-6.log` |
| `TestPromptDeliveryBudgetCheck_SafePrompt_RawThreshold` | `PASS@cmd-gc-process-5-of-6.log PASS@integration-packages-cmd-gc-2-of-6.log` |
| `TestPromptDeliveryBudgetCheck_SuspendedAgentSkipped` | `PASS@cmd-gc-process-4-of-6.log PASS@integration-packages-cmd-gc-1-of-6.log` |
| `TestPromptDeliveryBudgetCheck_UnresolvableProvider_DoesNotBlockDeliveryCheck` | `PASS@cmd-gc-process-1-of-6.log PASS@integration-packages-cmd-gc-4-of-6.log` |
| `TestPromptDeliveryOversized` | `PASS@cmd-gc-process-3-of-6.log PASS@integration-packages-cmd-gc-6-of-6.log` |

### Criterion 3a failure attribution

`failure_attribution: TestSweep_ReapsRealDoltDataDirAfterSIGKILL -> ga-cp7r41 | clause 3: a (MECHANISM) — the failing examples/gastown test binary's module import closure, computed with `go list -mod=readonly -test -deps`, contains no cmd/gc package. The failure occurred in external `dolt init` fixture setup before the sweep scenario ran.`

1. Clause 1: The test file `examples/gastown/dolt_orphan_sweep_integration_test.go` is unchanged by this diff; it is not among the 20 diff-owned tests.
2. Clause 2: Open gate tracker `ga-cp7r41` was created 2026-09-06, predates this run, and names this exact test and external `dolt init` killed signature. The 2026-10-04 sighting was appended and verified.
3. Clause 3(a): All changed production inputs are in cmd/gc. The examples/gastown test-binary import closure has no cmd/gc package, so the diff cannot reach the failing fixture setup.
4. Clause 4: The diff touches only cmd/gc; the failing test sits in examples/gastown. No path overlap.

This is this deploy bead's first hit of this signature. Its earlier gate HOLD involved the separate eventkit.lock condition (`ga-aik16g`), which has since landed. No waiver is used.
