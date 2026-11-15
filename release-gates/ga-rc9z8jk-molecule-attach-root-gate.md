# Attachment nesting root — ga-rc9z8jk

**Verdict:** **PASS**

Reviewed source `3e5f727632c6e3474cf19fb0c4b280f224b60884`. Build ga-6vvy9yt; review ga-b5d8h4x PASS at this exact resolved source. No reviewer-added tests or carryover.
Canonical merge `3fa0db231682055c4ab88fb920f461c81b06bf19`; tree `828b1ff4bc6bece4d95d82671dc7c9da929f3a9b`.
DEPLOY_MODE: remote. BASE_REF: origin/main.
merge_state: clean (origin/main@6935c35d6c1aa25215ae428a8572e399b811ed5c)
gate_base_kind: current-tip
gate_base: 6935c35d6c1aa25215ae428a8572e399b811ed5c
merge_validation: exact merged tree built/vetted; MPR and merge-group checks validate eventual merge.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Full ga-b5d8h4x reviewed; exact source resolves and matches reviewer commit, no HIGH finding. |
| 2 | Acceptance met | PASS | All5 owned roots and3 preserved neighbors PASS, including subcases. AC1–8 mapped below; AC7 prepared body must be rechecked on actual PR API body at publication. |
| 3 | Tests pass | PASS | Five fresh remote phases/497 result records, all16 native-floor jobs; 126515 PASS/0 FAIL/370 explained SKIP observations. No owned FAIL/SKIP/MISSING. |
| 4 | No unresolved HIGH | PASS | Review reports no blocker/major/security finding. Unchanged AC2 test comment is explicitly nonblocking and remains untouched. |
| 5 | Final source clean | PASS | Canonical merge clean at audit. Isolated deploy must contain reviewed source plus gate record only, committed and clean. |
| 6 | Merge state and build/vet | PASS | Clean merge onto pinned main; full Bazel build, make vet/nogo and normal hooks PASS. Expected merge-tree equals committed tree. |
| 7 | One feature theme | PASS | Four Go source/test files change read-side nesting root selection only. Both commits cite build molecule ga-gr4rfls; its typed gc.var.issue=ga-6vvy9yt proves accepted lineage. No unrelated theme, stack or internal-doc contamination. |

## Acceptance and scope

Nesting reads workflow_id, then gc.root_bead_id, then self ID, trimming blank strings. molecule_id names the source bead's own attachment, not its containing workflow. The unchanged cost/usage resolver retains molecule_id precedence. The closed/unresolvable-container guard remains intact. No migration, repair of existing mis-rooted beads, write-path change, attachment adoption/burn/clear, or new lifecycle mechanism.

AC1: both no-edge and blocks-edge incident cases assert WorkflowRootID/self, every mapped child root/self, no root under the old molecule, normal source-to-new-root dependency, and unchanged old molecule/step status and metadata. AC2: TestAttachResolvesRootFromRunChainNotOwnID is unchanged and PASS. AC3: both closed workflow_id/root-bead cases self-root and PASS; the retained molecule_id-closed test PASSes but intentionally no longer pins this guard. AC4: live workflow_id/root-bead precedence both PASS. AC5: six-row table and ResolveRunID molecule-id-priority pin PASS; grouping logic and other callers unchanged. AC6: both resolver comments and Attach comment explain current semantics; engdocs/design/usage-facts-v0.md has no shared-resolver claim to change. AC7: independently checked at source AND canonical merge: zero molecule_id/MoleculeIDMetadataKey references anywhere under internal/dispatch, one Attach(control.ID) call at control.go:815, and fixtures carrying workflow_id/gc.root_bead_id. The required evidence is in the prepared PR body and must be API-verified before clearance. AC8: historical reviewer mutations each assertion-killed: restoring ResolveRunID breaks both incident cases/root-bead precedence; deleting the guard breaks both closed-key cases; removing the molecule_id grouping hop breaks the new pin/old table cases. These are attributed inputs, not newly run deployer mutations.

Builder's mistaken initial RED hash was corrected in its notes. Resolved historical pins:

```json
{
  "red": "93d5e9e226d2c765f12775ab944bfae196fc4cea",
  "green": "3e5f727632c6e3474cf19fb0c4b280f224b60884",
  "both_resolved": true,
  "red_source_ancestor": true,
  "scope": "Builder/reviewer historical inputs; no deployer RED or mutation rerun"
}
```

The previous active-step fixture hold ended only after ga-q422865/ga-od4rcxy were typed shipped at `24d8bd2f57e336c9bad03bcfe9a1ef0961942a8b` and independently proved ancestor of pinned main. Their two obsolete blocks edges were removed with live readback. Old failed evidence remains intact and receives no credit; this is one complete new run. Builder/reviewer narrower/cached runs are inputs only. No source rewrite, self-rebase, runtime retry, waiver or production deployment.

Prepared PR checks:

```json
{
  "AC7_dispatch_evidence": true,
  "read_side_scope_and_grouping": true,
  "AC8_historical_mutations": true
}
```

## Criterion 3

test_cmd_scope: full-suite
test_exec_mode: remote-exec (BEP-derived)
waiver_ref: none (Gas City has no waiver path)
failure_attribution: none
heavy_mode: none
ci_lane_run: n/a (no CI configuration change)
fallback_reason: none
farm_test_seconds: 10414.19
test_log_dir: /var/tmp/deploy-ga-rc9z8jk-fresh.fizu3j6i/c3/evidence/remote/logs
test_counts: 126515 PASS/0 FAIL/370 SKIP (root observations per job, not unique tests)
diff_tests_executed: {"TestAttachClosedNestingRootSelfRoots": "PASS", "TestAttachDoesNotNestUnderMoleculeIDAttachment": "PASS", "TestAttachLiveRunChainBeatsMoleculeID": "PASS", "TestResolveNestingRootID": "PASS", "TestResolveRunIDKeepsMoleculeIDAheadOfRootBeadID": "PASS"}
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 24, "results": 24, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 85, "results": 85, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 373, "results": 373, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45", "worker_env_tree": "sha256:7065d0a100bca411f10804c9ea0e2af60a34be153e94f7cb86f81135e1b98b45"}
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 56.689056396484375, "waited_seconds": 0.001}
owned_topup: []
floor_topup: complete; 16 jobs; original commands/verdicts in execution.json
skip_inventory: {"environment-absence": 71, "other": 285}
latency_leg: {"attempts": [{"attempt": 1, "evidence_errors": [], "farm_test_seconds": 150.144, "phase": ["GATE_LATENCY phase=integration-latency tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$", "GATE_LATENCY threshold=50 hold_max=600 poll=10 probe_text_sha256=5fd31cdd75b202be3ab9de14bb2514f059b7969d1f14ab7fdc392c2c723e6b7b"], "probe": ["GATE_FARM_PROBE waiting waited=0s load1=56.20 load5=76.11 load15=43.06 cpus=32 threshold=50 max_wait=600s", "GATE_FARM_PROBE verdict=clear waited=30s load1=45.31 load5=71.87 load15=42.72 cpus=32 threshold=50 max_wait=600s"], "probe_verdict": "clear", "rc": 0, "results": {"TestGcBdLatencyBudget": {"PASS": 1}, "TestGcSeatHookLatencyBudget": {"PASS": 1}}, "test": ["stores: city proxied-server, rig proxied-server; cpus 32; loadavg 27.67 61.97 41.29 12/1125 51617"]}], "attempts_allowed": 2, "complete": true, "main_leg": ["GATE_LATENCY phase=integration-full tests=TestGcBdLatencyBudget,TestGcSeatHookLatencyBudget regex=^(TestGcBdLatencyBudget|TestGcSeatHookLatencyBudget)$"], "main_leg_verdicts": {"TestGcBdLatencyBudget": {}, "TestGcSeatHookLatencyBudget": {}}, "message": "measured under probe clear: TestGcBdLatencyBudget=PASS TestGcSeatHookLatencyBudget=PASS", "phase": "integration-latency", "rc": 0, "retired_farm_test_seconds": 0.0, "tests": ["TestGcBdLatencyBudget", "TestGcSeatHookLatencyBudget"], "verdict": "measured", "verdicts": {"TestGcBdLatencyBudget": {"PASS": 1}, "TestGcSeatHookLatencyBudget": {"PASS": 1}}}
city_protocol_commit_at_criterion3_start: ecc037f6615203a26e842733462e830a15a2fc30

| Phase | Results | Fresh remote | Cache credits | Retries | Verdict |
|---|---:|---:|---:|---:|---|
| unit | 373 | 373 | 0 | 0 | PASS |
| acceptance | 24 | 24 | 0 | 0 | PASS |
| integration-packages | 85 | 85 | 0 | 0 | PASS |
| integration-full | 14 | 14 | 0 | 0 | PASS |
| integration-latency | 1 | 1 | 0 | 0 | PASS |

Coverage matches the current required Bazel CI scope: unit/make check with policy, formatting, generated and dashboard gates; Tier-A acceptance; integration-tagged packages; full integration excluding latency names; separate latency. This includes dispatch and CLI cook/attach coverage. The native floor covers beadstest, Darwin compile, runner self-tests and every environment-absence skip package (parent selected for subtests). Raw BEP independently proves remote attempt1, in-window start, terminal result and neither cached flag for every record. All994 raw log/XML artifact SHA256s independently match. No farm/local suite stitching.

Actual canonical commands:

```text
test_cmd: phase=acceptance BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-rc9z8jk-fresh.fizu3j6i/c3/evidence/remote/acceptance.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-full BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-rc9z8jk-fresh.fizu3j6i/c3/evidence/remote/integration-full.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-latency BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-rc9z8jk-fresh.fizu3j6i/c3/evidence/remote/integration-latency.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=integration-packages BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-rc9z8jk-fresh.fizu3j6i/c3/evidence/remote/integration-packages.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
test_cmd: phase=unit BAZEL_FLAGS=--config=remote-exec --keep_going --config=ci --nocache_test_results --noremote_local_fallback --rewind_lost_inputs --remote_download_outputs=toplevel --test_env=GO_TEST_WRAP_TESTV=1 --build_event_json_file=/var/tmp/deploy-ga-rc9z8jk-fresh.fizu3j6i/c3/evidence/remote/unit.bep.json NOGO_BUILD_FLAGS=--keep_going --output_groups=nogo_fix --config=remote-exec
```

Owned reader:

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-rc9z8jk-fresh.fizu3j6i/c3/evidence/remote/logs logs=513 logs_with_results=447
test_counts: PASS=126515 FAIL=0 SKIP=370
  (top-level result lines, column 0, summed over the 513 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestResolveNestingRootID PASS (unit-526163b3abc0bd7503a0.log)
  TestResolveRunIDKeepsMoleculeIDAheadOfRootBeadID PASS (unit-526163b3abc0bd7503a0.log)
  TestAttachClosedNestingRootSelfRoots PASS (unit-46cac984b12b94b66cce.log)
  TestAttachDoesNotNestUnderMoleculeIDAttachment PASS (unit-46cac984b12b94b66cce.log)
  TestAttachLiveRunChainBeatsMoleculeID PASS (unit-46cac984b12b94b66cce.log)
GATE_TEST_EVIDENCE_RESULT ok: 5 diff-owned tests verified by name
```

All3 supplemental resolver/attachment roots and their reported subcases independently PASS (affected-neighbors-verified.json). They are not relabelled diff-owned.

policy_lane: PASS — pinned formatter2.12.0, all-tree nogo, eight shell guards and seven policy targets.
drift_lane: PASS — make bazel-sync and empty tracked/untracked status, plus seven generated schema/client/bundle/lockfile checks. Every fast lane uses its own fresh view and the frozen base.

```text
GLT_RECORD: {"golangci_lint":{"pin":"2.12.0","resolved":"2.12.0","mismatch":false}}
FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-rc9z8jk.d2v6inf0 head=3fa0db231682 tree=828b1ff4bc6b dir=/var/tmp/gc-fresh-tree.ktHqgC modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-ga-rc9z8jk.d2v6inf0,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=6935c35d6c1aa25215ae428a8572e399b811ed5c knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.ktHqgC
FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-rc9z8jk.d2v6inf0 head=3fa0db231682 tree=828b1ff4bc6b dir=/var/tmp/gc-fresh-tree.PNj1Zr modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-ga-rc9z8jk.d2v6inf0,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=6935c35d6c1aa25215ae428a8572e399b811ed5c knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.PNj1Zr
FRESH_TREE_RECORD from=/var/tmp/gc-merge-ga-rc9z8jk.d2v6inf0 head=3fa0db231682 tree=828b1ff4bc6b dir=/var/tmp/gc-fresh-tree.UZdJVy modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-gc-merge-ga-rc9z8jk.d2v6inf0,bazel-out,bazel-testlogs
GATE_BASE_RECORD base=6935c35d6c1aa25215ae428a8572e399b811ed5c knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.UZdJVy
```

Remote admission25GiB/cap2 requires no local-heavy slot/load wait. Native floor preserves its soft wait. Observed metrics:

```json
[
  {
    "load_threshold": "15",
    "load_waited_seconds": "0",
    "load_wait_timed_out": "0",
    "wait_timed_out": "0",
    "run_start_load": "13.74",
    "run_max_load": "15.17",
    "run_mean_load": "12.33",
    "run_readings": "45",
    "wait_first_load": "13.74",
    "wait_max_load": "13.74",
    "wait_mean_load": "13.74",
    "wait_readings": "1"
  },
  {
    "load_threshold": "15",
    "load_waited_seconds": "0",
    "load_wait_timed_out": "0",
    "wait_timed_out": "0",
    "run_start_load": "9.90",
    "run_max_load": "11.09",
    "run_mean_load": "10.60",
    "run_readings": "6",
    "wait_first_load": "9.90",
    "wait_max_load": "9.90",
    "wait_mean_load": "9.90",
    "wait_readings": "1"
  }
]
```

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.74 run_max_load=15.17 run_mean_load=12.33 run_readings=45 wait_first_load=13.74 wait_max_load=13.74 wait_mean_load=13.74 wait_readings=1 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=9.90 run_max_load=11.09 run_mean_load=10.60 run_readings=6 wait_first_load=9.90 wait_max_load=9.90 wait_mean_load=9.90 wait_readings=1 read_errors=0
```

Runtime wrapper reports PASS-THROUGH honestly. Rootless podman/socket and pinned image were checked before tests. Native ICU74, with no overlay or library override:

```text
GATE_ICU74 lib=libicui18n.so.74 file="/usr/local/lib64/icu74-compat/libicui18n.so.74.2" sha256=d90d62f0d7b6c6fbc279ae5e433cc7004aa47cdb106a71a02350099f8df9ae51 recorded=yes
GATE_ICU74 lib=libicuuc.so.74 file="/usr/local/lib64/icu74-compat/libicuuc.so.74.2" sha256=33499dd97a5393926b32e0b535f5340f7dae14b35e59025b572e6bd9ad48a2ae recorded=yes
GATE_ICU74 lib=libicudata.so.74 file="/usr/local/lib64/icu74-compat/libicudata.so.74.2" sha256=c891c5fdb45e3cf756b728abe256be7af1bfc97f00ca2f2fac450c96c09bd4b7 recorded=yes
GATE_ICU74 verdict=OK provenance=recorded sonames="libicui18n.so.74 libicuuc.so.74 libicudata.so.74"
```

## Skip ownership

The initial read-only audit stopped when a shared-helper filename was reported under its calling package. DOT returned undecided3 for that incorrect path, never a test verdict. The observer now resolves a missing path only when the calling test binary import closure contains exactly one tracked helper filename, under each actual phase tag set (acceptance_a and integration read from .bazelrc). Each source site is still independently checked. Original stopped observer versions/errors are preserved in audit-observer-initial and audit-observer-second. All seven missing-path groups were resolved in one batch; skip-helper-path-resolutions.json retains actual source lines and tag-specific import proofs. No runtime test, raw result or source was changed or retried. In particular close_notification_conformance.go:54 is in internal/beads/beadstest, not its caller internal/beads; its unchanged conditional-write-unsupported skip comes from the store conformance helper, not attachment nesting.

No owned test or test in an edited test file skipped. Each distinct skip source site was mechanically checked UNCHANGED with zero shared hunks and no file diff; original reasons retained in skip-ownership-reviewed.json (268 sites). Only shared hunks are reflect import and the incident fixture helper in attach_test.go, which has no SKIP. The tooling-formula os.Stat skip in molecule_test.go:30 is in an unchanged file before its assertions, independent of the pure metadata resolver. Remaining platform/provider/configuration/environment preconditions are unchanged. Environment-absence skips receive native top-ups; residual non-owned skips are inventoried, never credited as passes. Parallel-log reason clarifications preserve original counts/inventory/logs without reruns.

| Class | Entries | Original recorded reason |
|---|---:|---|
| environment-absence | 2 | activity_live_test.go:20: herdr not installed |
| environment-absence | 1 | bd_schema_parity_test.go:121: bd not available |
| environment-absence | 1 | bd_schema_parity_test.go:144: bd not available |
| environment-absence | 1 | binary_versions_test.go:66: bd not probeable in this environment: locate bd: exec: "bd": executable file not found in $PATH |
| environment-absence | 1 | br_test.go:18: br not available |
| environment-absence | 1 | checks_custom_types_test.go:297: bd binary not on PATH |
| environment-absence | 1 | checks_custom_types_test.go:399: bd binary not on PATH |
| environment-absence | 1 | checks_custom_types_test.go:467: bd binary not on PATH |
| environment-absence | 1 | conformance.go:124: concurrent Start(skip-start-3): provider environment unavailable |
| environment-absence | 1 | conformance.go:134: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:170: Start sentinel: provider environment unavailable |
| environment-absence | 1 | conformance.go:193: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:209: Start(skip-start-11): provider environment unavailable |
| environment-absence | 1 | conformance.go:238: Start(skip-start-14): provider environment unavailable |
| environment-absence | 1 | conformance.go:271: Start(skip-start-18): provider environment unavailable |
| environment-absence | 1 | conformance.go:294: Start skip-start-21: provider environment unavailable |
| environment-absence | 1 | conformance.go:313: Start skip-start-22: provider environment unavailable |
| environment-absence | 1 | conformance.go:333: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:349: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:367: Start(skip-start-25): provider environment unavailable |
| environment-absence | 1 | conformance.go:395: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:404: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:421: Start(skip-start-30): provider environment unavailable |
| environment-absence | 1 | conformance.go:61: Start shared session: provider environment unavailable |
| environment-absence | 1 | conformance.go:84: Start: provider environment unavailable |
| environment-absence | 1 | conformance.go:93: first Start: provider environment unavailable |
| environment-absence | 2 | conformance_test.go:18: herdr not installed |
| environment-absence | 2 | events_live_test.go:40: herdr not installed |
| environment-absence | 2 | gastown_test.go:4153: zsh not installed |
| environment-absence | 2 | kindpath_live_test.go:105: herdr not installed |
| environment-absence | 2 | maintenance_scripts_test.go:9254: awk does not emit a comma decimal under de_DE.UTF-8 (locale missing?): "1.50", <nil> |
| environment-absence | 1 | mcp_conformance_test.go:159: MCP mail script not found: file does not exist |
| environment-absence | 1 | mcp_conformance_test.go:195: MCP mail script not found: file does not exist |
| environment-absence | 1 | mcp_conformance_test.go:26: MCP mail script not found: file does not exist |
| environment-absence | 1 | mcp_conformance_test.go:286: MCP mail script not found: file does not exist |
| environment-absence | 1 | mcp_conformance_test.go:80: MCP mail script not found: file does not exist |
| environment-absence | 8 | metadata_empty_clear_conformance_test.go:55: GC_BEADS_TEST_PG_DSN not set; skipping postgres empty-clear conformance |
| environment-absence | 1 | migrate_journal_test.go:37: pinned beads v1.3.2 is not unpacked in the local module cache: open /tmp/bt/_tmp/35731ca362c311940500020a6c8a2007/go/pkg/mod/github.com/steveyegge/beads@v1.3.2/cmd/bd/migrate_dolt_mode.go: no such file or directory |
| environment-absence | 2 | panebinding_live_test.go:21: herdr not installed |
| environment-absence | 2 | provider_live_test.go:15: herdr not installed |
| environment-absence | 2 | reconcile_replay_test.go:574: GC_V2_REPLAY_DIR is not set |
| environment-absence | 2 | reconcile_sim_coverage_test.go:305: GC_V2_SIM_COVERAGE_FILES unset |
| environment-absence | 2 | session_event_pump_live_test.go:23: herdr not installed |
| environment-absence | 1 | start_drift_test.go:123: XDG_RUNTIME_DIR not set; user systemd not addressable |
| environment-absence | 2 | tmux_leak_guard_mntns_test.go:76: bwrap not installed; cannot build a private-/tmp mount namespace |
| environment-absence | 2 | tmux_leak_guard_mntns_test.go:98: bwrap not installed; cannot build a private-/tmp mount namespace |
| environment-absence | 1 | tmux_test.go:3637: not inside tmux — need server for bind-key |
| environment-absence | 1 | tmux_test.go:3663: not inside tmux — need server for bind-key |
| environment-absence | 1 | tmux_test.go:3689: not inside tmux — need server for bind-key |
| environment-absence | 1 | tmux_test.go:3719: not inside tmux — need server for bind-key |
| environment-absence | 1 | tool_home_test.go:315: no bd available (set GC_ACCEPTANCE_BD_BIN) |
| other | 1 | call_budget_test.go:536: ga-vnycm2.27: interrupted abort-scope converges with different skip metadata (original parallel log) |
| other | 2 | activity_poke_integration_test.go:21: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server) |
| other | 1 | api_state_test.go:582: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | api_state_test.go:622: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | atomic_test.go:185: filesystem did not preserve setuid bit in test mode: -rwxr-xr-x |
| other | 1 | bd_env_test.go:1396: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | bd_env_test.go:1643: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | bdstore_conditional_integration_test.go:69: installed bd lacks --if-revision (pre-beads#4682); the conformance row skips cleanly and becomes the authoritative guard for the provisional body codes once #4682 lands |
| other | 1 | bdstore_exec_internal_test.go:266: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | bdstore_test.go:54: BdStore fails ParentIDNamesARowThisStoreDoesNotHave: bd refuses an unresolvable --parent (ga-6od57); un-skip is ga-x4j5ka |
| other | 1 | beads_migrate_proxied_test.go:135: there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology |
| other | 1 | beads_migrate_proxied_test.go:357: there is no legacy gc available; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc that still initialises the GC-managed direct topology |
| other | 1 | beads_provider_dirty_tables_test.go:345: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:10308: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:10494: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:10777: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:11056: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:11536: starts the materialized gc-beads-bd shell fallback; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:11680: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:11767: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:11932: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:12978: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:14190: runs the real bd provider lifecycle; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:1723: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:5144: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:5695: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:6677: runs the materialized gc-beads-bd init script with GC_BIN helper; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_lifecycle_test.go:9693: starts the real gc-beads-bd lifecycle script; run make test-cmd-gc-process for full coverage |
| other | 1 | beads_provider_script_scope_lifecycle_linux_test.go:352: runs the real bd provider init; run make test-cmd-gc-process for full coverage |
| other | 3 | beads_topology.go:632: there is no pre-journal gc binary; set GC_ACCEPTANCE_LEGACY_GC_BIN to a gc built from a commit before the ownership journal |
| other | 1 | build_desired_state_test.go:10104: uses a live managed-dolt port probe for scale_check coverage; run make test-cmd-gc-process for full coverage |
| other | 1 | build_desired_state_test.go:6331: uses real bd subprocesses for routed-work scale checks; run make test-cmd-gc-process for full coverage |
| other | 1 | city_runtime_test.go:4327: runs a full runtime tick/reconcile path; run make test-cmd-gc-process for full coverage |
| other | 1 | city_status_snapshot_test.go:200: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | cityinit_impl_test.go:259: runs the full local init scaffold/finalize path; run make test-cmd-gc-process for full coverage |
| other | 2 | close_notification_conformance.go:54: CloseIfMatch: conditional writes unsupported |
| other | 2 | close_notification_conformance.go:57: UpdateIfMatch(status=closed): conditional writes unsupported |
| other | 2 | close_notification_conformance.go:89: backing *beads.FileStore lacks the capability UpdateIfAssignmentStatusClosed needs |
| other | 2 | close_notification_conformance.go:89: backing *beads.MemStore lacks the capability UpdateIfAssignmentStatusClosed needs |
| other | 2 | close_notification_conformance.go:89: backing *beads.NativeDoltStore lacks the capability UpdateIfAssignmentStatusClosed needs |
| other | 2 | close_notification_conformance.go:89: backing *beads.SQLiteStore lacks the capability UpdateIfAssignmentStatusClosed needs |
| other | 1 | cmd_bd_test.go:1314: requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_bd_test.go:1478: requires a managed bd lifecycle city; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_bd_test.go:1851: ga-klo4gz: this test's purpose is exercising resolveContextFromDir's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit GC_CITY/GC_CITY_PATH/GC_CITY_ROOT override would make it a no-op test rather than a fix |
| other | 1 | cmd_beads_city_test.go:188: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_beads_city_test.go:485: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_beads_city_test.go:677: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_beads_city_test.go:90: exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_commands_test.go:814: ga-klo4gz: this test's purpose is exercising ambient cwd-based city resolution (resolveContextFromDir step 10) to distinguish the ambient city's commands from an explicitly-selected one, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix |
| other | 1 | cmd_convoy_dispatch_test.go:3899: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_doctor_test.go:479: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_doctor_test.go:599: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_dolt_state_test.go:1023: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1184: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1307: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1347: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_dolt_state_test.go:1407: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:142: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1445: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1535: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1598: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_dolt_state_test.go:1661: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1720: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:1888: spawns managed dolt holder processes; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:189: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:2780: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:2888: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:3010: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:3051: spawns managed dolt recovery processes; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:3216: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:3313: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:3436: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_dolt_state_test.go:3511: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:398: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:447: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:485: spawns a delayed TCP listener process to emulate managed dolt recovery; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:526: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:560: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:582: spawns a TCP listener process and verifies cleanup; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:694: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:722: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:760: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:801: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:853: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:905: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_dolt_state_test.go:960: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_mail_test.go:1849: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 2 | cmd_pack_registry_test.go:758: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live catalog canary |
| other | 1 | cmd_prime_test.go:1269: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_prime_test.go:1336: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_reload_test.go:540: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_reload_test.go:606: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_reload_test.go:740: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | cmd_rig_endpoint_test.go:1651: requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_rig_endpoint_test.go:1689: requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_session_logs_test.go:388: probes provider transcript lookup before workdir fallback; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_stop_test.go:1376: exercises managed bd provider shutdown; run make test-cmd-gc-process for full coverage |
| other | 1 | cmd_supervisor_city_test.go:317: exercises supervisor registration retry behavior; run make test-cmd-gc-process for scenario coverage |
| other | 2 | cmd_supervisor_test.go:1473: unsupported-os test only applies outside darwin/linux |
| other | 1 | cmd_supervisor_test.go:1691: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | cmd_supervisor_test.go:1851: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 2 | cmd_supervisor_test.go:2527: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:2577: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:2642: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:2695: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:2742: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:2819: launchd path only applies on darwin |
| other | 2 | cmd_supervisor_test.go:6604: launchd path only applies on darwin |
| other | 1 | cmd_wait_test.go:777: builds a real bd binary from source; run make test-cmd-gc-process for full coverage |
| other | 1 | cmdline_portable_test.go:141: PID 1 argv is readable in this environment; cannot exercise the fail-closed path this way |
| other | 1 | compile_test.go:1359: tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.toml: permission denied |
| other | 1 | condition_test.go:594: darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms |
| other | 1 | conformance.go:124: concurrent Start(preexisting-concurrent-session): session already exists |
| other | 5 | conformance.go:508: provider cannot fence session input |
| other | 5 | conformance.go:525: provider cannot fence session input |
| other | 5 | conformance.go:551: provider cannot fence session input |
| other | 1 | conformance_skips_test.go:60: skipping LongLapsed (bead ga-example, expires 2000-01-01): example |
| other | 1 | conformance_test.go:147: Start: session already exists |
| other | 1 | controller_test.go:1746: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | controller_test.go:1808: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 2 | controller_watcher_fd_test.go:27: darwin-only: the kqueue Close() FD leak (fsnotify#732) is a Darwin/kqueue bug |
| other | 1 | delivery_lookup_test.go:133: start grace is raw-bridge bookkeeping |
| other | 1 | discovery_test.go:65: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | dolt_identity_test.go:156: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_lifecycle_race_test.go:148: spawns real processes; run via make test-cmd-gc-process |
| other | 1 | dolt_lifecycle_race_test.go:170: spawns real processes; run via make test-cmd-gc-process |
| other | 1 | dolt_lifecycle_race_test.go:238: spawns real processes; run via make test-cmd-gc-process |
| other | 1 | dolt_persistence_test.go:449: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:490: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:536: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:585: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:636: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:679: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:722: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:766: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_persistence_test.go:818: GC_INTEGRATION_BD_PERSISTENCE!=1; requires bd >= gastownhall/beads@1cf8337 |
| other | 1 | dolt_project_id_test.go:184: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_project_id_test.go:212: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_project_id_test.go:225: requires a managed dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_project_id_test.go:281: requires a managed dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_runtime_publication_test.go:130: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_runtime_publication_test.go:204: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_runtime_publication_test.go:28: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_runtime_publication_test.go:294: spawns a TCP listener process to emulate managed dolt; run make test-cmd-gc-process for full coverage |
| other | 2 | dolt_scope_watchdog_test.go:176: helper process only |
| other | 1 | dolt_sql_health_test.go:525: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | dolt_start_managed_test.go:1635: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 2 | dolt_start_managed_test.go:647: helper process only |
| other | 1 | evaluate_test.go:189: darwin-only: the /private/{tmp,var} alias collapse is a no-op on other platforms |
| other | 2 | exec_timeout_linux_test.go:66: harness process |
| other | 2 | gastown_test.go:492: embedded gascity-packs does not yet include #{alternate_on} in WheelUpPane (gastownhall/gascity-packs#136); test activates once go.mod is bumped to that release |
| other | 1 | gc_beads_bd_config_commit_test.go:457: requires a real Dolt server; run make test-cmd-gc-process for full coverage |
| other | 1 | golden_regen_test.go:15: set RUNPROJ_UPDATE_GOLDENS=1 to rewrite testdata goldens |
| other | 1 | handler_status_scoped_store_test.go:141: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | handler_status_scoped_store_test.go:507: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | handler_status_scoped_store_test.go:544: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | huma_binary_test.go:508: flaky on CI under load — 5s supervisor shutdown budget races with in-flight mayor-session-start; tracked in #2090 |
| other | 1 | implicit_agent_patch_repro_test.go:68: bug ga-91uk5m fixed; delete this characterization test |
| other | 1 | integration_selfhost_ux_test.go:100: placeholder for ga-q0bf.1 |
| other | 1 | integration_selfhost_ux_test.go:104: placeholder for ga-r8iz.1 |
| other | 1 | integration_selfhost_ux_test.go:108: placeholder for ga-6wrr.1 |
| other | 1 | integration_selfhost_ux_test.go:112: placeholder for ga-xxqx |
| other | 1 | integration_selfhost_ux_test.go:16: pending ga-cwdaqv: supervised gc start does not validate config pre-registration |
| other | 1 | integration_selfhost_ux_test.go:54: pending merge of builder/ga-xxqx-1 (PR#2219): drift auto-restart not on origin/main |
| other | 1 | integration_selfhost_ux_test.go:96: placeholder for ga-q0bf.1, ga-r8iz.1, ga-6wrr.1, and post-PR#2219 assertions |
| other | 1 | integration_test.go:2828: host has /proc; this test targets the no-/proc (macOS) fallback path |
| other | 1 | main_test.go:375: runs tutorial testscript scenarios; run make test-cmd-gc-process for full coverage |
| other | 2 | main_test.go:836: ga-klo4gz: this subtest's purpose is exercising resolveCity's ambient cwd-based fallback (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix |
| other | 1 | mcp_live_test.go:35: set GC_TEST_MCP_MAIL=1 to run (or use make test-mcp-mail) |
| other | 1 | molecule_test.go:30: tooling formula not present: stat /home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.formula.toml: permission denied |
| other | 2 | native_dolt_store_test.go:651: upstream beads storage unavailable: Dolt server unreachable at 127.0.0.1:0: dial tcp 127.0.0.1:0: connect: connection refused |
| other | 1 | nudge_confirm_lag_integration_test.go:174: set GC_TMUX_INTEGRATION=1 to run this real-tmux measurement |
| other | 1 | nudge_confirm_lag_integration_test.go:36: set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server) |
| other | 1 | nudge_poke_integration_test.go:27: set GC_TMUX_INTEGRATION=1 to run this real-tmux dogfood (spins a throwaway tmux server) |
| other | 1 | nudge_staged_draft_integration_test.go:26: set GC_TMUX_INTEGRATION=1 to run this real-tmux reproduction (spins a throwaway tmux server) |
| other | 1 | order_dispatch_test.go:3695: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | order_dispatch_test.go:3718: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | orphan_sweep_sigkill_test.go:29: not invoked as a subprocess helper |
| other | 1 | pack_registry_live_test.go:38: set GC_TEST_GASCITY_PACKS_REGISTRY to a gascity-packs registry.toml source to run this live import smoke test |
| other | 1 | path_helpers_test.go:1054: spawns subprocesses to simulate still-running dolt sql-servers; run make test-cmd-gc-process for full coverage |
| other | 7 | phase1_conformance_test.go:100: provider family "" has no invocation-usage extractor (out of scope) |
| other | 7 | phase1_conformance_test.go:110: provider family "" has no invocation-usage extractor (out of scope) |
| other | 1 | pinned_id_fence_conformance.go:467: store has no guarded batcher |
| other | 1 | pinned_id_fence_conformance.go:533: store has no guarded batcher |
| other | 1 | pool_test.go:108: uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage |
| other | 1 | pool_test.go:154: uses real bd and jq for default scale_check coverage; run make test-cmd-gc-process for full coverage |
| other | 1 | pool_test.go:215: uses real bd to prove test-owned HOME isolation; run make test-cmd-gc-process for full coverage |
| other | 1 | processgroup_unix_test.go:71: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 2 | prompt_test.go:1420: crew.template.md not readable at ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: open ../../examples/gastown/packs/gastown/assets/prompts/crew.template.md: no such file or directory |
| other | 1 | proxy_process_test.go:1160: harness process |
| other | 1 | proxy_process_test.go:167: helper process |
| other | 4 | ready_parity_conformance.go:69: skipping CachedReadyParity (bead ga-gmf8r, expires 2026-12-15): MemStore and FileStore Ready block on a missing or foreign blocker and on a closed blocker with gc.work_outcome=blocked, which a primed CachingStore cannot see without a ready projection, and return insertion order with Limit applied mid-scan instead of the canonical (priority, created_at, id) order. Replacement proof: MemStore implements enrichReadyProjectionForCache and canonical ready order, and TestMemStoreReadyParityConformance and TestFileStoreReadyParityConformance run this suite unwaived |
| other | 2 | rig_anywhere_test.go:377: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix |
| other | 2 | rig_anywhere_test.go:401: ga-klo4gz: this subtest's purpose is exercising resolveContext's ambient cwd walk-up (step 10) followed by a rig match, which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix |
| other | 2 | rig_anywhere_test.go:488: ga-klo4gz: this subtest's purpose is exercising the fallthrough from an ambiguous registered-rig match (step 9) to resolveContext's ambient cwd walk-up (step 10), which is now unconditionally refused inside test binaries; an explicit override would make it a no-op test rather than a fix |
| other | 1 | rules_test.go:186: needs root to chown the fixture to the Secret-mount shape |
| other | 1 | scoped_store_test.go:364: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | script_resolve_test.go:313: starts real Dolt lifecycle; set GC_FAST_UNIT=0 or run make test-cmd-gc-process for full process coverage |
| other | 1 | sessionlog_test.go:1084: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | sessionlog_test.go:1104: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | sessionlog_test.go:1195: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | sessionlog_test.go:1215: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | sessionlog_test.go:1397: macOS-only /tmp <-> /private/tmp Claude project path alias |
| other | 1 | sessionlog_test.go:2170: macOS /private/tmp path aliases only apply on darwin |
| other | 1 | sessionlog_test.go:2315: macOS-only /tmp <-> /private/tmp Gemini project path alias |
| other | 1 | ssh_localhost_integration_test.go:24: passwordless ssh to localhost unavailable |
| other | 1 | start_drift_test.go:275: permission-denied scenario requires root to drop to a secondary uid |
| other | 1 | starttime_portable_test.go:86: on linux /proc answers directly, so a ps stub cannot make the identity unreadable |
| other | 2 | state_cache_test.go:926: Darwin-specific syntax guard |
| other | 1 | structured_conformance_test.go:153: profile fixture has no edit result to evaluate |
| other | 1 | tmux_test.go:1615: Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions) |
| other | 1 | tmux_test.go:1713: Skipping: GT_TEST_ALLOW_CLEANUP_TEST=1 required (this test kills sessions) |
| other | 1 | triggers_test.go:464: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 1 | triggers_test.go:495: skipping real process signal test in unit lane; set GC_FAST_UNIT=0 or GC_REAL_PROCESS_SIGNAL_TESTS=1 |
| other | 2 | v2_g1_test.go:120: gate G1 times this host: set GC_V2_G1=1 |
| other | 2 | v2_g1_test.go:178: gate G1b reads a store copy: set GC_V2_G1B_STORE |
| other | 1 | worker_handle_test.go:1266: waits through stale session-key detection; run make test-cmd-gc-process for full coverage |
| other | 1 | worker_handle_test.go:36: waits through stale session-key detection; run make test-cmd-gc-process for full coverage |
| other | 1 | write_test.go:376: the test user has no secondary group and is not root |
| other | 1 | write_test.go:475: needs root to own a file as another user |
| other | 1 | x6_test.go:248: DES-O1's literal X6 line (5,000 notices in 60 s) is a manual measurement: --test_env=GC_LANE_X6_FULL=1 |

Evidence `/var/tmp/deploy-ga-rc9z8jk-fresh.fizu3j6i`. Independent BEP report bep-recomputed.json; AC7 raw proof dispatch-ac7-proof.json. Execution-record SHA256 `14e56bc1b8aa4f60d95ecaa070111c02f5840ea7d73471f3322e4a35b3701b17`. Original raw evidence survives owned cache/scratch cleanup. docs/PROJECT_MANIFEST.md is absent; TESTING.md, Makefile, CI and the supplied seven-criterion contract define coverage.

Native-floor residual inventory was independently read with the canonical population reader: 39 SKIP events, all non-owned and every original source site already independently verified UNCHANGED with no shared hunks. Reasons and original logs are retained in native-floor-skip-inventory.json; native-floor-reader.txt records native counts separately from the remote suite. Residual skips remain skips, not passes.
