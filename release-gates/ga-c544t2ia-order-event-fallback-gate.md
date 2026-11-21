# Order cooldown event fallback release gate

**Verdict:** **PASS**

Evaluated 2026-11-20T17:20:26.834302+00:00. Bead ga-c544t2ia; build ga-2z5y24; review ga-ddo1mjou.
Reviewed source: `1e1893e162f01aab926f41275b7ca70c816d4cd3`. Gate base: `origin/main@af1c2c308aa76afd54b557bba46728a4bb2c0139`.
Tested merge: `d3c1cd9f92c39f576824fa4cf393730b99371d82`; tree `91f1e9772a379c4631ad59a24118760eade4d4b8`. Deploy mode: remote; push remote: fork.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review ga-ddo1mjou records PASS for the exact source. The reviewer-added-tests helper independently returned reviewer-tests files=cmd/gc/order_dispatch_test.go. |
| 2 | Acceptance criteria met | PASS | Since-bound query, rotated-archive boundary tests, rig/city isolation, corrected comment and zero-growth complexity verified below. |
| 3 | Tests pass | PASS | Canonical complete five-phase suite; 548 fresh remote result shards, zero cached/retried results; all nine owned tests PASS by name; all 16 native-floor steps rc=0; required-CI closure accounted below. |
| 4 | No high-severity findings open | PASS | Live review record: no security findings; no HIGH findings; final verdict pass. |
| 5 | Final branch clean | PASS | Source checkout clean; the only deploy commit adds this gate record. Clean tracked/index state is verified after that commit, before push. |
| 6 | Merge state and build/vet | PASS | Clean merge onto pinned base; raw merge-tree equals the tested tree. Whole-tree Bazel build and make lint/nogo-vet ended rc=0 with BEP overallSuccess=true. |
| 7 | Single feature theme | PASS | One order-cooldown fallback theme: one production file and its helper/dispatch tests; no unrelated source, internal prompts or baseline changes. |

merge_state: clean (origin/main@af1c2c308aa76afd54b557bba46728a4bb2c0139)
gate_base_kind: current-tip
merge_validation: merge authority verifies the current merge result before landing.
reviewer_tests_verified: 630d7d784686384621517bb7b8cc976b74423b71 -> 1e1893e162f01aab926f41275b7ca70c816d4cd3 files=cmd/gc/order_dispatch_test.go

## Acceptance evidence

- LastRunFuncWithEventFallback queries order.fired with exact Subject and Since=time.Now()-30 days. The documented constant bounds archive scans and states the long-cooldown tradeoff.
- Fake-provider and real rotated-gzip tests cover events inside and beyond the lookback. The ordinary forward archive reader remains in use.
- Pruned tracking-bead tests hold the matching rig cooldown and allow city/other-rig orders to dispatch independently.
- buildOrderDispatcherFromList now describes the event providers used by current tests.
- Complexity zero-growth (ii): pinned-base and merge check diagnostics are byte-identical; COMPLEXITY_BASE_REF at the pinned base prints complexity: no threshold changes. engdocs/complexity-baseline.json is untouched.

## Test evidence

test_cmd: canonical gate-criterion3.sh, launched once by criterion3.sh through gate-detached-run.sh; the isolation wrapper used --rbe. See retained command and script files below.
test_cmd_scope: full-suite
heavy_mode: none
tests_green: true
test_counts: 157822 PASS, 0 FAIL, 617 SKIP (top-level result lines summed per job; subtests excluded).
waiver_ref: none
failure_attribution: none
ci_lane_run: n/a (no CI configuration change)
test_exec_mode: remote-exec
test_exec_bep: {"unit":{"results":422,"remote_executed":422,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}}
rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
fallback_reason: none
admission: {"class":"remote","floor_gib":25.0,"free_gib":60.50278854370117,"waited_seconds":0.001,"cap":2}
farm_test_seconds: 18070.633
latency_leg: measured on a clear worker probe; both budget tests PASS; the main integration leg excludes them. No latency result is inferred from the full suite.
owned_topup: none needed; all nine owned tests are present in the original unit and integration-package logs.
floor_topup: 16/16 steps complete, rc=0 each; 13 package top-ups, Darwin compile, push-gate-lock and local-concurrency self-tests.
test_log_dir: /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/c3-run/evidence/remote/logs

```text
TestOrderDispatchCityCooldownIgnoresRigScopedEventFallback PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-fd986cf5bab8709b8d45.log)
  TestOrderDispatchRigCooldownHonoursEventFallbackAfterBeadPrune PASS (integration-packages-bc482d2ae768251ad47f.log, unit-1ab526405ace21879f85.log)
  TestOrderDispatchRigCooldownIgnoresOtherScopesEventFallback PASS (integration-packages-18745ddf7d62465654f5.log, unit-d9e667428f972d067bd3.log)
  TestLastRunFuncWithEventFallback_FallsBackToEvents PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)
  TestLastRunFuncWithEventFallback_FallsBackToLatestEvent PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)
  TestLastRunFuncWithEventFallback_FindsEventInRotatedArchiveWithinLookback PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)
  TestLastRunFuncWithEventFallback_IgnoresArchivedEventBeyondLookback PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)
  TestLastRunFuncWithEventFallback_IgnoresEventsBeyondLookback PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)
  TestLastRunFuncWithEventFallback_IgnoresOtherOrderEvents PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)
GATE_TEST_EVIDENCE_RESULT ok: 9 diff-owned tests verified by name
```

skip_inventory: 603 original farm SKIP observations (90 environment-absence, 513 other); all environment-absence parents attempted by the canonical native floor. The native floor has 72 SKIP observations, including subtests.
skip_justification: all owned tests ran and passed. Remaining skips cover unchanged fixture guards, unit/process lane choices, unsupported provider capabilities, platform checks, optional live-service/tool fixtures and explicitly gated manual/legacy/registry tests. The native top-ups attempted absent farm environments; their remaining non-owned skips are inventoried rather than credited as exercised behavior.
skip_site_proof: all 603 original observations resolve to 318 unchanged sites in the merged tree. The nine owned names are identical when derived against the reviewed source and against the tested merge.
skip_comparison: test-binary import closures, including external-test variants and acceptance tags, select the shared-helper reach. All 486 farm observations requiring measurement match named tests at the pinned base under their original lane. All 44 native observations in that reach match the pinned base under their native-floor command and scrubbed environment. No changed reason, new skip, diff-owned skip or unjudged required observation.
shared_hunks: cmd/gc/order_dispatch_test.go adds the rig-order test helper and updates a comment; internal/orders/runtime_helpers_test.go adds io/path imports, fixture constants and the rotated-log helper. Only the new order tests invoke those helpers. No TestMain, init or skip condition changed; measured shared-reach skips are identical at base.

COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestProxyProcessReapRegistryMatchesAnEquivalentSpelling (base 753030732, head 333439264)

Native-floor load (the soft bounded wait timed out; this is recorded, not a failed test):
load_threshold: 15
load_waited_seconds: 1800
load_wait_timed_out: 1
run_start_load: 18.91
run_max_load: 19.50
run_mean_load: 18.33
run_readings: 8

## Required CI accounting

ci_required_closure: python3 packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.gYpoFM af1c2c308aa76afd54b557bba46728a4bb2c0139 1e1893e162f01aab926f41275b7ca70c816d4cd3 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status. check and ci-required are aggregators.

| Job | Status | Command/result |
|---|---|---|
| runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<retained output> python3 .github/workflows/scripts/runner_policy.py; rc=0; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/ci-required/runner-policy-output.txt |
| changes | LOCAL-PASS | ci_required_triggers.py over the merged workflow; rc=0; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/ci-required/closure.txt |
| credential-provider-windows | NOT-TRIGGERED | needs.changes.outputs.credential_provider == 'true'; tool row: credential_provider no match |
| pack-gate | NOT-TRIGGERED | needs.changes.outputs.packs == 'true'; tool row: packs no match |
| windows-lane | LOCAL-PASS | All four job run commands in ci-windows-lane.sh (Windows allowlist build/tests, nogo Windows compile, whole-tree Darwin build/test compile); rc=0; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/ci-required/windows-lane-output.txt |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | runner_windows=blacksmith-4vcpu-windows-2025; reach computed for all packages in .github/windows-lane/test.txt: none; native execution requires Windows |

windows_reach: none (credential provider and the complete native-Windows allowlist test-binary closures; checked for ordinary and integration builds).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
Merge requires CI / required and the deferred native-Windows job to succeed on the PR head. Gate PASS authorizes opening the PR.

## Retained evidence

Full canonical script retained: 10 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/criterion3.sh; SHA256 2c582fedb7ca6c8fe333c1458fde454ff9c2fd6a69c9f1378b4f6fd1b18c2450.
Full full execution record retained: 6050 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/c3-run/evidence/execution.json; SHA256 553f22662da22180a6798dbdc237a3e6f1e20cc294984b04fd5f4ecfdc639409.
Full C6 and raw BEP audit retained: 77 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/resumed-raw-evidence-audit.json; SHA256 0bd795f5abf86dab3bcccf15715ffba2a64cde4b0345361a064482567c703deb.
Full final independent audit retained: 17 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/final-independent-audit.json; SHA256 b95061d90ea276e7ee8f84b0703e1f552c08b6d6e520170f0b73c3a3f7d0c465.
Full fast-lane commands retained: 33 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/fast-lane.sh; SHA256 bb92531813f5748baf862371cd56878f2b8a015ccdfd3cc1295fd5e4875fd96d.
Full fast-lane results retained: 173 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/fast-result.json; SHA256 17087cbb6fc1a3f661a8aeec74c627d491a9d4e55f902555904ddb57a1a77e05.
Full fast-lane independent audit retained: 694 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/fast-independent-audit.json; SHA256 0dd20be36397faf41aee40f930ba4a612cffc29fabe6d5f50d035a0d386479b1.
Full complexity zero-growth audit retained: 547 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/complexity-zero-growth.json; SHA256 de2ee7d6e87b39b2bfc87620c868f0a81fb6a902a360ca0552a6ff843b731d61.
Full farm skip inventory retained: 4826 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/c3-run/evidence/remote/skip-inventory.json; SHA256 6c73631d8b09f1171db0b65d5c27eb9401e82a963075b7d5225ac7f4554badd3.
Full native skip inventory retained: 578 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/native-floor-skip-inventory.json; SHA256 d33c7a35d1874c04ce6184809c043e5c9cc259836d9cbc9e837cea5d5cca8ca8.
Full skip sites and reasons retained: 6851 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/skip-site-proofs-final.json; SHA256 2f10aa110bba0701ef54f0ec0e6b6d6dd1a694cd0527275f79c1ea510c05dcc2.
Full import-closure comparison plan retained: 382 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/skip-comparison-plan.json; SHA256 7760abfb9a954ba4d8b2ce8adb5f6fb70c6f6b47b9bcd482fbd6fa97d13e2029.
Full additional comparison plan retained: 93 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/skip-comparison-extra-plan.json; SHA256 f1a8ec1dca6bcddd53a5dd18a9d40c05b6741bf13a594bbfce63e15c31133164.
Full farm base comparison audit retained: 67 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/skip-base-proof-v2-independent-audit.json; SHA256 3830f2806ccd0612fa793be7a4d07668e5dbffd0430cc53626e39f2b554538cd.
Full additional farm comparison audit retained: 65 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/skip-base-proof-extra-independent-audit.json; SHA256 b2c4a70466f0975d9c9b73becff3552c455e0cc3b4a1fc9fe1a1acc1b06a666c.
Full native base comparison retained: 61 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/native-skip-base/summary.json; SHA256 ce898228b033a12bcc85ffc92d7695fdfc9b1403c06d8d2397de35c2d8c1d911.
Full native floor steps retained: 1056 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/c3-run/evidence/remote/floor-topup.json; SHA256 4658b74b9f3abc4e5eef522a1243f3b0c0f7c66b0cdd2fb6cc95d8c6713a95d4.
Full native load record retained: 2 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/c3-run/evidence/remote/floor-topup.stdout; SHA256 0c4f5b3c24a0f8a1d1d21070f9232412de6d85254ff8514b767de26a3acbb8c0.
Full CI closure retained: 17 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/ci-required/closure.txt; SHA256 2df353b166d646fffe6f6c65d758892683006d7bb3354318fb5f9855e1d9fd90.
Full CI command results retained: 68 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/ci-required/summary.json; SHA256 1482c72c2d72ab730b65af0cce498fc8e66c1ff11a63a093fad9a4b268a99ab1.
Full Windows commands retained: 6 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/ci-windows-lane.sh; SHA256 1f3ed47aaccacc37e89a79af9247d7bd00d20363ee79ec963482f80564ff0be6.
Full runtime preflight retained: 130 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/runtime-preflight.json; SHA256 43c42a7e1127769e9aa337d44145dee649710e74447b17e7506ae8b7d428667c.
Full runtime helper manifest retained: 142 lines; /var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/runtime-helper-start-manifest.json; SHA256 fca22cedf1df1b519fb80865db3c6b5f24185a7dcb7ddeb030fb668e1fbbfcb6.

Compact canonical execution record:

```json
{"test_exec_mode":"remote-exec","fallback_reason":null,"exit_code":0,"test_cmd_scope":"full-suite","evidence_dir":"/var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/c3-run/evidence/remote","gate_base":"af1c2c308aa76afd54b557bba46728a4bb2c0139","gate_head":"1e1893e162f01aab926f41275b7ca70c816d4cd3","admission":{"class":"remote","floor_gib":25.0,"free_gib":60.50278854370117,"waited_seconds":0.001,"cap":2},"rbe_state":{"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"},"delta_gate":{"mode":"shadow","decision":"full","reason":"not-gate-pr","detail":"no PR record (pr.json)","applied":false,"base":"af1c2c308aa76afd54b557bba46728a4bb2c0139","head":"1e1893e162f01aab926f41275b7ca70c816d4cd3","merge":"d3c1cd9f92c39f576824fa4cf393730b99371d82","prior":null,"delta":{},"changed_labels":[],"changed_label_count":0,"tests":{},"phases":{},"plan_sha256":"801853c6043565b967738934185855ed5d3f52049ba0a582849395ada67828be"},"test_exec_bep":{"unit":{"results":422,"remote_executed":422,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"acceptance":{"results":25,"remote_executed":25,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-packages":{"results":86,"remote_executed":86,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-full":{"results":14,"remote_executed":14,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true},"integration-latency":{"results":1,"remote_executed":1,"cached_remotely":0,"cached_locally":0,"other_strategy":0,"retried":0,"window_ok":true}},"farm_test_seconds":18070.633,"criterion3_valid":true,"latency_leg":{"complete":true,"phase":"integration-latency","tests":["TestGcBdLatencyBudget","TestGcSeatHookLatencyBudget"],"attempts_allowed":2,"attempts":[{"attempt":1,"rc":0,"probe_verdict":"clear","phase":["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$","GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"],"results":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"farm_test_seconds":71.566,"evidence_errors":[],"probe":["GATE_FARM_PROBE verdict=clear waited=0s load1=12.09 load5=22.53 load15=13.61 cpus=32 threshold=50 max_wait=600s"],"test":["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 9.44 20.87 13.34 11/949 55303"]}],"retired_farm_test_seconds":0.0,"main_leg":["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"],"main_leg_verdicts":{"TestGcBdLatencyBudget":{},"TestGcSeatHookLatencyBudget":{}},"verdicts":{"TestGcBdLatencyBudget":{"PASS":1},"TestGcSeatHookLatencyBudget":{"PASS":1}},"verdict":"measured","message":"measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS","rc":0},"owned_topup":[],"floor_topup":{"complete":true,"steps":{"elided_array":{"items":16,"bytes":38722}},"log_dir":"/var/tmp/gc-gate-ga-c544t2ia-8o0idpxl/c3-run/evidence/remote/floor-logs"},"skip_inventory":{"counts_by_reason_class":{"other":513,"environment-absence":90},"top_level_counts_by_reason_class":{"other":255,"environment-absence":43},"tests":{"elided_array":{"items":603,"bytes":217036}}},"test_cmd":{"elided_array":{"items":5,"bytes":2064}},"test_counts":{"PASS":157822,"FAIL":0,"SKIP":617},"diff_tests_executed":"diff_tests_executed:\n  TestOrderDispatchCityCooldownIgnoresRigScopedEventFallback PASS (integration-packages-9c8cd94da10f0dd04b00.log, unit-fd986cf5bab8709b8d45.log)\n  TestOrderDispatchRigCooldownHonoursEventFallbackAfterBeadPrune PASS (integration-packages-bc482d2ae768251ad47f.log, unit-1ab526405ace21879f85.log)\n  TestOrderDispatchRigCooldownIgnoresOtherScopesEventFallback PASS (integration-packages-18745ddf7d62465654f5.log, unit-d9e667428f972d067bd3.log)\n  TestLastRunFuncWithEventFallback_FallsBackToEvents PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)\n  TestLastRunFuncWithEventFallback_FallsBackToLatestEvent PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)\n  TestLastRunFuncWithEventFallback_FindsEventInRotatedArchiveWithinLookback PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)\n  TestLastRunFuncWithEventFallback_IgnoresArchivedEventBeyondLookback PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)\n  TestLastRunFuncWithEventFallback_IgnoresEventsBeyondLookback PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)\n  TestLastRunFuncWithEventFallback_IgnoresOtherOrderEvents PASS (integration-packages-4f3a116cb8b37d763509.log, unit-ff9cab7068d884a7ea3a.log)\nGATE_TEST_EVIDENCE_RESULT ok: 9 diff-owned tests verified by name\n"}
```
