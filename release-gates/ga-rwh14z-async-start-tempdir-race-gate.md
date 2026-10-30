**Verdict:** **PASS**

# Release Gate: async-start TempDir cleanup race

- Deploy bead: `ga-rwh14z`
- Source bead: `ga-9qs5gk`
- Reviewed PR: `gastownhall/gascity#6459`
- Reviewed source commit: `3cc60a6f8bd622895d498309be1c2a53c7314728`
- Base checked: `origin/main@bd8a9bb518cfc68b882cd6cec1db54942bfc0b74`
- Deploy mode: `remote` (push remote: `fork`)

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This checklist
applies the release criteria supplied in the deployer instructions and the
repository's documented test targets.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | The recovered deploy bead records the exact reviewed PR head as reviewed and passed. The source bead's mayor note records the second MPR run at this SHA as unanimous 3/3 (`Qwen`, `Claude`, and `Codex` all `ok`), high-confidence auto-merge with no disagreement. The PR's verified internal review comment carries `mpr-review head=3cc60a6f... verdict=auto-merge`. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | `TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync` now drains the async start wave after `tick()` so its goroutines finish writing the temporary city's event log before `t.TempDir` cleanup. The fixture sets a 30-second shutdown timeout, matching the existing trace-fixture hardening and avoiding a false timeout around the real approximately four-second stability wait. The diff-owned test ran in two green full-suite jobs. |
| 3 | Tests pass | PASS | The documented full local runner executed all 40 unit, process, formula, bead-store, runtime/tmux, and REST jobs through `isolated-test-run.sh`. Raw runner result: 36 PASS / 4 FAIL / 0 skipped or omitted jobs. All four failures are attributed pre-existing conditions under criterion 3a; none is diff-owned. `TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync` was selected and passed in both `cmd-gc-process-2-of-6` and `integration-packages-cmd-gc-4-of-6`. The later pre-push fast matrix ran all 10 jobs; nine passed and `unit-core` hit one separately attributed timing-bound failure tracked by `ga-hvjhay`. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures may be attributed | PASS | Three temporary-city failures stopped in external `bd` initialization on the tracked random-cursor shared-server migration refusal (`ga-lejnse`, root fix `ga-e2z1zb`), before scenario behavior. `TestPinnedIntegrationBeadsModuleVersion` reproduced the tracked stale `v1.3.0-rc.2` expectation while main pins `v1.3.0` (`ga-rnwg5u`). Both trackers predate this run, were opened before citation, and contain this run's verified sightings. Clause 3(a) mechanism proof lands for all four: the candidate changes only `cmd/gc/city_runtime_test.go`, which cannot alter external Beads initialization or the integration version constant. Clause 4 has no failing-file/package overlap. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy` passed all Python runner-policy and CI-coverage tests plus `scripts/cipolicy`, `scripts/prwatchdog`, and the static-scope contracts. `make vet` passed. `make build` and `git diff --check origin/main...HEAD` passed. |
| 3c | CI-config diff needs its own lane | PASS | `ci_lane_run: n/a (no CI configuration changed)`. |
| 4 | No high-severity review findings open | PASS | The exact-head MPR review was unanimous 3/3 and recorded no disagreement or blocking finding. Its requested 30-second timeout hardening is included in the reviewed head. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | The reviewed tree was clean at the exact source SHA before gate generation. The checklist is committed separately on the isolated deploy branch, after which cleanliness is rechecked. |
| 6 | Branch diverges cleanly from main | PASS | Pre-flight verified PR #6459 remains OPEN at the exact reviewed SHA. After fetching current main, `git merge-tree --write-tree origin/main 3cc60a6f...` exited 0 and produced tree `86f7e3e00d1668cdd0886cff51c91b379ed46fdb`; no bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | Both commits modify only `cmd/gc/city_runtime_test.go`: drain the async start wave before temporary-directory cleanup, then give that same drain sufficient timeout headroom. `assert_deploy_ancestry_scope` passed with `ga-rwh14z`, source bead `ga-9qs5gk`, and confirmed review-hardening bead `ga-hgjlhi`. |

## Test evidence

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
isolated-test-run.sh -- bash -c 'make test-local-full-parallel'

test_cmd_scope: full-suite
runner_jobs: 36 PASS / 4 attributed raw FAIL / 0 SKIP or omitted
diff_tests_executed:
  TestCityRuntimeTick_RefreshesManualSessionOverlayAfterSync PASS
    - cmd-gc-process-2-of-6
    - integration-packages-cmd-gc-4-of-6
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
aggregate_log: /var/tmp/ga-rwh14z-test-local-full.log
job_logs: /var/tmp/gc-local-tests.1HjYrf
```

The rootless Podman socket was live before the run. This rig has no
`dolt-tests-via-podman` cairn entry, and the documented full local runner has
no testcontainers image prerequisite. Ryuk remained disabled because the host
uses the external testcontainer sweep. The runner executed every one of its 40
jobs; no job was skipped or omitted. The non-verbose runner does not emit an
aggregate count of suite-internal platform or opt-in skips, so the auditable
count above is the runner-job count. The diff-owned test is explicitly present
in two passing job logs and did not skip.

### Failure attribution

```text
failure_attribution: TestPersonalWorkFormulaCompileAndRun -> ga-lejnse
  clause 3(a): mechanism — fixture gc init stopped on the tracked shared-server
  migration refusal at hq v56 -> v66 before formula behavior

failure_attribution: TestHumaBinary_SessionMessageAsync -> ga-lejnse
  clause 3(a): mechanism — async city init stopped on the same tracked refusal
  at hq v54 -> v66 before session-message behavior

failure_attribution: TestGCLiveContract_BeadsAndEvents -> ga-lejnse
  clause 3(a): mechanism — temporary rig initialization stopped on the same
  tracked refusal at v41 -> v66 before the live-contract scenario completed

failure_attribution: TestPinnedIntegrationBeadsModuleVersion -> ga-rnwg5u
  clause 3(a): mechanism — current main pins v1.3.0 while the untouched
  integration test still expects v1.3.0-rc.2
```

For all four failures, clause 1 is clear because no failing test file is in the
candidate diff; clause 2 is satisfied by an open tracker created before this
run; clause 3 has a direct mechanism proof; and clause 4 is clear because the
candidate touches only `cmd/gc/city_runtime_test.go`, while the failures are in
`test/integration/**` and stop before the candidate test can execute. No
inconclusive attribution path was used.

## Static and policy evidence

- `make test-ci-policy`: PASS.
- `make vet`: PASS.
- `make build`: PASS.
- `git diff --check origin/main...HEAD`: PASS.
- `make check-hooks`: PASS; `.githooks` owns `core.hooksPath`.

### Pre-push attribution

The normal push ran `make test-fast-parallel`: 9 jobs PASS / 1 raw FAIL / 0
SKIP or omitted. `TestPhase2StartupOutcomeBounds/claude/tmux-cli/Blocked`
reported `2.595847142s > 2.015s` in `internal/worker/workertest`. No pre-existing
tracker covered that exact condition, so tracker `ga-hvjhay` was created from
this run under criterion 3a's same-run escape: clause 1 is clear, clause 3(a)
mechanism proof lands, and clause 4 has no path overlap. The candidate's only
product path is the test-only `cmd/gc/city_runtime_test.go`; it is not compiled
into `internal/worker/workertest` or any production dependency of that package.
The release-gate markdown adds no test load. The other nine fast jobs passed,
including all six `cmd/gc` shards.

```text
failure_attribution: TestPhase2StartupOutcomeBounds/claude/tmux-cli/Blocked -> ga-hvjhay
  clause 3(a): mechanism — separate-package worker conformance timing bound;
  candidate is a cmd/gc test-only change plus this gate record
pre_push_log: /var/tmp/ga-rwh14z-push.log
failed_job_log: /var/tmp/gc-local-tests.cYMFny/unit-core.log
```

## Acceptance and scope evidence

- Changed path: `cmd/gc/city_runtime_test.go` only, 16 insertions.
- Commit `4452bc3d95...` adds the final async-start drain after the test's
  `tick()` call.
- Commit `3cc60a6f8b...` supplies a 30-second shutdown timeout for that same
  fixture, as requested by the MPR review under `ga-hgjlhi`.
- PR #6459's current required checks are green at the reviewed head, including
  Linux and macOS `cmd/gc` process shards, integration, policy, static, and
  required-summary lanes.
