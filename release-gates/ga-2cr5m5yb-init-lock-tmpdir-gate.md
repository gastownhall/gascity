**Verdict:** **PASS**

# Release gate: init-lock directory independent of TMPDIR

Bead: ga-2cr5m5yb. Build: ga-936yiczx (refresh of ga-d02ww0). Review: ga-7n12zri2.

Reviewed source: `4f4feb5f086a07812f0d4b203d779db466c4821b`.
Gate base: `887a253144d46bb177f977749eb6869b12e4aec4`.
Merge tree tested: `0de05282df6899600d9da946ade7a81663435302`.
Evidence root: `/var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm`.

The repository has no `docs/PROJECT_MANIFEST.md`; commands come from `TESTING.md`, `Makefile` and the merged CI workflows.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS present | PASS. Review ga-7n12zri2 is closed with verdict `pass` on the exact source above. This is a fresh review of the refresh; no carryover shortcut. |
| 2 | Acceptance criteria met | PASS. The prepared PR body carries the required enumeration and diagnostic excerpts. Acceptance map below; owned tests passed in both unit and integration configurations. |
| 3 | Tests pass | PASS. Complete fresh suite, local floor and required-job accounting below; zero failed tests. Native Windows tests are deferred to PR CI. |
| 4 | No open high-severity review findings | PASS. Review has no blockers or unresolved HIGH findings. The predictable default path's local-user hardening limit is explicitly outside this change. |
| 5 | Final branch clean | PASS. Source checkout clean before this record; the gate record is the sole deploy addition. |
| 6 | Merge state recorded; build and vet | PASS. Clean merge result builds via Bazel with nogo, which includes vet analyzers. `criterion6` exit 0. |
| 7 | Single feature theme | PASS. Four source commits, twelve changed files: agreement on init-lock paths, corresponding test isolation, and operator troubleshooting. Original-build citations ga-d02ww0 are accepted alongside refresh ga-936yiczx. |

merge_state: clean (origin/main@887a253144d46bb177f977749eb6869b12e4aec4)
gate_base_kind: current-tip
gate_base: 887a253144d46bb177f977749eb6869b12e4aec4
gate_head: 4f4feb5f086a07812f0d4b203d779db466c4821b
merge_validation: merge result built and vetted here
ancestry_scope: PASS (ga-2cr5m5yb, ga-936yiczx, ga-d02ww0); no stack base
waiver_ref: none
heavy_mode: none
ci_lane_run: n/a (no CI job, matrix, timeout or required-check change)

## Acceptance evidence

- The fallback assignment is `/tmp/gc-beads-bd-init-locks-$(id -u)` and does not read `TMPDIR`. Override precedence, empty-as-unset behavior, timeout defaults and both lock errors are unchanged. The rewritten pin test uses a recording mkdir wrapper and stub flock; it neither removes nor truncates host lock files.
- `TestMain` exports one directory under the process's temporary root. `sanitizedBaseEnv` restores that directory after stripping ambient `GC_*`, before explicit extras, and refuses an unset run directory. Its regression test checks one absolute value, repeat-call stability, host-path exclusion and explicit/empty overrides; the second new test checks the export.
- The three builders that bypass both mechanisms pin their own directory: the managed wait template, managed maintenance helper and acceptance `NewEnv`. The existing sanitized helper call sites receive no redundant pins. The integration environment's existing pin remains; its edit is a comment correction.
- The new troubleshooting section is titled by symptoms. Its three error excerpts match the script after placeholder substitution; the schema-settle excerpt is the first sentence. Defaults `60000`/`120000`, cursor-progress resets and eight unchanged reads agree with the implementation. The supervisor-environment anchor resolves. No generated config/schema or custom exec-provider reference changed.
- The Unreleased changelog and script header explain the fixed per-uid directory. Different OS users must explicitly agree on a writable directory; private `/tmp` namespaces still cannot share a local lock.
- Historical mutation evidence and dynamic enumeration are in the review. The PR body carries the required two slow-path probe PASS lines, explicitly attributed to the reviewer. It explains testscripts' early `GC_DOLT=skip` exit, the stale-binary guard and the provider-owned no-lock dispatch. It makes no exhaustive site-count claim and explains why `TESTING.md` is unchanged.

## Fast lane and isolation

fast_lane: PASS 2026-11-25T23:03:32Z merge_head=0de05282df6899600d9da946ade7a81663435302
policy_lane: PASS — go_format_test, four make-check shell guards and full make lint nogo //...; exit 0
drift_lane: PASS — make bazel-sync with remote flags, then git diff --exit-code; exit 0

Completed before any suite setup or launch:

- `bazel build --config=remote-exec --noremote_local_fallback //cmd/gc:gc //cmd/gc-osc7501:gc-osc7501`: PASS, exit 0; `criterion6`.
- `bazel test --config=remote-exec --noremote_local_fallback --config=ci //:go_format_test`: PASS; then `make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary` and full `make lint` with remote nogo flags: PASS, exit 0; `policy`.
- `make bazel-sync BAZEL_SYNC_FLAGS='--config=remote-exec --noremote_local_fallback'` then `git diff --exit-code`: PASS, exit 0; `drift`.
- Policy and drift each ran through `fresh-tree-run.sh`, `gate-base.sh run` and `isolated-test-run.sh --rbe`. Their FRESH_TREE_RECORD names distinct fresh views at merge head `0de05282df68`, tree `013007163a9c`, with zero tracked/untracked modifications. GATE_BASE_RECORD pins the base above. Neither lane modified the suite scratch.
- Known-red precheck: proceed, nothing known red; tracker ga-1ptv4jey's routed-row guard passes here. ICU 74 check: OK, recorded provenance. Rootless Podman socket probe: OK. The wrapper pins bd v1.3.4 and declines interactive credential requests.
- Every long run uses a systemd service and survives session recycle. Logs remain under the evidence root. `make check-hooks` confirms `.githooks` is active.

## SKIP verification

Pinned-base diagnostics are supplementary evidence, not substitutes for the full suite:

- cmd/gc unit: 189/189 SKIPs pre-existing; new=0, reason-changed=0, diff-owned=0, no-reason=0. Same 154 parent names selected at the base. Proof: `cmd-gc-unit-skip-compare.txt`.
- Acceptance: 14/14 SKIPs pre-existing; all corresponding fields zero. Proof: `acceptance-skip-compare.txt`.
- cmd/gc integration: 68/68 SKIPs pre-existing; all corresponding fields zero. Proof: `integration-packages-skip-compare.txt`. An initial diagnostic omitted nested results; the parent-name diagnostic supplies them. No failed test was retried. Remote worker account-only differences are disclosed by COMPARE-NOTE.
- Shared hunks are inventoried in `diff-owned-explain.txt`: the pin-test import/comment, wait template, test-control helper, sanitized helper, TestMain, managed maintenance helper and integration environment comment. The base comparisons cover the affected packages; they introduce no new SKIP condition. Other skips and local floor coverage are recorded with the completed execution evidence.

## Full-suite execution

test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_log_dir: /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/remote/logs
test_counts: PASS=159279 FAIL=0 SKIP=618 (top-level result lines across 571 logs; repeated configurations count separately)
failure_attribution: none (no failed tests)
fallback_reason: none
farm_test_seconds: 15123.428
owned_topup: none required
floor_topup: complete; 16/16 steps exit 0
delta_gate: shadow; decision=full; reason=not-gate-pr; applied=false

Phase results (all remote, first attempt, in-window, with neither cache flag):

| Phase | Results | Remote executed | Cached | Retried |
|---|---:|---:|---:|---:|
| unit | 427 | 427 | 0 | 0 |
| acceptance | 25 | 25 | 0 | 0 |
| integration-packages | 87 | 87 | 0 | 0 |
| integration-full | 15 | 15 | 0 | 0 |
| integration-latency | 1 | 1 | 0 | 0 |

Runner invocation:
```bash
env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-2cr5m5yb GC_GATE_DELTA=shadow GC_GATE_LOAD_DEFER_MAX=3 DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/gc-merge-validate.FQZ1iI --base 887a253144d46bb177f977749eb6869b12e4aec4 --bead-json /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/bead.json --owned /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/diff-owned.txt --run-dir /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence
```

Actual phase flags:
```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

diff_tests_executed:
  TestGcBeadsBdInitKeepsItsDefaultInitLockPerOSUser PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestMainExportsThePerRunInitLockDir PASS (integration-packages-b72a67a2f9444af705e3.log, unit-f679cd2551fcc64525aa.log)
  TestSanitizedBaseEnv_PinsInitLockDirToThePerRunDirectory PASS (integration-packages-830685c60d0c2dd88567.log, unit-10cc7795c0ec199a5530.log)
GATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name


The unit phase runs `make -k check`; acceptance runs Tier A plus its separate targets; integration-packages runs the complete tagged package suite. Full integration excludes only the two latency-budget names, which run in the following measured latency phase. The local floor runs the fixed beadstest, Darwin compile and runner self-checks plus every environment-absence selection. Its per-step commands and outcomes are retained in floor-topup.json. The runner launch and environment are retained in criterion3/cmd.argv; no hand-selected subset replaces any suite phase.

skip_justification: unchanged platform/opt-in/fixture gates, with the affected-package base comparisons above. Remote inventory contains 514 other and 90 environment-absence distinct named SKIPs (including subtests), a separate denominator from the repeated top-level event count. Every environment-absence selection received the required local floor. Its unowned SKIPs remain inventoried under the remote criterion-3 contract; none is an owned test. In particular, the local live-herdr check reaches the unchanged unit-lane opt-in gate, whereas the remote worker reports herdr absent. These are recorded as distinct environment reasons, not falsely paired. Of 36 cmd/gc floor SKIPs, 35 structural reasons match the pinned base; this one opt-in gate is read in unchanged herdrtest.SkipReason/RequireLive. The integration floor has zero SKIPs.

Latency leg: measured, complete, attempt 1, clear probe; TestGcBdLatencyBudget PASS and TestGcSeatHookLatencyBudget PASS. Farm probe waited 50 s; window readings=7, maximum load1=43.16, threshold=50, readings over threshold=0. Neither budget test ran in the full-integration phase.

Local load evidence (raw, including the run and the wait separately):
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=19.13 run_max_load=24.06 run_mean_load=15.73 run_readings=49 wait_first_load=19.13 wait_max_load=19.13 wait_mean_load=19.13 wait_readings=1 read_errors=0
```

Compact execution record:
```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/remote","gate_base":"887a253144d46bb177f977749eb6869b12e4aec4","gate_head":"4f4feb5f086a07812f0d4b203d779db466c4821b","admission":{"class":"remote","floor_gib":25.0,"free_gib":52.824344635009766,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"887a253144d46bb177f977749eb6869b12e4aec4","head":"4f4feb5f086a07812f0d4b203d779db466c4821b","merge":"0de05282df6899600d9da946ade7a81663435302","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"863b7cba100f62ec90e39578bc3d2f51818a11d095449e92b0cead8ac68ddd51"},"test_exec_bep":{"unit":{"results":427,"remote_executed":427,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":87,"remote_executed":87,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":15,"remote_executed":15,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":15123.428,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=7041e58bf7cf14360855d1b0246f923ff113bebe9eed60df14d6696752707b27"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":104.133,"evidence_errors":[],"probe":["GATE_FARM_PROBE waiting waited=0s load1=93.44 load5=100.10 load15=52.63 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE verdict=clear waited=50s load1=43.16 load5=85.28 load15=50.07 cpus=32 threshold=50 max_wait=600s","GATE_FARM_PROBE window readings=7 max_load1=43.16 over_threshold=0 threshold=50"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 35.91 78.73 49.18 17/1148 56203"],"window":{"readings":7,"max_load1":"43.16","over_threshold":0,"threshold":50}}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38935}},"log_dir":"/var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":514,"environment-absence":90},"top_level_counts_by_reason_class":{"other":256,"environment-absence":43},"tests":{"elided_array":{"items":604,"bytes":218523}}},"test_cmd":{"elided_array":{"items":5,"bytes":2074}},"test_counts":{"PASS":159279,"FAIL":0,"SKIP":618},"diff_tests_executed":"diff_tests_executed:\n  TestGcBeadsBdInitKeepsItsDefaultInitLockPerOSUser PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-0e1499ad8e8a0a6b9d81.log)\n  TestMainExportsThePerRunInitLockDir PASS (integration-packages-b72a67a2f9444af705e3.log, unit-f679cd2551fcc64525aa.log)\n  TestSanitizedBaseEnv_PinsInitLockDirToThePerRunDirectory PASS (integration-packages-830685c60d0c2dd88567.log, unit-10cc7795c0ec199a5530.log)\nGATE_TEST_EVIDENCE_RESULT ok: 3 diff-owned tests verified by name\n"}
```

## Required CI job accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.FQZ1iI 887a253144d46bb177f977749eb6869b12e4aec4 4f4feb5f086a07812f0d4b203d779db466c4821b fork; exit 0. Workflow blob: fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612. Six LEAF jobs; the check aggregator only rolls up its needs.

ci_required_accounting:

| LEAF job | Status | Evidence |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/ci-accounting/runner-policy-output.txt python3 .github/workflows/scripts/runner_policy.py; exit 0; ci-accounting/runner-policy.log and runner-policy-output.txt. Runner outputs resolve to Blacksmith Ubuntu and Windows as recorded. |
| changes | LOCAL-PASS | Closure tool, exit 0; packs, credential_provider and shared filters all have no match; suite_mode=filtered. ci-accounting/closure.txt. |
| credential-provider-windows | NOT-TRIGGERED | needs.changes.outputs.credential_provider == 'true'; no match (tool row). |
| pack-gate | NOT-TRIGGERED | needs.changes.outputs.packs == 'true'; no match (tool row). |
| windows-lane | LOCAL-PASS | bash /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/ci-windows-lane.sh; exact four run steps from merged ci.yml, including the tools/nogo working directory; exit 0. ci-accounting/windows-lane-output.txt and ci-windows-lane.sh. Separate fresh view, load-gated and isolated wrapper; four CI_STEP_PASS markers. |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | Runner output is blacksmith-4vcpu-windows-2025. The default test binary import closures of ./internal/credentialprovider, ./internal/genshell/... and ./internal/processgroup contain no changed input and go.mod/go.sum are unchanged; windows-native-reach.txt. |

windows_reach: windows-lane-test none (computed for this job, not inferred from the credential-provider helper)
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none

The windows-lane command is retained verbatim in ci-windows-lane.sh: Windows allowlist builds; Windows test compilation via -exec /bin/true; Windows nogo-analyzer build/test compilation; all-package Darwin build/test compilation. These are compile checks, not native Windows runtime results. The latter remain deferred as stated. A gate PASS authorizes PR publication only; merge still requires CI / required and the deferred native Windows job to pass on the exact PR head.

Cross-compile job load evidence:
```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=56.88 run_max_load=56.33 run_mean_load=52.72 run_readings=5 wait_first_load=16.31 wait_max_load=56.88 wait_mean_load=29.66 wait_readings=61 read_errors=0
```
Full execution.json retained: 6066 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/execution.json; SHA256 a41a85ccaf5d392c6a5def0a283ce4cf30c98cf9f706ad12117179fcdfd19681.
Full SKIP inventory retained: 4834 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/remote/skip-inventory.json; SHA256 de90eb3d952625f4b184e11c0eef70d1c1b5411fdc985aa2e5bb09fe14ebc888.
Full local floor commands and results retained: 1056 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/criterion3/evidence/remote/floor-topup.json; SHA256 6f69057ba885676eaf95b80c98b6dc22d47bbd7e8dc53c883e17cb6919936c14.
Full diff-owned/shared-hunk evidence retained: 630 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/diff-owned-explain.txt; SHA256 05bd8bfe711643c979711a585b5b9a948d4d0bd2337aa4cb69215fe84a3cafae.
Full unit SKIP comparison retained: 190 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/cmd-gc-unit-skip-compare.txt; SHA256 c917e4b86e8c551d2999cb7a75ba69d86bd2e8d3cfef8b3e734e3992c14dcb28.
Full acceptance SKIP comparison retained: 15 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/acceptance-skip-compare.txt; SHA256 43df849bc79a10f3f659c0b0772499f8b62078c3e194d44885fca61a7350d785.
Full integration-package SKIP comparison retained: 70 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/integration-packages-skip-compare.txt; SHA256 593b9d0e18bc1b32340c38990cf45d7d1c9c98683470c904587eeb4373884acc.
Full full-integration SKIP comparison retained: 15 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/integration-full-skip-compare.txt; SHA256 b1c1ca6ae988d073cdab3401dc438075e17b85c49abfebfa9cf6b4859c79542f.
Full CI closure retained: 17 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/ci-accounting/closure.txt; SHA256 c1d1186c96a2f2a0f7890a8bcf7104dbb67343e4e8706f4f83a6c8aa2736ef60.
Full Windows native-job reach retained: 3 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/windows-native-reach.txt; SHA256 d49060c97653f38c0d98d3b293fe4575f5885e9ca1dd225c5ac2f75a59b369da.
Full CI cross-compile script retained: 13 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/ci-windows-lane.sh; SHA256 e89a2063545481e2aab14606c6d721196b8f3e14d76c8f8744afb1c0336202de.
Full CI cross-compile output retained: 358 lines; /var/tmp/gc-gate-ga-2cr5m5yb.YW3ptm/ci-accounting/windows-lane-output.txt; SHA256 50f1e68073ac94e3b9141b8f4828213d08114bfbdb45b37c8e97f41610420c0e.
