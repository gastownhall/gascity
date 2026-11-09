# Release gate: Dolt GC live-session safety

**Verdict:** **PASS**

- Bead: `ga-r7gel0j`; build: `ga-skyxjl.2`; review: `ga-b4wm9l8`.
- Evaluated: 2026-11-09T08:05:39.533662+00:00.
- Reviewed source: `aa1dca3a219282fba06e79271b87551bedc73cdd`.
- Deploy mode: `remote`; push remote: `fork`; intended branch: `deploy/ga-r7gel0j-gate`.
- Observed base: `origin/main@aa7507b2c2edd4c82f06d55b02cf2900a2edadf0`.
- gate_base_kind: own-base
- gate_base_sha: `37bd09351ac80604606366872b4efeed63b624b8`
- Gated scratch commit: `3b52ab4e1edb251386a8365315a4a6ecb0720518`; tree: `a5f618c7a8397bfd07f0fc3e22ba4691d6c88a47`.
- merge_state: conflicts (origin/main@aa7507b2c2edd4c82f06d55b02cf2900a2edadf0; files: internal/supervisor/maintenance.go)
- merge_validation: deferred to merge time — MPR resolves the source conflict and re-gates any changed head; GitHub's SQUASH/ALLGREEN merge queue tests the merge group. Live main rules re-read successfully; retained in `/var/tmp/deploy-ga-r7gel0j.6i0wct_x/merge-rules-live.json`. A conflict is not a gate failure. Gap 1 of the deployer pack's merge-time guarantee applies: the mayor must arrange a builder rebase-only handoff if the queue cannot take this conflicting PR.
- Inputs: this checkout has neither `docs/PROJECT_MANIFEST.md` nor a matching work-package file. The four Done-when clauses in build bead `ga-skyxjl.2`, its reviewed source, `TESTING.md`, and the deployed release-gate criteria supply the checklist. No criterion was silently dropped.
- Preflight: deploy-ref interlock exited 0; the source resolves locally. The source is unpublished, commit-to-PR lookup returned 422, and the provenance branch had no PR in any state. No merged target was found; no PR, source ref, or branch was mutated by preflight.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS | PASS | Live review bead `ga-b4wm9l8` is closed with `verdict: pass` and `deploy_commit: aa1dca3a219282fba06e79271b87551bedc73cdd`. No carryover or source substitution. |
| 2 | Acceptance criteria | PASS | Both config generators name the ignored-table/working-set GC hazard beside the journal rationale without changing the auto-GC default. `runDoltGC` probes the current database before GC, excludes its own connection, logs and skips on live sessions, and fails closed with stage `gc` on probe error. The point-in-time/TOCTOU limitation is explicit. Five diff-owned tests pass by name below. |
| 3 | Full-scope tests and required CI accounting | PASS | Fresh five-phase remote execution, 126834 PASS / 0 FAIL / 372 SKIP top-level result lines, all five owned tests PASS; 16 mandatory native-floor steps exit 0. Four CI leaf jobs accounted below. |
| 4 | No unresolved HIGH review findings | PASS | The live reviewer record reports no security findings and a PASS. Follow-up `ga-4br1ib0` contains only explicitly non-blocking observations (probe deadline, skipped-cycle alert semantics, maintenance prose). |
| 5 | Clean source and evidence tree | PASS | Role checkout and gated scratch are clean before adding this expected gate record. Scratch HEAD/tree match the recorded source tree. No source, test, generated artifact or provenance branch was edited. |
| 6 | Merge state recorded; build/vet on gate base | PASS | Conflict recorded above; own-base materialization has the exact reviewed source tree. Full `bazel build //...` succeeds for 1385 targets; nogo/vet exits 0. Retained `/var/tmp/deploy-ga-r7gel0j.6i0wct_x/c6-verified.json`. No rebase or conflict resolution. |
| 7 | Single feature theme | PASS | Two reviewed commits, six files: one Dolt GC live-session-safety change and its paired operator warnings/tests. Ancestry accepts exactly this deploy bead and build bead; no internal-doc contamination. |

## Test execution

- test_cmd: `gate-detached-run.sh start --run-dir /var/tmp/deploy-ga-r7gel0j.6i0wct_x/c3-run --key a5f618c7a8397bfd07f0fc3e22ba4691d6c88a47 -- env GC_GATE_REMOTE_ENABLED=1 GC_REMOTE_GATE_FLOOR_GIB=25 GC_REMOTE_GATE_CAP=2 ISOLATED_TEST_BEAD=ga-r7gel0j DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1 gate-criterion3.sh --repo /var/tmp/deploy-ga-r7gel0j.6i0wct_x/merge --base 37bd09351ac80604606366872b4efeed63b624b8 --bead-json /var/tmp/deploy-ga-r7gel0j.6i0wct_x/bead-criterion3.json --owned /var/tmp/deploy-ga-r7gel0j.6i0wct_x/owned.tsv --run-dir /var/tmp/deploy-ga-r7gel0j.6i0wct_x/c3-run/evidence -- python3 /var/tmp/deploy-ga-r7gel0j.6i0wct_x/local-bazel.py`. The fallback command did not run.
- test_cmd_scope: full-suite
- tests_green: true
- test_counts: PASS=126834 FAIL=0 SKIP=372. These are top-level result lines summed across 513 retained logs, not unique tests; repeated jobs count separately. Compile, shell and non-Go targets may have no Go result lines.
- test_exec_mode: remote-exec
- test_exec_bep: 497 terminal results, all remote, uncached locally and remotely, attempt 1, with starts inside their phase windows. Independently re-audited from the original BEPs; digests and predicates retained in `/var/tmp/deploy-ga-r7gel0j.6i0wct_x/final-audit.json` and `/var/tmp/deploy-ga-r7gel0j.6i0wct_x/*.reaudit.json`.
- Every remote phase forces `--nocache_test_results --noremote_local_fallback`; native floor evidence supplements, never stitches a local replacement suite into, the remote run.

| Phase | Result | Fresh remote results | Remote/local cached | Retried | In window |
|---|---|---:|---|---:|---|
| unit | PASS | 373 | 0 / 0 | 0 | true |
| acceptance | PASS | 24 | 0 / 0 | 0 | true |
| integration-packages | PASS | 85 | 0 / 0 | 0 | true |
| integration-full | PASS | 14 | 0 / 0 | 0 | true |
| integration-latency | PASS | 1 | 0 / 0 | 0 | true |

- rbe_state: rc marker present; kill switch absent; certificate valid to Jan 9 2027; source/main worker environment digests identical (`sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98`).
- fallback_reason: none
- admission: remote; floor 25 GiB; free 76.174 GiB; cap 2; wait 0.001 seconds. No local-heavy admission was acquired for the remote suite.
- farm_test_seconds: 9350.673
- heavy_mode: none
- heavy_units: n/a
- coverage_check: n/a (ordinary full-suite path, not a heavy composite)
- owned_topup: none; all five owned tests passed in the full-suite logs.
- floor_topup: complete; 16/16 steps exit 0, including every environment-absence package selection, beadstest, Darwin compile and runner self-tests. Commands, exit statuses and remaining non-owned skips: `/var/tmp/deploy-ga-r7gel0j.6i0wct_x/c3-run/evidence/remote/floor-topup.json` and `floor-logs/`.
- latency_leg: measured, rc=0, complete=true; attempt 1 under a clear farm probe. Both tests from the canonical latency registry PASS. Main integration excluded those budget tests.
- load_threshold: 15; load_waited_seconds: 0; load_wait_timed_out: 0
- run_start_load: 62.82; run_max_load: 101.88; run_mean_load: 59.54; run_readings: 82. These are host observations for a remote-class run, not local-suite admission; native floor selections retain their own local load waits.
- test_environment: rootless Podman socket and pinned cached image tags verified before the suite; Ryuk disabled only with the live enabled testcontainer sweep and paired unreaped-container opt-out. ICU74 provenance checked. The isolation wrapper honestly reports PASS-THROUGH for the throwaway scratch; the canonical runner separately scrubs workload selectors and forwarded credentials are non-interactive.
- test_bd: isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.2 bin=/home/jaword/.local/bd-versions/v1.3.2/bd reports='bd version 1.3.2 (9b65b9e19: HEAD@9b65b9e198ac)' ref_check=match:9b65b9e19 origin=preexisting pin_source=/var/tmp/deploy-ga-r7gel0j.6i0wct_x/merge/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
- test_log_dir: `/var/tmp/deploy-ga-r7gel0j.6i0wct_x/c3-run/evidence/remote/logs`; all evidence, BEPs and per-job logs retained outside the disposable scratch.
- skip_inventory: `/var/tmp/deploy-ga-r7gel0j.6i0wct_x/c3-run/evidence/remote/skip-inventory.json` (358 suite entries: 71 environment-absence, 287 other); native-floor residual skips are also inventoried in `floor-topup.json`. The final reader reports 372 skipped top-level lines.
- skip_justification: non-owned, unchanged capability/platform/opt-in-fixture/live-service skips; environment-absence cases were selected again in the mandatory native floor. None of the changed test files contains a reported skip site, and no supervisor test skipped. The seven SHARED-HUNKS in `maintenance_gc_test.go` and `maintenance_trigger_test.go` are package-private test fakes/imports, not compiled into importing packages; they reach no skipped test. No build tag, TestMain, init or skip condition changed. Existing scoped/fixture gates and missing optional live services do not exercise this patch's GC probe or generated warning. No owned SKIP is excused.
- failure_attribution: none
- waiver_ref: none (gascity has no waiver path)
- ci_lane_run: n/a (no CI job, matrix, timeout or required-check changes)

### Diff-owned tests

Ownership is computed by test body using `diff-owned-tests.py`; retained `owned.tsv` and `owned-explain.txt`. Independent final reader: `/var/tmp/deploy-ga-r7gel0j.6i0wct_x/final-by-name.txt`.

- `cmd/gc.TestWriteManagedDoltConfigFile_AutoGCCommentNamesDoltIgnoreHazard`: PASS (unit and integration-packages).
- `internal/supervisor.TestRunDoltGC_ProbesLiveSessionsBeforeGC`: PASS.
- `internal/supervisor.TestRunDoltGC_LiveSessionsSkipGCAndLogReason`: PASS.
- `internal/supervisor.TestRunDoltGC_ProbeError_FailsClosedWithStageGC`: PASS.
- `internal/supervisor.TestTriggerNow_RunsSnapshotThenGC`: PASS.

### Exact retained test evidence

```text
GATE_TEST_EVIDENCE logs_dir=/var/tmp/deploy-ga-r7gel0j.6i0wct_x/c3-run/evidence/remote/logs logs=513 logs_with_results=447
test_counts: PASS=126834 FAIL=0 SKIP=372
  (top-level result lines, column 0, summed over the 513 logs; a test that several jobs run counts once per job; sub-tests are not counted)
logs_without_results: floor-13.log floor-14.log floor-15.log unit-02e0324f1ef05430d7e4.log unit-04c4f9e4335a6f1c2dfa.log unit-0787047af2c4dad6aebc.log unit-0eb92a6bd00590d91db6.log unit-0f39cb7e4c03399a72c4.log unit-1a34395a607c0208a005.log unit-1e67f201b300dc5f9e78.log unit-21372f682c4033d14a80.log unit-22d5121476fcaa40b586.log (+54 more)
diff_tests_executed:
  TestWriteManagedDoltConfigFile_AutoGCCommentNamesDoltIgnoreHazard PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-5a9df316125cd0aeff2e.log)
  TestRunDoltGC_LiveSessionsSkipGCAndLogReason PASS (unit-55002856e0cdcc042d6d.log)
  TestRunDoltGC_ProbeError_FailsClosedWithStageGC PASS (unit-55002856e0cdcc042d6d.log)
  TestRunDoltGC_ProbesLiveSessionsBeforeGC PASS (unit-55002856e0cdcc042d6d.log)
  TestTriggerNow_RunsSnapshotThenGC PASS (unit-55002856e0cdcc042d6d.log)
GATE_TEST_EVIDENCE_RESULT ok: 5 diff-owned tests verified by name
test_log_dir: /var/tmp/deploy-ga-r7gel0j.6i0wct_x/c3-run/evidence/remote/logs
```

## Fast lanes and CI accounting

- fast_lane: PASS before runtime setup/full-suite launch. Three independent fresh views use the pinned own base; the suite scratch is never regenerated or modified.
- policy_lane: `run-pinned-lint.sh -- make fmt-check-changed`, `make lint` (full nogo), four `make check` shell guards, `make check-hooks`, and 13 policy/format Bazel targets: PASS.
- drift_lane: `make bazel-sync`, `git diff --exit-code`, empty full porcelain status, and seven generated-artifact checks: PASS.
- OpenAPI compatibility: `make openapi-breaking-check OPENAPI_BREAKING_BASE=37bd09351ac80604606366872b4efeed63b624b8`: PASS.
- Fresh-view records: `FRESH_TREE_RECORD from=/var/tmp/deploy-ga-r7gel0j.6i0wct_x/merge head=3b52ab4e1edb tree=a5f618c7a839 dir=/var/tmp/gc-fresh-tree.IxAlXy modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs`; `GATE_BASE_RECORD base=37bd09351ac80604606366872b4efeed63b624b8 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.IxAlXy`; `FRESH_TREE_RECORD from=/var/tmp/deploy-ga-r7gel0j.6i0wct_x/merge head=3b52ab4e1edb tree=a5f618c7a839 dir=/var/tmp/gc-fresh-tree.PX5FIj modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs`; `GATE_BASE_RECORD base=37bd09351ac80604606366872b4efeed63b624b8 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.PX5FIj`; `FRESH_TREE_RECORD from=/var/tmp/deploy-ga-r7gel0j.6i0wct_x/merge head=3b52ab4e1edb tree=a5f618c7a839 dir=/var/tmp/gc-fresh-tree.ujSXei modified=0 untracked=0 ignored=5 ignored_names=.bazelrc.local,bazel-bin,bazel-merge,bazel-out,bazel-testlogs`; `GATE_BASE_RECORD base=37bd09351ac80604606366872b4efeed63b624b8 knobs=CI_BEADS_DIFF_BASE,BASE_SHA,COMPLEXITY_BASE_REF,LINT_BASE dir=/var/tmp/gc-fresh-tree.ujSXei`.
- ci_required_closure: `python3 $GC_CITY_ROOT/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-r7gel0j.6i0wct_x/merge 37bd09351ac80604606366872b4efeed63b624b8 aa1dca3a219282fba06e79271b87551bedc73cdd fork`; rc=0; workflow blob `654b52ade60492ba2cc64e3ef1a0a0d2afe88e18`; “4 LEAF jobs need a status; the rest are rolled up through their needs.” Full filter/trigger rows: `/var/tmp/deploy-ga-r7gel0j.6i0wct_x/ci-required/closure.txt`.
- ci_required_accounting:
  - runner-policy | LOCAL-PASS | `EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-r7gel0j.6i0wct_x/ci-required/runner-policy.out python3 .github/workflows/scripts/runner_policy.py`; rc=0; `/var/tmp/deploy-ga-r7gel0j.6i0wct_x/ci-required/runner-policy.log`. Linux outputs resolve to Blacksmith Ubuntu; Windows output resolves to Windows.
  - changes | LOCAL-PASS | closure-tool invocation above, rc=0; six changed files, no path filters match.
  - credential-provider-windows | NOT-TRIGGERED | tool row: `LEAF NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED if: needs.changes.outputs.credential_provider == 'true'`.
  - pack-gate | NOT-TRIGGERED | tool row: `LEAF linux NOT-TRIGGERED if: needs.changes.outputs.packs == 'true'`.
- windows_reach: none; `windows_reach.sh /var/tmp/deploy-ga-r7gel0j.6i0wct_x/merge 37bd09351ac80604606366872b4efeed63b624b8 aa1dca3a219282fba06e79271b87551bedc73cdd` exits 0 with empty stdout; closure diagnostics retained separately.
- deferred_to_pr_ci: none
- not_covered: none

This PASS authorizes opening the PR, not merging it. PR-head checks must still be green; any head-changing rebase requires a new gate and clearance. The conflict is merge-authority work, not repaired or waived by this run.
