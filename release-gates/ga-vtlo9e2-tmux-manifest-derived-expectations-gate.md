# Tmux manifest derived-expectations release gate

**Verdict:** **PASS**

bead: ga-vtlo9e2
build_bead: ga-hnat6x
review_bead: ga-y60uktc
reviewed_commit: 1368a31ab0e835a4dd84bc2c3c19107eca147186
gate_base_sha: 814867f5cbe767dbf1d9253e614ce16d0530e55e
gate_base_kind: current-tip
merge_head: 2d83a74251a0cd5b81fb7c3e590320f17ca1794e
merge_tree: 468fd5e2ad75bda69ae8608e59f9382513b5ca2c
merge_state: clean (origin/main@814867f5cbe767dbf1d9253e614ce16d0530e55e)
deploy_mode: remote
evaluated_at: 2026-10-24T23:19:51.406801+00:00
evidence_dir: /var/tmp/deploy-ga-vtlo9e2-fresh.2eslvwtp
pm_ac9_revision: 2026-10-24-ac9-count-conflict-or-lost-increment

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed ga-y60uktc records style/security/spec PASS on the resolved reviewed source. No reviewer-added tests or carryover substitution. |
| 2 | Acceptance criteria met | PASS | AC1-8 verified by source/static proofs, actual named guards and negative controls; AC9 has the real old-guard lost increment, three passing merged trees and expected manifest-only same-gap conflict. D3 recount:20 integration-only files/183 tests plus647 default tests =830. AC10 complete required CI-equivalent coverage and native floor audited below. Prepared PR body carries AC5/6/9/D3 results, cutover instructions and Supersedes#8041; its actual readback is enforced before clearance/close. |
| 3 | Full-scope tests and required lanes | PASS | All five forced fresh remote phases and16 mandatory native checks complete. Two unrelated first-hit conditions are retained and attributed under all four clauses below. All7 owned tests plus3 required kept guards PASS by name. No candidate retry or waiver. |
| 4 | No unresolved HIGH findings | PASS | Review style/security/spec notes have none. |
| 5 | Branch clean | PASS | Frozen merge scratch and assigned starting checkout verified clean. Isolated deploy will contain the reviewed source plus this gate record; normal hooks and final clean-head checks are enforced. |
| 6 | Merge state and build/vet | PASS | Clean frozen merge above, normal staged hook, full remote Bazel build/nogo //... and make vet/check-hooks passed before independent fast views. Main may advance; merge authority validates the actual merged tree. |
| 7 | Single feature theme | PASS | Seven files, all under scripts: derived tmux manifest expectations, file-level lane record, real-script six-shard checks and owning documentation. One subsystem/theme. |

## Acceptance evidence

- AC1/2/7: seven owned roots and all required fixture subtests PASS in full unit output; lane/parser diagnostics cover missing/lost/absent/empty/negated files and malformed sorted basenames. Static/source evidence in acceptance-static-proof.json and acceptance.json.
- AC3: six preserved function bodies byte-identical to the frozen base; three kept discovery/drift roots PASS. Runner scripts, CODEOWNERS and manifest data entries are unchanged.
- AC4: header-only manifest mutant fails the named inventory guard with "runtime-tmux manifest contains no tests", exit3.
- AC5: whole-word grep of both specified Go test files for588/433/155/98 and this base's830/647/183/139/138 yields no matches, exit1. ac5-grep.txt and acceptance-static-proof.json retain the command/result.
- AC6: real-script root and all6 shard children PASS. Stripe mutation ==shard_index and dropped-name mutation each fail that exact root, exit3; the latter reports TestPokeActivityRealTmux selected by0 shards. No runner script change is committed.
- AC8: manifest header and scripts/AGENTS.md explain derived counts and per-file lane maintenance.
- AC9: old A/B each edit total830->831 and default647->648; their clean merge0 discovers832 while recording831 and the OLD named inventory guard fails specifically with that mismatch, exit3. This satisfies PM's recorded corrected requirement, not a claim that identical increments must conflict. New A+B/A+C/C+D each merge0 and PASS inventory/lane/real-script roots plus6 shard children. Same-gap control conflicts1 solely in the manifest. Raw branch diffs, merged-tree results and logs: merge-proof-corrected/ and same-gap.json.
- AC10/D3: migration-proof.json and merge-proof-corrected/base-inventory.json retain the20-file per-name canonical Linux lane census:183 integration-only +647 untagged =830. Full gate coverage below completes the required check surface. PR body includes the table and required cutover instructions; refreshed PR-list provenance and actual body readback remain publication records rather than test substitutes.

## Full-scope execution

test_cmd: canonical gate-criterion3.sh, followed ONLY by never-run successors after each documented attribution; canonical gate-rbe-run.py performs the native floor after all five original farm phases are retained
test_cmd_scope: full-suite
test_counts: PASS=126261 FAIL=2 SKIP=370
native_floor_counts: 21 PASS,0 FAIL,14 SKIP plus3 non-Go compile/runner checks exit0
actual_remote_results: 497
test_exec_mode: remote-exec (BEP-derived; mandatory native floor recorded separately)
test_exec_bep: /var/tmp/deploy-ga-vtlo9e2-fresh.2eslvwtp/c3/evidence/remote/{unit,acceptance,integration-packages,integration-full,integration-latency}.bep.json; all results remote, neither cached flag, attempt1, start in phase window, terminal verdict present
farm_test_seconds: 10587.721
heavy_mode: none
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
owned_topup: [] (all required tests already PASS)
floor_topup: /var/tmp/deploy-ga-vtlo9e2-fresh.2eslvwtp/c3/evidence/remote/floor-topup.json; complete=true,16/16 steps exit0 including beadstest, Darwin compile and both runner self-tests
latency_leg: /var/tmp/deploy-ga-vtlo9e2-fresh.2eslvwtp/c3/evidence/remote/latency-leg.json; original FAILED after clear probe, attributed to ga-bqsrp4o; never retried, never relabelled measured/PASS or busy/deferred
skip_inventory: /var/tmp/deploy-ga-vtlo9e2-fresh.2eslvwtp/c3/evidence/remote/skip-inventory.json;356 remote rows,71 environment-absence and285 other; native residual reasons retained in floor logs
skip_justification: no owned or required guard SKIP. Every environment-absence row selected for canonical package top-up, with parents selected for subtests. Remaining cases are unchanged platform/opt-in/tool/fixture/conformance exclusions, with printed reasons retained. The changed tests and their required helper/discovery callers all report real PASS; no production input changed. Native residual non-owned SKIPs remain inventoried, not credited as PASS.
test_log_dir: /var/tmp/deploy-ga-vtlo9e2-fresh.2eslvwtp/c3/evidence/remote/logs

- TestRuntimeTmuxGuardAddingATestCostsOnlyManifestLines PASS (unit-b0c13e82fd961ff4f637.log)
- TestRuntimeTmuxIntegrationShardUsesCheckedManifestOnLinux PASS (unit-9f907e0fea7ca680f21c.log)
- TestRuntimeTmuxLaneDriftDiagnostics PASS (unit-9f907e0fea7ca680f21c.log)
- TestRuntimeTmuxLaneRecordMatchesTree PASS (unit-ee115c844954da53d34d.log)
- TestRuntimeTmuxLaneRecordParser PASS (unit-81d8a9af827e251f6b12.log)
- TestRuntimeTmuxManifestDiscoveryDistinguishesTestMainHarness PASS (unit-81d8a9af827e251f6b12.log)
- TestRuntimeTmuxManifestDiscoveryUsesCanonicalLinuxPlatform PASS (unit-271bc77ad6715453dfe1.log)
- TestRuntimeTmuxManifestDriftDiagnostics PASS (unit-9f907e0fea7ca680f21c.log)
- TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory PASS (unit-db0feb4ad70da6248282.log)
- TestRuntimeTmuxPerFileDiscoveryOrdersFilesThenTests PASS (unit-271bc77ad6715453dfe1.log)

| Canonical phase | Fresh remote results | Farm test seconds | Verdict |
|---|---|---|---|
| unit | 373 | 2743.031 | PASS |
| acceptance | 24 | 1120.756 | PASS |
| integration-packages | 85 | 3847.619 | PASS |
| integration-full | 14 | 2705.339 | original FAILED retained; attributed below |
| integration-latency | 1 | 170.976 | original FAILED retained; attributed below |

All five original phases force --nocache_test_results and --noremote_local_fallback. All994 raw remote artifacts were independently SHA256-verified. Original primary execution.json remains criterion3_valid=false/exit1; initial continuation-result.json remains exit1 after the real latency failure. Both failed phase verdicts, stdout/BEP/evidence files, latency-leg.json and original raw logs are hash-fenced by retained-all-phases-sha256.json and unchanged. floor-continuation-result.json records exit0 only for completion of the remaining native work, original_evidence_unchanged=true and no candidate/latency retry. No remote/local replacement suites were stitched.

rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45", "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"}
admission_initial: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 69.79384994506836, "waited_seconds": 0.014}
admission_latency_continuation: {"class": "remote", "floor_gib": 25.0, "free_gib": 72.75864028930664, "waited_seconds": 0.001, "cap": 2}
admission_native_continuation: {"class": "remote", "floor_gib": 25.0, "free_gib": 71.75809478759766, "waited_seconds": 0.001, "cap": 2}
fallback_reason: none

## Failure attribution

1. TestGCLiveContract_BeadsAndEvents/GET_/v0/city/{cityName}/work/active-step -> ga-od4rcxy, proof(a). Site1512 UNCHANGED/shared_hunks0; fixture file byte-identical to frozen base. Fresh63-directory integration test and178-directory external gc binary closures reach no changed non-test input. The only changes are scripts guards/data; the422 missing query.assignee is unchanged URL construction, not a timing assertion. No path overlap. Tracker predates run, was read, and historical-base sighting was appended/read-verified. Its separate remedy landed in#8276 after the frozen base; tracker correctly remains closed. First active-step hit on this deploy, no fix-carrying exception, post-fix-main regression or inconclusive-load claim. Full proof: integration-failure-proof.json.
2. TestGcSeatHookLatencyBudget -> ga-bqsrp4o, proof(a). Site180 UNCHANGED/shared_hunks0; latency test file and external gc production inputs unchanged. Same exact-tree integration AND external gc closures reach none of the diff. New scripts fixtures ran in the earlier unit phase and cannot execute in this latency binary. No path overlap. Existing OPEN tracker predates run; prior comments checked for this deploy's first sighting, new sighting appended/read-verified. Fastest prime call1.019s exceeds1s after a clear farm probe: a real failure, with budget/threshold unchanged. Designated fix ga-yu01v6x is unlanded; its reviewed deploy ga-k6vx9u7 is mayor's next item. No fix-carrying exception or retry. Full proof: latency-failure-proof.json.

This diff adds a test source/fixtures in the existing scripts target, not a new test target or census change. Neither attribution uses the inconclusive added-load escape; both have decisive runtime/import mechanism proofs including the external executable edge.

## Fast lanes, runtime and load

policy_lane: PASS; pinned scripts lint, fmt-check, full nogo/vet, required shell guards and policy targets in a fresh independent view
drift_lane: PASS; make bazel-sync then git diff --exit-code in a separate view, and schema/spec/client/dashboard/lockfile checks in another
runtime_setup: ICU74 recorded-library check PASS, active podman.socket, pinned cached Dolt image matches deps.env; caller explicitly scrubs fleet selectors before the isolation wrapper's PASS-THROUGH mode

```text
GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-vtlo9e2-fresh.2eslvwtp/merge head=2d83a74251a0 tree=468fd5e2ad75 dir=/var/tmp/gc-fresh-tree.QOF4Br modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=814867f5cbe767dbf1d9253e614ce16d0530e55e knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.QOF4Br
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-vtlo9e2-fresh.2eslvwtp/merge head=2d83a74251a0 tree=468fd5e2ad75 dir=/var/tmp/gc-fresh-tree.A764M9 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
FRESH_TREE_RECORD from=/var/tmp/deploy-ga-vtlo9e2-fresh.2eslvwtp/merge head=2d83a74251a0 tree=468fd5e2ad75 dir=/var/tmp/gc-fresh-tree.JgNmG0 modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1802 wait_timed_out=1 run_start_load=133.90 run_max_load=149.33 run_mean_load=136.25 run_readings=11 wait_first_load=79.67 wait_max_load=133.90 wait_mean_load=104.23 wait_readings=61 read_errors=0
```

The native floor keeps the documented bounded local soft wait and local-heavy cap; the exact observed loads and timeout flag are above. Remote admission uses25GiB/cap2 without a host-heavy slot or host load wait. Raw logs and original failed records remain retained. Publication must preserve the earlier FAIL gate commit/branch before mechanically cutting the new isolated deploy branch, then verify PR body/current head and clearance before mayor handoff.

diff_tests_executed:
  TestRuntimeTmuxGuardAddingATestCostsOnlyManifestLines PASS (unit-b0c13e82fd961ff4f637.log)
  TestRuntimeTmuxIntegrationShardUsesCheckedManifestOnLinux PASS (unit-9f907e0fea7ca680f21c.log)
  TestRuntimeTmuxLaneDriftDiagnostics PASS (unit-9f907e0fea7ca680f21c.log)
  TestRuntimeTmuxLaneRecordMatchesTree PASS (unit-ee115c844954da53d34d.log)
  TestRuntimeTmuxLaneRecordParser PASS (unit-81d8a9af827e251f6b12.log)
  TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory PASS (unit-db0feb4ad70da6248282.log)
  TestRuntimeTmuxPerFileDiscoveryOrdersFilesThenTests PASS (unit-271bc77ad6715453dfe1.log)

Publication snapshot: mayor msg gm-wisp-77g0zt8 delivered 2026-10-24T23:19:44Z, complete639 OPEN PRs and all per-PR file pages/counts checked;22 affected (4 team/18 external), unchanged from earlier list. Deployer independently rechecked only the4 team PRs and only known automation on predecessor#8041. Current main inventory recount at15d478a06ea2b0c17ff13620f21fc5444eb875a5 has identical per-file names/lanes (647 default/830 integration); current-main-inventory-proof.json retains the exact read-only evidence.
