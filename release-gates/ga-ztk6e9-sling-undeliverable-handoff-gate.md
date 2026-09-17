**Verdict:** **PASS**

# Release gate: undeliverable sling hand-off warning (`ga-ztk6e9`)

- Reviewed source: `ad7fe8f245aa9363c3f396eb0b8a3dc2daba956a`
- Review bead: `ga-3ejr8r`
- Gate base: `origin/main@af8844ad697961e6f9d0503b5a001fbb9adfc0cb`
- Deploy mode: remote; push remote: `fork`
- `docs/PROJECT_MANIFEST.md` is not present in this worktree. The gate uses the deployer role's explicit seven criteria plus `engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | **PASS** | `ga-3ejr8r` is closed with reason `pass` and records reviewed commit `ad7fe8f245aa9363c3f396eb0b8a3dc2daba956a`; no review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | The assigned-parent/default-formula conflict now emits a distinct warning naming the blocking assignee and the undeliverable target state. The unassigned control still auto-burns the stale molecule and attaches normally. `TestDefaultFormulaFallbackOnAssignedBeadIsSilentlyUndeliverable`, `TestDefaultFormulaFallbackControlUnassignedBeadAttaches`, `TestOnFormulaExistingMoleculeErrors`, and `TestCheckNoMoleculeChildrenConvoyLookupDoesNotShadowV1Children` each passed twice in the full union, with 0 FAIL and 0 SKIP. |
| 3 | Tests pass | **PASS with two attributed raw failures** | The documented full local CI union scheduled all 40 jobs: **38 PASS / 2 raw FAIL / 0 omitted**, comprising **48,717 PASS / 2 raw FAIL / 227 SKIP** top-level executions. Both diff-owned tests ran twice and passed twice: **4 PASS / 0 FAIL / 0 SKIP**. The two raw failures stopped during unrelated fixture `gc init` before workflow or sling execution and satisfy criterion 3a below. `test_cmd_scope: full-suite`; `waiver_ref: none`; `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-3ejr8r` records no style, security, or specification finding requiring changes; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | Before writing this gate record, `git status --short` was empty at the exact reviewed SHA. `gofmt -l` on both changed Go files and `git diff --check origin/main...HEAD` produced no output. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree --messages origin/main ad7fe8f245aa9363c3f396eb0b8a3dc2daba956a` exited 0 against `origin/main@af8844ad697961e6f9d0503b5a001fbb9adfc0cb`, producing tree `a854381f375c2aafe438f77160eab684e4856530`; no bounded self-rebase was needed. The already-merged preflight found no PR carrying the reviewed SHA. |
| 7 | Single feature theme | **PASS** | The two-commit delta touches only `internal/sling/sling_core.go` and its `cmd/gc` regression test. Both changes address the same default-formula hand-off observability defect; there is no independent feature theme. |

## Criterion 3 evidence

`test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m GOFLAGS=-v isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`

- `test_cmd_scope: full-suite`
- `test_counts: 48,717 PASS / 2 raw FAIL / 227 SKIP` across 40 job logs
- `job_counts: 38 PASS / 2 raw FAIL / 0 omitted`
- `diff_tests_executed: TestDefaultFormulaFallbackOnAssignedBeadIsSilentlyUndeliverable PASS x2; TestDefaultFormulaFallbackControlUnassignedBeadAttaches PASS x2; 0 diff-owned FAIL; 0 diff-owned SKIP`
- `skip_justification: 227 suite-controlled pre-existing opt-in, live-provider, platform/privilege, persistence, and helper-process exclusions; none is diff-owned. There were 179 unique skipped top-level names across repeated shards.`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI configuration changed)`
- `policy_lane: make test-ci-policy — PASS`
- `go vet ./... — PASS`
- Full log: `/var/tmp/gc-deploy-ga-ztk6e9-full.hBsGiH.log`
- Job logs: `/var/tmp/gc-local-tests.8BqufF`
- Policy log: `/var/tmp/gc-deploy-ga-ztk6e9-policy.log`
- Vet log: `/var/tmp/gc-deploy-ga-ztk6e9-vet.log`
- Isolation tripwire count: 0

Rootless Podman 5.8.4 was active before the run at `unix:///run/user/1000/podman/podman.sock`; Ryuk was disabled. The cached `docker.io/dolthub/dolt-sql-server:1.32.4` image matches the default pinned by `github.com/testcontainers/testcontainers-go/modules/dolt@v0.43.0`.

## Criterion 3a attribution

Both failures map to the open condition tracker `ga-lejnse`, created 2026-09-13 before this run. The tracker already names both tests and this exact random-cursor shared-server migration-refusal signature. This run's sighting was appended and read back as comment `b035ad6d-84bb-5694-aa69-66a526d77a1e`.

- `TestAdoptPRFormulaCompileAndRun -> ga-lejnse | clause 1: test/integration/review_formula_test.go is not diff-owned | clause 2: predating open tracker inspected and updated | clause 3(a): setupReviewFormulaCity failed inside initCityWithManagedDoltRecovery when external bd refused 58 pending shared-server migrations (v8 -> v66), before startReviewWorkflow and before changed attachFormulaToBead could execute | clause 4: failing test package test/integration has no path overlap with internal/sling/sling_core.go or cmd/gc/cmd_sling_undeliverable_test.go`
- `TestAdoptPRFormulaRetriesTransientReviewerStep -> ga-lejnse | clause 1: test/integration/review_formula_test.go is not diff-owned | clause 2: predating open tracker inspected and updated | clause 3(a): setupReviewFormulaCity failed inside initCityWithManagedDoltRecovery when external bd refused 11 pending shared-server migrations (v55 -> v66), before startReviewWorkflow and before changed attachFormulaToBead could execute | clause 4: failing test package test/integration has no path overlap with internal/sling/sling_core.go or cmd/gc/cmd_sling_undeliverable_test.go`

No retry was used to turn either failure green. The first-run results remain preserved and attributed under the non-diff-owned gate-failure protocol.
