# CLI path identity release gate

**Verdict:** **PASS**

Bead: ga-3pheeu2x; build: ga-mdl2a7; review: ga-9t301uyd.
Reviewed source: `5ef7a1d66c064620eaab9c6c15118c9f43eb929e`.
gate_base: `1042f51083f93b7b16d3d80cbcd7f3eb73fb374b`
gate_head: `7a0361f224f40617316d1d4ebcac88be2957677a`; tree: `b57bf4e135a01c2bd67ddf96029cc4a6090ca18e`
merge_state: clean (origin/main@1042f51083f93b7b16d3d80cbcd7f3eb73fb374b)
gate_base_kind: current-tip
merge_validation: canonical merge built and vetted; merge authority rechecks the current landing tree.
deploy_mode: remote; push_remote: fork; branch: deploy/ga-3pheeu2x-gate.

| # | Criterion | Result and evidence |
|---|---|---|
| 1 | Review PASS | PASS: round-2 review ga-9t301uyd names the resolved source; no carryover or reviewer-added commit. |
| 2 | Acceptance | PASS: all 10 sites classified below; 11 owned tests PASS; both native Darwin witnesses PASS at source and canonical merge; strict error and escape rejection controls preserved. |
| 3 | Tests | PASS: complete five-phase remote suite, native floor, skip audit and required CI below. |
| 4 | High findings | PASS: zero unresolved HIGH findings; reviewer verdict pass. |
| 5 | Clean branch | PASS: source and merge checkouts clean; this record is the sole administrative child, committed before push. |
| 6 | Merge/build/vet | PASS: clean pinned merge, whole-tree Bazel build plus explicit nogo/lint; successful BEPs in c6-audit.json. |
| 7 | Single theme | PASS: three reviewed commits, 14 files under cmd/gc, one CLI path-identity theme. |

## Acceptance sites

| Site | Disposition and preserved behavior |
|---|---|
| api_state.go / symlinkAwareContainment | Normalize both comparison operands; strict target resolution still fails closed. |
| bd_inventory_trust.go / statBdBinary | Keep strict EvalSymlinks; physical identity rejects missing/broken paths. |
| cmd_bd.go / resolveBdInvokingGCBinary | Keep strict resolution; GC_BIN must identify a real executable. |
| cmd_hook_claim.go / hookClaimPathOutside | Normalize missing-leaf ancestry; relative/dot-dot refusals preserved. |
| doctor_bd_inventory.go / bdInventoryCheck.Run | Normalize dedupe key; complexity improves 26 to 25. |
| dolt_handoff_projection.go / committedBeadsHandoffOwnsScope | Keep strict scope resolution and journal-trust refusal. |
| storage_reverse.go / reverseCityTOMLPath | Keep strict physical city.toml resolution and errors. |
| storage_snapshot_city.go / cityTOMLFile | Keep strict physical city.toml resolution and errors. |
| storage_store_identity.go / canonicalIdentityPath | Normalize aliased/missing-leaf identity; preserve empty input. |
| supervisor_binary_mismatch.go / resolveExecutablePath | Normalize advisory identity; preserve unknown/empty path. |

All five strict sites have adjacent canonical-path-exception markers. No guard allowlist, dependency or complexity baseline changed. Complexity satisfies zero-growth form (ii): no row worsens against pinned base; standalone complexity-check is red at both trees (complexity-zero-growth.json).

## Runtime and policy evidence

Evidence root: `/var/tmp/gc-gate-ga-3pheeu2x-r2pnu6ri`. Large logs remain there.
test_cmd: `/var/tmp/gc-gate-ga-3pheeu2x-r2pnu6ri/criterion3.sh`, invoking gate-criterion3.sh on the pinned canonical merge: whole unit, acceptance, integration-packages, integration-full and separate integration-latency phases.
test_cmd_scope: full-suite
test_exec_mode: remote-exec; fallback_reason: none; heavy_mode: none.
test_counts: 158765 PASS, 0 FAIL, 618 SKIP (top-level verdict occurrences across phases and floor).
farm_test_seconds: 14821.937; fresh remote results: 554; cached result actions: 0.
test_exec_bep: c3-run/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json; strategy remote, attempt 1, phase-window starts and all terminal outcomes verified; 1108 retained CAS log/XML hashes verified.
rbe_state: worker environment equals pinned base; valid certificate; kill switch absent; no local fallback.
admission: remote, 25 GiB floor, cap 2, 59.164 GiB free, waited 0.001 seconds.
owned_topup: none; floor_topup: 16/16 commands exit 0, including environment-absence packages, beadstest, Darwin compile and runner self-tests.
latency_leg: separate clear-probe run; TestGcBdLatencyBudget and TestGcSeatHookLatencyBudget PASS; probe waited 30 seconds, load1 48.41 at threshold 50.
test_log_dir: c3-run/evidence/remote/logs and floor-logs; read with gate-test-evidence.py; retained.
skip_inventory: c3-run/evidence/remote/skip-inventory.json; 337 unique skips remain after native floor. Unchanged sites; all 64 shared/helper-hunk cases have pinned-base comparisons (unit 62; integration-packages 64). No new, reason-changed or owned skip. Per-site/reason proof: base-skip-comparisons-final.json and base-skip-evidence-v1/.
skip_justification: non-owned platform/optional-environment conditions; unchanged sites and baseline reasons; every reachable changed-hunk skip has a measured comparison.
waiver_ref: none (gascity has no waiver path).
load_threshold: 15; load_waited_seconds: 0; load_wait_timed_out: 0; run_start_load: 14.63; run_max_load: 15.27; run_mean_load: 14.87; run_readings: 6 (native floor; farm admission has no local-load wait).
policy_lane: PASS: shell guards check-release-dist-ignore, check-routed-test-rows, check-split-topology-rows, check-residency-boundary, check-hooks; format/policy Bazel targets; trusted-base strict storeclass baseline/growth predicates.
drift_lane: PASS: make bazel-sync plus empty tracked/untracked diff; genspec, genschema, genclient, dashboard dist/client and lockfile drift targets; openapi-breaking-check. Fresh-tree isolation and pinned gate-base used.
dashboard: committed bundle preview served page, JS and CSS with HTTP 200; owned process group stopped (dashboard-preview/verification.json).
ci_lane_run: n/a (product diff changes no CI configuration).

```text
diff_tests_executed:
  TestBdBinaryIdentityStillRejectsUnresolvablePaths PASS (integration-packages-821a75e4a503601af052.log, unit-fd986cf5bab8709b8d45.log)
  TestBdPassthroughWalkUpScript PASS (integration-packages-cb110414c557c0928e82.log, unit-1ab526405ace21879f85.log)
  TestCanonicalStoreIdentityPreservesEmptyPath PASS (integration-packages-aa6cf5c138f0b61b866a.log, unit-27b23e708b477fce4dad.log)
  TestCityConfigPathProbesKeepPhysicalSymlinkTarget PASS (integration-packages-074fc6c34a1fb2577127.log, unit-1cc8c8ab55e59a9e03a9.log)
  TestCityConfigPathProbesStillRequireResolvableFile PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-752a0bf25b15b465517a.log)
  TestHookClaimMissingPathsKeepCanonicalAncestry PASS (integration-packages-12697e2ea0d32c278115.log, unit-637c74c578a2b17fc654.log)
  TestHookClaimOutsideEvidenceStillRejectsUnanchoredPaths PASS (integration-packages-9326181a736eb30b71ca.log, unit-423c6a0da1986b90e9a7.log)
  TestSupervisorExecutableIdentityCanonicalizesMissingLeaf PASS (integration-packages-56f04454dd9158a9ee8a.log, unit-1ab526405ace21879f85.log)
  TestSupervisorExecutableIdentityPreservesUnknownPath PASS (integration-packages-e4245ddf6a37030111bc.log, unit-d9e667428f972d067bd3.log)
  TestWorkStoreIdentityCanonicalizesMissingScope PASS (integration-packages-f7fba3a97bc02cffff0b.log, unit-0e1499ad8e8a0a6b9d81.log)
  TestWorkStoreIdentityCanonicalizesWithoutCreatingScope PASS (integration-packages-e5330b8c981927473156.log, unit-3d6d3036a2f5902472b5.log)
GATE_TEST_EVIDENCE_RESULT ok: 11 diff-owned tests verified by name
```

## Native macOS acceptance

Official Mac Regression smoke: https://github.com/gastownhall/gascity/actions/runs/45002815023. Process shards 8/12 succeeded but output omitted named PASS lines. Approved supplemental workflow runs the unchanged whole shards with verbose output on native macos-15.

| Tree | Run | Both required named witnesses |
|---|---|---|
| `5ef7a1d66c064620eaab9c6c15118c9f43eb929e` | https://github.com/quad341/gascity/actions/runs/45006908643 | PASS; both whole shards succeeded, actual checkout/Darwin assertions and log hashes verified. |
| `7a0361f224f40617316d1d4ebcac88be2957677a` | https://github.com/quad341/gascity/actions/runs/45014158576 | PASS; both whole shards succeeded, actual checkout/Darwin assertions and log hashes verified. |

Witnesses: TestContainmentPassesAcceptLegitimateInCityRigs; TestAssertRigPathWithinCityAcceptsResolvedTargetUnderSymlinkedCity.
failure_attribution: TestProxyProcessReapRegistryMatchesAnEquivalentSpelling -> ga-4ytvawfe. Official raw-source smoke remains FAILURE. Clause 1: test blob/site unchanged, no shared hunks. Clause 2: predating closed condition opened. Clause 3(a): measured Darwin test-binary closure excludes cmd/gc, the only changed production package. Clause 4: no path/package overlap. Landed fix 9ca38e632d41 is in pinned base and canonical merge, satisfying REFRESH. Mayor correction gcg-14670849 explicitly applies 3a to this raw/canonical split; no rebase, force-push or waiver. Raw proof: mac-source-v2/failures/raw-source-failure-proof.json.
Auxiliary first attempt 45003943176 cancelled at 25 minutes before test outcomes; no acceptance evidence. Budget-only extension gcg-14673921 permits 120-minute jobs/60-minute process timeout and final-head measurements. Approved workflow SHA256: ad958c4494ef2c1f3dde8292d6f55244a797f46f91079a258a6682a7f2c00a5b.

## Required CI accounting

ci_required_closure: ci_required_triggers.py /var/tmp/gc-merge-validate.iYq1QM 1042f51083f93b7b16d3d80cbcd7f3eb73fb374b 5ef7a1d66c064620eaab9c6c15118c9f43eb929e fork; workflow blob `fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612`; 6 LEAF jobs.
Draft CI at reviewed source authorized by mayor gcg-14633985; raw run/jobs/log hashes retained in ci-required-source/.

| Leaf | Status | Evidence |
|---|---|---|
| runner-policy | CI-PASS | https://github.com/gastownhall/gascity/actions/runs/45002675529 |
| changes | CI-PASS | https://github.com/gastownhall/gascity/actions/runs/45002675529 |
| windows-lane | CI-PASS | https://github.com/gastownhall/gascity/actions/runs/45002675529 |
| windows-lane-test | CI-PASS | https://github.com/gastownhall/gascity/actions/runs/45002675529 |
| credential-provider-windows | NOT-TRIGGERED | ci-required-closure.txt: path filter did not match |
| pack-gate | NOT-TRIGGERED | ci-required-closure.txt: path filter did not match |

ci_required_accounting: all 6 leaves accounted; aggregate succeeded.
windows_reach: none; credential-provider-windows filter false; windows-lane-test actually succeeded on Windows.
deferred_to_pr_ci: none; not_covered: none.
Publication condition: this record is the only administrative child. Keep draft and clearance absent until exact final-head native named proof and required CI are verified; record receipts on the bead, delete the two authorized evidence refs, then route the pinned-head merge-request.
