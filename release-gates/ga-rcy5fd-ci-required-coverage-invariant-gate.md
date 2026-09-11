# Release gate: CI-required coverage invariant

- Deploy bead: `ga-rcy5fd`
- Review bead: `ga-9sqq1b`
- Reviewed commit: `ad8bb5128cb5db3b229902a3e14369aae3a9c3c8`
- Current base: `origin/main@21eca31d1c18e57e3bf83a968d64fd80db589ba5`
- Isolated branch: `deploy/ga-rcy5fd-gate`
- Gate state: **HOLD** — local evidence is complete, but the CI-config change has not yet received its first real GitHub Actions run.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-9sqq1b` records `verdict: pass` for the exact reviewed commit. |
| 2 | Acceptance criteria met | PASS | The workflow now includes `preflight-unit-cover-noncmdgc` and `preflight-unit-cover-cmdgc` in both `ci-required.needs` and `allow_skipped`; `TestCIRequiredCoversEveryRealJob`, `TestCIRequiredSkipsPushOnlyCoverageJobs`, and the CI policy hash checks all pass. |
| 3 | Tests pass | **HOLD** | Full local suite and policy evidence passes after attribution, with every diff-owned test green. Criterion 3c remains undecidable until the modified CI graph completes its first real PR run; see details below. |
| 4 | No unresolved HIGH review findings | PASS | Reviewer recorded no blockers or high-severity findings. One grammar nit was explicitly non-blocking. |
| 5 | Final branch clean | PASS | Candidate was clean at the reviewed SHA before branch creation; `git diff --check origin/main...HEAD` and changed-file formatting both pass. |
| 6 | Branch diverges cleanly from main | PASS | Re-evaluated after main advanced: `git merge-tree --write-tree origin/main ad8bb5128cb5db3b229902a3e14369aae3a9c3c8` exited 0 and produced tree `fa0b8659a0ef47869cb32eb28c5a9d7eeab818e8`; no self-rebase was needed. |
| 7 | Single feature theme | PASS | One commit changes only the required-CI reachability graph, its invariant test, and the tamper-evident execution hash. |

## Criterion 2: acceptance evidence

- Every real job in `.github/workflows/ci.yml` is reachable from `ci-required`, except the documented advisory jobs `contract-radar-bd-head` and `mcp-mail`: `TestCIRequiredCoversEveryRealJob` PASS.
- Both push-only coverage jobs are dependencies of `ci-required` and are accepted as skipped on pull requests: `TestCIRequiredSkipsPushOnlyCoverageJobs` PASS.
- The expected CI execution hash matches the changed dependency graph: `make test-ci-policy` PASS, including `go test -count=1 ./scripts/cipolicy`.

## Criterion 3: test evidence

`test_cmd: make test-local-full-parallel`

`test_cmd_scope: full-suite`

Environment: rootless Podman 5.8.4 via `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`; `TESTCONTAINERS_RYUK_DISABLED=true`. Cached/pulled test images were verified as `dolt-sql-server:1.32.4@sha256:b0400696666ab7f4743e15d28d9e934efebde99794a967fe14b3a3a13bfa0b3d` and `dolt:2.1.7@sha256:eba699ca1821847c2e8070475f8e504b476834ee425db4036f89c956cceaf472`.

- Raw result: 40/40 jobs completed; 21 job-level PASS and 19 job-level FAIL.
- Top-level test counts across preserved shard logs: 29,917 PASS, 6 FAIL, 101 SKIP.
- Skip justification: all 101 skips are pre-existing explicit environment/platform/live-provider/golden-regeneration opt-outs. No diff-owned test skipped.
- `diff_tests_executed`: `TestCIRequiredCoversEveryRealJob` PASS and `TestCIRequiredSkipsPushOnlyCoverageJobs` PASS in both `unit-core` and `integration-packages-core-3-of-4`; 0 diff-owned FAIL, 0 diff-owned SKIP.
- Fresh package verification: `go test ./scripts/... -count=1 -v` PASS (199 top-level PASS, 0 FAIL, 0 SKIP); `go vet ./scripts/...` PASS.
- Build/static verification: `go build ./...` PASS; changed-file formatting PASS; `git diff --check origin/main...HEAD` PASS.
- Policy lane: `make test-ci-policy` PASS (5 runner-policy Python tests, 15 CI-suite-coverage Python tests, `scripts/cipolicy`, `scripts/prwatchdog`, and the documented static-scope Go checks).
- Full `go vet ./...` raw FAIL was the same pre-existing `cmd/gc` compile condition tracked by `ga-q2jkyr`; the exact condition independently reproduced on then-current `origin/main@411413b1055d660a1d8cb7beac98d5f5f4cd7216`. Main subsequently advanced to `21eca31d1c18e57e3bf83a968d64fd80db589ba5` with that repair, so the synchronized PR run is the current-merge-ref verification.
- `waiver_ref: none` — the failures below are attributed under the non-diff-owned failure protocol, not waived.

### Failure attribution

All trackers predate this run, all failing tests are outside the three changed paths, and the proofs below are landed rather than inconclusive:

- `cmd/gc` compile failure in six `cmd-gc-process` shards, `productmetrics-testhook`, six `integration-packages-cmd-gc` shards, and `go vet ./...` -> `ga-q2jkyr`. Proof: then-current `origin/main@411413b1055d660a1d8cb7beac98d5f5f4cd7216` independently reproduced the same `releaseOrphanedPoolAssignments` argument-count mismatch. No path overlap.
- `TestReaperWorkflowRootCleanupRealDoltSemantics` -> `ga-cp7r41`. Proof: external `dolt init` was killed during fixture setup before the scenario ran. The CI-policy-only diff cannot affect that process. No path overlap.
- `TestAdoptPRFormulaCompileAndRun`, `TestPersonalWorkFormulaCompileAndRun`, `TestGCLiveContract_BeadsAndEvents`, and `TestGraphWorkflowFailureRunsCleanup` -> `ga-esyijp`. Proof: external `bd init` refused pending shared-server schema migrations before any formula/live-contract behavior. The diff has no schema/bootstrap path. No path overlap.
- `TestE2E_SuspendResume_City` -> `ga-dc9utn`. Proof: exact proven `citysus.report` timeout; the root cause is in the reconciler suspend/wake path, which this diff does not touch. No path overlap.

Tracker sightings were appended and read back for this run. Because each attribution has an affirmative mechanism or base-ref proof, the candidate's two small added meta-tests do not invoke the inconclusive added-load guard.

### CI-config lane evidence

`ci_lane_run: not-yet-run`

This diff modifies `.github/workflows/ci.yml` and the `ci-required` dependency list. Draft PR #6286's first pull-request run recorded both push-only coverage jobs as expected `skipped`; the modified `ci-required` roll-up has not yet completed. The gate remains HOLD until a synchronized run against current main proves that the roll-up consumes those expected skips and finishes successfully. Do not publish `release-gate/deploy-clearance` or route a merge request while this gate remains HOLD.
