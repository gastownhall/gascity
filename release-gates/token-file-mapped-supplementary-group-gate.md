**Verdict:** **PASS**

# Release gate: mapped supplementary group test

Bead ga-akfxxfq; build ga-pbnygbd; review ga-uksz7yf. Assessed 2026-10-31T14:08:57.797397+00:00.

Reviewed source: `62f56914279325cb76fb440a47d3a152621672e7`. The builder branch is provenance only.
gate_base_sha: `d1a150cac8f82649cf7f16d943cb25c3505a4379`; gate_base_kind: current-tip.
merge_state: clean (origin/main@d1a150cac8f8); conflicts: none.
Tested merge: `9ea457eefc2e629e8803aeec2ed721efa1deeaa9`; tree: `faaed96d5199f0778e51fe025aec5af8f5fd695b`.
merge_validation: MPR validates the merge result and resolves any later conflict; this gate authorizes PR opening only.
Evidence root: `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe`. `docs/PROJECT_MANIFEST.md` is absent; repository AGENTS.md, TESTING.md, Makefile and deployer criteria govern.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review passes | PASS | ga-uksz7yf records verdict pass at the resolved source; no review carryover or reviewer-added tests. |
| 2 | Acceptance criteria verified | PASS | Source selects a usable secondary gid, excludes overflow, retries EINVAL, fails other chown errors, and names the unmapped-group cause when skipping. Fresh mapped-group assertion PASS and namespace named SKIP are both retained. Root fallback and EINVAL retry were inspected, not independently forced. Upstream landing is explicitly still pending below. |
| 3 | Tests and required lanes | PASS | Full local scope with all non-owned failures attributed under 3a, including the real isolated latency failure. Raw nonzero exits remain unchanged. Counts and exact owned result below. No waiver. |
| 4 | No open high findings | PASS | Reviewer reports zero security/style findings; no unresolved HIGH findings. |
| 5 | Clean branch | PASS | Source worktree and canonical merge scratch clean before adding only this record; normal commit hooks and final cleanliness checked before publication. |
| 6 | Merge state and build/vet | PASS | Clean scratch merge on pinned main; normal merge hooks and `bazel build --keep_going --output_groups=nogo_fix --config=remote-exec --noremote_local_fallback //...` passed; retained C6 result complete. |
| 7 | Single feature theme | PASS | One source commit, only internal/connect/tokenrender/write_test.go, +37/-9. Test portability only. |

## Executed evidence and original outcomes

test_cmd: pinned-base `make check` with the documented gascity-bazel-legs wrapper, followed by all unstarted required acceptance/integration tiers, native floor and the canonical isolated latency leg. Complete commands, flags and phase windows are in the assessment pointer below. The supplemental collector's scope is focused; it supplements the original full-tree unit command, never replaces it.
test_cmd_scope: full-suite.
test_exec_mode: local, derived from first-attempt uncached BEP actions; no farm/local runtime-suite stitching. C6 build and fast-lane cache/build evidence are separate.
test_exec_bep: `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/c3/evidence/local/unit*.bep*.json` and `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/supplemental-run/evidence/local/*.bep*.json`.
test_log_dir: `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/c3/evidence/local/logs`, `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/supplemental-run/evidence/local/logs`, `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/latency-only-run/evidence/local/logs`.
rbe_state: runtime-suite remote switch disabled for this invocation only; global rollout unchanged.
fallback_reason: complete local path selected so the owned assertion runs with a mapped supplementary group.
farm_test_seconds: 0 for the runtime suite.
admission: local-heavy, 40 GiB floor, cap 3; actual admission records retained under each run. No limits changed.
heavy_mode: none.

test_bd: isolated-test-run:   pinned test bd (gm-i3csn7): GASCITY_TEST_BD version=v1.3.2 bin=/home/jaword/.local/bd-versions/v1.3.2/bd reports='bd version 1.3.2 (9b65b9e19: HEAD@9b65b9e198ac)' ref_check=match:9b65b9e19 origin=preexisting pin_source=/var/tmp/gc-merge-validate.akfxxfq.5p1xc3mx/deps.env displaces='bd version 1.3.2 (9b65b9e19)'
diff_owned_evidence: `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/owned-explain.txt`; by-name owned assertion result: `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/unit-owned-by-name.txt`.
not_owned_skip_evidence: `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/required-suite-skip-sites-proof.txt` and `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/required-native-skip-sites-proof.txt`; unchanged site, printed reason and shared-hunk review are bound by the inventory pointer below.
skip_justification: Every non-owned skip site is unchanged, and this test-only diff changes no linked production path; missing optional prerequisites and deliberate suite partitions remain inventoried. Environment-absence parents received the required native top-ups. The owned namespace skip is paired with an actual mapped-group assertion PASS.

| Phase | Bazel actions | Original exit | Top-level PASS/FAIL/SKIP | Original target verdicts |
|---|---:|---:|---|---|
| unit | 378 | 2 | 35617/4/184 | {"FAILED": 4, "PASSED": 363, "TIMEOUT": 11} |
| tokenrender-namespace | 1 | 0 | 65/0/2 | {"PASSED": 1} |
| acceptance | 24 | 0 | 121/0/11 | {"PASSED": 24} |
| integration-packages | 85 | 3 | 25043/1/46 | {"FAILED": 1, "PASSED": 84} |
| integration-full | 14 | 3 | 191/2/14 | {"FAILED": 2, "PASSED": 12} |
| Native floor | 12 package jobs + 3 fixed checks | 0 each | 17/0/14 | All commands exited 0; non-owned skips retained |
| Isolated latency | One measured attempt, two tests | 1 | 1/1/0 | Bd latency PASS; seat mail-hook latency FAIL, attributed below |

test_counts: PASS=61055 FAIL=8 SKIP=271.
These are terminal top-level result lines summed across jobs/configurations, excluding slash subtests. They do not count interrupted tests as passing. Unit inclusive counts are 74505 PASS/6 FAIL/252 SKIP; true unit top-level counts are 35617/4/184. Rules_go framing flattens indentation, so the helper's column-zero tally includes subtests; the independent frame audit corrects that. All 502 Bazel test actions are first-attempt uncached local results; all 1506 retained log/XML/normalized artifacts were hash-verified at final assessment.

diff_tests_executed: TestWriteTokenFileKeepsGroup PASS in the full unit processwrapper leg (`unit-f79997cecbebd6b14e33.log`); also SKIP under linux-sandbox, with the named no-mapped-supplementary-group reason. That SKIP is evidence of portability, never assertion execution. The untouched root-only owner test still skips. No claim that RBE or linux-sandbox runs the group assertion.
owned_topup: documented processwrapper leg within original full unit scope; extra uncached namespace witness.
floor_topup: all 31 required parent roots observed in 12 package jobs; three fixed checks exited 0.
skip_inventory: 354 suite skip events, 279 non-owned site candidates unchanged; 39 native-floor skip events, 36 candidates unchanged. Every reason/site is retained. The only shared-file non-owned site is the unchanged root-only tokenrender owner test: the new overflowGID helper is called only by the owned group test; the strconv import and owned comment cannot affect root/owner detection. Other skips are unchanged environment prerequisites or intentional suite partitions, with no changed production input reaching them.
waiver_ref: none.
ci_lane_run: n/a, no CI configuration change.

## Failure attribution, never rewritten results

Criterion 3 PASS is the explicit 3a assessment, not a zero-failure runner result. Original C3 exit 2 / criterion3_valid=false, supplemental exit 1 and latency result=failed / rc=1 / complete=false are retained verbatim. The failed latency leg has terminal verdicts for both registered tests after a clear probe; it is an attributable measured failure, not an incomplete/machinery deferral. No failed test, completed unit or measured latency verdict was relaunched.

All attribution uses clause 1 (unchanged/non-owned test), clause 3(a) (computed test import closures and no changed non-test input, including unchanged externally built gc/supervisor and invoked scripts), and clause 4 (disjoint package). No new test target/file, resource-census bump, sleep, subprocess, listener or parallelism was added. All are the first gate sighting for this deploy; subsequent comments document different required tiers of the same gate. Trackers were opened and sightings read back. The newly discovered mail-reply condition uses the documented discovering-run exception with landed mechanism proof, not a claim it predates this run.

| Unit action | Original verdict | Tracker |
|---|---|---|
| `//internal/productmetrics:productmetrics_test` shard 1 | TIMEOUT | ga-46vdxg |
| `//internal/productmetrics:productmetrics_test` shard 2 | TIMEOUT | ga-46vdxg |
| `//scripts:scripts_test` shard 3 | TIMEOUT | ga-vg3wto5 |
| `//scripts:scripts_test` shard 2 | TIMEOUT | ga-vg3wto5 |
| `//internal/runtime/t3bridge:t3bridge_test` shard 1 | FAILED | ga-vkhfnj |
| `//internal/beads:bd_conditional_release_contract_test` shard 1 | FAILED | ga-vkhfnj |
| `//internal/beads:beads_native_doltlite_test` shard 1 | TIMEOUT | ga-vkhfnj |
| `//internal/beads:beads_test` shard 3 | TIMEOUT | ga-vkhfnj |
| `//internal/readywork:readywork_test` shard 1 | TIMEOUT | ga-vkhfnj |
| `//internal/storebinding/sqlite:sqlite_test` shard 1 | TIMEOUT | ga-vkhfnj |
| `//internal/session:session_test` shard 1 | TIMEOUT | ga-vkhfnj |
| `//internal/storebinding:storebinding_test` shard 1 | TIMEOUT | ga-vkhfnj |
| `//internal/dispatch:dispatch_test` shard 1 | TIMEOUT | ga-vkhfnj |
| `//test/acceptance:bd_cli_contract_current_test` shard 1 | FAILED | ga-vkhfnj |
| `//test/acceptance:bd_cli_contract_prev_test` shard 1 | FAILED | ga-vkhfnj |

The explicit unit assertion failures are TestBdDependencies (current and previous bd), TestBdStoreReleaseIfCurrentAgainstRealBd, TestObserveTurnStalledServerReturnsByTheDeadline/bearer=test-bearer and TestComplexityDiffDuplicateKeysRespectThreshold/low_duplicates_stay_ignored_when_a_later_row_is_high. Timeout interrupted-name mappings are retained per action in the unit-attribution pointer.

- TestX6FanIn/steady: p99 6.233953562s exceeds 5s; ga-s4hclbi, sighting cc8f01c5-f83c-5a16-a7ef-684138572bc8.
- TestMail_BashAgent: reply timeout; ga-aoy1f5a, newly tracked after confirming the older prefix remedy is already in the tested tree; no causal claim that host load caused it.
- TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash: fixture gc init 2m timeout before workflow; ga-vkhfnj, sighting 9ae02c13-53ce-5aa9-8327-6693d92f6c72.
- TestGcSeatHookLatencyBudget: mail hook fastest-of-seven 1.087s exceeds unchanged 1s; ga-y39d8tx, sighting ddb1fdc2-8d25-5c71-b1aa-899b8c08d0c9. Site test/integration/gc_seat_hook_latency_test.go:183 UNCHANGED, shared hunks 0. TestGcBdLatencyBudget PASS. Probe: GATE_FARM_PROBE verdict=clear waited=0s load1=16.59 load5=43.43 load15=62.83 cpus=20 threshold=50 max_wait=600s. Test header reports cpus32/load1=53.37; the probe reports cpus20. Neither discrepancy nor high later load is used to dismiss or retry this real failure. Root cause is not inferred.

Full unit per-action/test attribution retained: 967 lines; `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/failure-evidence/unit-failure-attribution.json`; SHA256 `0925737a18a9e00faef36b81550f5dd0bf70d38fa248602042ab55049b1aa9ad`.
Full required-tier attribution retained: 117 lines; `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/supplemental-attribution-record.json`; SHA256 `a8362b1ef8f875d8909d2b4ebc53e2e3ca92d9f0bfcf81872ea3955179982284`.
Full isolated latency attribution retained: 25 lines; `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/latency-attribution.json`; SHA256 `38c02e91d6e1e47cb23f6cf3faac42195fecaea21f12f7c1871c9dba02d017f1`.
Full final separate evidence assessment and exact commands retained: 231 lines; `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/final-evidence-assessment.json`; SHA256 `721af5959179f746906f563b0dd6cdfffd1aee2aa34b668c2b86812fdcf036df`.
Full skip inventory and site proofs retained: 33 lines; `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/required-tier-skip-audit.json`; SHA256 `a872df861785105381f9f8219fcdfcda4d9b7978baa189dd3a808f637f34c7dd`.
Full native-floor commands/results retained: 1038 lines; `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/remaining-tiers-independent-audit.json`; SHA256 `95ace529f4b39a74ec55a69dc5551e077380bf1de61ebf95fb19f01235f93288`.
Full mapped-group and namespace proof retained: 19 lines; `/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/group-environment-proof.json`; SHA256 `9b9f8efa19ab17e4aa5c97c184d8b5aebd07c18b09ee774791103fff0c19c63a`.

## Fast lane, runtime and load

policy_lane: PASS, pinned-base shell guards, formatting and required policy/nogo/format tests in fresh views before runtime/suite; `fast-policy.log`, `fast-lanes.stdout.log`.
drift_lane: PASS, `make bazel-sync`, `git diff --exit-code` and empty status plus generated-artifact tests in a separate fresh view; `fast-drift.log`.
openapi_breaking: PASS against pinned base; `fast-openapi.log`.
Runtime: ICU74, rootless podman and pinned image checked before suite; no host endpoint or library-path edits. Outer wrapper reported PASS-THROUGH, NOT ISOLATED; it did not clone/scrub the tree. Bazel uses pinned test environments; native floor and canonical latency helper scrub fleet environment. Retention override 876000h was checked before builds.
latency_leg: one canonical isolated LOCAL attempt under the unchanged probe50/hold600/poll10/attempts2 defaults; both named verdicts present; failed mail budget attributed under 3a. No retry-to-green.

```text
c3: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=52.46 run_max_load=129.19 run_mean_load=103.13 run_readings=63 wait_first_load=26.66 wait_max_load=74.16 wait_mean_load=54.25 wait_readings=62 read_errors=0 prewait_s=1801
supplemental-run: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1804 wait_timed_out=1 run_start_load=73.42 run_max_load=176.82 run_mean_load=107.39 run_readings=78 wait_first_load=73.39 wait_max_load=100.32 wait_mean_load=76.74 wait_readings=62 read_errors=0 prewait_s=1804
latency-only-run: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=43.37 run_max_load=54.04 run_mean_load=50.46 run_readings=3 wait_first_load=77.08 wait_max_load=78.83 wait_mean_load=67.54 wait_readings=62 read_errors=0 prewait_s=1801
```

Bounded ordinary load waits timed out and continued as policy allows; this is not a claim the host was unloaded.

## Required CI accounting

ci_required_closure: python3 /home/jaword/projects/gc-management/packs/actual/deployer/scripts/ci_required_triggers.py /var/tmp/gc-merge-validate.akfxxfq.5p1xc3mx d1a150cac8f82649cf7f16d943cb25c3505a4379 62f56914279325cb76fb440a47d3a152621672e7 fork; exit0; workflow blob e5b11c348505dab28dc223e874bc2c61cbbb33c5; 4 LEAF jobs need a status.

runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=/var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/ci-required-attributed/runner-policy.out python3 .github/workflows/scripts/runner_policy.py; exit0; /var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/ci-required-attributed/runner-policy.log. Read-only exception: formula 3e explicitly runs this command as shown.
changes | LOCAL-PASS | closure/path matcher command above; exit0; /var/tmp/deploy-ga-akfxxfq-resume.9z31vwbe/ci-required-attributed/closure.log.
credential-provider-windows | NOT-TRIGGERED | credential-provider-windows            LEAF       NON-LINUX ${{ needs.runner-policy.outputs.runner_windows }} NOT-TRIGGERED  if: needs.changes.outputs.credential_provider == 'true'
pack-gate | NOT-TRIGGERED | pack-gate                              LEAF       linux                      NOT-TRIGGERED  if: needs.changes.outputs.packs == 'true'

check is AGGREGATOR and takes no separate status.
windows_reach: none (exit0, empty output)
deferred_to_pr_ci: none
not_covered: none


## Landing still pending

The build bead's DONE WHEN, merged upstream through the normal path, remains MPR's merge-time exit condition. Opening the PR is not shipment. After actual landing, retire the separate gc-management workaround under gm-b3v057 and notify gascity/builder that its temporary pre-push rule ended; neither action is claimed here.
