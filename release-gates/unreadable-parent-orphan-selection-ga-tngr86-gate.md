# Unreadable-parent orphan selection release gate (ga-tngr86)

**Verdict:** **PASS**

Reviewed source: `dac38702d81732401a3567893d48705939cf606a`; pinned main base: `438b1f6b7b50766b135b578e13a7882c04ea9b6e`.
Canonical normal-hook merge: `631329382646dbdf7da6f806f3d419efd9415b29`; tree: `cd6f8459d8a7bd5c5ef02c224773ac7358d61b0f`.
The isolated deploy branch preserves the reviewed source and adds only this release record.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | ga-m548b0 PASS resolves to this exact source; style/security have zero unresolved findings. No review carryover or branch-tip substitution. |
| 2 | Acceptance criteria met | PASS | Fresh non-root host probes: pinned main live orphan pair FAIL2/2; RED commit has three roots FAIL5/5 and the negative/scrubbed pins PASS5/5; canonical merge all five PASS5/5. Whole proctable package59parentPASS/95totalPASS,0FAIL/0SKIP. Existing scan/identity/sudo fences unchanged and actual PASS in full suite. AC5 literal complexity-check FAIL is explicitly attributed under3b below; never described as a green command. AC3 actual before/after output is verified in the prepared PR-body draft; the final remote body is checked upon creation. |
| 3 | Tests and required coverage | PASS | Fresh whole-tree make check plus every required Bazel acceptance/integration lane; 369 first-attempt uncached jobs. Exact configured target sets, raw logs/XML/BEP and hashes verified. Both diff-owned roots actually PASS in the full suite; every SKIP has a per-file explain, exact unchanged site, source reason and causal review. All18fast targets and whole-tree nogo/lint/policy/BUILD/generated drift verified before first suite. |
| 4 | No unresolved HIGH findings | PASS | Full ga-m548b0 review read; zero HIGH/blocker/major/minor style/security findings. Parent-root selection remains bounded by session/city/live-bead/infrastructure/identity kill fences. |
| 5 | Clean branch | PASS | Canonical and assigned worktrees clean before record; fast lanes use separate fresh views. Record is committed with normal hooks and final branch cleanliness rechecked before push/clearance. |
| 6 | Clean divergence | PASS | merge-tree rc0 and materialized normal-hook tree equals prediction. Fresh go build ./... and go vet ./... exit0. No rebase. |
| 7 | Single feature theme | PASS | Two proctable files, +74/-2: one parent-environ permission branch, its public doc comment, two tests. Shared environ helper, identity lookup, Darwin, signatures, BUILD files, modules and resource census unchanged. |

Acceptance: the positive fake procfs case reports the child as root with ParentIsProviderInfrastructure=false, still reports the unreadable parent candidate, and agrees with IsScanRoot. EISDIR still omits the child and reports checking-root error. RED test-only commit precedes the production fix. No subreaper wrapper masks the host's unreadable systemd-user parent. Supplemental focused probes are acceptance evidence only, never criterion3 scope evidence.

`test_cmd: bash /var/tmp/ga-tngr86-suite-driver.sh`; `test_cmd_scope: full-suite`.
Official detached runner -> load-gate-run.sh -> isolated-test-run.sh -> private ICU runtime -> suite-runtime.sh.
Required coverage from pinned bazel.yml/ci.yml: make check (whole //..., unit/nogo/format/generated/policy/dashboard); acceptance main+solo under --config=acceptance (acceptance_a tags); //test:integration_packages under --config=integration; prescribed //test/integration:integration_test under --config=integration-smoke. No ad-hoc name filter replaces a lane. ci.yml mail/beads/packs/credential/shared path filters do not select an extra live lane for the two proctable paths.
Common flags: --config=ci --config=fork-cache --nocache_test_results --jobs=4 --remote_download_outputs=all --rewind_lost_inputs. Exact argv and raw manifests are retained.
`diff_tests_executed: TestScanWithRootOmitsChildWhenParentEnvironFailsForOtherReasons PASS; TestScanWithRootReportsChildOfUnreadableParentAsRoot PASS`.
`heavy_mode: none` (official classify); `ci_lane_run: n/a (no CI-config diff)`; `failure_attribution: none (suite)`; `waiver_ref: none` (gascity has no waiver path).
`test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-tngr86.c3/logs`; official gate-test-evidence.py re-read every retained job log.

Counts include subtests and repeated Bazel jobs; parent counts exclude '/' names. Bazel policy/dashboard results are separately checked.

| Lane | PASS | FAIL | SKIP | Parent PASS/FAIL/SKIP |
|---|---:|---:|---:|---|
| unit | 53053 | 0 | 217 | 29066/0/166 |
| acceptance | 402 | 0 | 11 | 114/0/10 |
| integration-packages | 34839 | 0 | 54 | 18418/0/43 |
| integration-smoke | 16 | 0 | 1 | 13/0/1 |
| Total | 88310 | 0 | 283 | 47611/0/220 |

SKIP evidence: `/var/tmp/gc-heavy-gate/runs/ga-tngr86.c3/full-skip-site-proof.json`. Every skipped function/site and its helper/import/var/TestMain shared hunks are mechanically checked. Skipped files match the pinned base; no shared hunk reaches a skip. Complete Go test-binary import closures under default/integration/acceptance_a tags are retained at /var/tmp/ga-tngr86-import-reach-verified.json. Reachable binaries are explicitly acknowledged: platform, fixture, executable/runfile, helper or opt-in guards execute before parent-environ classification and depend on inputs this two-file diff cannot change. Unit process omissions have the same test's fresh integration PASS. Unreachable packages are proven by the complete closure, not a direct-import grep. Individual printed reasons and causal guard arguments accompany all 283 rows. No new test target/file or census bump.

Policy/drift lane: independent fresh-tree-run.sh views, gate-base.sh pinned base. Whole-tree make lint/nogo; complete make test-ci-policy (all six commands); shell and active hook guards; seven format/policy targets; six generated drift targets; genclient; three remaining policy targets; OpenAPI comparator. make bazel-sync plus git diff --exit-code and empty tracked/untracked status PASS; canonical suite tree untouched.

AC5 literal `make complexity-check`: FAIL on both candidate and untouched pinned main (make rc2, check rc1). `policy_attribution: make complexity-check -> ga-q7p6is`, explicit criterion3b policy-baseline mirror.
Clause1 script/baseline untouched. Clause2 same-run tracker searched, created once and read back under3a(ii)'s exception because clause3d has landed. Clause3d same pinned gocyclo0.6.0 tool/command on untouched BASE_REF reproduces byte-identical185-line stderr, sha256 9a13aba935a47536311dd3370f55d9d03a556e62728a87936b90d86d905c4660. Clause4 reported offender paths are disjoint from proctable; no proctable offender. Changed rootCCN7->8 below20; make complexity-diff PASS with no threshold change. Initial missing-gocyclo tooling attempt retained separately; private pin supplied without host changes. This is attribution, not a waiver or a claim that the literal command passed.

OpenAPI: form: workflow-equivalent per ga-3rtlng; standard form not run (ga-twhw87 open).
E1 actual command/env/rc and25PASS/0FAIL/0SKIP comparator/fixture output retained in fast-openapi-x2; full unit uses the same real target.
E2 pinned base `438b1f6b7b50766b135b578e13a7882c04ea9b6e` supplied via GC_OPENAPI_BREAKING_BASE_SPEC as an existing /var/tmp file outside client /tmp.
E3 actual base bytes equal git show of that exact commit; sha256 04d6dfe60530cb1d81b79080a8baab3f7f5887a2f66c5ccdfeb94c4dba8d1912.
E4 raw log declares the real base path, TestCommittedSpecAgainstBase and TestGateAgainstOasdiff (six fixtures) PASS, no ENOENT/SKIP; artifact/raw hashes checked.
E5 live preconditions captured before each invocation: ga-twhw87 OPEN and canonical rule's exact rctx.symlink(spec, "openapi.json") line.
E6 decisive teeth pointer: ga-2egszb independent RED/GREEN/breaking-probe and /tmp/bt refutation, retained /var/tmp/gc-heavy-gate/runs/ga-8tafjg.openapi-base-repro. No unset-input self-comparison is base evidence. First tracker close or rule fix sunsets X2; later gates re-pin and use standard form. The already-open fix PR#7329 itself was gated with standard form.

Environment: uid1000; rootless Podman socket and pinned Dolt2.2.0 digest verified before C3; Ryuk disabled only with enabled testcontainer-sweep and explicit unreaped opt-out. The original coordinator stopped before launching C3 when the cached image disappeared. Documented runtime recipe restored the exact image; completed checks were retained; guarded continuation launched the first full suite. Failed coordinator and restore logs remain, no test retry-to-green.
Isolation wrapper reports PASS-THROUGH/NOT ISOLATED for throwaway views; actual test isolation comes from Bazel sandboxes and Make env-i scrubs, with pinned bd1.3.1. Private ICU74 bubblewrap overlay leaves host libraries untouched; four local jobs bound misses when remote-cache circuit breaker warns.

Full-suite load:
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.14 run_max_load=19.74 run_mean_load=15.00 run_readings=72 wait_first_load=10.14 wait_max_load=10.14 wait_mean_load=10.14 wait_readings=1 read_errors=0

Supplemental acceptance load (short probes have NA peaks/means and zero run readings, never inferred):
base-live: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=14.76 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=14.76 wait_max_load=14.76 wait_mean_load=14.76 wait_readings=1 read_errors=0
red: LOAD_GATE_SUMMARY threshold=15 waited_seconds=30 wait_timed_out=0 run_start_load=14.81 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=15.10 wait_max_load=15.10 wait_mean_load=14.96 wait_readings=2 read_errors=0
green: LOAD_GATE_SUMMARY threshold=15 waited_seconds=390 wait_timed_out=0 run_start_load=14.78 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=15.94 wait_max_load=17.98 wait_mean_load=16.90 wait_readings=14 read_errors=0
package: LOAD_GATE_SUMMARY threshold=15 waited_seconds=360 wait_timed_out=0 run_start_load=14.76 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=15.51 wait_max_load=17.45 wait_mean_load=15.95 wait_readings=13 read_errors=0

Evidence: /var/tmp/gc-heavy-gate/runs/ga-tngr86.c3/FULL_SUITE_VERIFIED.json, exact manifests/logs/XML/BEP, full-skip-site-proof.json, and evidence-tools; ga-tngr86.acceptance/ACCEPTANCE_PROBE_VERIFIED.json; fast-rest/COMPLETE_FAST_VERIFIED.json; complexity-base/BASE_PROOF_VERIFIED.json; runtime-restore logs.

Subsequent normal commit/push, PR, exact-head deploy-clearance and mayor merge-request are independently verified before success notes. MPR owns merging. The fix is not shipped until merged; tracker ga-q17kpi remains OPEN until actual landing proof.

Publication freshness check: later origin/main f1686bc676a46e537c3f312ff480a4671dc9a7c4 merges with the reviewed source cleanly (merge-tree rc0), yielding tree b20a2318ada7f4c21234008d70a2b8e286e17063. Its two intervening main commits change controller effect plumbing and API/race fixes, with no proctable source changes. This supplemental check does not replace or relabel the full-suite base 438b1f6b7b50766b135b578e13a7882c04ea9b6e.
