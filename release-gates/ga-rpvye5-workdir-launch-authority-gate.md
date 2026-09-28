# Release gate: gc.work_dir launch authority

- Bead: `ga-rpvye5`
- Reviewed source: `builder/ga-rpvye5-workdir-authority@6c24ff8487b217bf17a1d262e4c7c34a81ec4e64`
- Base: `origin/main@fc7a23a292dc50b9a2251e90b76ab4d467c799f1`
- Merge base: `3c4ba7c5dad60f1db2ba7a241120b120acfddda7`
- Deploy mode: remote; push target: `fork`
- Evaluation date: 2026-09-05
- Verdict: **PASS**
- Waiver: `mayor-2026-09-05-ga-rpvye5-c3-c3b`

## Release criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | The bead notes contain an unambiguous reviewer PASS for the exact reviewed commit. The recorded SHA resolves to the full commit above. |
| 2 | Acceptance criteria met | PASS | The diff stops reading canonical `gc.work_dir` as non-drain launch authority, preserves legacy `work_dir`, keeps drain-source resolution first, and leaves launcher precedence unchanged. All diff-owned regression and control tests executed and passed. |
| 3 | Tests pass | PASS with waiver | The documented full local suite ran on the exact reviewed commit with 48,777 PASS, 3 FAIL, and 208 SKIP top-level terminal events. Every diff-owned test passed. The three failures are independently attributed below; the same-package dirty-schema failure is covered by mayor waiver `mayor-2026-09-05-ga-rpvye5-c3-c3b`. |
| 3b | Policy/lint lane | PASS with waiver | Eight required lanes passed. `make lint` and `make fmt-check` exposed pre-existing findings outside this diff's hunks; mayor independently verified the hunk boundaries and granted waiver `mayor-2026-09-05-ga-rpvye5-c3-c3b`. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI configuration changed)`. |
| 4 | No high-severity review findings open | PASS | Reviewer verdict is PASS with zero unresolved HIGH findings. |
| 5 | Final branch is clean | PASS | The exact reviewed source was clean before the gate record was added. The committed deploy head is clean after gate creation. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 6c24ff8487b217bf17a1d262e4c7c34a81ec4e64` exited 0 and produced tree `48f851c036b6f6c3d818dbbc0efaadbeba4e8acc`; no self-rebase was required. |
| 7 | Single feature theme | PASS | Both commits and all three changed files form one reconciler change: prevent an observability stamp from becoming launch authority while preserving creator and drain-source behavior. |

## Acceptance evidence

- A task bead carrying only canonical `gc.work_dir` no longer overrides the
  session's configured directory.
- Legacy creator-owned `work_dir` remains launch authority.
- Drain-source anchor resolution remains ahead of task metadata resolution.
- No-task and nonexistent-stamp controls preserve the session directory.
- The existing session-ID legacy-key behavior remains unchanged.

Observed diff-owned tests:

```text
TestResolveTaskWorkDirPrefersCreatorWorkDirOverStampedCanonical: PASS twice
TestPrepareStartCandidate_StampOnlyWorkDirDoesNotOverrideSessionID: PASS
TestPrepareStartCandidate_StampOnlyWorkDirDoesNotOverrideRoleName: PASS twice
TestPrepareStartCandidate_NoAssignedTaskKeepsSessionWorkDir: PASS twice
TestPrepareStartCandidate_NonexistentStampedWorkDirKeepsSessionWorkDir: PASS twice
TestPrepareStartCandidate_UsesSessionIDForTaskWorkDir: PASS twice
TestResolveTaskWorkDirPrefersPreparedDrainSourceAnchor: PASS twice
TestPrepareStartCandidate_UsesTriggerBeadWorkDirBeforeClaim: PASS twice
```

## Criterion 3 evidence

The rootless Podman socket was active at
`/run/user/1000/podman/podman.sock`. `DOCKER_HOST` and
`TESTCONTAINERS_RYUK_DISABLED=true` were set before the run. Cached Dolt images
matched the repository's `2.1.7` pin.

```text
test_cmd: GOFLAGS=-v make test-local-full-parallel
test_cmd_scope: full-suite
result: 48,777 PASS, 3 FAIL, 208 SKIP
diff_tests_executed: all added or modified tests PASS
waiver_ref: mayor-2026-09-05-ga-rpvye5-c3-c3b
ci_lane_run: n/a (no CI configuration changed)
runner_log: /var/tmp/gc-gate-ga-rpvye5.BujTZd/full-suite.log
job_logs: /var/tmp/gc-local-tests.CSYleG
```

Failure attribution:

| Test | Tracker and proof |
|---|---|
| `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` | `ga-esyijp`; dirty-schema migration found a dirty `comments` table on shared Dolt port 28231. Mayor verified the reviewed diff only narrows a metadata read in `resolveTaskBeadWorkDir`, has no schema/init/migration writes or calls, and cannot reach the failing initialization path. The tracker contains the identical test, mechanism, and port on unrelated candidates predating this run. Waiver: `mayor-2026-09-05-ga-rpvye5-c3-c3b`. |
| `TestBdFlagManifestCurrent` | `ga-f0uceo`; installed `bd` flags differ from the manifest. The candidate changes neither the installed binary nor the manifest, and has no path overlap. |
| `TestE2E_SuspendResume_City` | `ga-dqd7gf`; timeout waiting for `citysus.report`, with prior cross-change sightings and no diff path overlap. |

None of the 208 skipped top-level events is diff-owned. All three tracker
sightings from this run were appended and read back from the store.

## Policy and lint evidence

```text
policy_lane:
  PASS: make test-ci-policy
  PASS: make check-gomod-replace
  PASS: make check-native-dependency-surface
  PASS: make check-eventexport-isolation
  PASS: make check-core-boundary
  PASS: make test-native-doltlite-beads
  PASS with waiver: make lint
  PASS with waiver: make fmt-check
  PASS: make vet
  PASS: make check-docs
policy_logs: /var/tmp/gc-gate-ga-rpvye5.BujTZd/policy
waiver_ref: mayor-2026-09-05-ga-rpvye5-c3-c3b
```

Mayor independently verified that this branch's only hunk in
`cmd/gc/session_lifecycle_parallel_test.go` is around base line 797. The
reported SA1019 finding is at branch line 7395 and is byte-identical to
`origin/main` line 7175. The reported SA4006 is in unchanged
`internal/api/huma_handlers_sessions_command.go`, and all four formatting
drifts are in files outside the diff. The branch's added lines produce no lint
finding.

## Final disposition

All criteria pass, including criteria 3 and 3b under independently verified
mayor waiver `mayor-2026-09-05-ga-rpvye5-c3-c3b`. Deploy the exact reviewed
commit from isolated branch `deploy/ga-rpvye5-gate`; do not substitute the
rebased builder-worktree tip.
