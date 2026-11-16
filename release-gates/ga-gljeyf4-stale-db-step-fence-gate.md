# Stale-db step note fence release gate

**Verdict:** **PASS**

Deploy bead: ga-gljeyf4. Build: ga-1l0pz0t. Review: ga-w8afh5o (PASS).
Reviewed source: `d6d84b5e9fa6493451539e19be811f2b61d188c9`, resolved as a commit.
Evaluation completed 2026-10-17. Mode: remote, gastownhall/gascity.

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS | PASS | ga-w8afh5o verdict pass names the exact source; no carryover or reviewer-added tests. |
| 2 | Acceptance criteria | PASS | Independently TOML-decoded first-fence extraction: base Bash exits 2; reviewed Bash exits 0. Owned dry-run note test and all 16 stale-db tests PASS by name in both unit and integration-package output. Reviewer also verified note byte identity and a json-to-jsonx mutation; those are input evidence, not this gate's runs. |
| 3 | Full suite | PASS | Four fresh remote phases plus required native floor; 116387 PASS, 0 FAIL, 373 SKIP top-level results per job. Strict execution.json exit_code=0 and criterion3_valid=true. |
| 3a | Failure attribution | PASS | No failed tests or targets; no attribution or waiver. |
| 3b | Format, policy/lint, generated drift | PASS | Separate fresh views ran pinned format/lint, shell guards and nine policy/format targets. make bazel-sync followed by git diff --exit-code and clean untracked status passed. Build/nogo and make vet passed first. |
| 3c | CI configuration | PASS | No CI configuration changes. |
| 3d | Heavy package classification | PASS | heavy_mode=none; no selected heavy package. |
| 4 | No unresolved high-severity findings | PASS | Reviewer style/security findings: none; no blockers. |
| 5 | Clean branch | PASS | Materialized merge and assigned worktree clean before this record. Only this evidence file is added to the isolated deploy branch; cleanliness checked after commit. |
| 6 | Merge state, build/vet | PASS | Clean pinned merge built and vetted, with active pre-commit hook. |
| 7 | Single feature theme | PASS | One reviewed commit changes only the stale-db formula note construction and its tests, two files +27/-2. |

The repository has no docs/PROJECT_MANIFEST.md or matching work-package file.
The release criteria fragment, TESTING.md, investigation request and review
supply this checklist. No missing document is claimed as evidence.

merge_state: clean (origin/main@45c804d59ecc8844eacdeb7b1a3ff263dda7e689)
gate_base_kind: current-tip
gate_base: 45c804d59ecc8844eacdeb7b1a3ff263dda7e689
gated_merge: e5f1af1f78adde6576d6583b5a17b3bdcaaa72c6
gated_tree: fe3a0c80c9e4b002a6d3551ce002374787b4e387

The suite used the base pinned at evaluation start. Publication preparation
also merge-trees clean against newer origin/main b3d2cdf3172d325a5556c2321df30e642334d527;
that newer tree was not the suite's input. MPR owns merge-time validation.

test_counts: PASS=116387 FAIL=0 SKIP=373
diff_tests_executed:
  TestStaleDBFormulaDryRunFailureAppendsScanJSON PASS (integration-packages-130aa78402132a9cb052.log, unit-98fce3c243b1e6971368.log)
test_log_dir: /var/tmp/deploy-ga-gljeyf4.1cfdi3kg/c3-run/evidence/remote/logs

test_cmd_scope: full-suite
test_exec_mode: remote-exec
fallback_reason: none
waiver_ref: none
ci_lane_run: n/a (no CI-config change in this diff)
heavy_mode: none
owned_topup: none required; owned test present in both full-suite lanes
floor_topup: complete, 16/16 steps exit 0

All four phases forced --nocache_test_results and --noremote_local_fallback.
Each of 490 result attempts is remote, uncached locally/remotely, attempt 1,
started inside its phase window and has a terminal verdict. All four BEP
predicates have errors=[] and transport_failure=false. The native floor is
supplemental coverage, never a local replacement for any remote suite phase.

| Phase / command | Remote results | Top-level PASS / FAIL / SKIP | Farm test seconds |
| --- | --- | --- | --- |
| unit: `make check (bazel test //...)` | 371/371 | 68978 / 0 / 247 | 2375.346 |
| acceptance: `bazel test --config=acceptance //test/acceptance:acceptance_test //test/acceptance:acceptance_solo_tests` | 24/24 | 410 / 0 / 13 | 1037.516 |
| integration-packages: `bazel test --config=integration //test:integration_packages` | 81/81 | 46789 / 0 / 85 | 1949.803 |
| integration-full: `bazel test --config=integration --test_arg=-test.v //test/integration:integration_test` | 14/14 | 189 / 0 / 14 | 2381.381 |

farm_test_seconds: 7744.046
admission: {"cap": 2, "class": "remote", "floor_gib": 25.0, "free_gib": 77.64160537719727, "waited_seconds": 0.001}
rbe_state: {"cert_notAfter": "notAfter=Jan  9 20:13:22 2027 GMT", "kill_switch_absent": true, "rc_marker_present": true, "worker_env_origin_main": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0", "worker_env_tree": "sha256:0c6c05c2196c246dba37e106a3d028e2f47f2e3d7eae0e41c2fe2bd9b2e4f6f0"}

The floor ran all environment-absence SKIPs by package (parent names for
subtests), the beadstest pinned-module check, make test-fsys-darwin-compile,
push-gate-lock self-test, and local-concurrency self-test. Its 16 command
results and logs are in `/var/tmp/deploy-ga-gljeyf4.1cfdi3kg/c3-run/evidence/remote/floor-topup.json`.

Native floor load record, copied from retained output:
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1800 wait_timed_out=1 run_start_load=82.23 run_max_load=88.60 run_mean_load=79.91 run_readings=10 wait_first_load=79.95 wait_max_load=83.96 wait_mean_load=65.84 wait_readings=61 read_errors=0

Outer remote run host observation (not a remote admission wait):
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=19.24 run_max_load=87.86 run_mean_load=62.53 run_readings=96 wait_first_load=19.24 wait_max_load=19.24 wait_mean_load=19.24 wait_readings=1 read_errors=0

The floor's ordinary bounded wait expired and then proceeded; no native step
failed. The remote phases use remote admission without a host-load wait.
The isolation wrapper printed PASS-THROUGH for the temporary merge tree.
Bazel remote action environments supplied the suite; the native floor scrubbed
GC_, BD_, BEADS_ and DOLT_ selectors except test-owned bd pins, and pinned
non-interactive git configuration.

skip_inventory: 398 events including subtests, 260 unique sites;
classes={"environment-absence": 100, "other": 298}. Every site is
UNCHANGED with shared_hunks_in_file=0. Full per-test reasons, source sites,
mechanism explanations and tool output are retained in
`/var/tmp/deploy-ga-gljeyf4.1cfdi3kg/not-owned-skip-evidence.json` and `skip-sites-evidence.txt`.
The 373 top-level SKIPs count differently because the inventory includes
subtests. No test in examples/bd/dolt skipped. Three ambiguous conformance.go
references were resolved against the imported runtimetest helper and exact
fenced-input call lines. Optional environment/provider/deferred skips cannot
be controlled by the changed note-format fence or test extraction helper.

The ownership tool finds one owned test:
TestStaleDBFormulaDryRunFailureAppendsScanJSON, PASS in both unit and integration.
Its single SHARED-HUNKS helper, extractFirstBashFence, rejects embedded fences
and has no skip path. All 16 tests in stale_db_formula_test.go passed by name,
including the 14 that render through that helper. By-name output is retained
in final-helper-callers.txt; these supplemental names do not widen the actual
one-test ownership list.

Independent acceptance probe: `/var/tmp/deploy-ga-gljeyf4.1cfdi3kg/fence-parse-proof.json`.
It TOML-decodes and extracts the first Bash fence, then runs bash -n: pinned
base exits 2 with an unterminated quote; reviewed source exits 0. The note's
fenced-JSON shape is pinned by the owned test. Reviewer red/mutation/byte
comparisons were read as input, not claimed as deployer executions.

The fix enables the stale-db maintenance step to reach its dry-run scan for
the first time after the parse defect. Existing probe, force-blocker,
identifier, error and max-orphan guards still determine apply versus
escalation. This operational consequence is carried to mayor in the handoff.
The PR must say Closes #7967 and credit @creenders for the TOML diagnosis.

Raw evidence, original BEPs, logs, preparation results, ownership and scripts
remain in `/var/tmp/deploy-ga-gljeyf4.1cfdi3kg` under .gc-no-reap. No evidence directory is removed.
