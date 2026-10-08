# bd schema-probe temporary storage release gate (ga-bn169x)

**Verdict:** **PASS**

Reviewed source: `bfceb4029d74b1b81b4bc04acd4abf13f1a7a568` (build ga-01i5ul; PASS review ga-0kep20).
Pinned main: `c9ec454417654442ab901de4b405df6c6c844132`; canonical normal-hook merge: `3097d78fc68708ef8c71aa4d56d6fc35286745ee`; tree: `7098d39d563c4656d6e3c6912ff901a9d00f0e2c`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Full review read and recorded source resolved against git. No carryover or branch-tip substitution. |
| 2 | Acceptance criteria met | PASS | Linux /dev/shm preference with default-temp fallback, all-refusals diagnostics and nested db/probe.db cleanup each have actual full-unit PASS. Original polluted-HOME regression also actually PASSes with pinned real bd. Probe timeout remains 60 seconds. |
| 3 | Tests and required coverage | PASS | Fresh whole-scope make check and four required Bazel lanes; 369 first-attempt uncached jobs; 88360 PASS, 0 FAIL, 281 individually reviewed SKIP lines. Every owned root/descendant PASSes. Exact configured target sets, retained raw logs/XML/BEP and hashes verified. All18FAST targets, complete lint/nogo/policy and BUILD/generated drift pass before suite. |
| 4 | No unresolved HIGH findings | PASS | Full style/security/spec review reports no blocking or high finding. |
| 5 | Clean branch | PASS | Canonical and assigned trees clean before gate record. Fresh views isolate regeneration; only this record is added to the isolated branch. Normal commit/push and final cleanliness verified before publication. |
| 6 | Clean divergence | PASS | merge-tree rc0; normal-hook materialized tree equals prediction. Independent fresh-view go build ./... and go vet ./... exit0. No rebase/force push. |
| 7 | Single feature theme | PASS | One two-file schema-probe storage/cleanup fix. Absolute-library ancestry scope check accepts this deploy and ga-01i5ul; no stack, unrelated commits or forbidden internal paths. |

`test_cmd: bash /var/tmp/ga-bn169x-current-suite-driver.sh`; `test_cmd_scope: full-suite`.
Actual command chain: official detached service → load15/maxwait1800 → isolation wrapper → private readonly ICU runtime → make check (whole //...) and documented Bazel acceptance main+solo, full //test:integration_packages, prescribed //test/integration:integration_test under --config=integration-smoke.
All CI path filters were checked against the exact two-file diff: no additional ci.yml mail/beads/packs/credential/shared lane is selected. Current bazel.yml runs topology/proxied-native acceptance in the acceptance lane. No old October5 Go command or evidence was reused. The smoke filter is the enumerated CI configuration, not a selected test-name substitute.
Common flags: --config=ci --config=fork-cache --nocache_test_results --jobs=4 --remote_download_outputs=all --rewind_lost_inputs; actual tag sets default/integration/acceptance_a. Runtime provides exact pinned bd1.3.1 through GC_ACCEPTANCE_BD_BIN before the first full run, real Podman socket and Dolt2.2.0; Ryuk is disabled only with unreaped opt-out and an enabled testcontainer sweep.
`diff_tests_executed: TestBdLatestSchemaVersionFallsBackPastAnUnusableRoot PASS; TestBdLatestSchemaVersionLeavesNothingInItsTempRoot PASS; TestBdSchemaProbeRootsPreferRAMBackedStorageOnLinux PASS; TestMkdirTempUnderNamesEveryRootThatRefused PASS`.
`heavy_mode: none`; `ci_lane_run: n/a (no CI-config diff)`; `failure_attribution: none`; `policy_attribution: none`; `waiver_ref: none` (gascity has no waiver path).
`test_log_dir: /var/tmp/gc-heavy-gate/runs/ga-bn169x-current.c3/logs`. gate-test-evidence.py re-read retained per-job logs.

Counts include subtests and repeated Bazel jobs; parent counts exclude '/' names. Non-Go policy/dashboard target results are verified separately.

| Lane | PASS | FAIL | SKIP | Parent PASS/FAIL/SKIP |
|---|---:|---:|---:|---|
| unit | 53080 | 0 | 215 | 29090/0/164 |
| acceptance | 402 | 0 | 11 | 114/0/10 |
| integration-packages | 34862 | 0 | 54 | 18438/0/43 |
| integration-smoke | 16 | 0 | 1 | 13/0/1 |
| Total | 88360 | 0 | 281 | 47655/0/218 |

Every SKIP is recorded individually in `/var/tmp/gc-heavy-gate/runs/ga-bn169x-current.c3/full-skip-site-proof.json`: exact site/function UNCHANGED, full per-file/shared-hunk explanation, printed reason, source guard and causal review. Complete Go test-binary import closures for default, integration and acceptance_a acknowledge helper reach into acceptance and integration; a Go graph is never claimed to rule out subprocess execution. The changed functions select the probe's temporary root and DB layout; unchanged skip guards depend on actual platform/opt-in/executable/runfile/fixed fixture inputs, reviewed separately. Unit process omissions are matched by full name and package to actual fresh integration PASS. The touched test file's only shared hunk is the standard slices import used in the new equality test; it changes no existing helper, TestMain/state or skip condition. All four new roots and the original isolation regression run with real pinned bd where needed.

`policy_lane: make lint/nogo, complete make test-ci-policy, required shell/hook guards and policy targets PASS`.
`drift_lane: make bazel-sync then git diff --exit-code and empty tracked/untracked status; all required generated targets PASS`.
Each fast lane uses a separate fresh-tree-run view and gate-base.sh's exact recorded base; no installer/generator writes the canonical suite input.

OpenAPI form: workflow-equivalent per ga-3rtlng; standard form not run (ga-twhw87 open).
E1 exact target invocation/environment/exit0 retained in fast-openapi-x2; same comparator runs in full unit.
E2 real pinned base `c9ec454417654442ab901de4b405df6c6c844132` written under /var/tmp/gc-heavy-gate/runs, outside client /tmp, and exported via GC_OPENAPI_BREAKING_BASE_SPEC.
E3 input bytes equal git show of that commit; sha256 04d6dfe60530cb1d81b79080a8baab3f7f5887a2f66c5ccdfeb94c4dba8d1912.
E4 retained raw log names the real input, TestCommittedSpecAgainstBase and TestGateAgainstOasdiff with six fixtures PASS; raw artifact hashes verified. No unset-input self-comparison is counted.
E5 both live preconditions recorded: ga-twhw87 OPEN and one canonical rctx.symlink(spec, "openapi.json") line.
E6 teeth pointer: ga-2egszb RED/GREEN/breaking-probe and /tmp/bt refutation, /var/tmp/gc-heavy-gate/runs/ga-8tafjg.openapi-base-repro. First tracker close or rule fix sunsets this temporary form.

Load evidence:
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 run_start_load=13.97 run_max_load=21.26 run_mean_load=17.11 run_readings=70 wait_first_load=13.97 wait_max_load=13.97 wait_mean_load=13.97 wait_readings=1 read_errors=0

Publication conflict/scope check: current main `4ae4bb56580d59bbc1a87a16e306061653c403bd` merged cleanly with the unchanged reviewed source, resulting tree `b84696fa1b6a0114d4c03a914b7486c1f7215f43`; absolute-library ancestry scope exit0 with this bead and ga-01i5ul only, no stack. This is a supplemental current-main check, not a claim that the full suite ran on that newer main; suite evidence remains pinned to `c9ec454417654442ab901de4b405df6c6c844132`. Retained proof: /var/tmp/gc-heavy-gate/runs/ga-bn169x-current.c3/PUBLICATION_CONFLICT_SCOPE_VERIFIED.json.

Earlier condition ga-c5k69f is CLOSED after its fixture correction e55798f4c4019d11d3527526b742be106abdc0f1 landed; actual ancestor of this pinned main verified. This is a fresh gate, not an attribution of the old failure.
The origin push-selection dry run invoked normal hooks on role/main and failed to load ICU74. It pushed nothing, is not candidate C3 evidence or an attributed PASS, and chose fork by exit status. Its original log remains /var/tmp/ga-bn169x-current-origin-push-dry-run.txt. The candidate gate and normal publication use the private readonly runtime configured beforehand; no host-library edits, store migration or hook bypass.
An overlapping push-setup context save briefly removed local merge identifiers. They were restored before suite launch from completed C6 output, with head/tree/base checked against git and gate-base.sh; canonical input never changed. Proof: /var/tmp/ga-bn169x-current-context-recovery-verified.json.
The isolation wrapper reports PASS-THROUGH/NOT ISOLATED for this rig; Bazel sandboxes and repo environment scrubs supply test isolation. Private bubblewrap adds runtime libraries without a PID namespace/subreaper; local cache misses are limited to four jobs.

Evidence: /var/tmp/gc-heavy-gate/runs/ga-bn169x-current.c3/FULL_SUITE_VERIFIED.json; per-lane target manifests/raw log/XML/BEP/hashes; full skip/source/import proofs; sibling fast-rest/COMPLETE_FAST_VERIFIED.json and fast-openapi-x2 E1-E6; C6 build/vet and immutable source records. All per-job logs retained.
Normal commit/push, PR head/body, BASE-repo exact-head clearance and peek-verified mayor merge-request are checked separately before success notes. MPR owns merging; the task closes blocked until merge. Public issue: gastownhall/gascity#7105.
