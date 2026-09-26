**Verdict:** **PASS**

# Release gate: harden molecule attachment lineage and stamp the issue variable

- Deploy bead: `ga-sgcfil`
- Build bead: `ga-korly3.1`
- Reviewed commit: `04e79bad681f5dd7541be4482a0b8676bfc09d0c`
- Base checked: `origin/main@9cfb391f6f379a626a34748aa7dbcaa2f67f5cf2`
- Deploy mode: `remote` (push remote: `fork`)
- Gate date: 2026-09-20

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Deploy bead `ga-sgcfil` records the reviewer's PASS for the exact resolved commit. The reviewer independently ran build, vet, targeted tests, the complete `internal/molecule` package, and direct-caller `internal/dispatch` tests, and found no security or specification blocker. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | `Attach` now distrusts an open root reached only through `molecule_id` unless a pre-existing blocking dependency structurally proves a prior pour; closed-root and `workflow_id` behavior remain intact. Every instantiated step receives `gc.var.issue = attachBeadID`. The three new regressions and both named must-not-regress cases pass. The change is confined to `internal/molecule`, with no formula/template edit. The deploy-point safeguard is live: `GC_REAPER_SESSION_PURGE_AGE = "876000h"`, and safety commit `56d8e6aa47` is an ancestor of the reviewed commit. |
| 3 | Tests pass | PASS | The documented full local runner, `make test-local-full-parallel`, ran all 40 jobs through `isolated-test-run.sh`: 34 jobs PASS / 6 jobs raw FAIL / 0 jobs skipped. The six raw failing test occurrences are attributed under 3a and are not diff-owned. `internal/molecule` reported `ok` in both the unit and integration package sweeps. A supplemental verbose whole-package run produced 175 PASS / 0 FAIL / 1 SKIP and resolved all three diff-owned tests as PASS by name. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures may be attributed | PASS | Three shared-Dolt schema-refusal failures map to pre-existing tracker `ga-lejnse`; the stale integration Beads-version assertion maps to `ga-rnwg5u`; and both doctor JSON-parse occurrences map to `ga-3t5hvu`. The former doctor tracker was closed when its fix was authored but before it landed; because the current candidate structurally cannot reach `internal/doctor` and has no path overlap, the protocol's landed-proof escape permits `ga-3t5hvu`, created during this discovering run. All sightings are recorded. Full four-clause evidence appears below. |
| 3b | Policy/lint lane | PASS | At the exact reviewed commit, `make lint-affected` completed with 0 issues; `make fmt-check-changed`, `make test-ci-policy`, `go vet ./...`, and `go build ./...` all passed. `make check-hooks` confirmed `.githooks` owns `core.hooksPath`. |
| 3c | CI-config diff needs its own lane | PASS | `ci_lane_run: n/a` — the candidate changes no workflow, matrix, timeout, required-check, Makefile, or test-runner configuration. |
| 4 | No high-severity review findings open | PASS | The exact-head review reports no unresolved HIGH finding and no security finding; it describes the change as a net hardening from a metadata-string trust decision to a structural dependency proof. |
| 5 | Final branch is clean | PASS | `git status --porcelain` was empty at the exact reviewed commit before this gate record was created. The gate record is the only deploy-branch addition. |
| 6 | Branch diverges cleanly from main | PASS | Remote preflight found no PR carrying the reviewed commit. The reviewed branch is based directly on current `origin/main@9cfb391f...`; `git merge-tree --write-tree origin/main 04e79bad...` exited 0 with tree `0a09aa89fd97db70bbae261ffe2d40f7f85ae74d`, and `git diff --check origin/main...04e79bad...` exited 0. No self-rebase was needed. |
| 7 | Single feature theme | PASS | Both commits and both changed files serve one `internal/molecule.Attach` integrity theme: distinguish legitimate prior attachment lineage from a stale live `molecule_id`, and stamp the issue variable that lets the existing downstream consistency guard evaluate the same attachment. `assert_deploy_ancestry_scope` passed after deliberately naming the build/deploy IDs and the two architect-confirmed source defects, `gm-qzxkf5` and `gm-5ckjy0`; no `.claude/**` path or unrelated theme is present. |

## Criterion 3 evidence

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true isolated-test-run.sh -- bash -lc 'make test-local-full-parallel'
test_cmd_scope: full-suite
runner_jobs: 34 PASS / 6 raw FAIL / 0 SKIP
raw_test_results: 6 FAIL / 0 explicit SKIP (the runner is non-verbose for passing tests)
diff_package_in_full_suite:
  github.com/gastownhall/gascity/internal/molecule ok (unit-core)
  github.com/gastownhall/gascity/internal/molecule ok (integration-packages-core-3-of-4)
full_suite_log: /var/tmp/ga-sgcfil-test-local-full-parallel.log
full_suite_job_logs: /var/tmp/gc-local-tests.K8J0Bq
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
```

The rootless Podman socket was healthy before the run, Ryuk was disabled for
this host's external reaper, and the required Dolt SQL Server images were
cached. All 40 runner jobs executed.

The mandatory full runner is non-verbose for ordinary passing package tests,
so the follow-up whole-package verbose run provides exact counts and names
without narrowing the criterion-3 command:

```text
supplemental_cmd: isolated-test-run.sh -- bash -lc 'go test -count=1 -v ./internal/molecule/...'
supplemental_counts: 175 PASS / 0 FAIL / 1 SKIP
supplemental_log: /var/tmp/ga-sgcfil-molecule-verbose.log
diff_tests_executed:
  TestAttachFallsBackToSelfWhenMoleculeIDPointsAtUnrelatedOpenMolecule PASS
  TestAttachHonorsOpenMoleculeIDRootWithPriorBlockingDep PASS
  TestAttachStampsFormulaVarIssue PASS
```

The one supplemental skip is the pre-existing
`TestBuildRecipeApplyPlanBugReportFlowV2`, whose optional tooling formula is
absent at `/home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.formula.toml`.
It is not diff-owned; every changed test executed and passed.

### Failure attribution

- `TestCustomTypesCheck_ServerBackedStoreIgnoresAmbientEndpoint` (two jobs) -> `ga-3t5hvu`.
  - Clause 1: `internal/doctor/checks_custom_types_test.go` is not diff-owned and has the identical blob `312e38ca...` at the base and candidate.
  - Clause 2: no open tracker remained after `ga-x5wacn` closed when its fix was authored on an unmerged branch. `ga-3t5hvu` was therefore created as the one open condition tracker during this discovering run, using the `gm-sf3238` escape below.
  - Clause 3(a), mechanism: `go list -deps ./internal/doctor` contains no `internal/molecule`; the failing test cannot execute either changed helper or the changed `Attach` path. The known failure comes from the test helper merging diagnostic stderr into JSON stdout.
  - Clause 4: `internal/doctor/**` has no path overlap with the two-file `internal/molecule` candidate. Clauses 1 and 4 are clear and clause 3 has a landed proof, satisfying the during-run tracker exception.
- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`, `TestAdoptPRFormulaCompileAndRun`, and `TestPersonalWorkFormulaCompileAndRun` -> `ga-lejnse`.
  - Clause 1: none of the three failing tests is diff-owned.
  - Clause 2: `ga-lejnse` predates this run, covers the exact shared-server pending-migration refusal, and now records all three sightings.
  - Clause 3(a), mechanism: each test failed during fixture `bd init` because the shared server refused schema promotion (`v24 -> v66`, `v54 -> v66`, and `v53 -> v66`), before formula compilation, attachment, or scenario assertions could run.
  - Clause 4: `cmd/gc/cmd_bd_test.go` and `test/integration/review_formula_test.go` do not overlap the candidate's `internal/molecule` paths.
- `TestPinnedIntegrationBeadsModuleVersion` -> `ga-rnwg5u`.
  - Clause 1: neither the integration assertion nor the dependency pin is diff-owned.
  - Clause 2: `ga-rnwg5u` predates this run, covers the exact `v1.3.0` versus `v1.3.0-rc.2` mismatch, and now records this sighting.
  - Clause 3(a), mechanism: the assertion reads the module version and compares it with an untouched hard-coded expectation; molecule attachment cannot affect either value.
  - Clause 4: the version assertion and dependency-pin paths do not overlap the candidate diff.

No inconclusive attribution path and no waiver were used. The candidate adds
three tests inside an already-tracked package but adds no resource-census
entry, test target, build lane, process, listener, sleep, or suite parallelism.

## Policy evidence

```text
LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make lint-affected: PASS (0 issues)
LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make fmt-check-changed: PASS
make test-ci-policy: PASS
go vet ./...: PASS
go build ./...: PASS
log: /var/tmp/ga-sgcfil-policy-lanes.log
```

## Acceptance and scope evidence

- `resolvedViaMoleculeIDOnly` narrows the structural check to the low-trust `molecule_id` fallback; a higher-precedence `workflow_id` remains authoritative.
- `hasBlockingDepTo` requires the same `blocks` edge every successful prior `Attach` creates and fails safe to self-root if the dependency read fails.
- An unrelated but still-open molecule target now self-roots at the attach bead.
- An open molecule root with a genuine prior blocking edge remains honored.
- The pre-existing open `workflow_id` and closed-root regressions remain green.
- Every instantiated step receives `gc.var.issue` with the attach target's own bead ID.
- The candidate is 193 insertions and 3 deletions across `internal/molecule/molecule.go` and `internal/molecule/attach_test.go` only.
