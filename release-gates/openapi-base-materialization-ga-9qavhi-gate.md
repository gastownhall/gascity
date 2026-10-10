# OpenAPI base-spec materialization release gate (ga-9qavhi)

**Verdict:** **PASS**

Reviewed source: `5f91974a5fbd54049361078ebfee350f2236cee5`. Pinned main base: `20e2c3da48c150e015dcbfa009dbc0c8dd46afd6`.
Canonical normal-hook merge: `14f219315a2939a9766a86b3706978c433269468`; tree: `9c2a5e520960fbd55aa5b49e512116692109bad4`.
The deploy branch contains the exact reviewed source plus this gate record. No builder branch is a push target.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-datr75 verdict pass resolves to the exact source above. Style has no findings; security has no blocker, major or minor finding. The focused reviewer run and externally stopped full-suite run are not reused as gate evidence. |
| 2 | Acceptance criteria met | PASS | Fresh standard-form RED reproduces the declared-data-file ENOENT on pinned main with TMPDIR unset. The same standard command on the canonical merge passes and materializes exact base bytes as a regular file. A synthetic base-only endpoint is rejected by the real comparator, without ENOENT. Unset-input behavior passes separately. Scope is one .bzl rule, +4/-2; no Makefile, test, selection or assertion change. |
| 3 | Full suite and required coverage | PASS | Fresh unit 232/232, acceptance 5/5, integration-packages 20/20, integration-smoke 1/1; all 369 Bazel jobs complete with attempt=1 and uncached results. Raw log/XML/BEP hashes and exact configured target sets verified. All 281 SKIP rows have individual source/reason/reach proofs. Both targets in the changed rule/data reverse-dependency closures have only PASS results, with no FAIL or SKIP. All 18 fast targets, whole-tree lint, policy and generated/BUILD drift passed before this suite. |
| 4 | No unresolved HIGH findings | PASS | Full review read; unresolved HIGH count zero. Security observation concerns persistent public-spec bytes and requests no change. Regression-guard follow-up ga-xr55kz remains separate under this bead's scope limit; it is not an uncovered acceptance criterion. |
| 5 | Clean final branch | PASS | Canonical merge and assigned worktrees were clean before the gate record. Independent fast views leave the canonical suite tree untouched. The gate record is the sole deploy chore and is committed before push; final branch cleanliness is rechecked before push and clearance. |
| 6 | Clean divergence from main | PASS | merge-tree rc0; normal-hook canonical merge tree equals the predicted tree. Independent fresh-view go build ./... and go vet ./... both exit0. No rebase required. |
| 7 | Single feature theme | PASS | One repository-rule change fixes OpenAPI base input materialization. The unset committed-spec branch, watch and missing-path failure remain intact. No unrelated feature or assistant configuration is included. |

Required checks were derived from current pinned `.github/workflows/bazel.yml`: unit (`make check`, Bazel //...), acceptance main and solo targets, full integration-packages, and the prescribed integration-smoke config. Current `ci.yml` changed-path filters select no mail, beads, packs, credential-provider or shared job for this sole tools/bazel/rules path. `ci_lane_run: n/a (no CI-config change)`.

`test_cmd: bash /var/tmp/ga-9qavhi-suite-driver.sh`, detached through official gate-detached-run.sh. It calls load-gate-run.sh, isolated-test-run.sh, a private runtime overlay and suite-runtime.sh.
The complete chain runs `make check BAZEL='bazel --batch'` on //..., acceptance main+solo targets, //test:integration_packages and //test/integration:integration_test with the repository-prescribed configs. Common flags: --config=ci --config=fork-cache --nocache_test_results --jobs=4 --remote_download_outputs=all --rewind_lost_inputs. Exact argv is retained in evidence-tools/suite-runtime.sh. No filtered substitute or retry-to-green.
`test_cmd_scope: full-suite`; `diff_tests_executed: none (no diff-owned tests)`; `heavy_mode: none` (official classifier); `failure_attribution: none`; `waiver_ref: none` (gascity has no waiver path).
`test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-9qavhi.c3/logs`; per-job raw logs/XML, BEP and manifests are retained and were re-read with gate-test-evidence.py.

Counts are terminal Go result lines, including subtests and repeated jobs. Bazel policy/dashboard target statuses are checked separately. Parent counts exclude names containing `/`.

| Lane | PASS | FAIL | SKIP | Parent PASS/FAIL/SKIP |
|---|---:|---:|---:|---|
| unit | 52942 | 0 | 216 | 29048/0/166 |
| acceptance | 402 | 0 | 11 | 114/0/10 |
| integration-packages | 34737 | 0 | 53 | 18409/0/43 |
| integration-smoke | 16 | 0 | 1 | 13/0/1 |
| Total | 88097 | 0 | 281 | 47584/0/220 |

Every SKIP is reviewed in full-skip-site-proof.json with official per-file --explain, exact --site UNCHANGED, printed reason, source guard and causal reach. All test file blobs are identical between base and canonical merge. Computed reverse-dependency closure limits changed inputs to //cmd/openapi-breaking:openapi-breaking_test and //scripts:scripts_test; both targets have no skipped or failing result. Skips outside that closure cannot be caused by this rule/data change. No added test load. Environment-dependent branches were assessed individually, including actual integration execution for default unit process opt-outs.

OpenAPI standard acceptance matrix, each independent fresh view with TMPDIR unset:
- RED on pinned main: 24 PASS/1 expected FAIL/0 SKIP; exact ENOENT reproduction.
- GREEN on the canonical merge: 25 PASS/0 FAIL/0 SKIP; regular materialized input has exact pinned-main spec bytes.
- Breaking negative control: 24 PASS/1 expected FAIL/0 SKIP; only TestCommittedSpecAgainstBase fails on removed GET /v0/ga-9qavhi-deployer-breaking-probe, with no ENOENT. Its synthetic base object has no ref and is never included in the deploy branch.
- Unset input: 25 PASS/0 FAIL/0 SKIP; this separately verifies the unchanged self-comparison branch and is not used as OpenAPI compatibility evidence.

Both the standard GREEN comparator and all six fixtures passed. In the full unit lane, TestCommittedSpecAgainstBase, TestGateAgainstOasdiff and TestOpenAPIBreakingGateRunsInTheBazelUnitLane also passed. The full suite supplies a real client /tmp base file to the fixed copy rule; retained base sha256 04d6dfe60530cb1d81b79080a8baab3f7f5887a2f66c5ccdfeb94c4dba8d1912 equals git bytes at the pinned base. Independent stat/byte verification of that fetched external-repository file confirms a regular file, mode 0644, no symlink (full-unit-materialized-mode-verified.json). No X2 invocation, symlink workaround or waiver: build ga-2egszb carries gc.fixes_tracker=ga-twhw87, so ga-3rtlng requires this standard gate. Tracker ga-twhw87 remains open until the fix lands on main, then receives a verified landing close.

Fast lane uses fresh-tree-run.sh views and gate-base.sh pinned to `20e2c3da48c150e015dcbfa009dbc0c8dd46afd6`. Whole-tree nogo/lint and complete make test-ci-policy (six commands); release-dist, routed-row, topology, residency and hook guards; seven policy/format targets, six generated targets, genclient, three remaining policy targets and OpenAPI PASS. make bazel-sync plus tracked and untracked clean-tree drift PASS. Complete target sets and hashes are retained in fast-rest/COMPLETE_FAST_VERIFIED.json.

Environment: rootless Podman socket and pinned Dolt 2.2.0 image verified; Ryuk disabled only with enabled testcontainer-sweep. The wrapper reports PASS-THROUGH/NOT ISOLATED for the disposable scratch, with no inherited BD_/BEADS_/GC_/DOLT_ names. Bazel hermetic test sandboxes, explicit runtime parameters and Make env -i commands supply test isolation. Private ICU74 bubblewrap overlay leaves host libraries untouched. Remote-cache breaker warnings led to local misses at four jobs and are not test failures.

Full-suite load:
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.36 run_max_load=19.89 run_mean_load=14.75 run_readings=75 wait_first_load=13.36 wait_max_load=13.36 wait_mean_load=13.36 wait_readings=1 read_errors=0

Acceptance probe loads (recorded separately from the full suite):
{
  "red": "LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=7.40 run_max_load=7.85 run_mean_load=7.85 run_readings=1 wait_first_load=7.40 wait_max_load=7.40 wait_mean_load=7.40 wait_readings=1 read_errors=0",
  "green": "LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=8.96 run_max_load=11.87 run_mean_load=10.71 run_readings=2 wait_first_load=8.96 wait_max_load=8.96 wait_mean_load=8.96 wait_readings=1 read_errors=0",
  "teeth": "LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=12.37 run_max_load=15.12 run_mean_load=13.89 run_readings=2 wait_first_load=12.37 wait_max_load=12.37 wait_mean_load=12.37 wait_readings=1 read_errors=0",
  "unset": "LOAD_GATE_SUMMARY threshold=15 waited_seconds=60 wait_timed_out=0 run_start_load=14.91 run_max_load=18.03 run_mean_load=16.91 run_readings=2 wait_first_load=15.10 wait_max_load=15.10 wait_mean_load=15.02 wait_readings=3 read_errors=0"
}

Clearance and the mayor merge-request are subsequent verified actions on the exact committed deploy head. MPR owns merging. This change is not shipped until merged.

After the suite, origin/main advanced to e39772dd11bb408369ef08ce8d108d179435ac2b. A fresh pre-push merge-tree check at that ref returned rc0 and tree da79d500b7fdfa909fb46dfb4b66b5867a7158fd; this is supplemental conflict evidence. Full-suite evidence remains bound to pinned base 20e2c3da48c150e015dcbfa009dbc0c8dd46afd6, not claimed as a run on the later main tip.
