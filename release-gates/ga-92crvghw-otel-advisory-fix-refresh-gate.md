**Verdict:** **PASS**

# OpenTelemetry advisory fix: refreshed deploy

- bead: ga-92crvghw; original build: ga-b41drsoj; fresh review: ga-33ii5wek.
- reviewed_commit: cc21a0654d55e9ac1d97998f91c9f6cea2ff6f36
- gate_base: 33a7d167c8b7ffcf1fe75bb81c675028130b4c10
- tested_merge: f9cbc5bbfd37dd7b2b899da9dab2a8dd0e21bdb5
- tested_tree: b2325e322ec8c73d9c75e64da85fa58ea69fdd06
- deploy_mode: remote; push_remote: fork; isolated branch: deploy/ga-92crvghw-gate.
- evidence_root: /var/tmp/gc-ga-92crvghw-gate.X1JqMP (relative log paths below are under this retained directory).
- criteria_source: release-gate-criteria.md.tmpl and TESTING.md; docs/PROJECT_MANIFEST.md is absent.

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS | PASS | Fresh reviewer verdict names the exact reviewed commit; no reviewer-added commit. Independently rederived refresh patch-id equals 9fa26288024def95012bf5df62ed3e83980767c8; review-refresh-proof.json. |
| 2 | Acceptance | PASS | All affected OTel module floors are tagged v1.47.0; 11 changed versions are tagged and all 22 new sum rows match the live checksum DB. Module-name set remains 523 (524 with main). x/crypto remains v0.58.0. Fresh CI-pinned scanner analyzes 246 root packages and reports zero reachable vulnerabilities. Both owned tests pass by name. |
| 3 | Tests and required lanes | PASS | Fresh complete runner, skip comparisons, fast lane and six CI-job accounts below; two Windows jobs deferred to native PR CI. |
| 4 | Unresolved HIGH findings | PASS | Fresh review reports no blocker, major or minor finding; HIGH count 0. |
| 5 | Clean branch | PASS | Reviewed source clean; final deliverable adds only this committed gate record. Publishing checks cleanliness after the commit. |
| 6 | Merge state and build/vet | PASS | Clean merged tree built with bazel build --config=remote-exec --noremote_local_fallback //..., rc=0. Actual build strategy LOCAL because the initial scratch lacked generated farm config; full nogo later passed remotely. No remote C6 execution claim. |
| 7 | Single theme | PASS | Two commits fix the OTel advisories, migrate logger-provider calls, add the floor test, and synchronize BUILD files; 11 source paths. Ancestry guard rc=0 accepting this bead and ga-b41drsoj; no stack or denied paths. |

merge_state: clean (origin/main@33a7d167c8b7)
gate_base_kind: current-tip
merge_validation: merge authority must check current mergeability and PR CI; this record pins the clean merged tree above, not a future main tip.

test_cmd: gate-criterion3.sh --repo /var/tmp/gc-merge-validate.6glbs9 --base 33a7d167c8b7ffcf1fe75bb81c675028130b4c10 --bead-json <evidence_root>/bead-suite.json --owned <evidence_root>/owned.tsv --run-dir <evidence_root>/criterion3/evidence; detached, through isolated-test-run.sh, remote enabled, cap 2/floor 25 GiB.
test_cmd_scope: full-suite
test_counts: 159266 PASS, 0 FAIL, 618 SKIP (column-zero results across 570 logs; subtests excluded).
diff_tests_executed: TestInit_WiresResourceWithInstanceID PASS; TestGoModOTelModulesCarryAdvisoryFixes PASS; gate-test-evidence.py verified both by name.
test_log_dir: /var/tmp/gc-ga-92crvghw-gate.X1JqMP/criterion3/evidence/remote/logs
failure_attribution: none; waiver_ref: none (no gascity waiver path).
test_exec_mode: remote-exec; fallback_reason: none; criterion3_valid: true; farm_test_seconds: 14031.254.
test_exec_bep: unit 426, acceptance 25, integration-packages 87, integration-full 15, integration-latency 1; all 554 remote, attempt 1, start in phase window, terminal verdicts, zero remote/local cache credits or other strategy. Per-phase BEP and evidence JSON retained under criterion3/evidence/remote/.
rbe_state: rc marker present, kill switch absent, certificate valid through 2027-01-09, worker environment hashes match pinned main; criterion3/evidence/rbe-state.json.
admission: remote cap 2, floor 25 GiB, free 54.528 GiB, wait 0.001s.
delta_gate: shadow, full; reason not-gate-pr; no narrowing applied.
heavy_mode: none (classifier verified); coverage_check: n/a (ordinary complete runner).
owned_topup: none needed; floor_topup: 16/16 steps rc=0 including beadstest, Darwin compile, runner self-tests and every environment-absence SKIP package.
latency_leg: both named latency tests PASS under a clear probe on attempt 1; window 7 readings, max load1 45.21, threshold 50, zero over-threshold readings; latency-leg.json retained.
GATE_ICU74 verdict=OK provenance=recorded (all three checked sonames/hashes in icu74.txt).
LOAD_GATE_SUMMARY threshold=15 waited_seconds=1801 wait_timed_out=1 run_start_load=22.61 run_max_load=23.65 run_mean_load=22.79 run_readings=8 wait_first_load=22.07 wait_max_load=55.77 wait_mean_load=33.99 wait_readings=61 read_errors=0

skip_inventory: 604 farm and 72 native rows including subtests (676); distinct population from the top-level tally. All 320 source sites UNCHANGED, no owned skips and no unknown import roots. SHARED-HUNKS in internal/beads, internal/telemetry and scripts were named by tool/import-closure evidence. Because go.mod/go.sum changed, every skipped package was conservatively compared at the pinned base, not only the helper importers.
skip_justification: all 54 remote and six native comparison groups rc=0; same-command base skips have the same reasons (environment/capability, subprocess helpers, platform-specific cases and explicit upstream skips). No edited test body or skip site is excused. See skip-audit-summary.json, all-skip-site-proofs.json, shared-hunk-reach.json, skip-baseline-evidence and native-baseline-evidence.
COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestIsGoTestExecutableRecognizesThisBinaryWhateverItIsCalled (base 01b2d44d-9072-4a7e-81c9-5c3aaf241742 and 1067308542, head 85b13b9b-ce50-4ded-85e7-e8fb77d8b2e6 and 1934407769)
COMPARE-NOTE the remote worker's account differs between the runs in 1 SKIP(s), read as the same reason because the account is the only difference (an account is rbe-, a letter and two digits, standing alone; the logs are not changed): TestInfraScopedManagedDoltPIDIdentityUnderRealSystemd (base rbe-a10, head rbe-a00)
COMPARE-NOTE a generated path name differs between the runs in 1 SKIP(s), read as the same reason because it is the only difference and the same file and line were printed (a generated name is the digits Go appends to a temp directory's name, the process id in the name of gascity's temp root gct<pid>-<digits>, or a UUID right after a /work/ directory, which is how a Bazel remote worker names its work directory; the logs are not changed): TestProxyProcessReapRegistryMatchesAnEquivalentSpelling (base 1912763476, head 3880303270)

policy_lane: make lint with full nogo remote, bazel test //:go_format_test, and make check-release-dist-ignore check-routed-test-rows check-split-topology-rows check-residency-boundary; rc=0; fast.stdout.txt and fast.stderr.txt. Each fast lane used its own fresh view and gate-base pin before the suite.
drift_lane: make bazel-sync then git diff --exit-code; rc=0, separate fresh view; CI workflow has this one regeneration/drift step.
ci_lane_run: n/a (no CI-config change).
ci_required_closure: ci_required_triggers.py <merged-tree> 33a7d167c8b7ffcf1fe75bb81c675028130b4c10 cc21a0654d55e9ac1d97998f91c9f6cea2ff6f36 fork; workflow blob fa2c1f1b1466278b2eed8f4288d5a4f8eb3cc612; 6 LEAF jobs; shared filter MATCH, suite_mode full; ci-required/closure-v2.stdout.
ci_required_accounting:
runner-policy | LOCAL-PASS | EVENT_NAME=pull_request PR_AUTHOR=quad341 FORCE_BLACKSMITH= GITHUB_OUTPUT=<file> python3 .github/workflows/scripts/runner_policy.py; rc=0; ci-required/runner-policy.out
changes | LOCAL-PASS | ci_required_triggers.py on the merged tree after the suite; rc=0; ci-required/closure-v2.stdout
credential-provider-windows | DEFERRED-TO-PR-CI reason=non-linux-runner | own Windows reach: go.mod, go.sum; unit/integration GOOS=windows GOARCH=amd64 go vet ./internal/credentialprovider both rc=0; ci-required/windows-vet-output.stdout
windows-lane | LOCAL-PASS | exact CI commands in ci-required/windows-lane.sh (Windows allowlist build/test-compile, nogo cross-compile, full Darwin build/test-compile); rc=0; ci-required/windows-lane-output.stdout
windows-lane-test | DEFERRED-TO-PR-CI reason=non-linux-runner | own Windows closure for .github/windows-lane/test.txt under unit/integration tags reaches go.mod, go.sum; both allowlist vets rc=0; ci-required/native-windows-reach.json and windows-vet-output.stdout
pack-gate | LOCAL-PASS | scripts/update-bundled-gastown-pack --check and GC_TEST_GASCITY_PACKS_REGISTRY=main make test-pack-registry-live, exact CI retry loops; rc=0; ci-required/pack-gate-output.stdout
windows_reach: credential-provider-windows -> go.mod, go.sum; windows-lane-test own allowlist closure -> go.mod, go.sum (both tag sets computed independently).
deferred_to_pr_ci: credential-provider-windows (non-linux-runner), windows-lane-test (non-linux-runner)
not_covered: none

security_evidence: govulncheck -show verbose ./..., scanner v1.7.0 built Go1.27.4, actual rc=0/246 analyzed root packages/zero affected vulnerabilities; security-ac.json and govulncheck.txt. Only informational x/crypto advisory remains; no pseudo-version pinned. Native guard passed: modules=524 aws=25 azure=9 dolthub=13 googleapi=1 binary_bytes=222649744 (Bazel binary/cap); no new module names.
security_load: LOAD_GATE_SUMMARY threshold=15 waited_seconds=1171 wait_timed_out=0 run_start_load=14.79 run_max_load=NA run_mean_load=NA run_readings=0 wait_first_load=66.86 wait_max_load=66.86 wait_mean_load=29.40 wait_readings=40 read_errors=0
evidence_disclosures: security service initially failed only its post-scan wording parser; retained successful scanner report was validated without rerunning it. Original continuation-audits then failed only its closure parser on the summary '6 LEAF jobs', before any CI lane started; corrected CI-only continuation runs each remaining lane once. Original failed records and correction hashes retained; no failed test was retried or attributed.
