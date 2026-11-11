# One startup-health episode read per reconcile tick

**Verdict:** **PASS**

Bead: ga-bzz7vfy0; build: ga-cat367; review: ga-7hgzhgl0.
Reviewed source: `de0b9ec71449d10dd324c48473ae36f02c9a4ace`; frozen base: `6bd609f5bfb41f43f444843b1220d0cde4473446`.
Clean validation merge: `c0f92261db8beae12d594e62d24102c0c8da24ce`; tree: `c7c0fa444f8fbe09eda1f1405e68a73c200de76f`.
Evaluated: 2026-11-11T20:30:55.768717+00:00

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Reviewer PASS and reviewer-only test addition mechanically verified. |
| 2 | Acceptance criteria met | PASS | Lazy, one-read indexing; per-name/open/newest/tier equivalence; memoized fail-open; quarantine and no-read controls verified. Four cmd/gc regressions fail on unchanged main production with only the reviewed test file and BUILD entry added; the no-down control passes. All eight owned roots pass on the branch. |
| 3 | Tests pass | PASS | Five fresh remote phases, full native floor, fast lanes, by-name audit, skip proofs and six-leaf CI accounting complete. |
| 4 | No high-severity findings open | PASS | Review security/style/spec findings have zero unresolved HIGH issues. |
| 5 | Final branch clean | PASS | Reviewed source, validation tree and deployment checkout clean before adding this intentional gate record; final committed cleanliness checked before publication. |
| 6 | Merge state recorded; build/vet pass | PASS | Frozen clean merge builds and passes whole nogo/vet; terminal BEPs and source/parents/tree verified. |
| 7 | Single feature theme | PASS | Lazy startup-health episode batching for the synchronous wake-candidate loop, its selection API, tests and scenario row. |

merge_state: clean (origin/main@6bd609f5bfb41f43f444843b1220d0cde4473446)
gate_base_kind: current-tip
merge_validation: clean merged tree tested; MPR retains exact-head and merge-time validation.
deploy_mode: remote; push_remote: fork
project_manifest: docs/PROJECT_MANIFEST.md absent; supplied release criteria, TESTING.md, Makefile and actual CI workflows determine this gate.

## Test evidence

test_cmd: /home/jaword/projects/gc-management/packs/actual/all/scripts/gate-criterion3.sh --repo /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/merge --base 6bd609f5bfb41f43f444843b1220d0cde4473446 --bead-json /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/ga-bzz7vfy0.json --owned /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/owned.tsv --run-dir /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/c3-run/evidence; exit 0
test_cmd_scope: full-suite
test_counts: PASS=150714 FAIL=0 SKIP=516
canonical_full_suite_counts: {"FAIL": 0, "PASS": 150714, "SKIP": 515}; supplemental_native_counts: {"FAIL": 0, "PASS": 0, "SKIP": 1}
Counts are top-level Go result lines per job; subtest skip events are inventoried separately.
test_log_dir: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/c3-run/evidence/remote/logs
test_exec_mode: remote-exec
test_exec_bep: {"acceptance": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 25, "results": 25, "retried": 0, "window_ok": true}, "integration-full": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 14, "results": 14, "retried": 0, "window_ok": true}, "integration-latency": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 1, "results": 1, "retried": 0, "window_ok": true}, "integration-packages": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 86, "results": 86, "retried": 0, "window_ok": true}, "unit": {"cached_locally": 0, "cached_remotely": 0, "other_strategy": 0, "remote_executed": 408, "results": 408, "retried": 0, "window_ok": true}}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98", "worker_env_tree": "sha256:1c13f58eb37a36d4961b8db0e37825d1ddc39ebce86a33bd58b50abb495bfd98"}
fallback_reason: none
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 78.62497329711914, "waited_seconds": 0.001}
farm_test_seconds: 11813.187
skip_inventory: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/c3-run/evidence/remote/skip-inventory.json; SHA256 58f2fa64b81a3a29d2ecd9782709096fb00f7d5d86a3ef2472a0abc840275cdd
owned_topup: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/c3-run/evidence/remote/owned-topup.json; SHA256 37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570
floor_topup: complete=true, 16/16 commands exit 0; /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/c3-run/evidence/remote/floor-topup.json; SHA256 321e5afd3af01a465b014ee28ec6dca0138c65a3df5cf00547ded76131617c95
supplemental_floor_topup: one additional gated-tree native command exit 0 for upstream-storage-unavailable; counts={"FAIL": 0, "PASS": 0, "SKIP": 1}; /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/supplemental-floor/verified.json; SHA256 fe0bddabe56e674c352268d3b2db67f2a3b4b132bfe49a14323426acdbeae65b
latency_leg: measured, complete=true, rc=0; both named tests PASS in a clear-probe leg; /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/c3-run/evidence/remote/latency-leg.json; SHA256 e5efcb731095c86e847514d34175f79778e7799497fa6b96d4050ce8dfd6d50b
heavy_mode: none (computed)
waiver_ref: none (gascity has no waiver path)
failure_attribution: none
ci_lane_run: n/a (no CI-config change)

diff_tests_executed:
```text
TestReconcileSessionBeads_NoEpisodeReadWithoutDownCandidate PASS (integration-packages-074fc6c34a1fb2577127.log, unit-912ab643330b35bd0b6b.log)
  TestReconcileSessionBeads_StartupHealthEpisodeIndexServesEachCandidateItsOwnEpisode PASS (integration-packages-514989cc035c0e9a8ff5.log, unit-cd6930bb0d0da8646500.log)
  TestReconcileSessionBeads_StartupHealthEpisodesReadOncePerTick PASS (integration-packages-bf8ed8ba46004086b3b9.log, unit-f93a0ca96ccc61d0cae9.log)
  TestReconcileSessionBeads_StartupHealthListFailureFailsOpenForAQuarantinedCandidate PASS (integration-packages-2ab73b52d8fd4dc346df.log, unit-5a9df316125cd0aeff2e.log)
  TestReconcileSessionBeads_StartupHealthListFailureFailsOpenOnce PASS (integration-packages-75af79934639abaae69c.log, unit-088cc38d26d094729229.log)
  TestOpenStartupHealthEpisodesMatchesPerSessionLoad PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestOpenStartupHealthEpisodesPropagatesListError PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
  TestOpenStartupHealthEpisodesReadsOnce PASS (integration-packages-1a2f03374de01d151211.log, unit-723353f4f1923fba5e16.log)
```

## Acceptance and review

reviewer_tests_verified: 66438ec6c008472362af44af0fc516a6820a7b2f -> de0b9ec71449d10dd324c48473ae36f02c9a4ace files=cmd/gc/session_reconciler_startup_health_batch_test.go
AC1: mixed candidates use one List and their own episodes/quarantine. AC2: a tick with no down wake candidate reads zero episodes. AC3: newest/open/type/name/tier selection equals per-session Load. AC4: one failed read is memoized; all candidates, including quarantined, fail open and log. AC5: existing test assertions remain unchanged; effect-lint classification and failure injector support are updated and explained. AC6: commit-phase source file is byte-identical to base.
PR requirements: Refs #7170; baseline RED/branch GREEN; no synchronous episode writer between loop lookups; prior-tick async commit can overlap and permit one extra wake attempt before refresh; SESSION-RUNTIME-034 added for the new contract (pending row-number collision is merge-time bookkeeping); two existing test support edits named; all outside-slice work retained. Batch returns one O(E) payload, preserving zero-tier semantics. The synchronous mirror writes session rows only and commit-phase readers/writers are unchanged.
Baseline: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/baseline-audit.json; SHA256 fb9f4331b7f71dc1a5b53412cac3c08076d79927571fb27a05f441094f5a5338

## Fast lanes

policy_lane: fresh-tree-run.sh -> gate-base.sh run -> bash /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/fast-lane.sh policy; exit 0; run-pinned-lint.sh -- make fmt-check-changed, make lint, make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary check-hooks, and all 13 explicit policy targets PASS.
drift_lane: fresh-tree-run.sh -> gate-base.sh run -> bash /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/fast-lane.sh drift; exit 0; make bazel-sync, git diff --exit-code, empty git status --porcelain --untracked-files=all, and all seven generated/client/lockfile targets PASS.
openapi_lane: fresh-tree-run.sh -> gate-base.sh run -> bash /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/fast-lane.sh openapi; make openapi-breaking-check against pinned GATE_BASE_SHA exit 0.
storeclass_policy: fourth fresh view; trusted-base generator, strict baseline and growth checks exit 0.
known_red_precheck: proceed, no known-red blocker.

## Skip justification

The original suite has 501 remote and39 native SKIP events (top-level tallies are reported separately). All327 distinct reported source sites are outside changed hunks and their complete source files are blob-identical to the frozen base. Shared/helper hunks are confined to cmd/gc and internal/session: the effect-lint read classification, startup-health failure injector, and new batch-test fixtures. For every skip whose test-binary import closure reaches those packages, same-command named baseline runs reproduce the exact reason:339 remote events across11 packages, plus7 native events in cmd/gc and internal/runtime/tmux. The other162 remote and32 native events cannot reach any changed shared/helper package. Reasons are existing lane guards, platform-only paths, subprocess helpers, fixture/capability exclusions, optional live integrations and absent tools/endpoints; no changed test skipped or failed. The supplemental native test also skips on the existing upstream-storage-unavailable guard; its file is unchanged and its closure has no changed helper reach. Full multiline reason and source proof are retained. Native load wait cleared after180s at14.58; the earlier canonical native and cross-compile waits timed out and their observed loads remain explicitly recorded.
Full skip site, source/blob, import-closure and same-reason baseline proofs: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/skip-proof-automated.json; SHA256 c9391d5ed1fffdc2b42cdfcfd9a4d32fd29d682f68718f4815af0c3e15ed7cf4
Non-Go skip review: One existing Python unittest skip: bridge lockfile test_rules_ts_typescript_matches_lock is disabled because that target omits --rules-ts-typescript. Its source, macro and all build/lockfile inputs are identical to base; dashboard counterpart opts in and passes. All other non-Go targets pass without unexplained skips.

Audit tooling: the initial skip preparation stopped before baseline tests when interleaved CONT output hid one reason from the population reader. That reason was recovered from the exact same-action hash-verified testcase XML and matched to the original raw message and literal source. All540 original skip events were cross-checked against the independent diff-owned parser; two multiline reasons retain their full raw text. Original failed audit and helper remain under /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/skip-initial-failed-run and *.initial.py. Only that failed audit continuation was restarted; no runtime-suite result was replaced. The additional native observation covers an environment absence the canonical classifier called other.

## Required-job accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/merge 6bd609f5bfb41f43f444843b1220d0cde4473446 de0b9ec71449d10dd324c48473ae36f02c9a4ace fork; exit 0; workflow_blob=fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs.
```text
diff 6bd609f5bfb41f43f444843b1220d0cde4473446...de0b9ec71449d10dd324c48473ae36f02c9a4ace: 10 changed files
  filter packs                no match
  filter credential_provider  no match
  filter shared               no match
suite_mode: filtered  (shared filter did not match)

ci-required needs 7 jobs; transitive closure 7 jobs
job key                                  kind       runs-on                    trigger
runner-policy                            LEAF       linux                      EVERY-PR
changes                                  LEAF       linux                      EVERY-PR
check                                    AGGREGATOR linux                      EVERY-PR
  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
  windows-lane                           LEAF       linux                      EVERY-PR
  windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR

6 LEAF jobs need a status; the rest are rolled up through their needs.
```

ci_required_accounting:

- changes | LOCAL-PASS | {"evidence": "/var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/ci-required/closure.stdout", "status": "LOCAL-PASS"}
- runner-policy | LOCAL-PASS | {"evidence": "/var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/ci-required/runner-policy.stdout", "status": "LOCAL-PASS"}
- windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | {"evidence": "/var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/ci-required/windows-native-reach.json", "reason": "non-linux-runner", "status": "DEFERRED-TO-PR-CI", "windows_reach": "none"}
- credential-provider-windows | NOT-TRIGGERED | {"evidence": "  credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'", "status": "NOT-TRIGGERED", "windows_reach": "none"}
- windows-lane | LOCAL-PASS | {"command": ["/home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh", "--from", "/var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/merge", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh", "--threshold", "15", "--max-wait", "1800", "--", "/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh", "--", "python3", "/var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/run-ci-job.py", "windows-lane"], "rc": 0, "status": "LOCAL-PASS", "stderr": "/var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/ci-required/windows-lane.stderr", "stdout": "/var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/ci-required/windows-lane.stdout"}
- pack-gate | NOT-TRIGGERED | {"evidence": "  pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'", "status": "NOT-TRIGGERED"}

windows_reach: {"credential": [], "native": []}
deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none
Merge requires current-head CI / required and each deferred job to pass. This gate authorizes PR-open only.

## Observed load

c3-run/evidence/remote/floor-topup.stdout: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=69.54 run_max_load=63.81 run_mean_load=56.80 run_readings=7 wait_first_load=43.70 wait_max_load=81.62 wait_mean_load=53.55 wait_readings=61 read_errors=0
c3-run/units/gate.w1.stderr.log: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=15.04 run_max_load=82.16 run_mean_load=38.13 run_readings=115 wait_first_load=15.04 wait_max_load=15.04 wait_mean_load=15.04 wait_readings=1 read_errors=0
storeclass-proof/wrapper.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=900 wait_timed_out=0 run_start_load=14.86 run_max_load=15.35 run_mean_load=15.11 run_readings=4 wait_first_load=33.77 wait_max_load=33.77 wait_mean_load=23.88 wait_readings=31 read_errors=0
ci-required/windows-lane.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=77.63 run_max_load=80.21 run_mean_load=78.58 run_readings=5 wait_first_load=42.97 wait_max_load=81.78 wait_mean_load=65.83 wait_readings=61 read_errors=0
skip-base-evidence/native.stderr: LOAD_GATE_SUMMARY threshold=15 waited_seconds=180 wait_timed_out=0 run_start_load=14.58 run_max_load=15.96 run_mean_load=15.53 run_readings=2 wait_first_load=18.77 wait_max_load=18.77 wait_mean_load=16.72 wait_readings=7 read_errors=0
load_threshold: 15
load_waited_seconds: 1800
load_wait_timed_out: 1
run_start_load: 69.54
run_max_load: 63.81
run_mean_load: 56.80
run_readings: 7

## Retained evidence

C6 build/vet: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/c6-verified.json; SHA256 c8c161f8b581887309be2188703197385b05e5ccc44a5bfc908b9befa71cf9ac
Fast lane: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/fast-verified.json; SHA256 0b4097b2dda01aefc682d38b35c46b92f27c7be54ea26e2a8f587f3165b9b8c8
Raw full-suite and artifact audit: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/resume-c3-audit.json; SHA256 06058a7741e5cd697b471d09f581df858ed855720ee8abea7dedc38ae1b17f35
Package-bound owned results: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/owned-package-verified.json; SHA256 cfc7c6d08c9162b3066d9d8826651b904373d6b2690c0acdbd0065378a680edf
Required CI jobs: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/ci-required/accounting-verified.json; SHA256 cefca89fd9c1940b2a259b0fce8a1bbdd8898fd9d9ae30c4335f6aca240c48cd
Manual acceptance/skip review: /var/tmp/deploy-ga-bzz7vfy0.p1n8nupn/final-manual-review.json; SHA256 46eee7cd4ee20cd500a8910ff0fbe57ec8cc340d2fcfc008255da05ecef2ec62

Durable evidence snapshot: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-bzz7vfy0/gate-pass-20261111T203055Z (artifact-manifest.json records copied file SHA256 hashes).
