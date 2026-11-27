**Verdict:** **PASS**

# Hybrid local-route conformance gate

Bead: ga-c0zlfz10; build: ga-dnvax9i; review: ga-3c92a594.
Reviewed source: 8ce2412c9d473d49b37a9a8f6812a16a86591bd9 (resolved object; no branch-tip substitution).
Validation merge: 3b7faf7cacb9b1d286573ea79e660588dc93b6e8; tree: d5afd9e0d49769440831fdac718608d4b652b5bc.
gate_base: 08fa6180490d771477d278b19daf1aca484e6411
gate_base_kind: current-tip
merge_state: clean (origin/main@08fa6180490d771477d278b19daf1aca484e6411)
merge_validation: canonical merge built and vetted; final merge validation belongs to the merge authority.
Publication: new isolated deploy/ga-c0zlfz10-gate; PR description must explicitly replace gastownhall/gascity#6520 and be read back before the completion handoff.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer PASS re-read at exact source; all reviewed TDD commits retained; no carryover or reviewer-added-test substitution. |
| 2 | PASS | Hybrid shared contract and both changed ledger roots PASS by name. Actual generated-ledger synchronization test PASS; the proof covers two fakes routed locally. The production registry wrapper still builds K8s before selecting its route and remains separately accounted through2026-12-26. acceptance-code-audit.json and ledger-sync-test-proof.json retain the evidence. New-PR/replacement-body obligations are guarded by publication and checked before reporting completion. |
| 3 | PASS | One fresh complete full-suite chain on this canonical tree, with every remote phase, native floor, owned roots, SKIP controls and exact CI accounting below. |
| 4 | PASS | No unresolved HIGH review findings. |
| 5 | PASS | Reviewed and canonical trees clean. Publication adds only this gate record and requires a clean normal-hook commit. |
| 6 | PASS | Independent merge-tree equality; whole-tree Bazel build and vet/nogo PASS, normal hooks active, ICU74 OK. |
| 7 | PASS | One feature: local-route contract evidence and the ledger that references it; six source files, no runtime/API/config/dependency behavior change. |

test_cmd: gate-criterion3.sh --repo /var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/merge.1gwd39jl --base 08fa6180490d771477d278b19daf1aca484e6411 --bead-json /var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/bead.json --owned /var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/owned-tests.tsv --run-dir /var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/criterion3-run/evidence
test_cmd_scope: full-suite
test_exec_mode: remote-exec
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 415, "results": 415, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72", "worker_env_tree": "sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
fallback_reason: none
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 61.136207580566406, "waited_seconds": 0.001}
farm_test_seconds: 13219.18
test_counts: 155274 PASS / 0 FAIL / 613 SKIP (top-level per-job Go results, including native floor)
test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c0zlfz10/gate-pass-20261117T095433Z/criterion3-run/evidence/remote/logs
diff_tests_executed: all3 changed roots below PASS, including reported subtests
owned_topup: none required; owned roots passed in full phases
floor_topup: complete 16 source/native steps, all exit0; equal pinned-base controls complete
latency_leg: both budget tests PASS after a clear farm probe
heavy_mode: none, verified classifier
failure_attribution: none
waiver_ref: none (the provider testing-ledger exception is separate; gascity has no release-gate waiver)
policy_lane: ["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/merge.1gwd39jl", "--", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh", "run", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py", "--class", "remote", "--bead", "ga-c0zlfz10", "--record", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/fast-policy-admission.json", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--bead", "ga-c0zlfz10", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/run-pinned-lint.sh", "--", "bash", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/fast-policy.sh"] — PASS; fast-policy.sh retains pinned fmt/lint/nogo, four shell guards and all required targets
drift_lane: ["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/merge.1gwd39jl", "--", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh", "run", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py", "--class", "remote", "--bead", "ga-c0zlfz10", "--record", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/fast-drift-admission.json", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--bead", "ga-c0zlfz10", "--", "bash", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/fast-drift.sh"] and ["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/merge.1gwd39jl", "--", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/gate-base.sh", "run", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py", "--class", "remote", "--bead", "ga-c0zlfz10", "--record", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/fast-generated-admission.json", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--bead", "ga-c0zlfz10", "--", "bash", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/fast-generated.sh"] — PASS; BUILD synchronization and all generated checks in separate fresh views
ci_lane_run: n/a (no CI-config change)
complexity: zero_growth=true; base/merge 251/251 rows; no new/regressed/deleted row; baseline unchanged

| Phase | Results | Verdict |
|---|---:|---|
| unit | 415 | PASS, fresh remote attempt1 |
| acceptance | 25 | PASS, fresh remote attempt1 |
| integration-packages | 86 | PASS, fresh remote attempt1 |
| integration-full | 14 | PASS, fresh remote attempt1 |
| integration-latency | 1 | PASS, fresh remote attempt1 |

All 541 results satisfy remote execution, no cached flags, first attempt and valid phase windows. The full five-phase chain is judged alone.

- TestHybridConformance: PASS
- TestCatalogBindsHybridCompositionToLocalRoute: PASS
- TestCatalogReturnsIndependentEntries: PASS

skip_inventory: complete-skip-audit-plan.json, 671 events at 325 unchanged sites; no owned SKIP.
skip_justification: every raw reason/site mapped, environment-absence cases receive the native floor and pinned-base controls. No shared/helper test hunk changed. All literal cmd/gc comparisons pass. The 2 filesystem comparison gaps retain their actual reason-changed result; unchanged guards observe the same EXDEV/case-sensitive filesystem condition, and measured unit/integration import closures exclude every changed non-test input. No reason normalization or false comparator PASS is claimed. Source-site/blob checks use the actual canonical merge, so tests added on main since the reviewed source are accounted correctly.

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/merge.1gwd39jl 08fa6180490d771477d278b19daf1aca484e6411 8ce2412c9d473d49b37a9a8f6812a16a86591bd9 origin; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs need a status; the rest are rolled up through their needs..

| CI leaf | Status | Exit | Evidence |
|---|---|---:|---|
| runner-policy | LOCAL-PASS | 0 | command=["bash", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/ci-runner-policy.sh"]; log=/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/ci-required/runner-policy.log |
| changes | LOCAL-PASS | 0 | command=["python3", "/home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/merge.1gwd39jl", "08fa6180490d771477d278b19daf1aca484e6411", "8ce2412c9d473d49b37a9a8f6812a16a86591bd9", "fork"]; log=/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/ci-required/triggers.txt |
| credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | n/a |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'; Windows supplemental vet proof={"command": ["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/merge.1gwd39jl", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py", "--class", "local-heavy", "--record", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/ci-required/credential-provider-windows-supplement-admission.json", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh", "--class", "local-heavy", "--threshold", "15", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--", "bash", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/supplement-credential-provider-windows.sh"], "exit_code": 0, "log": "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/ci-required/credential-provider-windows-supplement.log"} |
| pack-gate | NOT-TRIGGERED | n/a |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true' |
| windows-lane | LOCAL-PASS | 0 | command=["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/merge.1gwd39jl", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py", "--class", "local-heavy", "--record", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/ci-required/windows-lane-admission.json", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh", "--class", "local-heavy", "--threshold", "15", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--", "bash", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/ci-windows-lane.sh"]; log=/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/ci-required/windows-lane.log |
| windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | n/a |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR |

windows_reach: {"credential-provider-windows": "internal/testutil/providerledger/BUILD.bazel\ninternal/testutil/providerledger/ledger.go\ninternal/testutil/providerledger/ledger_test.go", "windows-lane-test": "none"}
windows_supplements: {"credential-provider-windows": {"command": ["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/merge.1gwd39jl", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/gate-admission.py", "--class", "local-heavy", "--record", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/ci-required/credential-provider-windows-supplement-admission.json", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh", "--class", "local-heavy", "--threshold", "15", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--", "bash", "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/supplement-credential-provider-windows.sh"], "exit_code": 0, "log": "/var/tmp/gate-ga-c0zlfz10-fresh-s_fynt8r/ci-required/credential-provider-windows-supplement.log"}} (each successful strict shell sequence includes both required default and integration Windows vet commands; runtime jobs remain deferred)
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none
Actual publication mode origin; any earlier fork/origin difference has identical leaf rows and suite_mode, verified in publication-push-mode-proof.json. The PR must have CI / required green and each deferred leaf successful (or path-filter false) before merge. Gate PASS authorizes PR publication.

Explicit load fields (run measurements are separate from the wait):
- {"load_threshold": "15", "load_wait_timed_out": "1", "load_waited_seconds": "1802", "run": "source native floor", "run_max_load": "38.11", "run_mean_load": "33.26", "run_readings": "7", "run_start_load": "37.38"}
- {"load_threshold": "15", "load_wait_timed_out": "0", "load_waited_seconds": "660", "run": "base native floor", "run_max_load": "18.23", "run_mean_load": "16.70", "run_readings": "8", "run_start_load": "14.73"}
- {"load_threshold": "15", "load_wait_timed_out": "0", "load_waited_seconds": "60", "run": "CI Windows compilation", "run_max_load": "17.98", "run_mean_load": "17.35", "run_readings": "4", "run_start_load": "14.71"}

- {"load_threshold": "15", "load_wait_timed_out": "0", "load_waited_seconds": "660", "run": "credential-provider Windows vet supplement", "run_max_load": "NA", "run_mean_load": "NA", "run_readings": "0", "run_start_load": "14.73"}

Test environment: rootless podman socket active and _ping OK; exact pinned Dolt2.6.0 image cached; ICU74 provenance recorded. Runtime argv explicitly pairs Ryuk disabled with unreaped-testcontainers opt-out. Native pinned-base controls use the same settings.

Earlier stopped gate: /var/tmp/gc-gate-ga-c0zlfz10-zj3jt29_ remains separate. It failed suspended-city acceptance before the designated fix landed; no old result is stitched into this fresh run. Landed fix 36447913f2aecd88512cc086d59c806c59e486e1 is reachable from the pinned base, and both suspended-city and suspended-rig tests PASS in this fresh acceptance output. The prior six captured services have MainPID0.

Evidence bundle: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-c0zlfz10/gate-pass-20261117T095433Z
Final audit: final-evidence-audit.json; independent BEP/raw artifact audit: remote-phases-independent-audit.json; static acceptance: acceptance-code-audit.json; actual ledger-sync test: ledger-sync-test-proof.json; SKIP proofs: complete-skip-audit-plan.json, shared-helper-audit.json, filesystem-skip-gap-proofs.json; exact CI commands and exits: ci-required/accounting.json. EVIDENCE-SHA256.json hashes every retained file. Publication targets only the isolated deploy branch.
