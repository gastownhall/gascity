# Herdr shared town workspace release gate

**Verdict:** **PASS**

Recorded: 2026-11-10T18:38:04.476490+00:00
Bead: ga-bh88q2ec; build: ga-8nznbbyc; review: ga-dw6nbnbl.
Reviewed source: `766b9109fd01555d2fe984e24e1d70fbe54d7965`.
Gated merge: `44df38a12ffdadcd022df5a7f2a1d2330dae6937`; tree: `57c8c831fce87d0bc43485ce0869357e9b515321`.
deploy_mode: remote; push_remote: fork; provenance: own reviewed code.
merge_state: clean (origin/main@bad83762c64a670ec2c3626f15e498705e464ec7; frozen at criterion 6).
gate_base_kind: current-tip; gate_base_sha: bad83762c64a670ec2c3626f15e498705e464ec7
merge_validation: MPR validates the eventual merge against its current base; this record preserves the tested base and tree.
Evidence root: `/var/tmp/deploy-ga-bh88q2ec.v5hd2moa`; raw logs retained.

| # | Result | Evidence |
|---|---|---|
| 1 | PASS | Reviewer PASS records the exact source above. No carryover or reviewer-added tests. |
| 2 | PASS | Bare town-agent names share the city workspace; explicit town/rig names, GC_RIG grouping and the empty-town fallback are covered by both placement roots and all 24 table cases in unit and integration output. Provider.start passes its existing city session name. Both documentation changes match the implementation. Optional cosmetic tab labels remain a separate concern, as the build contract permits. See acceptance-scope-audit.json. |
| 3 | PASS | Canonical full suite, native floor, by-name audit, skip proofs and every required-CI LEAF account complete; details below. |
| 4 | PASS | Zero unresolved HIGH findings. The review records only informational residuals. |
| 5 | PASS | Exact reviewed source is checked out on isolated deploy/ga-bh88q2ec-gate, clean before this record; the suite scratch is also clean. Commit cleanliness is rechecked before push. |
| 6 | PASS | Clean synthetic merge onto the frozen base; bazel build //... and make vet (nogo) pass. Raw terminal BEPs, exact parents/tree and hashes verified in c6-verified.json. |
| 7 | PASS | One herdr workspace-grouping theme: provider logic, placement tests and matching reference/design text. Two source commits cite the verified build lineage; ancestry scope passes, no stack. |

## Tests and policy

test_cmd: gate-criterion3.sh --repo /var/tmp/deploy-ga-bh88q2ec.v5hd2moa/merge --base bad83762c64a670ec2c3626f15e498705e464ec7 --bead-json /var/tmp/deploy-ga-bh88q2ec.v5hd2moa/ga-bh88q2ec.json --owned /var/tmp/deploy-ga-bh88q2ec.v5hd2moa/owned.tsv --run-dir /var/tmp/deploy-ga-bh88q2ec.v5hd2moa/c3-run/evidence
test_cmd_scope: full-suite
test_counts: {"FAIL": 0, "PASS": 150487, "SKIP": 515} (canonical top-level Go result lines; inventories include subtest events).
non_go_skips: 1 Python check, LockfilesInSyncTest.test_rules_ts_typescript_matches_lock, configured non-applicable for openclaw-bridge; non-go-skip-reviewed.json retains the raw summary, unchanged source/data proof and the inference from the only skip site plus the false macro flag. The dashboard counterpart runs all three checks successfully.
test_log_dir: /var/tmp/deploy-ga-bh88q2ec.v5hd2moa/c3-run/evidence/remote/logs
waiver_ref: none; failure_attribution: none.
policy_lane: PASS — pinned changed-format check plus full-tree //:go_format_test, full lint/nogo, shell guards, hook check and 13 Bazel policy targets.
drift_lane: PASS — make bazel-sync then git diff --exit-code and empty full porcelain; seven generated-artifact targets plus OpenAPI compatibility pass. Each of the three fast lanes used a separate fresh view before runtime setup and the suite.
storeclass_policy: PASS — merge-base trusted generator checks baseline and growth with no allow-raise flag; storeclass-proof/verified.json retains commands and raw output. This separate required policy was discovered in the live ruleset review after the original fast lanes and suite launch, and completed before PASS. No actual PR-CI result claimed.
ci_lane_run: n/a (no CI configuration changed).
heavy_mode: none (computed classifier).
runtime_preflight: ICU74 OK; pinned bd v1.3.4 and noninteractive credentials. The isolation wrapper reports pass-through for the scratch; canonical remote/native commands provide their prescribed environment isolation.

The five canonical phases cover the full unit/policy tree, acceptance, integration-tagged packages, full integration excluding the latency inventory, then the latency inventory under a clear farm probe. Every remote test result is uncached, remote-executed, attempt 1, within the phase window and terminal. No farm/local suite stitching or local fallback.

```json
{
  "test_exec_mode": "remote-exec",
  "test_exec_bep": {
    "unit": {
      "results": 408,
      "remote_executed": 408,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "acceptance": {
      "results": 25,
      "remote_executed": 25,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-packages": {
      "results": 86,
      "remote_executed": 86,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-full": {
      "results": 14,
      "remote_executed": 14,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    },
    "integration-latency": {
      "results": 1,
      "remote_executed": 1,
      "cached_remotely": 0,
      "cached_locally": 0,
      "other_strategy": 0,
      "retried": 0,
      "window_ok": true
    }
  },
  "rbe_state": {
    "rc_marker_present": true,
    "kill_switch_absent": true,
    "cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT",
    "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98",
    "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"
  },
  "fallback_reason": null,
  "admission": {
    "class": "remote",
    "floor_gib": 25.0,
    "free_gib": 70.0458755493164,
    "waited_seconds": 0.001,
    "cap": 2
  },
  "farm_test_seconds": 13800.583,
  "owned_topup": []
}
```

skip_inventory: 501 remote events; 39 native events; full per-test reasons in remote/skip-inventory.json and floor-skip-inventory.json.
floor_topup: complete; 16 steps exit 0, including beadstest, Darwin compilation and both runner self-tests; remote/floor-topup.json.
latency_leg: measured under a clear probe; both inventory tests PASS; remote/latency-leg.json.

diff_tests_executed:
```text
TestPlacementFor PASS (integration-packages-0ba2c1cd80ef2ec12177.log, unit-d920ddbd08781200929a.log)
  TestWorkspaceTabFor PASS (integration-packages-0ba2c1cd80ef2ec12177.log, unit-d920ddbd08781200929a.log)
GATE_TEST_EVIDENCE_RESULT ok: 2 diff-owned tests verified by name
```

## Skip justification

No owned test skipped or failed. All skip sites are outside the diff and their complete source-file blobs match the frozen base. Changed shared test state is the placement testTown constant; only the placement tests read it. Import-closure-reachable remote skip events (200) reproduce at the pinned base with the same commands and reasons; 301 other remote events have no shared-hunk reach. All native skips have same-reason base comparisons. Site, helper, closure and reason evidence is retained in skip-proof-verified.json, skip-comparisons/ and both skip-audit-prepared files.
Reviewed all normalized skip reason groups and source sites. Process-lane and GC_FAST_UNIT skips are guards before the process fixtures; unsupported-OS/provider, helper-only, fixed pending/golden/replay switches and absent artifacts retain their existing conditions. Runtime optional dead-server/nudged fixture checks precede provider calls. Herdr RequireLive uses only short-mode, executable presence and explicit live-tier opt-in before placement; native herdr is present but live tier remains off. Changed testTown is used only by the placement tests. The other 301 remote events have no shared-input reach per transitive, tag-aware closures. The one dispatch subtest reason was recovered from its retained raw log, not rerun or invented. Native floor ran every required absence package; remaining skips are inventoried rather than credited as executions. All 200 reachable remote events and all 39 native events now reproduce at the pinned base with matching reasons and no new/changed/owned/unjudged skip. The 301 unreachable remote events retain complete unchanged source and tag-aware closure proof. Original controller failed before any baseline test; corrected continuation completes the proof, with original failure retained in skip-controller-recovery.json.

## Required CI job accounting

ci_required_closure: ci_required_triggers.py /var/tmp/deploy-ga-bh88q2ec.v5hd2moa/merge bad83762c64a670ec2c3626f15e498705e464ec7 766b9109fd01555d2fe984e24e1d70fbe54d7965 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
ci_required_accounting:

- changes | LOCAL-PASS | /var/tmp/deploy-ga-bh88q2ec.v5hd2moa/ci-required/closure.stdout
- runner-policy | LOCAL-PASS | /var/tmp/deploy-ga-bh88q2ec.v5hd2moa/ci-required/runner-policy.stdout
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | /var/tmp/deploy-ga-bh88q2ec.v5hd2moa/ci-required/windows-native-reach.json
- windows-lane | LOCAL-PASS | /var/tmp/deploy-ga-bh88q2ec.v5hd2moa/ci-required/windows-lane.stdout; exit=0
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

Locally run job steps match the frozen workflow run commands, environment and working directories. Full commands and streams are in ci-required/*-steps.json. Runner policy used EVENT_NAME=pull_request, PR_AUTHOR=quad341 and empty FORCE_BLACKSMITH. Every locally run build job used a fresh view and the official load/isolation wrappers, serially. Trigger rows are copied from the derived closure.
windows_reach: none (credential-provider and native Windows-test closures checked).
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
Merge readiness requires CI / required green on the PR head and every deferred job successful, or skipped only because its own path filter is false.

## Load and retained proof

Remote admission uses its prescribed exemption from the host load wait. Every native leg retains the official measurements below; a bounded-wait timeout is recorded and is not a waiver.

- c3-run/evidence/remote/floor-topup.stdout: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.92 run_max_load=54.80 run_mean_load=39.77 run_readings=12 wait_first_load=11.92 wait_max_load=11.92 wait_mean_load=11.92 wait_readings=1 read_errors=0`
- ci-required/windows-lane.stderr: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=60.26 run_max_load=90.19 run_mean_load=80.01 run_readings=8 wait_first_load=57.12 wait_max_load=107.54 wait_mean_load=70.68 wait_readings=61 read_errors=0`
- skip-base-evidence/native.stderr: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=15.10 run_max_load=17.45 run_mean_load=16.01 run_readings=3 wait_first_load=72.51 wait_max_load=84.81 wait_mean_load=57.06 wait_readings=61 read_errors=0`
- storeclass-proof/wrapper.stderr: `LOAD_GATE_SUMMARY threshold=15 waited_seconds=1531 wait_timed_out=0 run_start_load=14.88 run_max_load=17.27 run_mean_load=16.52 run_readings=4 wait_first_load=71.61 wait_max_load=84.35 wait_mean_load=55.63 wait_readings=52 read_errors=0`

Independent audits: c6-verified.json; fast-verified.json; unit-owned-verified.json; acceptance-verified.json; integration-packages-verified.json; resume-c3-audit.json (1068 artifact digests); acceptance-scope-audit.json; ci-required/accounting-verified.json; skip-proof-verified.json.
Skip-audit continuation: skip-controller-recovery.json records the deployer-owned early-context setup error; original failed service retained, no baseline test started there. The continuation reloads the finalized context and completes the pending proof. No test failure was attributed or waived.
All raw evidence remains at the root above. The deployer performs no merge.
