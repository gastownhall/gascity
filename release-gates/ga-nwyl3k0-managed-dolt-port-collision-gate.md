# Integration fixture managed Dolt port collision release gate

**Verdict:** **PASS**

- Deploy bead: ga-nwyl3k0; build: ga-pxhsn5u; review: ga-e4zwvdq.
- Reviewed source: `19596d7b6ef7bfbabc0f02f7fabd54c1a8a78f29` (resolved with git, never a branch-tip substitute).
- Deploy branch: `deploy/ga-nwyl3k0-gate`, cut at the reviewed source; gate record is the only deployer addition.
- DEPLOY_MODE: remote; BASE_REF: origin/main.
- Frozen base: `fec82025d24392176eef9153ebf7af8759c6ef0c`; unchanged on fetch before publication.
- Tested merge commit: `f2067bcab7bdf7785bd47c77ddbbe56c7959a08c`; tree: `bec38e27b4d91c87145b9f2987982e0d24d92391`.
- Evidence retained: `/var/tmp/deploy-ga-nwyl3k0.RYerclgL`; context and raw logs remain there.
- Audit date: 2026-10-17T21:46:42.003689+00:00.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — live review ga-e4zwvdq records `verdict: pass` and the exact resolved source. No carryover or added-test substitution. |
| 2 | Acceptance criteria met | PASS — both new tests, all seven helper callers and the resource census test pass by name. Independent private-namespace collision/control experiment verifies recovery versus failing frozen base. Details below. |
| 3 | Tests pass | PASS — full four-phase `gate-criterion3.sh` chain, 490 fresh remote results, no cached test result, retries or local fallback; native floor complete. Aggregate 116,873 PASS / 0 FAIL / 370 SKIP top-level result events. |
| 4 | No high-severity findings open | PASS — unresolved HIGH findings: 0. Reviewer noted only missing test-function comment consistency nits. |
| 5 | Final branch clean | PASS — source checkout and tested merge scratch clean; this gate record is the only added file. Publication verifies clean status after the record commit. |
| 6 | Merge state and build/vet | PASS — clean merge materialized on frozen current main, build/nogo and vet completed successfully. Same merge tree independently recomputed during final audit. |
| 7 | Single feature theme | PASS — source changes only `test/integration/helpers_test.go`: fixture recovery and diagnostics for managed Dolt port collisions. Both source commits cite build ga-pxhsn5u; ancestry scope guard passes. |

merge_state: clean (origin/main@fec82025d24392176eef9153ebf7af8759c6ef0c)
gate_base_kind: current-tip
merge_validation: tested materialized merge result; MPR owns merge-time freshness and exact-head merge.
criterion-6-merge-validated: PASS

## Acceptance evidence

The build bead's five done-when requirements are verified independently:

1. `TestIsBDProxiedPortCollisionInitFailure` and `TestResetBDProxiedScopeRemovesOnlyBeads` pass under the integration tag. Predicate cases cover ping/timeout plus the Dolt bind signature, missing log, missing bind signature and unrelated failure; reset preserves `.gc` and `city.toml`.
2. The collision experiment binds another loopback listener to the freshly chosen backend port. `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` passes on the candidate, prints the lost-port retry, `server.log`'s `already in use` evidence and recovery message. The untouched frozen base fails the same experiment with `unexpected EOF`, `uow: ping db` refused and a captured Dolt bind failure. That intentional red control is acceptance evidence, not a failing candidate gate.
3. Candidate without the listener passes and emits neither lost-port nor retry output. Each case used private PID/network/mount namespaces. Raw test2json, exit files, Dolt/proxy logs, binaries, harness and SHA256SUMS remain under `collision-evidence/`; `collision-results.json` verifies all three cases.
4. `make check` passes, including `TestRepositoryLedgerMatchesCensusAndDocumentation`, with no census/ledger change. Both integration tiers complete. `make bazel-sync` followed by `git diff --exit-code` and empty all-files porcelain passes. Format, build/nogo and vet pass.
5. The source subject cites `(refs ga-pxhsn5u)`; its body names tracker ga-86zysul and upstream beads#7184.

## Full-suite and fast-lane evidence

`test_cmd_scope: full-suite`

`test_cmd`: standard `gate-criterion3.sh` on the tested merge, using these unfiltered phases:

- `make check` (Bazel unit, nogo, policy, generated and dashboard checks; 283 targets).
- `bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` (7 targets).
- `bazel test --config=integration //test:integration_packages` (30 targets).
- `bazel test --config=integration --test_arg=-test.v //test/integration:integration_test` (all 14 shards; no smoke or name filter).

All four phases add `--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1` and a phase-specific BEP file. These cover the required Bazel unit/integration lanes and BUILD-sync check in `.github/workflows/bazel.yml`; acceptance and full evidence-only integration were also run. No workflow, job matrix, timeout or required-check configuration changes in this diff.

| Phase | Fresh remote results (including shards) | PASS events | FAIL events | SKIP events |
|---|---:|---:|---:|---:|
| unit | 371 | 69,241 | 0 | 256 |
| acceptance | 24 | 410 | 0 | 13 |
| integration-packages | 81 | 47,010 | 0 | 73 |
| integration-full | 14 | 191 | 0 | 14 |
| Native floor | 16 successful steps | 21 | 0 | 14 |

Counts are top-level `--- PASS/FAIL/SKIP` events summed across retained jobs, including repeated tests across configurations; subtests are excluded. `gate-test-evidence.py` independently re-read 506 logs and verified both owned tests plus eight additional acceptance names. A Go-only invocation is not used as full-suite evidence.

policy_lane: PASS — fresh view via `gate-base.sh run`; pinned golangci-lint `make fmt-check`, release/routed/split/residency/hook/CI-policy guards, `make lint`, plus 11 policy/format/dependency/wire targets. Pin/resolved version both 2.12.0, mismatch=false.

drift_lane: PASS — separate fresh view; `make bazel-sync`, clean diff and porcelain; six spec/schema/dashboard/generated-client/lockfile drift targets. OpenAPI compatibility check also passed in a separate fresh view. No fast lane wrote the suite scratch. These completed before runtime setup and the suite.

ci_lane_run: n/a (no CI-config change).
heavy_mode: none (mandatory classifier: `shard_required=- watch=-`).
failure_attribution: none; policy_attribution: none; waiver_ref: none (no waiver path on gascity).

## By-name test audit

Exactly two tests are diff-owned (`owned.tsv` and `owned-explain.txt`). The helper-only shared hunks affect the seven callers listed here; they also passed. The final entry verifies the unchanged resource census.

```text
  TestIsBDProxiedPortCollisionInitFailure PASS (integration-full-c9e44143ff02c6cd1f9f.log)
  TestResetBDProxiedScopeRemovesOnlyBeads PASS (integration-full-e914b9bbd3c7222e0ecf.log)
  TestAdoptPRFormulaCompileAndRun PASS (integration-full-c9e44143ff02c6cd1f9f.log)
  TestPersonalWorkFormulaCompileAndRun PASS (integration-full-e914b9bbd3c7222e0ecf.log)
  TestAdoptPRFormulaRetriesTransientReviewerStep PASS (integration-full-caf9552c6795b8d49441.log)
  TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries PASS (integration-full-f7217e413eb188f8ffa6.log)
  TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash PASS (integration-full-c8d8bacbe500deacd942.log)
  TestReviewLoopApprovesInIterationTwoWithBodyRetry PASS (integration-full-a8d16ec096a75542e5fb.log)
  TestGcBdLatencyBudget PASS (integration-full-1f156f3a859a93c84364.log)
  TestRepositoryLedgerMatchesCensusAndDocumentation PASS (unit-776861a2f8c8f5484f43.log)
```

diff_tests_executed: TestIsBDProxiedPortCollisionInitFailure PASS; TestResetBDProxiedScopeRemovesOnlyBeads PASS; no owned SKIP/FAIL/MISSING.

test_log_dir: `/var/tmp/deploy-ga-nwyl3k0.RYerclgL/c3-evidence/remote/logs`

## Execution integrity, native floor and skips

- test_exec_mode: remote-exec, derived from BEP; no mixed suite or fallback.
- test_exec_bep: `c3-evidence/remote/{unit,acceptance,integration-packages,integration-full}.bep.json`. Recorded per-phase counts 371/24/81/14; every result strategy=remote, no cached flags, attempt=1, start within that phase's window, matching terminal summary/result targets. Read-only predicate re-audits are retained as `*-bep-audit.json`; all errors arrays empty.
- rbe_state: rc present, kill switch absent, certificate expires 2027-01-09; tree/main worker environment both `sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0`.
- fallback_reason: none.
- admission: remote, 25 GiB floor, cap 2; observed free 87.667 GiB; wait 0.001 seconds. Remote phases require no local load admission.
- farm_test_seconds: 7929.278.
- owned_topup: none — both owned tests passed in the full remote integration output.
- floor_topup: complete; 13 package/test groups plus Darwin fsys compile and two runner self-tests, all exit 0. Includes beadstest and every environment-absence remote SKIP selected by package (parent selected for subtests). Manifest: `c3-evidence/remote/floor-topup.json`; native logs retained under `floor-logs/` and copied into the aggregate logs directory.
- skip_inventory: 356 remote records (71 environment-absence, 285 other; includes subtests and repeated configurations); full names/reasons in `c3-evidence/remote/skip-inventory.json`. The 370 aggregate SKIPs above count only top-level events and also include the native floor.

skip_justification: None of the skipped tests is owned by this diff. No skipped test is in the changed helper file. The change is confined to the integration test binary and cannot affect other packages' test binaries. Its changed helpers reach only the seven callers above, all of which passed; they cannot reach the remaining integration skips. All 14 integration skip sites were checked with `diff-owned-tests.py --site` and returned UNCHANGED (`integration-skip-sites.txt`). They concern the existing bd-store skip, opt-in persistence tests, unrelated async unregister timing, platform/privilege checks and the systemd test that subsequently passed in the native floor.

The native floor recovered missing zsh/locale, bd/module-cache, MCP script, mount-namespace and systemd coverage where available. Remaining native skips are opt-in replay/simulation inputs, live herdr journeys, missing br/Postgres setup, outside-tmux binding tests and deliberate provider-unavailable conformance subtests. Other remote skips are unit-lane process guards covered by the integration lane, opt-in live/legacy/registry journeys, helper subprocess entry points, platform/privilege restrictions, existing placeholders and provider contract capability cases. These unchanged conditions do not bear on this fixture recovery. No omitted or skipped owned test is excused by this inventory.

## Host-load evidence

Local work used the isolation wrapper and bounded load waits; all soft waits timed out and proceeded under the ordinary non-heavy rule. Remote execution does not use this host-load wait. Load samples are retained in the corresponding raw logs.

| Local step | load_threshold | load_waited_seconds | load_wait_timed_out | run_start_load | run_max_load | run_mean_load | run_readings |
|---|---:|---:|---:|---:|---:|---:|---:|
| policy | 15 | 1801 | 1 | 47.66 | 55.33 | 52.84 | 8 |
| collision experiment | 15 | 1802 | 1 | 103.12 | 124.50 | 107.10 | 19 |
| native floor | 15 | 1801 | 1 | 86.10 | 114.93 | 101.60 | 15 |

## Publication boundaries

This record adds evidence only. No source code was edited by the deployer. Both detached evaluation services completed successfully and are inactive; no phase was relaunched on resume. Opening the PR requires clean status and the reviewed source as ancestor. Clearance is posted only on the verified PR head containing this record, before a peek-verified merge-request to mayor for MPR. The deployer does not merge.
