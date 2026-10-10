# City-store worktree redirect release gate (ga-rkzyd4)

**Verdict:** **PASS**

Reviewed implementation: `113f3489a870fe9f8ed10b5f5ca666419e362921`; recorded re-gate source: `eb1e2688ec9b2102f1651df99b5a912f2a481c61`.
Pinned main base: `f1686bc676a46e537c3f312ff480a4671dc9a7c4`; canonical normal-hook merge: `5abe98ef270336a6e61f5ccda26ec85267d694f2`; tree: `2bdfcc9bcd0c0a280769dbcc6d36cac7cdfa11eb`.
The implementation/test diff between the reviewed commit and this source is empty: later source commits changed only this prior gate record. This fresh record replaces its earlier HOLD.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Deploy bead records the recovered exact-implementation MPR review: Qwen/Claude/Codex all ok, synthesis auto-merge on the reviewed source. Current re-gate source is explicitly pinned by live notes/metadata and own PR#6623; only gate bookkeeping differs. Both SHA objects resolve. No branch-tip substitution. |
| 2 | Acceptance criteria met | PASS | The added command-scope test actually passes create/list/bare help/missing HQ-ID cases; the other added root and both ordinary/symlinked city-path subtests pass. Existing explicit-rig/city precedence, rig redirect, GC_RIG and foreign-redirect boundary roots actually pass in the full unit lane. Code returns city scope only for its own normalized HQ store, after checking declared rigs. |
| 3 | Tests and required coverage | PASS (attributed failure) | Fresh whole-tree make check and all required Bazel acceptance/integration lanes: 369 first-attempt uncached jobs. One original unchanged finalize-settlement failure is retained and attributed via measured clause3(c) coverage and clear same-package load guard; details below. Exact configured target sets, logs/XML/BEP and hashes verified. Both owned roots actually PASS; every SKIP has an individual unchanged-site, shared-hunk, printed-reason and causal guard review. All18fast targets, whole-tree lint/nogo, complete policy and BUILD/generated drift passed first. |
| 4 | No unresolved HIGH findings | PASS | Recovered review reports no findings. Current own PR actor streams contain only our account; its deploy-gate hold is cleared by this fresh evidence, not treated as a review defect. No contributor response or interaction performed. |
| 5 | Clean branch | PASS | Canonical and assigned trees clean before release record; fast checks use independent fresh views. Only this gate file is committed on the isolated deploy branch, with normal hooks and final cleanliness checked before push/clearance. |
| 6 | Clean divergence | PASS | merge-tree rc0; normal-hook materialized tree equals prediction. Independent fresh-view go build ./... and go vet ./... exit0. No rebase/force push. |
| 7 | Single feature theme | PASS | One eight-line store-routing correction and two regression roots. Absolute-library ancestry scope guard passes for ga-rkzyd4/ga-k1e9yp, no stack; all four source commits belong to this fix or its gate history. No forbidden internal-doc paths or unrelated themes. |

`test_cmd: bash /var/tmp/ga-rkzyd4-current-suite-driver.sh`; `test_cmd_scope: full-suite`.
Official detached runner → load-gate-run.sh → isolated-test-run.sh → private ICU runtime → suite runtime.
Required CI coverage from the pinned bazel.yml/ci.yml: make check (whole //..., unit/nogo/format/generated/policy/dashboard); acceptance main+solo under --config=acceptance (acceptance_a); full //test:integration_packages under --config=integration with GC_FAST_UNIT=0; prescribed //test/integration:integration_test under --config=integration-smoke. The last filter is the documented CI configuration, not a gate-selected subset. These cmd/gc paths select no additional ci.yml live mail/packs/beads/credential lane.
Common flags: --config=ci --config=fork-cache --nocache_test_results --jobs=4 --remote_download_outputs=all --rewind_lost_inputs. Exact argv/manifests retained.
`diff_tests_executed: TestResolveBdScopeTargetCityStoreRedirectUsesCityStore PASS; TestRigFromRedirectedBeadsDirTreatsCityStoreRedirectAsCityScope PASS`.
`heavy_mode: none` (official classifier); `ci_lane_run: n/a (no CI-config diff)`; `failure_attribution: TestExecutorCausesCarryTheFinalizePrefix -> ga-cqfwch | clause3:c COVERAGE`; `policy_attribution: none`; `waiver_ref: none` (gascity has no waiver path).
`test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-rkzyd4-current.c3/logs`. Official gate-test-evidence.py re-read all retained logs.

Counts include subtests and repeated Bazel jobs; parent counts exclude '/' names. Policy/dashboard target statuses are checked separately.

| Lane | PASS | FAIL | SKIP | Parent PASS/FAIL/SKIP |
|---|---:|---:|---:|---|
| unit | 53061 | 0 | 217 | 29072/0/166 |
| acceptance | 402 | 0 | 11 | 114/0/10 |
| integration-packages | 34846 | 1 | 54 | 18425/1/43 |
| integration-smoke | 16 | 0 | 1 | 13/0/1 |
| Total | 88325 | 1 | 283 | 47624/1/220 |

SKIP evidence: `/var/tmp/gc-heavy-gate/runs/ga-rkzyd4-current.c3/full-skip-site-proof.json`; 283 individual rows. Full per-file explain checks helpers/imports/vars/TestMain and each exact skip function/site. The two touched test files add only their owned roots, with no shared hunk. Untouched neighboring skips are checked individually. Complete Go test-binary import closures under default/integration/acceptance_a are retained in /var/tmp/ga-rkzyd4-current-import-reach-verified.json. Reachable cmd/gc tests are explicitly acknowledged; each guard's platform, opt-in, external executable/runfile, fixed fixture or helper-entry input is reviewed before the guarded body. Unit process omissions match the same full-name/same-package test's actual fresh integration PASS. No changed guard or missing owned root is excused.

The Go import proof establishes in-process reach only. Tests that execute gc runfiles can still reach command production code across a subprocess boundary. Each specific unchanged guard is also reviewed for its real precondition before the changed HQ resolver can run; BUILD/runfile declarations and guard inputs are unchanged.

Original failure: TestExecutorCausesCarryTheFinalizePrefix, full integration //cmd/gc:gc_test shard4 first attempt; reconcile_effects_test.go:270, no settlement posted after10.00s. All20 integration targets ran (19 PASS/1 FAIL); no failed job was retried. Its site/function is UNCHANGED and there are no shared hunks. The only changed production function, rigFromRedirectedBeadsDir, measures0.0% in a retained single-test coverage profile built under the same integration tag. That focused PASS is proof3(c) only, never replacement full-suite evidence. Clause4: same_package=yes proof=c added_test_load=no; no census bump, new suite target or new cmd/gc test file. Existing tracker searches found none covering this condition; ga-cqfwch is a first-sighting tracker filed and GET-verified during this run, not represented as pre-existing. The shared non-diff-owned failure file's gm-sf3238 discovering-run exception applies with landed clause3 proof and clear clauses1/4. It remains open and unrouted until a designated fix lands. This attribution is not a waiver. Retained proof/hashes: /var/tmp/gc-heavy-gate/runs/ga-rkzyd4-current.c3/FAILURE_ATTRIBUTION_VERIFIED.json and sibling failure-coverage/COVERAGE_PROOF_VERIFIED.json. Original C3 service remains honestly failed; the separate c3-remainder service ran only the never-started prescribed smoke lane on the identical canonical merge. The complete four-lane aggregate includes that one attributed FAIL.

`policy_lane: complete make lint/nogo, make test-ci-policy and required policy targets PASS`.
`drift_lane: make bazel-sync, git diff --exit-code, empty tracked/untracked status and all generated drift targets PASS`.

Fast policy/drift evidence: separate fresh-tree-run views through gate-base.sh with the same pinned base. Whole-tree make lint/nogo, complete six-command make test-ci-policy, shell/active-hook guards, seven policy targets, six generated drift targets, genclient, three remaining policy targets and the OpenAPI comparator. make bazel-sync followed by git diff --exit-code and empty tracked/untracked status passes. The canonical suite input is unchanged by installers or regeneration.

OpenAPI form: workflow-equivalent per ga-3rtlng; standard form not run (ga-twhw87 open).
E1 actual target invocation/environment/exit0 retained in fast-openapi-x2; full unit uses the same real comparator target.
E2 pinned base `f1686bc676a46e537c3f312ff480a4671dc9a7c4` supplied through GC_OPENAPI_BREAKING_BASE_SPEC as an existing /var/tmp file outside client /tmp.
E3 base bytes equal git show of that exact commit; sha256 04d6dfe60530cb1d81b79080a8baab3f7f5887a2f66c5ccdfeb94c4dba8d1912.
E4 raw log names that input; TestCommittedSpecAgainstBase and TestGateAgainstOasdiff with all six fixtures PASS, no ENOENT/SKIP; raw artifact hashes verified.
E5 live preconditions before each invocation: ga-twhw87 OPEN and exactly one canonical rule line rctx.symlink(spec, "openapi.json").
E6 teeth pointer: ga-2egszb independent RED/GREEN/breaking-probe and /tmp/bt refutation; /var/tmp/gc-heavy-gate/runs/ga-8tafjg.openapi-base-repro. No unset-input self-comparison is claimed as base evidence. First tracker close or rule fix sunsets this temporary form.

Environment: rootless Podman socket, exact Dolt2.2.0 digest and enabled testcontainer-sweep verified before C3; Ryuk disabled only with explicit unreaped opt-out. Test bd remains pinned1.3.1. Metadata writes and normal-hook live-store reads use the captured installed bd1.1.0, avoiding a test pin against the fleet store. No store migration or host-library edit occurred.
The initial normal-hook materialization stopped before docsync could start because ICU74 was missing. A second attempt stopped before materialization because the test bd refused a live-store schema upgrade. Neither produced C3 evidence. Their logs remain; private readonly ICU overlay and explicit metadata CLI restored prerequisites before the successful normal-hook C6 and first fresh full C3 sweep. No failed suite job was retried; the single coverage-supported attribution is disclosed above.
The wrapper truthfully reports PASS-THROUGH/NOT ISOLATED for this rig. Bazel sandboxes and repository environment scrubs provide test isolation; private bubblewrap adds libraries without a PID namespace/subreaper. Four local jobs bound cache misses.

Full-suite load:
LOAD_GATE_SUMMARY threshold=15 waited_seconds=30 wait_timed_out=0 run_start_load=14.39 run_max_load=16.54 run_mean_load=13.87 run_readings=59 wait_first_load=15.56 wait_max_load=15.56 wait_mean_load=14.98 wait_readings=2 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.97 run_max_load=9.34 run_mean_load=8.93 run_readings=4 wait_first_load=7.97 wait_max_load=7.97 wait_mean_load=7.97 wait_readings=1 read_errors=0

Evidence: /var/tmp/gc-heavy-gate/runs/ga-rkzyd4-current.c3/FULL_SUITE_VERIFIED.json, per-lane manifests/logs/XML/BEP, complete skip/site/reason proofs and evidence-tools; sibling fast-rest/COMPLETE_FAST_VERIFIED.json and fast-openapi-x2 E1-E6; current C6 source/scope proofs. Old October5 failed evidence is not reused.

Normal commit/push, PR head/body, BASE-repo exact-head clearance and mayor merge-request are verified separately before success notes. The pre-push hook decides whether to repeat a suite from the actual remote-to-local Go diff; a gate-record-only update needs its normal ownership/beads checks and is never counted as fresh C3 coverage. MPR owns merging; the bead closes blocked, not shipped.
