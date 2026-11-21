# Release Gate: bound add-testenv-import to the current worktree

Bead: `ga-wkaxsf`
Source bead: `ga-t00ejy`
Review bead: `ga-vjo3ho`
Reviewed commit: `1a7c890c7a6c201b67d3a152c7fc102751ebc6a7`
Base: `origin/main@3c1de2a1e0d0d4a1a654933dd29e7ebe8e1525a9`
Deploy mode: `remote`

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-vjo3ho` is closed with verdict `pass`. Its notes explicitly re-derived and tested the final reviewed tip `1a7c890c7a6c201b67d3a152c7fc102751ebc6a7`, correcting the stale metadata value that predated the resource-census commit. |
| 2 | Acceptance criteria met | PASS | The walk now prunes any nested directory whose `.git` entry is a file, while the `path != root` guard preserves operation when the generator itself runs from a linked worktree. The regression fixture proves an ordinary package still receives `testenv_import_test.go` and a sibling linked-worktree package does not. Existing named-directory exclusions and generator behavior are otherwise unchanged. |
| 3 | Tests pass | PASS | The documented full local sweep ran 40 jobs. Raw job result: 37 PASS / 3 FAIL; raw top-level test events: 46,030 PASS / 3 FAIL / 208 SKIP. Every raw failure is a tracked, pre-existing, non-diff-owned condition with a landed mechanism or cross-branch proof and no path overlap; attribution is detailed below. The only diff-owned test passed in both the unit and integration-tagged core sweeps. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no style, security, specification, or severity findings. Unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | The exact reviewed source tree was clean before gate generation. This checklist is the only new file to carry onto the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | PASS | Pre-flight found no PR carrying the reviewed commit. After a fresh fetch, `git merge-tree --write-tree origin/main 1a7c890c7a...` completed without conflicts and produced tree `6a63d938031af6b027efd3c017c5ff5bd54fdb82`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | Both source commits implement and account for one bounded filesystem-walk fix in `scripts/add-testenv-import.go`: the behavior, its regression test, and the required resource-census ledger update. `assert_deploy_ancestry_scope origin/main 1a7c890c7a... ga-wkaxsf ga-t00ejy ga-vjo3ho` passed. |

## Acceptance evidence

- `isNestedWorktreeRoot` detects a linked worktree structurally from its `.git` file, independent of directory naming conventions.
- `path != root` prevents the generator from pruning its own root when invoked from a linked worktree.
- `TestAddTestenvImportSkipsNestedGitWorktrees` creates an ordinary package and a nested linked-worktree fixture, then verifies output is written only to the ordinary package.
- The subprocess introduced by that regression test is recorded consistently in `internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, and the generated ledger table in `TESTING.md`.

## Test evidence

- `test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel`
- `test_cmd_scope: full-suite`
- `test_job_counts: 37 PASS / 3 FAIL / 0 SKIP`
- `test_counts: 46,030 PASS / 3 FAIL / 208 SKIP`
- `skip_justification: expected platform, privilege, live-provider, persistence-opt-in, registry, and helper-process skips in the repository's full local runner; no diff-owned test skipped.`
- `diff_tests_executed: TestAddTestenvImportSkipsNestedGitWorktrees` — PASS in `unit-core` (0.18s) and `integration-packages-core-3-of-4` (0.16s).
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI configuration change in this diff)`
- Raw runner log: `/var/tmp/ga-wkaxsf-full-suite.log`
- Per-job logs: `/var/tmp/gc-local-tests.PvRudY`

### Failure attribution

- `TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates/pool_respawn_after_drain -> ga-rh76sz | clause 3(a): cross-PR proof landed.` The exact five-second `async starts did not finish` condition predates this run and is recorded across unrelated branches. The candidate changes no `cmd/gc` reconciler path, the failing test is not diff-owned, and no failing-test path overlaps the diff.
- `TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a): BASE_REF and cross-branch proof landed.` The installed `bd` exposes create/list/ready/show/update flags absent from the repository manifest. The tracker predates this run and records identical origin/main and unrelated-branch reproductions. The candidate cannot alter `internal/bdflags` or the installed binary, and no failing-test path overlaps the diff.
- `TestE2E_SuspendResume_City -> ga-dc9utn | clause 3(a): mechanism proof landed.` The 95.14-second `citysus.report` timeout exactly matches the tracker's proven suspend/reconciler identity-loss condition. The candidate does not touch the reconciler suspend/wake path, the failing test is not diff-owned, and no failing-test path overlaps the diff.

All three trackers predated this run and now contain this run's sighting. No inconclusive attribution path was used, so the inconclusive reachability/test-load guard was not invoked.

## Policy lane

- `policy_lane: make test-ci-policy` — PASS (runner-policy Python checks, CI-suite coverage checks, `scripts/cipolicy`, `scripts/prwatchdog`, and static-scope contracts).

## Environment integrity

- Rootless Podman was active at `/run/user/1000/podman/podman.sock`; `DOCKER_HOST` and `TESTCONTAINERS_RYUK_DISABLED=true` were exported before the full-suite command.
- No `dolt-tests-via-podman` cairn entry exists for this rig, and the documented local full-suite command selected the repository's own required test environments.
