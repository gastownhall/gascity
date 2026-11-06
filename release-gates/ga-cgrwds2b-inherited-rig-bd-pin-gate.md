# Release gate: inherited rig workspace BD_BIN pin

**Verdict:** **FAIL**

Disposition: **HOLD** for ga-dhnv01xi; criterion 3 failed in a non-diff-owned native test, without a qualifying attribution proof. No push, PR, clearance or merge-request.

- Deploy bead: ga-cgrwds2b; build: ga-xn1enfv; review: ga-9jqzgh39 (PASS).
- Reviewed source: `0d1c9fb62a0897750028808fbf48157afa08800f`; isolated local branch: `deploy/ga-cgrwds2b-gate`.
- Fresh release evidence: 2026-11-06, current run only. Earlier held runs and reviewer results were not substituted.
- DEPLOY_MODE: remote; PUSH_REMOTE: fork; BASE_REF: origin/main.
- merge_state: clean (origin/main@0bd038188e7947cd283868b8640091fd14f90857).
- gate_base_kind: current-tip; gate_base_sha: `0bd038188e7947cd283868b8640091fd14f90857`.
- Gated merge: `ec904d87c670d26173371baa471dfd5d8cf531ae`; tree: `3255eef86f12db90197d0fe87481775783316432`.
- merge_validation: clean merged result built and tested independently; no rebase performed.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Live ga-9jqzgh39 verdict pass names the exact source; no carryover/reviewer-added commit. |
| 2 | Acceptance criteria checked | PASS | Both owned regression owners and six existing trust/refusal owners PASS by name; AC mapping below. Release certification is blocked separately by criterion 3. |
| 3 | Full release tests pass | FAIL | Five uncached remote phases PASS; required native floor failed step 13/16. Canonical execution.json exit1, criterion3_valid=false. Last three floor checks and CI accounting not run. |
| 4 | No unresolved HIGH findings | PASS | Review style/security/spec findings read; HIGH count 0. |
| 5 | Clean feature tree | PASS | Clean tracked merge and role intake; the sole intended local deploy addition is this evidence record. Active normal hooks confirmed. No source modification. |
| 6 | Merge state and build/vet | PASS | Clean merge on the pinned base. Full bazel build --config=remote-exec --noremote_local_fallback --keep_going //... completed1456 targets, zero failed, exit0; full nogo/vet also PASS in the fast lane. |
| 7 | Single feature theme | PASS | Two source commits cite ga-xn1enfv; three changed cmd/gc files implement/test inherited-rig pin selection. Ancestry scope check accepted ga-cgrwds2b and ga-xn1enfv, no stack or denied paths. |

Acceptance evidence:

- AC1: fresh `TestResolveBdBinaryForScope/explicit_dolt_metadata_with_inherited_endpoint_honors_workspace_BD_BIN` and executable/relative/missing-pin cases PASS. The inherited endpoint uses the workspace executable and invalid declared pins fail closed.
- AC2: fresh `TestBdVouchInheritedManagedRigUsesWorkspacePin` PASS. City/rig vouch share the pinned inventory; an allowlisted rig read uses it; the unrelated PATH wrapper is neither walked nor executed. No production inventory validation changed.
- AC3: independent endpoint beside invalid city binding PASS; existing resolver rows and `TestBdVouchCommand`, `TestBdTrustMatchesTheCommandTreeNotTheVersion`, `TestBdTrustBuildInfo`, both invalid-pin owners and `TestGcBdRefusesAClassFlipOnASplitCity` PASS by name.
- AC4: rebased RED parent resolved as `3a0976cbc9ed85f898aa9618e9fc8e8e5c0d3b7b`; GREEN resolved as `0d1c9fb62a0897750028808fbf48157afa08800f`. Historic corrected RED output independently inspected; fresh GREEN PASS lines re-read. Existing BUILD ownership confirmed; fresh bazel-sync has no drift. This deploy produced its own fresh-tree views and release evidence. Fresh release success is NOT established: criterion3 fails. No deadline, budget or inventory exception.
- AC5: zero-growth clause(ii) PASS. Base and merge make complexity-check both exit2 with byte-identical measurement output; pinned-base make complexity-diff exits0 with measurement header and no threshold changes. Complexity baseline untouched.

Fast lane (completed before the suite):

- policy_lane: PASS — four shell guards, pinned fmt-check-changed, full make lint/nogo and14 policy/docs/format Bazel targets. Unit phase independently ran make -k check, exit0.
- drift_lane: PASS — make bazel-sync then git diff --exit-code, exit0.
- fresh_tree: policy, drift and complexity each used fresh-tree-run.sh and gate-base.sh run on the recorded merged HEAD/tree; modified0/untracked0. Ignored outputs were excluded; full FRESH_TREE_RECORD lines retained in lane logs.
- hooks: make check-hooks PASS, core.hooksPath=.githooks; gate-record commit must use the normal pre-commit hook.
- GATE_ICU74: verdict OK with recorded library hashes. No testcontainers/container imports in this source tree; wrapper pinned test bd and scrubbed live selectors.
- heavy_mode: none; coverage_check: n/a (ordinary full-suite chain).

Criterion3 execution:

- test_cmd: gate-criterion3.sh --repo <recorded merged tree> --base <pinned base> --bead-json bead.json --owned owned-tests.tsv --run-dir criterion3/evidence. Own-code eligibility verified; city.toml enables GC_GATE_REMOTE_ENABLED=1.
- test_cmd_scope: full-suite (requested canonical chain; required native floor incomplete, NOT a passing full-scope run).
- Unit: make -k check / bazel test //...; acceptance TierA+solo; integration-packages; full integration minus declared latency owners; dedicated latency phase. Every phase forces --config=remote-exec --config=ci --nocache_test_results --noremote_local_fallback --test_env=GO_TEST_WRAP_TESTV=1.
- test_exec_mode: remote-exec, derived from BEP; native floor is its required separate top-up, not a stitched local fallback.
- test_exec_bep: unit395/395, acceptance25/25, integration-packages86/86, integration-full14/14, integration-latency1/1 remote results PASS. Every result has strategy remote, no cached flags, attempt1 and start inside phase window. Independent BEP audit retained.
- farm_test_seconds: 13886.232; fallback_reason: none.
- admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 53.94281768798828, "waited_seconds": 0.001}.
- rbe_state: valid certificate, no kill switch, matching worker pins; captured state retained.
- test_counts: PASS=147704 FAIL=1 SKIP=510
- Counts are top-level terminal result lines summed across per-job remote AND attempted floor logs; repeated executions count per job, subtests excluded. Supplementary pinned-base SKIP comparisons are excluded. Raw execution.json has no final by-name count because the floor stopped; this separate union preserves the failed result without altering raw evidence.

```text
diff_tests_executed:
  TestBdVouchInheritedManagedRigUsesWorkspacePin PASS (integration-packages-c2ab4e7d4735e1bd66a7.log, unit-2ef13e1b6898c5544764.log)
  TestResolveBdBinaryForScope PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-2bde55fab377f50b4739.log)
GATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name
```

- diff_tests_executed: both owned tests PASS, zero owned FAIL/SKIP; six existing acceptance owners also PASS (acceptance-name-evidence.txt).
- test_log_dir: /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/all-result-logs; original remote logs: /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/criterion3/evidence/remote/logs; floor logs: /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/criterion3/evidence/remote/floor-logs.
- skip_inventory: full remote inventory496 entries (424 other,72 environment-absence); all292 unique source sites UNCHANGED. The shared hunk is a stdlib bytes import in cmd/gc's test file. Fresh pinned-base whole-target comparisons: unit151/151 and integration31/31 SKIPs pre-existing, none new/reason-changed/diff-owned. Required environment-absence floor ran until the failed native step; remaining non-owned native SKIPs and reasons are inventoried separately, never credited as passes.
- owned_topup: not reached; both owned tests already PASS in the full remote suite.
- floor_topup: incomplete;12 steps exit0, step13 exit1, Darwin compile/push-gate-lock-selftest/local-concurrency-selftest UNRUN. Non-owned SKIPs inventoried; the real FAIL is retained.
- latency_leg: complete on first attempt after farm probe clear; both budget owners PASS; no latency result credited from the loading suite.
- waiver_ref: none. failure_attribution: NOT GRANTED.

Native failure and disposition:

- TestStartDrift_SystemdManaged_RestartsToNewBuildID at start_drift_test.go:128: gc start exit=-1; output reports systemd restart ready31.1s, the new supervisor build and city registration. Test elapsed180.79s; package239.266s. Underlying error/root cause NOT_ENOUGH_INFO; load alone does not prove a cause.
- Canonical native command: go test -tags integration -count=1 -v -run '^(TestStartDrift_SystemdManaged_RestartsToNewBuildID)$' ./test/integration, canonical scrubbed environment/private TMPDIR.
- Clause1: failure site UNCHANGED, no touched body/shared hunks in its file; not diff-owned. Clause4: no package/path overlap (test/integration versus cmd/gc).
- Clause2/3: no matching same-signature predating open tracker or positive mechanism, cross-PR, coverage or base-ref proof. The test builds/spawns gc, so a missing direct Go import of package-main would not prove its runtime code unreachable. No candidate retry or base reproduction performed.
- Closed-tracker lookup: FIX-IN-TREE, ga-ltzlhr0, fix0883fb40ef124e3bc6b93ba823d70418ac67f376 present. Prior missing unit-file signature differs; keep that tracker closed. New condition record ga-dhnv01xi links it and this deploy; it does not self-certify pre-existence.
- Disposition HOLD under non-diff-owned-gate-failure: guard-or-clause-4, missing=no-predating-tracker. Same-store blocks dependency on ga-dhnv01xi. Named exit: verified condition-specific fix lands, or independent evidence resolves the attribution basis on the tracker; re-read, then a fresh complete gate. Tracker close alone is insufficient. No waiver/exception, PR or clearance.

CI-required accounting:

- ci_lane_run: n/a (no CI configuration change).
- ci_required_closure: preliminary tool found4 LEAF jobs at the pinned merged workflow (credential-provider-windows and pack-gate NOT-TRIGGERED). Final accounting/runner-policy NOT RUN because C3 exit1 and criterion3_valid=false. No LOCAL-PASS or deferral invented.
- deferred_to_pr_ci: none (no PR).
- not_covered: final accounting not reached; release gate FAIL, not a promised CI deferral.

Load evidence:

- LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=46.32 run_max_load=95.13 run_mean_load=72.07 run_readings=17 wait_first_load=59.04 wait_max_load=80.20 wait_mean_load=62.47 wait_readings=61 read_errors=0

Evidence retained (full raw records kept outside the commit):

- Full execution retained: 5016 lines; /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/criterion3/evidence/execution.json; SHA256 19d81c076a6b9d9422a4b8b9bc11c3b55b546a84893b5a118ec680ff87d7d4c5.
- Full floor failure retained: 15 lines; /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/criterion3/evidence/remote/floor-logs/floor-12.log; SHA256 146a3b00d2ee58f3183804fcb0208fce21f2e4792d32f63f948e541a70b249fe.
- Full floor record retained: 903 lines; /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/criterion3/evidence/remote/floor-topup.json; SHA256 c17f8282889bf47ae64d16f6d0fcf10e56a241c7219ae1dc0c513d63736561d9.
- Full counts/by-name retained: 9 lines; /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/failed-final-by-name.txt; SHA256 d2d319bfde82851cc317647a84fcc63257a5e4f633b43e3b9a00388e6535aa3f.
- Full BEP audit retained: 52 lines; /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/independent-bep-audit.json; SHA256 12fe6238100a414c1eab7a433cdb20d9284457a148dbf12cb94195a21d4dbf26.
- Full fresh policy retained: 667 lines; /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/fast-policy-output.log; SHA256 96e54b6d89e60df1237ff070638686f9ac6ca9f705e020d356b409d28c5cb3d0.
- Full fresh drift retained: 59 lines; /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/drift-output.log; SHA256 3a1fa1237f01dd18c56dc3c17af198b8c317a3194ef784693add707bd001b153.
- Full complexity proof retained: 11 lines; /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/complexity-validation.json; SHA256 24841fc89186450004f4a9b26b3251910833a57695b7ce94da5363955180832b.
- Full unit skip comparison retained: 152 lines; /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/skip-compare-unit.txt; SHA256 4a2937856b209e13e12a3b80ef0571e11828207e57576de81b1050b9d4312cc6.
- Full integration skip comparison retained: 32 lines; /var/tmp/gc-gate.ga-cgrwds2b.resume-jcwilr18/skip-compare-integration.txt; SHA256 c6484c852b79642910baf18f243879d7bdeaa92a28290ebbbd9192e47ab44287.
