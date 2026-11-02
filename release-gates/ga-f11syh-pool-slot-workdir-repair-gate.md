# Release gate: reconciler pool-slot work-dir repair

- Deploy bead: `ga-f11syh`
- Build bead: `ga-3c5isi`
- Review bead: `ga-a0471c`
- Reviewed commit: `0aae22b74ebd928f732577c842c339db6d5a4f9f`
- Provenance branch: `builder/ga-3c5isi`
- Base: `origin/main@734f18f45915399a900561c3f964e3af96cace0b`
- Deploy mode: remote
- Intended deploy branch: `deploy/ga-f11syh-gate`
- Evaluated: 2026-09-04
- Verdict: **PASS**

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-a0471c` records an unqualified PASS for the exact reviewed commit. The SHA resolves to `0aae22b74ebd928f732577c842c339db6d5a4f9f`; no review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | The two reconciler stamp sites now refuse to replace real `gc.work_dir` evidence with a pool-slot root. The repair sweep covers both assigned and reopened/unassigned-routed beads, restores only unequal canonical/legacy pairs whose canonical value is exactly pool-slot-shaped, leaves other shapes untouched, and converges without a second write. The five changed behavior tests passed. |
| 3 | Tests pass | **PASS WITH ATTRIBUTED FAILURES** | The documented 40-job full local suite completed with 38 jobs PASS and 2 raw FAIL. Both failures are non-diff-owned, have predating condition trackers, satisfy the no-path-overlap rule, and have decisive mechanism or cross-run evidence below. All diff-owned tests ran in green shards. |
| 3a | Pre-existing failures may be attributed | **PASS** | `TestBdFlagManifestCurrent` is tracked by `ga-f0uceo` and is structurally outside the changed code. `TestE2E_SuspendResume_City` is tracked by `ga-dqd7gf` and reproduced the tracker's exact 93-second missing-report signature already observed on an unrelated diff. Both tracker records were opened and this run's sightings were appended and verified. |
| 3b | Policy/lint lane | **PASS** | Workflow policy, module policy, native-dependency surface, event-export isolation, open-core boundary, native DoltLite tests, affected-package lint/vet, changed-file formatting, full `go vet`, docs sync, and `go build ./...` passed. Affected lint reported 0 issues. |
| 3c | CI-config lane | **PASS / n/a** | `ci_lane_run: n/a (no CI job, matrix, timeout, required-check list, workflow, or build target changed)`. |
| 4 | No high-severity review findings open | **PASS** | Reviewer reported no blockers and no security concerns; unresolved HIGH findings: 0. |
| 5 | Final branch is clean | **PASS** | The exact reviewed SHA was evaluated from a clean detached checkout. `git diff --check` passed and `core.hooksPath` is `.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first, then refreshed after the full suite. No PR carries the reviewed SHA. `git merge-tree --write-tree origin/main 0aae22b74ebd928f732577c842c339db6d5a4f9f` exited 0 against refreshed base `734f18f45915399a900561c3f964e3af96cace0b`, producing tree `c53084a458c7af27e16a93723a3cc3af95c1049d`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Four files in `cmd/gc`: the reconciler stamp/repair implementation and its tests. The two commits implement one behavior theme: preserving and repairing canonical per-bead worktree evidence. |

## Criterion 3 evidence

```text
test_cmd_scope: full-suite
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=2 LOCAL_TEST_LOG_DIR=/var/tmp/gc-deploy-ga-f11syh.9BgXA4 make test-local-full-parallel
test_counts: 38/40 jobs PASS, 2/40 jobs raw FAIL, 0/40 jobs SKIP
top_level_skip_markers: 0
package_no_tests_notices: 11 (expected build-tag/profile and shard partitioning; none is a diff-owned test package)
job_logs: /var/tmp/gc-deploy-ga-f11syh.9BgXA4
diff_tests_executed: TestStampRunSessionIdentityPreservesRealEvidenceAgainstPoolSlotSelfCwd PASS; TestIsPoolSlotWorkDirRoot PASS; TestWorkDirStampWouldClobberEvidence PASS; TestPoolSlotWorkDirRepairFor PASS; TestRepairPoolSlotWorkDirClobber PASS
skip_justification: no top-level tests skipped; package-level no-test notices are intentional runner partitioning and do not include the diff-owned tests
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
```

The rootless Podman socket was active before the run. The cached container set
included the repository's Dolt images and `testcontainers/ryuk:0.14.0`; Gas
City has no `cairn` entry for `dolt-tests-via-podman`.

The five diff-owned tests were explicitly assigned by the documented runner to
completed green `cmd/gc` shards, and neither changed test file contains a skip
call. The same tests were also assigned across the green integration `cmd/gc`
shards where applicable.

### Raw failure attribution

| Raw failing test | Predating tracker | Attribution evidence |
|---|---|---|
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | Clauses 1 and 4: `internal/bdflags/freshness_test.go` and `internal/bdflags/**` are untouched. Clause 3(a), mechanism: the test compares the repository manifest with the separately installed `bd` executable; the changed `cmd/gc` reconciler functions cannot alter either input. Exact installed-binary drift is the tracker's standing condition. Log: `integration-packages-core-1-of-4.log`. |
| `TestE2E_SuspendResume_City` | `ga-dqd7gf` | Clauses 1 and 4: `test/integration/e2e_lifecycle_test.go` is untouched and has no path overlap with the diff. Clause 3(b), cross-run/cross-diff: the exact 93-second timeout waiting for `citysus.report` is recorded on the predating tracker from an unrelated container-security-only diff. Log: `integration-rest-full-2-of-8.log`. |

No inconclusive attribution path was used.

## Policy and static evidence

Passed on reviewed commit `0aae22b74ebd928f732577c842c339db6d5a4f9f`:

```text
make test-ci-policy check-gomod-replace check-native-dependency-surface \
  check-eventexport-isolation check-core-boundary test-native-doltlite-beads
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed
make vet check-docs
go build ./...
git diff --check origin/main...0aae22b74ebd928f732577c842c339db6d5a4f9f
```

## Decision

Gate **PASS**. Prepare the isolated `deploy/ga-f11syh-gate` branch from the
reviewed SHA, commit this checklist, push it to the configured fork, and open a
PR against `gastownhall/gascity:main`. Publish deploy clearance on the exact PR
head before routing the merge request. The deployer does not merge.
