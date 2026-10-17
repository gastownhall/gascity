# Proctable orphan reproduction exec wait

**Verdict:** **PASS**

Deploy bead: ga-81zad89. Build/investigation: ga-lt0wedw. Review: ga-6g2l2wo.
Reviewed source: `b93b351688dd8275038b955d1f1098078b081a64` (resolved as a commit).
Evaluation date: 2026-10-17. Deploy mode: remote, gastownhall/gascity.

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS | PASS | ga-6g2l2wo records `verdict: pass` on the exact reviewed source. No reviewer-added tests or carryover. |
| 2 | Acceptance criteria | PASS | The test launcher now waits for `comm=sleep` and a non-empty environment before scanning. All three callers passed independently in this gate. The investigation provides base controls, mechanism evidence, and a separate open landing guard, detailed below. |
| 3 | Full CI-equivalent suite | PASS | Four uncached remote phases plus the required native floor completed on the pinned merge. Raw total: 116418 PASS, 1 FAIL, 373 SKIP, counting top-level test results per job. The single unrelated failure is attributed below; it was not retried or removed from the evidence. |
| 3a | Failure attribution | PASS | `TestRunSetupCommandActivityStreamingSurvivesIdleWindow` -> ga-886gn1; clauses 1-4 and mechanism proof 3(a), below. |
| 3b | Format, policy/lint, generated drift | PASS | Fresh views ran pinned lint/format, `make lint-changed`, shell guards, policy Bazel targets, and `make bazel-sync` followed by an empty tracked/untracked diff. Build/nogo and `make vet` passed first. |
| 3c | CI configuration | PASS | No CI configuration changes. |
| 3d | Heavy package classification | PASS | `heavy-composite-gate.sh classify`: `HEAVY_MODE mode=none shard_required=- watch=-`. |
| 4 | No unresolved high-severity review findings | PASS | Review reports no blockers or security findings. Two minor style findings and one informational finding are non-blocking. |
| 5 | Clean branch | PASS | Source role worktree and materialized merge were clean before writing this record. The deploy commit adds this record only; final cleanliness is verified before push. |
| 6 | Merge state and build/vet | PASS | The clean merge below passed Bazel build/nogo and `make vet`. No branch rebase or conflict resolution performed. |
| 7 | Single feature theme | PASS | One reviewed commit changes one Linux test-support file, +31/-0. No production, API, configuration, dependency, resource-budget, or independent feature changes. |

`docs/PROJECT_MANIFEST.md` and a matching work-package file are absent. The
release-gate criteria fragment, TESTING.md, repository conventions, investigation
request and reviewer findings supply the checklist; no missing file was invented.

merge_state: clean (origin/main@a85d80d73b5efcda2a5c1ce89b60d2f899af78cd)
gate_base_kind: current-tip
gate_base: a85d80d73b5efcda2a5c1ce89b60d2f899af78cd
gated_merge: bcbd242a9e0dd2c1b29decdc742455bfda584334
gated_tree: 28c0e91e7613ba69ef27b89d7f72f41e5678aa98

The base was pinned when evaluation began. At publication preparation, fetched
origin/main is `b3d2cdf3172d325a5556c2321df30e642334d527`; `git merge-tree`
also reports a clean merge with the reviewed source. The suite evidence belongs
to the pinned merge above, not to that newer main. Merge-time validation belongs
to MPR and the repository's merge queue.

test_counts: PASS=116418 FAIL=1 SKIP=373
diff_tests_executed: none (no diff-owned tests)
test_log_dir: /var/tmp/deploy-ga-81zad89.dqNMn8lE/c3-run/evidence/remote/logs
test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
waiver_ref: none (gascity has no waiver path)
ci_lane_run: n/a (no CI-config change in this diff)
owned_topup: none needed; computed diff-owned test list is empty
test_exec_bep: /var/tmp/deploy-ga-81zad89.dqNMn8lE/c3-run/evidence/remote/{unit,acceptance,integration-packages,integration-full}.bep.json

The suite started through `gate-criterion3.sh` after the fast lane passed, as a
detached systemd service. After the attributed unit failure, only the three
never-run phases and native floor continued, on the same clean merge. The unit
phase was not repeated. Every phase forced `--nocache_test_results` and
`--noremote_local_fallback`, with verbose retained test logs.

| Phase / command | Remote results | Top-level PASS / FAIL / SKIP | Farm test seconds |
| --- | --- | --- | --- |
| Unit: `make check` (`bazel test //...`) | 371/371 | 68993 / 1 / 247 | 1949.675 |
| Acceptance: `bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` | 24/24 | 410 / 0 / 13 | 993.853 |
| Integration packages: `bazel test --config=integration //test:integration_packages` | 81/81 | 46805 / 0 / 85 | 1904.322 |
| Full integration: `bazel test --config=integration --test_arg=-test.v //test/integration:integration_test` | 14/14 | 189 / 0 / 14 | 2680.347 |
| Required native floor | 16 completed steps | 21 / 0 / 14 | n/a |
| Total | 490/490 remote phase results | 116418 / 1 / 373 | 7528.197 |

All 490 remote results have strategy `remote`, neither cached flag, attempt 1,
a start inside the phase window, and a terminal verdict. No transport failures
or mixed/local suite fallback occurred. Unit BEP errors record the one failed
test, its failed summary, and unsuccessful build completion; the other three
phase predicates have no errors. The original `execution.json` is retained
unchanged with `exit_code=1` and `criterion3_valid=false`; this checklist's PASS
uses explicit attribution, never a rewritten machine verdict.

admission: remote, floor=25 GiB, free=75.84215927124023 GiB, cap=2, waited=0.001 s
rbe_state: rc_marker_present=true; kill_switch_absent=true; certificate expires 2027-01-09T20:13:22Z
worker_env_tree: sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0
worker_env_origin_main: sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0
farm_test_seconds: 7528.197

The isolation wrapper printed PASS-THROUGH for the temporary merge tree, which
was not on its isolate list. Remote tests used Bazel's action environment. Native
floor commands explicitly scrubbed GC_, BD_, BEADS_, and DOLT_ selectors, retaining
only the test-owned bd pins and setting non-interactive git configuration.

floor_topup: complete=true; all 16 steps exit 0; record at
`/var/tmp/deploy-ga-81zad89.dqNMn8lE/c3-run/evidence/remote/floor-topup.json`.
It selected every environment-absence skip by package, using parent test names
for skipped subtests. It also ran the beadstest pinned-module check,
`make test-fsys-darwin-compile`, and both push-gate-lock and local-concurrency
runner self-tests. Native execution is this required supplemental floor, not a
stitched replacement for any remote suite phase.

Native floor load evidence (ordinary bounded soft wait):
load_threshold: 15
load_waited_seconds: 1801
load_wait_timed_out: 1
run_start_load: 113.65
run_max_load: 113.62
run_mean_load: 100.94
run_readings: 12
wait_first_load: 76.04
wait_max_load: 113.65
wait_mean_load: 89.95
wait_readings: 61
read_errors: 0

The remote suite has no host-load wait under the remote admission protocol.
The native floor proceeded after its bounded wait expired; all steps passed.

skip_inventory: 398 events including subtests (100 environment-absence,
298 other); 260 unique source sites, all UNCHANGED with zero shared hunks in
their files. Every event's reason, site and explanation is retained in
`/var/tmp/deploy-ga-81zad89.dqNMn8lE/not-owned-skip-evidence.json`;
`skip-sites-evidence.txt` is the raw ownership-tool output. This differs from
the 373 top-level SKIPs because subtests are included in the inventory.
None occurs in the changed proctable package. Optional tools, opt-in live tiers,
platform/provider-specific cases and historical skips cannot execute this
package-local test helper. Three ambiguous `conformance.go` references were
resolved by checking the containerhost import/call and the exact fenced-input
call sites in `internal/runtime/runtimetest/conformance.go`.

The ownership tool reports four shared hunks: standard-library imports
`path/filepath` and `time`, the call in `spawnReparentedChild`, and new helper
`waitForExecedSleep`. They change no test body or skip condition. The helpers
contain no skip and are used by these three tests, all verified by name in the
retained independent full-suite output:

- `TestBareForkedSupervisorIsSelectedAsOrphanKillTarget`: PASS.
- `TestScrubbedForkedSupervisorIsNotSelected`: PASS.
- `TestSetsidDoesNotPreventOrphanSelection`: PASS.

failure_attribution: TestRunSetupCommandActivityStreamingSurvivesIdleWindow -> ga-886gn1 | clause 3(a) MECHANISM

The tracker was opened and names this exact cancellation-despite-progress
condition; it predates this run (created 2026-07-25). This sighting was appended
as verified comment `84859fa3-974f-5302-8bc8-68a445d98184`. Clause 1:
`internal/runtime/tmux/startup_test.go:3287` is UNCHANGED, its test body untouched,
shared_hunks_in_file=0. Clause 3(a): the tmux test binary's 56 main-module import
variants include proctable's regular package, but not its test variant; the
changed proctable `_test.go` cannot execute in that binary. Clause 4: different
packages, no overlap. No production input, census, target, new test file or
process-count increase. This is this gate's first hit; no unit retry. The raw
failure and original machine verdict remain present.

Acceptance evidence from the earlier investigation was read and recounted, not
re-executed: base PID namespace runs had 292 PASS/8 FAIL and 957 PASS/43 FAIL;
interleaved fixed namespace run had 1000 PASS/0 FAIL; base host run had 300
PASS/0 FAIL; fixed package namespace run had 103 PASS/0 FAIL. Logs remain under
`/var/tmp/ga-lt0wedw-evidence.jlr2Ko/cases`. This supports the reproduction but
does not replace the independent suite above. Idle remote base runs did not
reproduce the race, so remote green alone does not establish that it is closed.

Landing guard ga-tpbk32d and tracker ga-x5z33aq remain open and block ga-uiw0pcz.
A deploy PASS or closed deploy bead does not resolve them. Mayor/MPR must verify
actual landing on main, including the new SHA after a squash, before resolving
the guard and requesting the named fresh PR 7000 gate.

Raw evidence, BEPs, logs, scripts, attribution, source ownership and service
results remain under `/var/tmp/deploy-ga-81zad89.dqNMn8lE` with `.gc-no-reap`.
