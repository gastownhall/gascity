# Clone-local string contract — release gate

**Verdict:** **PASS**

Deploy `ga-apvezumd`; build `ga-8r64wur`; review `ga-di6xwdf8`. Completed 2026-11-21T21:35:01.110763+00:00.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Exact resolved source equals reviewer PASS; no carryover or reviewer-added tests. |
| 2 | Acceptance criteria | PASS | Interface comment limits verified against sidecar/implementations and ruling; new independence case passes on all seven default-tier entry points. Complexity has zero growth, baseline untouched. Only the two approved source files change. |
| 3 | Tests and required checks | PASS | Complete five-phase suite, 16 native floor steps, all owned names and SKIP provenance verified; required CI leaves accounted below. |
| 4 | No unresolved HIGH findings | PASS | Reviewer style/security/spec findings read; zero HIGH. Existing unpinned Revision/UpdatedAt behavior and old proxied comment are non-blocking observations. |
| 5 | Clean branch | PASS | Reviewed source and canonical merge clean. Publication guards require the isolated branch to contain only the committed gate record above that source and a clean worktree. |
| 6 | Merge state and build/vet | PASS | Frozen clean merge; whole-tree Bazel build/nogo terminal success independently audited. |
| 7 | Single feature theme | PASS | Storage contract and characterization test; full denylist/citation ancestry check passed for deploy/build IDs. |

- reviewed_source: `7fd60a0c7eab7366b5fe7f07873674b51fb55ddd`.
- gate_base: `36a3a2fd906866d380f76d21eda2aa74f14057ce`; base_ref: origin/main; gate_base_kind: current-tip.
- gate_head: `642cf0d19a8d375c4367a2d096b3b0169eaf376d`; gate_tree: `50220743d9c8155dae1fb6be952572a89463ac10`.
- merge_state: clean (origin/main@36a3a2fd906866d380f76d21eda2aa74f14057ce; files: none).
- merge_validation: exact parents/raw merge tree audited. Later base movement is validated by the merge authority under the merge-time guarantee; no deployer rebase.
- deploy_mode: remote; push_remote: fork; base_branch: main.
- evidence_root: `/var/tmp/gc-gate-ga-apvezumd-u36mcfbu`; raw BEP, CAS artifacts, job logs, load telemetry and comparison receipts retained.

## Acceptance

`beads.go` changes comments only: removes illustrative keys, states load-once/whole-file sidecar behavior, process/instance limits, single storage home and key suitability. `local_sidecar.go` and proxied H2 agree. No production key consumer was found beyond wrappers/pass-throughs; `beadstest` is test support. The added fixed-literal case checks durable set/clear leaves local data intact, local clear preserves durable data, and local reads never fall back to durable metadata. No backend behavior is changed. `acceptance-independent-audit.json` and `source-assessment.json` retain the checks.

- `TestExecStoreConformance/SetLocalStringIndependentOfDurableWrites`: PASS in canonical unit and integration-package logs.
- `TestFileStore/SetLocalStringIndependentOfDurableWrites`: PASS in canonical unit and integration-package logs.
- `TestMemStoreHonoringIDsPassesStoreConformance/SetLocalStringIndependentOfDurableWrites`: PASS in canonical unit and integration-package logs.
- `TestMemStore/SetLocalStringIndependentOfDurableWrites`: PASS in canonical unit and integration-package logs.
- `TestNativeDoltStoreConformance/SetLocalStringIndependentOfDurableWrites`: PASS in canonical unit and integration-package logs.
- `TestProxiedStoreConformance/SetLocalStringIndependentOfDurableWrites`: PASS in canonical unit and integration-package logs.
- `TestSQLiteStoreConformance/SetLocalStringIndependentOfDurableWrites`: PASS in canonical unit and integration-package logs.

Unchanged parents `TestBdStoreConformance` and `TestBrProviderConformance` skip before the helper; identical focused pinned-base/head commands confirm both reasons pre-existing. The canonical full integration target ran. The builder's temporary un-skip/mutation runs remain review inputs and are not gate credit.

## Criterion 3 evidence

- test_cmd: canonical `gate-criterion3.sh` on the canonical merge with pinned base, live bead snapshot and derived owned set; exact wrapper `criterion3.sh`, per-phase argv/BEP retained. Unit is `make -k check` (`bazel test //...` plus shell guards), followed by acceptance, integration packages, full integration minus dedicated latency names, then the latency lane.
- test_cmd_scope: full-suite; test_exec_mode: remote-exec; test_exec_bep: `c3-run/evidence/remote/<phase>.bep.json`.
- test_counts: 158581 PASS, 0 FAIL, 618 SKIP top-level events, summed per job (subtests excluded).
- diff_tests_executed: all seven `SetLocalStringIndependentOfDurableWrites` entry points listed above PASS by name; no owned SKIP or MISSING.
- test_log_dir: `/var/tmp/gc-gate-ga-apvezumd-u36mcfbu/c3-run/evidence/remote/logs`; `final-by-name.stdout` verifies all7 owned names.
- BEP audit: 554 results, strategy remote, no cached flags/fallback/retries, starts inside phase windows; 1108 CAS log/XML digests/sizes verified. Flattened logs match canonical byte22 normalization.
- farm_test_seconds: 11891.336; admission: `{"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 61.664493560791016, "waited_seconds": 0.001}`.
- rbe_state: own-code provenance, worker pin matches frozen main, kill switch absent, certificate valid through Jan9 2027. Pinned bd1.3.4, ICU74, rootless Podman socket/Dolt2.6.0 image and recent sweep verified before launch.
- fallback_reason: none; heavy_mode: none; owned_topup: none; floor_topup: all16 steps exit0, commands/logs in `remote/floor-topup.json`.
- latency_leg: measured under clear farm probe, both configured names PASS; no latency name ran in the main integration leg.
- delta_gate: shadow/full/not-gate-pr; no narrowed credit. waiver_ref: none; failure_attribution: none; ci_lane_run: n/a (no CI configuration changed).

| Phase | Actions | Result |
|---|---:|---|
| unit | 426 | PASS, fresh remote attempt 1 |
| acceptance | 25 | PASS, fresh remote attempt 1 |
| integration-packages | 87 | PASS, fresh remote attempt 1 |
| integration-full | 15 | PASS, fresh remote attempt 1 |
| integration-latency | 1 | PASS, fresh remote attempt 1 |

### Fast lanes and host load

policy_lane: format1/policy13/OpenAPI1 targets and whole-tree nogo PASS; trusted-base storeclass baseline/growth predicates PASS, no raise flag. drift_lane: seven generated targets, `make bazel-sync` and empty diff/status PASS. Complexity uses zero-growth form(ii), no row worse than pinned base, baseline untouched. Every write-capable lane used its own fresh tree before runtime/suite. Dashboard preview n/a (no API/spec/dashboard source change); dashboard Bazel checks are in unit. `fast-independent-audit.json` and `static-complete-audit-200x.json` contain raw hashes and receipts.

Native floor load: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=360 wait_timed_out=0 run_start_load=14.61 run_max_load=16.46 run_mean_load=16.05 run_readings=8 wait_first_load=23.51 wait_max_load=23.51 wait_mean_load=18.66 wait_readings=13 read_errors=0`. load_threshold: 15; load_waited_seconds: 360; load_wait_timed_out: 0; run_start_load: 14.61; run_max_load: 16.46; run_mean_load: 16.05; run_readings: 8. Supplemental local base comparisons retain their own load/admission receipts. Remote phases use remote admission without host load waits. Strict storeclass load: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1771 wait_timed_out=0 run_start_load=14.86 run_max_load=20.22 run_mean_load=17.31 run_readings=4 wait_first_load=44.11 wait_max_load=44.11 wait_mean_load=24.19 wait_readings=60 read_errors=0`.

### SKIP provenance

skip_justification: every remaining SKIP has an unchanged site and reason; reached helper cases require the same-command comparisons below.

skip_inventory: `remote/skip-inventory.json` (604 events) and `native-all-skips.json` (72 events). All319 remote sites and every native site are unchanged. Exact reasons/logs and shared-helper source remaps retained in `skip-comparison-plan.json` and `native-skip-sites.json`; no owned SKIP/FAIL.

The changed helper is `beadstest.RunStoreTestsWithOptions`, an additive case with no skip condition. Actual plain/integration test-binary import closures, including bracketed test variants, identified15 reached skipped packages. Every reached SKIP is measured against the frozen base under the same focused command and configuration: `reached-skip-comparison-v2/verified.json`, `parent-skip-comparison-v3/comparison.txt`, `native-skip-comparison/verified.json`; all comparisons say ALL PRE-EXISTING. Other packages do not import the changed helper, and interface comment changes cannot affect their unchanged skip branches. Remaining skips cover platform/provider capability, helper-only/opt-in scenarios and existing environment restrictions; the native floor exercised available host integrations. Focused comparisons provide SKIP provenance only, never suite credit. The original parent collector hit an evidence-directory-mode refusal before Bazel; its no-test setup evidence is retained in parent-collector-setup-recovery.json and parent-skip-comparison/. The corrected v3 run uses new0700 evidence directories; no product test was retried.

Comparison normalization notes:

```text
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a02, head rbe-a01)
```

### ci-required accounting

ci_required_closure: `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.rHzab1 36a3a2fd906866d380f76d21eda2aa74f14057ce 7fd60a0c7eab7366b5fe7f07873674b51fb55ddd fork`; merged workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; six LEAF jobs. Raw output `ci-required/closure.stdout`; The pack-gate and windows-lane jobs ran sequentially through detached fresh-tree/admission/load/isolation wrappers with exact workflow commands and environment. The changes tool and runner-policy receipts are recorded separately. Every job has a real receipt or explicit deferral.

| Job | Status | Evidence |
|---|---|---|
| changes | LOCAL-PASS | `python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.rHzab1 36a3a2fd906866d380f76d21eda2aa74f14057ce 7fd60a0c7eab7366b5fe7f07873674b51fb55ddd fork`; exit 0; log `/var/tmp/gc-gate-ga-apvezumd-u36mcfbu/ci-required/closure.stdout` |
| runner-policy | LOCAL-PASS | `EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<captured temporary file>` `python3 .github/workflows/scripts/runner_policy.py`; exit 0; log `/var/tmp/gc-gate-ga-apvezumd-u36mcfbu/ci-required/runner-policy.stdout` |
| pack-gate | LOCAL-PASS | `if scripts/update-bundled-gastown-pack --check; then`; `export GC_TEST_GASCITY_PACKS_REGISTRY=main`; `if make test-pack-registry-live; then`; exit 0; log `/var/tmp/gc-gate-ga-apvezumd-u36mcfbu/ci-required/pack-gate-run-output.txt` |
| windows-lane | LOCAL-PASS | `GOOS=windows go build $(cat .github/windows-lane/build.txt)`; `GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true $(cat .github/windows-lane/test-build.txt)`; `GOOS=windows go build ./analyzers/... && GOOS=windows go test -count=1 -vet=off -run '^$' -exec /bin/true ./analyzers/...`; `GOOS=darwin go build ./... && GOOS=darwin go test -count=1 -vet=off -run '^$' -exec /bin/true ./...`; exit 0; log `/var/tmp/gc-gate-ga-apvezumd-u36mcfbu/ci-required/windows-lane-run-output.txt` |
| credential-provider-windows | DEFERRED-TO-PR-CI (non-linux-runner) | `  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'`; unchanged Windows closure; actual runtime result deferred |
| windows-lane-test | DEFERRED-TO-PR-CI (non-linux-runner) | `  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR`; unchanged Windows closure; actual runtime result deferred |

Required Linux job load receipts:

- pack-gate: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=18.77 run_max_load=20.95 run_mean_load=20.21 run_readings=5 wait_first_load=16.23 wait_max_load=32.22 wait_mean_load=23.52 wait_readings=62 read_errors=0 prewait_s=1800`.
- windows-lane: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=270 wait_timed_out=0 run_start_load=14.47 run_max_load=20.14 run_mean_load=17.84 run_readings=4 wait_first_load=18.89 wait_max_load=19.04 wait_mean_load=16.85 wait_readings=11 read_errors=0 prewait_s=270`.

- windows_reach: credential provider `[]`; native Windows job `[]`, separately computed from its allowlist and Windows test-binary closure. Cross-build/vet receipts are retained when required; Windows execution remains deferred.
- deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner); not_covered: none.
- Gate PASS authorizes PR opening. Merge requires current-head `CI / required` green and every deferred job successful or legitimately path-filter skipped. Exact published-head clearance precedes the mayor merge-request; deployer never merges.
