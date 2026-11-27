# Release gate: confirm runtime absence before session start

**Verdict:** **PASS**

Bead: ga-65n7ikf7. Review: ga-7rmxc0vk PASS on the verified exact source. Evaluated independently 2026-11-21 UTC; no carryover or current reviewer-added commit required.

`docs/PROJECT_MANIFEST.md` is absent at the pinned base; deployed release-gate protocols, repository TESTING.md, CI workflows and bead acceptance criteria supply the checks.

| Criterion | Result | Evidence |
| --- | --- | --- |
| 1. Review PASS | PASS | Live review notes and description unchanged; exact reviewed source resolved to a commit. |
| 2. Acceptance | PASS | All 15 changed regression roots PASS in unit and integration-package output; source, fresh-read routing, recovery error chains and pure cleanup refactor independently inspected. |
| 3. Tests and CI | PASS | Five complete fresh remote phases; native floor; full manual split-strict targets and workflow verdicts; all six required CI leaves accounted. Native Windows awaits PR CI. |
| 4. Review findings | PASS | Zero unresolved HIGH findings. Four accepted minor coverage gaps remain on ga-601vpk1v, blocked on actual implementation landing through ga-cjg58n4q. |
| 5. Clean branch | PASS | Reviewed and validation trees clean; this record is the only added artifact. Record-only parent and empty worktree status are enforced again before push. |
| 6. Merge state/build/vet | PASS | Clean synthetic merge on the pinned main commit; complete Bazel build and explicit nogo lint exit0, raw BEPs independently checked. |
| 7. Single theme | PASS | Only session-start liveness, its tests/requirement/BUILD entry and inherited gate records. No unrelated feature or internal-agent configuration. |

## Source and merge state

- deploy_mode: remote
- push_remote: fork
- reviewed_commit: c1b659ef5b623dd5fadb86ccb73c9b7e13d0b895
- gate_head: c1b659ef5b623dd5fadb86ccb73c9b7e13d0b895
- gate_base: 8357024c456bc648b4a514152274acea42569ef4
- synthetic_merge: adf51f28b010f0e818bad7eb1648c1ceabad6554
- merge_tree: d42e5ec0bbe8a4193190324a47c06fcc2fdd9d95
- merge_state: clean (origin/main@8357024c456bc648b4a514152274acea42569ef4; files:none)
- gate_base_kind: current-tip
- waiver_ref: none
- failure_attribution: none; no failed job attributed

merge_validation: merge-time guarantee applies at the eventual PR head; MPR checks current CI and pins the exact cleared head. No rebase, branch reset or conflict resolution performed. The older bdce-based evaluation is superseded and contributes no test receipts here. This fresh evaluation includes landed baseline8357024c456bc648b4a514152274acea42569ef4.

## Acceptance evidence

A cached running result takes no extra fresh read. Before any absent-session start, orphan scan or stale-key retry, the manager confirms absence through the leaf serving the session name. An unreadable runtime returns the deferral error without starting, resetting or clearing resume identity. A corpse/confirmed absence starts once. Session-exists converges only on fresh confirmed running; other errors preserve the original start/resume classification.

Architecture ruling ga-f6y5q8n / FR-04 supersedes Change3 and acceptance8 (also called14 earlier) of ga-gkooyt: hybrid deliberately does not forward FreshLivenessObserver; the manager reads the routed leaf and checks both backends for an unseeded auto route. The earlier RED TestProvider_ForwardsFreshLivenessToRoutedBackend was deliberately removed under that ruled specification change. Public description uses Refs #7170, never Closes/Fixes.

The releaseRoute cleanup is independently equivalent after replacing the exact nil-guarded callbacks and removing the helper; normalized SHA256 55816f6399841a9fc17c488f21874418dc63b8df51af51140f55d660b3f3ea1c. The current merge chat.go equals the reviewed source. Complexity scores fall52→45 and29→27. Global base and merge checks both exit2; documented zero-growth form(ii) passes, no new/increased rows and baseline untouched. No waiver or red-job attribution used.

Owned regression roots (each PASS in both canonical unit and integration-packages):

- TestAStartThatFailsAfterItsLaunchQueuesTheTurn — PASS
- TestEnsureRunning_CachedRunningTakesNoFreshReadAndStartsNothing — PASS
- TestEnsureRunning_CompositeInFrontOfCachingProviderReadsTheLeafFresh — PASS
- TestEnsureRunning_ConfirmedNotRunningStartsOnce — PASS
- TestEnsureRunning_FreshCompositeOverALeafWithoutFreshReadConfirmsTheGate — PASS
- TestEnsureRunning_ProviderWithLivenessReadButNoFreshReadTakesNoExtraRead — PASS
- TestEnsureRunning_ProviderWithoutFreshReadKeepsItsIsRunningReadsBeforeStart — PASS
- TestEnsureRunning_SessionExistsConvergesOnFreshRead — PASS
- TestEnsureRunning_SessionExistsOnAFreshNotRunningReadIsNotConvergence — PASS
- TestEnsureRunning_StaleAbsentSnapshotOverLiveRuntimeConverges — PASS
- TestEnsureRunning_StaleKeyCheckOnConfirmedAbsenceStillRetriesFresh — PASS
- TestEnsureRunning_StaleKeyCheckOnUnconfirmedLivenessDoesNotRetainACommittedConversation — PASS
- TestEnsureRunning_StaleKeyCheckOnUnconfirmedLivenessKeepsResumeState — PASS
- TestEnsureRunning_UnconfirmedLivenessStartsNothingAndChangesNothing — PASS
- TestEnsureRunning_UnseededAutoRouteAsksTheOtherBackendBeforeStarting — PASS

## Static lanes and canonical test evidence

Five independent fresh views passed format, policy/shell guards, generated-file drift (including make bazel-sync and clean diff), OpenAPI drift and documented complexity zero-growth before runtime setup or suite launch. No API/OpenAPI/dashboard implementation changed.

- static_audit: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/fast-independent-audit.json; SHA256 7070e4c5742d305f3142518aa088e68a7f549927edc37782d3edc1e265caf002
- criterion6_audit: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/c6-audit.json; SHA256 530a555dfefeabdef3af045ee9f573d7bd5cdbf68c001e351d38a452202be621
- test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.L7p7n0 --base 8357024c456bc648b4a514152274acea42569ef4 --bead-json /var/tmp/gc-gate-ga-65n7ikf7-current-main-xsh6pdpj/bead-criterion3.json --owned /var/tmp/gc-gate-ga-65n7ikf7-current-main-xsh6pdpj/owned.tsv --run-dir /var/tmp/gc-gate-ga-65n7ikf7-current-main-xsh6pdpj/c3-run/evidence; exit0, criterion3_valid:true
- test_cmd_scope: full-suite
- heavy_mode: none (preflight classifier; no reached heavy package)
- delta_gate: {"mode":"shadow","decision":"full","reason":"not-gate-pr","applied":false}
- test_counts: PASS=158051 FAIL=0 SKIP=617 (official reader top-level lines, per job)
- diff_tests_executed: 15 PASS; 0 FAIL; 0 SKIP; all roots/subtests verified by package and name
- test_exec_mode: remote-exec
- fallback_reason: none
- test_log_dir: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/c3-run/evidence/remote/logs
- test_exec_bep: every phase below; no cache-credited result, local action or retried measured test
- farm_test_seconds: 11723.795
- admission: {"class":"remote","floor_gib":25.0,"free_gib":72.57419967651367,"waited_seconds":0.001,"cap":2}
- rbe_state: {"rc_marker_present":true,"kill_switch_absent":true,"cert_notAfter":"notAfter=Jan  9 20:13:22 2027 GMT","worker_env_tree":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72","worker_env_origin_main":"sha256:54ea5bdd4c5c8a43f1cb376bd2f5888749e198a398cd8893006d48d8a4c48e72"}
- owned_topup: []
- floor_topup: 16 steps, all exit0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/c3-run/evidence/remote/floor-topup.json; SHA256 21befbb83afe24a3a02755dec66addd19d01a1d84e16c4355a71299b71093ae3
- skip_inventory: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/skip-independent-audit.json; SHA256 ab627a7f961e1386d875765af5d94a671d923960132ca885541be58383bde874

| Phase | Fresh remote actions | Cached/local/retried | Inside phase window |
| --- | ---: | --- | --- |
| unit | 422 | 0/0/0 | true |
| acceptance | 25 | 0/0/0 | true |
| integration-packages | 87 | 0/0/0 | true |
| integration-full | 14 | 0/0/0 | true |
| integration-latency | 1 | 0/0/0 | true |

549 actions PASSED; all 1098 raw log/XML artifact digests and lengths verified independently. Full execution record retained: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/c3-run/evidence/execution.json; SHA256 2f16358f60c96636249cee1868995293d5f44fd699a212318906cb0fe6ea2358. Raw audit: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/five-phase-raw-audit.json; SHA256 7376a253c9174be8f01ef7af7eaffd769a34166c8f026b9233088805b912030c.

latency_leg: measured; 1 probe attempt(s), 0 unmeasured load deferral(s); every registered test PASS after a clear probe. Probe lines:

- GATE_FARM_PROBE verdict=clear waited=0s load1=24.31 load5=21.73 load15=16.33 cpus=32 threshold=50 max_wait=600s

Remote suite admission has no host-load wait; native floor and BASE native comparisons keep their prescribed local wait. Native floor observed load:

- LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=10.45 run_max_load=12.79 run_mean_load=12.34 run_readings=7 wait_first_load=10.45 wait_max_load=10.45 wait_mean_load=10.45 wait_readings=1 read_errors=0

load_threshold: 15; load_waited_seconds: 0; load_wait_timed_out: 0. The run_start_load/run_max_load/run_mean_load/run_readings and separate wait observations are preserved above.

Runtime preflight: ICU74 OK, rootless podman socket active and ping healthy, pinned Dolt image present, recent testcontainer sweep verified, disk floors checked. Canonical native floor includes environment-absence follow-ups, beadstest, Darwin compile and runner self-tests; all complete.

## Required manual split-strict variant

The complete cmd/gc and internal/api manual targets ran in a new merged-tree view with original sharding and GC_TEST_SPLIT_STRICT=1. 30 first-attempt remote uncached actions and 60 raw artifact hashes verified. Literal frozen bazel.yml Split-strict verdicts step returned0: ratchet PASS, tests PASS, combined line caps PASS, no missing reports. 381 expected raw failed-root events are all in the frozen per-package known-failures list; they are separate from canonical suite failure counts and no failing job was attributed.

- split_strict_workflow_blob: 1c49e5b9c3e28776e4e97d6d298ca9105205e884
- split_strict_audit: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/strict-independent-audit.json; SHA256 51492f695e2de08bffc66f1945efe730fb39d7ef85b37b35dcaf40adda34ec4c
- integration_smoke_coverage: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/integration-smoke-equivalence.json; SHA256 929e1c43ce29e35c2d983d38b7a8b8cad6c2d17ad4a9da04186321d8ded29deb (same executable/config/env/runfiles, subset args only; all14 selected roots observed in this full integration run; its pre-existing skip is accounted below)

## Exhaustive skip justification

675 all-depth SKIP events (603 remote plus 72 native), 317 source sites; every event paired with the same test/reason at this pinned BASE using the same target configuration or exact native-floor command/environment/umask. Official top-level counts intentionally differ from all-depth inventory. Owned skips0; unresolved0. Phase comparisons: {"unit":442,"integration-packages":133,"acceptance":14,"integration-full":14,"native":72}.

Every guard file is unchanged between pinned BASE and merged head, every site is outside changed test bodies/hunks, and the printed guard/source matrix was reviewed. The only shared hunk adds session-test fixtures with no init/TestMain; the three non-owned session skips are unchanged immediate platform guards before start behavior. Remaining guards concern platform/UID/filesystem, process tiers, deliberate fixtures/capabilities, helper selection, opt-in/live tools and existing characterization cases. All environment-absence cases were attempted natively. Full source witnesses, reasons, BASE outputs and reader normalization notes remain retained; no raw log rewritten.

named_shared_hunks: ["SHARED-HUNKS internal/session/ensure_running_unconfirmed_test.go: 1  (a helper, TestMain/init or file-level code changed; a not-owned SKIP in this package is pre-existing ONLY if none of these can reach it: --compare-skips measures that)"]
skip_guard_matrix_sha256: bd55a9b6c3cff44a1674695465649797ca8af933f1c7723fe59908e91ef2e5c3

COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestIsGoTestExecutableRecognizesThisBinaryWhateverItIsCalled (base 9085ca24-579a-4efe-811a-c3711b177630 and 1626169251, head 588fbf09-4253-4eec-bc5a-4e514e14b6e8 and 839179999)
COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestProxyProcessReapRegistryMatchesAnEquivalentSpelling (base 1047656714, head 324252023)
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a01, head rbe-a09)

## Required CI closure

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.L7p7n0 8357024c456bc648b4a514152274acea42569ef4 c1b659ef5b623dd5fadb86ccb73c9b7e13d0b895 fork; ci.yml blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; `6 LEAF jobs need a status; the rest are rolled up through their needs.`

ci_required_accounting:

- runner-policy | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.L7p7n0 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-65n7ikf7-current-main-xsh6pdpj/ci-runner-policy-v3.sh; exit0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/ci-required-v3/runner-policy-output.txt
- windows-lane | LOCAL-PASS | /home/jaword/projects/gc-management/packs/actual/all/scripts/fresh-tree-run.sh --from /var/tmp/gc-merge-validate.L7p7n0 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/load-gate-run.sh --threshold 15 --max-wait 1800 -- /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash /var/tmp/gc-gate-ga-65n7ikf7-current-main-xsh6pdpj/ci-windows-lane.sh; exit0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/ci-required-v3/windows-lane-output.txt
- credential-provider-windows | NOT-TRIGGERED |   credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
- pack-gate | NOT-TRIGGERED |   pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'
- windows-lane-test | DEFERRED-TO-PR-CI |   windows-lane-test                      LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} EVERY-PR; reason=non-linux-runner
- changes | LOCAL-PASS | python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.L7p7n0 8357024c456bc648b4a514152274acea42569ef4 c1b659ef5b623dd5fadb86ccb73c9b7e13d0b895 fork; exit0; /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/ci-required-v3/closure.txt
- runner-policy observed load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.25 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=11.25 wait_max_load=11.25 wait_mean_load=11.25 wait_readings=1 read_errors=0
- windows-lane observed load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=11.65 run_max_load=15.18 run_mean_load=13.66 run_readings=3 wait_first_load=11.65 wait_max_load=11.65 wait_mean_load=11.65 wait_readings=1 read_errors=0

windows_reach: credential-provider-windows:none; windows-lane-test:none. Native Windows reach computed from that job own test allowlist and ordinary/integration dependency closures; no credential-only proxy reused. Native runner resolves to blacksmith-4vcpu-windows-2025. Linux job ran all four literal Windows/macOS cross-build commands and nogo build/test compilation.

deferred_to_pr_ci: windows-lane-test (non-linux-runner)
not_covered: none
host_gaps: none

Full job commands, trigger-tool rows, terminal statuses, per-job load and separate reach evidence: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz/evidence/ci-independent-audit.json; SHA256 6dd3f42ca9b1e6db6305430fa29c80bd6ff954dad01ed774d5cc9e86df858643.

PASS authorizes PR-open only. Require CI / required green on the current head, successful deferred native Windows CI, and the required Bazel lanes/merge-group checks before MPR merges. Clearance and merge-request must pin the exact pushed head; a changed head requires a new gate.

## Retained evidence

Hash-verified archive: /home/jaword/projects/gc-management/.gc/artifacts/gascity/ga-65n7ikf7/gate-20261121-kmtgzsmz; 2806 files, 273607883 bytes. Initial manifest SHA256 7d9a0aed6b6d83880c6c91574f78ec0f090f7b3aeec8483f1ec1e37ca11f0ec6. Tested merge bundle SHA256 34cbc50f530aa897ab6209313ea8ae44a429546816f93d359c76f6d600956453. Publication artifacts are added under a separate manifest; this initial snapshot stays immutable. No reviewer or superseded-run test result was credited.
