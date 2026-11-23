# Provider credential isolation release gate

**Verdict:** **PASS**

- Bead: ga-4d9tvafd; review: ga-avkj5yfz; build: ga-qnqmzy.2; carried same-theme isolation: ga-5cokvb.1.
- Evaluated: 2026-11-21T15:13:45.964557+00:00
- Reviewed source: `3382c3dfb6ef5548589d255172c8585a16806b5e`.
- gate_base_sha: `9ba0ebde1019026ef23a602787a5bfb868b6b97f`; gate_base_kind: current-tip.
- merge_head: `2910cb5e66d9f9997c9b94b6c92a1a7ce7a341f5`; gate_tree: `0ecaff99f82f2bab727e2bfb3b61644296bf84a8`.
- merge_state: clean (origin/main@9ba0ebde1019026ef23a602787a5bfb868b6b97f).
- merge_validation: hooks-off scratch merge built and vetted; source was never rebased or re-pointed.
- deploy_mode: remote; base_branch: main; push_remote: fork.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS — reviewer ga-avkj5yfz records PASS for the resolved reviewed source and the complete six-commit range. No carryover substitution. |
| 2 | Acceptance criteria met | PASS — five named regression tests and subtests pass. HOME/readiness interpretation is disclosed below. |
| 3 | Tests pass | PASS — fresh full-scope five-phase Bazel chain plus all 16 native-floor jobs; required-CI closure accounted for below. |
| 4 | No high-severity findings open | PASS — zero unresolved HIGH findings; ga-2o2mlmo5 is the disclosed P3 credential-environment follow-up. |
| 5 | Final branch clean | PASS — deploy branch is clean; its only addition over the reviewed source is this gate record. Frozen merge scratch is clean. |
| 6 | Merge state and build/vet | PASS — clean merge against the frozen base; raw build and nogo-vet BEPs each complete SUCCESS with 1,500 successful targets. |
| 7 | Single feature theme | PASS — integration credential isolation only: two test files and their BUILD rule; all six source commits cite the accepted build/carried-theme IDs. |

## Acceptance evidence

1. The comment scopes its guarantee to the Codex/Claude provider homes and documents the readiness-probe boundary. This item is comment-only.
2. realHomeDir uses the passwd entry for the current UID, independently of HOME, and fails closed. TestRealHomeDir_IgnoresHOMEEnv PASS.
3. Empty private CODEX_HOME/CLAUDE_CONFIG_DIR directories, catalog credential stripping, the independent five-name literal floor, and bootstrap inheritance are exercised by the other four owned tests.
4. The requested provider-query narrowing was not implemented. The reviewer explicitly approved the outcome via existing private HOME isolation: the live-contract supervisor uses integrationEnvFor, buildIntegrationEnv invokes isolateGCHomeEnv, and workspaceHomeDir prefers that HOME. TestGCLiveContract_BeadsAndEvents and TestIntegrationEnvForIsolatesHome both PASS. Readiness probes still run against private directories. The residual non-blocking token/XDG follow-up is ga-2o2mlmo5.

## Test evidence

- test_cmd: gate-criterion3.sh --repo <frozen merge scratch> --base 9ba0ebde1019026ef23a602787a5bfb868b6b97f --bead-json <live bead snapshot> --owned owned.tsv --run-dir <fresh evidence directory>. Exact argv: c3-command.json in the retained evidence.
- test_cmd_scope: full-suite; test_exec_mode: remote-exec; fallback_reason: none; heavy_mode: none; waiver_ref: none.
- test_counts: 158183 PASS, 0 FAIL, 617 SKIP. Top-level event lines summed across jobs; subtests are additionally checked by name.
- Commands: make -k check (unit); Bazel acceptance plus acceptance_solo_tests; --config=integration //test:integration_packages; full //test/integration:integration_test with only the canonical latency names split out; those latency tests in their dedicated clear-probe phase. Every remote phase uses --nocache_test_results and --noremote_local_fallback.
- test_exec_bep: unit426, acceptance25, integration-packages87, integration-full15, integration-latency1: 554 remote, uncached, first-attempt results in their phase windows; all terminal PASS. All 1,108 retained log/XML hashes checked independently.
- farm_test_seconds: 12126.852; admission: remote floor25GiB, cap2, free61.796GiB, waited0.001s.
- rbe_state: kill switch absent, RC marker present, worker-env digest equals the frozen base; certificate valid until 2027-01-09. Full evidence: c3-v2-run/evidence/rbe-state.json.
- latency_leg: both canonical latency tests PASS at attempt1 under GATE_FARM_PROBE verdict=clear, load5=43.95, threshold50. Neither ran in the main integration leg.
- floor_topup: 16/16 jobs rc0, including native beadstest, environment-absence top-ups, Darwin compile and both runner self-tests. The systemd-managed drift restart ran natively and PASSed; restart600ms against15s budget.
- load_threshold: 15; load_waited_seconds: 450; load_wait_timed_out: 0; run_start_load: 14.76; run_max_load: 16.75; run_mean_load: 16.10; run_readings: 7. Native floor wait_first/max34.59, wait_mean23.24, wait_readings16. Runtime/trusted-base preflight load is retained separately and is not suite load.
- GATE_ICU74 verdict=OK provenance=recorded; pinned Dolt image/runtime socket and reaper preflights passed before the suite.
- owned_topup: none; every diff-owned test already passed in the full integration phase.
- diff_tests_executed: TestBootstrapDriftEnv_InheritsProviderIsolation PASS; TestIntegrationEnvFor_IsolatedHomesAreNeverTheRealHome PASS; TestIntegrationEnvFor_IsolatesProviderHomes PASS; TestIntegrationEnvFor_StripsProviderCredentialEnvVars PASS; TestRealHomeDir_IgnoresHOMEEnv PASS. All owned subtests PASS; no owned SKIP or FAIL.
- skip_inventory: farm603 (513 other, 90 environment-absence), plus14 native top-level SKIPs. All are non-owned. Site audit finds321 unchanged source sites. Only test/integration can reach the changed test-only helpers; its14 farm SKIPs compare ALL PRE-EXISTING on the same integration target at the frozen base, and its native top-up has zero SKIPs. Other skips are existing opt-in/credential, unsupported capability, synthetic fixture, platform or host-precondition exclusions; the test-only integration helper cannot be imported by those binaries.
- skip_justification: exact reasons, sites, shared-hunk analysis and base comparison are retained under skip-static-proof/, skip-proof/ and the canonical skip-inventory.json. No failing test was retried or attributed.
- failure_attribution: none; ci_lane_run: n/a (no CI workflow/job/timeout/required-check configuration changed). The BUILD shard count15 is the checked separation of heavy integration tests, not a timeout change.
- test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-4d9tvafd/gate-ga-4d9tvafd-20261121-2titmo0r/c3-v2-run/evidence/remote/logs; original logs retained at /var/tmp/gate-ga-4d9tvafd-20261121-2titmo0r/c3-v2-run/evidence/remote/logs.

## Fast lane and required CI

- policy_lane: fresh-tree-run.sh -- gate-base.sh run -- isolated-test-run.sh --rbe -- bash fast-lane.sh policy (exit0), plus format/OpenAPI and trusted-base storeclass commands recorded in fast-verified.json/storeclass-proof/verified.json: PASS — thirteen policy/guard targets, format target, nogo build/vet and strict trusted-base storeclass baseline/growth predicates.
- drift_lane: PASS — make bazel-sync plus empty git diff and seven generated-artifact targets; OpenAPI sync target PASS. Each lane ran in its own fresh view at the same frozen head/tree, before runtime setup and suite launch.
- ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-4d9tvafd-20261121-2titmo0r/merge 9ba0ebde1019026ef23a602787a5bfb868b6b97f 3382c3dfb6ef5548589d255172c8585a16806b5e fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status.
- ci_required_accounting:
  - runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gate-ga-4d9tvafd-20261121-2titmo0r/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit=0; log=/var/tmp/gate-ga-4d9tvafd-20261121-2titmo0r/ci-required/runner-policy.stdout
  - changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-4d9tvafd-20261121-2titmo0r/merge 9ba0ebde1019026ef23a602787a5bfb868b6b97f 3382c3dfb6ef5548589d255172c8585a16806b5e fork; exit=0; log=/var/tmp/gate-ga-4d9tvafd-20261121-2titmo0r/ci-required/closure.stdout
  - credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  - pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
  - windows-lane | LOCAL-PASS | bash ci-required/windows-lane.sh (exact four YAML commands and tools/nogo working directory); exit=0; log=/var/tmp/gate-ga-4d9tvafd-20261121-2titmo0r/ci-required/windows-output.stdout
  - windows-lane-test | DEFERRED-TO-PR-CI | reason=non-linux-runner; Windows-native import reach=none; proof=/var/tmp/gate-ga-4d9tvafd-20261121-2titmo0r/ci-required/windows-reach.json
- windows_reach: none — computed from the actual windows-lane-test allowlist under GOOS=windows/GOARCH=amd64; its native Windows execution remains deferred.
- deferred_to_pr_ci: windows-lane-test (non-linux-runner); not_covered: none; host_gaps: none.
- This PASS authorizes PR-open. Merge still requires CI / required green on the PR head and the deferred native Windows job successful.

## Retained evidence

Full evidence retained at /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-4d9tvafd/gate-ga-4d9tvafd-20261121-2titmo0r.
SHA256SUMS: 2274 files; SHA256 2618554467318191c7e14ee81d955a0b859af31dc70f106f7c692dd938d4d51f.
Independent summaries: remote-independent-audit.json, final-independent-audit.json, c6-verified.json, fast-independent-audit.json, ci-required/accounting.json and skip-proof/verified.json. Raw BEPs and per-job logs remain in the same relative paths. Older partial evidence from the held November9 run was not used.
