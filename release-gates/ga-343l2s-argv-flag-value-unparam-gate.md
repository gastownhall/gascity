# Release gate: remove the unused `argvFlagValue` parameter (`ga-343l2s`)

- Overall verdict: **PASS with attributed raw test failures**
- Evaluated: 2026-09-09 UTC
- Deploy mode: `remote`; push remote: `origin`
- Reviewed deploy source: `ca0089cc344c0bb36c70d6278b10350778907c04`
- Source branch: `builder/ga-343l2s` (provenance only)
- Base evaluated: `origin/main@3f924d2a79481bfbb02dfe130aed42404951df12`
- Existing source PR: `https://github.com/gastownhall/gascity/pull/6210` (open, internally authored, exact reviewed head)

`docs/PROJECT_MANIFEST.md` is not present at the reviewed commit, so this
checklist uses the seven release criteria embedded in `mol-deployer-gate` and
the Deployer instructions. The pre-flight found the internally authored source
PR open, mergeable, and clean at the exact reviewed SHA. Its only commenter is
its team author; no external contributor has engaged on it.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | The bead contains an exact-head reviewer verdict of PASS for `ca0089cc344c0bb36c70d6278b10350778907c04`. The reviewer read the full two-file diff, verified the only two call sites, and reported no security, specification, or coverage regression. |
| 2 | Acceptance criteria met | **PASS** | The accepted option was implemented: the unused `flag` parameter was removed and `--data-dir` became a local constant. Both callers and the stale “generic parser” comment were updated. Full-tree search finds exactly the two intended callers. The unchanged space-separated, equals-form, and missing-value tests all passed in the full suite. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full-suite command scheduled all 40 jobs and completed **38 PASS / 2 raw FAIL / 0 omitted jobs**. Its result lines contain **45,588 PASS / 2 FAIL / 202 SKIP** results, including subtests. Both failures are non-diff-owned and satisfy criterion 3a below. No test file changed, so there is no diff-owned test that failed or skipped. |
| 3a | Non-diff-owned failures attributed | **PASS** | `TestBdFlagManifestCurrent` is attributed to predating tracker `ga-f0uceo`; `TestE2E_SuspendResume_City` is attributed to predating tracker `ga-dc9utn`, with the production fix tracked by `ga-pmafyc`. Each occurrence was appended to and read back from its tracker. Both attributions meet all four required clauses, as detailed below. |
| 3b | Policy/lint lane | **PASS** | The PR-equivalent static lane passed: `make test-ci-policy`, module/native-dependency/event-export/open-core guards, native DoltLite tests, fresh-cache `make lint-affected` (`0 issues`), changed-file formatting, docs sync, and `go vet ./...`. |
| 3c | CI-config lane run | **PASS / n/a** | The diff changes no workflow, CI job, matrix, timeout, required-check list, Makefile, or runner policy. `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | The exact-head reviewer verdict is PASS and records no open finding, request for changes, or high-severity issue. |
| 5 | Final branch clean | **PASS** | The detached worktree at the reviewed SHA remained clean after the full suite, static lane, build, and smoke checks. This checklist is the deployer's only new file and will be committed on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main ca0089cc344c0bb36c70d6278b10350778907c04` exited 0 and produced tree `d82da4ee52d531f8c41a7be6f1dc472327c43338`. The candidate and base are one commit ahead of each other; no bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | One commit changes two `cmd/gc` files for one internal data-directory argument parser refactor. There is no independent feature or unrelated ancestry. `assert_deploy_ancestry_scope` passed for `ga-343l2s`. |

## Build and acceptance evidence

- `make build`: **PASS**; `./bin/gc version` returned `dev` and `./bin/gc --help` exited 0.
- `git diff --check origin/main...HEAD`: **PASS**.
- Full-tree search at the candidate found two `argvFlagValue` callers plus its definition and comment, with no old two-argument call.
- `TestExtractDataDirPath_SpaceSeparated`: **PASS** in both a process shard and an integration-package shard.
- `TestExtractDataDirPath_EqualsForm`: **PASS** in the full suite.
- `TestExtractDataDirPath_Missing`: **PASS** in both a process shard and an integration-package shard.
- Build/smoke log: `/var/tmp/ga-343l2s-build-smoke.log` (`sha256:b208f5611b4d2df1888583a561ab911715f3ef088768ca417009db777dea6af9`).

## Criterion 3 evidence

The container-backed environment was established before the run:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- Both values were passed through `EXTRA_TEST_ENV` to the repository's scrubbed test environment.
- Rootless Podman 5.8.4 was reachable.
- The cached `docker.io/dolthub/dolt-sql-server:1.32.4` image matches the default tag pinned by `testcontainers-go/modules/dolt@v0.43.0`; the repository-pinned `docker.io/dolthub/dolt:2.1.7` image was also present.

Test record:

- `test_cmd`: `EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true" LOCAL_TEST_LOG_DIR=/var/tmp/ga-343l2s-full-gate.FKTzPW GO_TEST_TIMEOUT=30m GOFLAGS=-v make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: **38 PASS / 2 raw FAIL / 0 omitted jobs**; **45,588 PASS / 2 FAIL / 202 SKIP** result lines, including subtests
- `diff_tests_executed`: **none added or modified**; 0 diff-owned FAIL and 0 diff-owned SKIP
- `skip_justification`: all 202 skips come from unchanged suite code and cover platform/capability gates or documented quarantines. Examples include the opt-in real-`bd` persistence check and the tracked city-unregister shutdown quarantine. No candidate-owned test was skipped.
- `waiver_ref`: none
- `ci_lane_run`: n/a (no CI-config change)
- `shard_logs`: `/var/tmp/ga-343l2s-full-gate.FKTzPW`
- `runner_log_sha256`: `8c0a4307787fb710c3a6f2971479e67a8512cd267ddc7ea277097f2596474d31`

### Raw failure attribution

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | mechanism plus independent-main/cross-candidate proof — attributed`
  - Clause 1: the candidate changes no test and no file under `internal/bdflags`.
  - Clause 2: open `gate-tracker` bead `ga-f0uceo` was created on 2026-08-15, before this run. This occurrence was appended and verified.
  - Clause 3: the tracker records the identical installed-`bd` manifest drift on exact `origin/main` and many unrelated candidates. This run reported the same missing flags for create/list/ready/show/update. A `cmd/gc` helper refactor cannot change the separately installed `bd` binary or the `internal/bdflags` manifest.
  - Clause 4: the failing path is `internal/bdflags/freshness_test.go`; the diff paths are `cmd/gc/dolt_cleanup_reaper.go` and `cmd/gc/dolt_standalone_conflict.go`, with no overlap.
  - Raw log: `/var/tmp/ga-343l2s-full-gate.FKTzPW/integration-packages-core-1-of-4.log` (`sha256:79d1bbf8c7fd0ca17c7c0a461c279850be3df20584ffab6d545c1b93c379c27e`).

- `failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn (fix ga-pmafyc) | mechanism plus independent-main/standalone proof — attributed`
  - Clause 1: the candidate changes neither `test/integration/e2e_lifecycle_test.go` nor the named-session suspend/resume mechanism.
  - Clause 2: open `gate-tracker` bead `ga-dc9utn` was created before this run and points to fix bead `ga-pmafyc`. This occurrence was appended and verified.
  - Clause 3: the tracker identifies the exact 90-second `citysus.report` timeout and proves the cause: city suspension creates an empty desired state, after which the reconciler retires and clears the named session before resume. The condition reproduces alone on unchanged main. This candidate only inlines the unchanged `--data-dir` constant in a parser used by two Dolt process-inspection callers; it does not touch the desired-state, reconciler, suspension, or session-identity paths.
  - Clause 4: the failing test path is under `test/integration`; neither candidate path overlaps it or the proven production mechanism.
  - Raw log: `/var/tmp/ga-343l2s-full-gate.FKTzPW/integration-rest-full-2-of-8.log` (`sha256:2bec7b3f5a9db14e5bea32c6b8d98143b34f8499463499884dc2fe9bbaebc09f`).

Both attributions use landed mechanism and reproduction evidence, so the
inconclusive reachability/load guard is not invoked. The first candidate run
is preserved; it was not retried into green.

## Policy and static evidence

- `make test-ci-policy`: **PASS** (runner policy, suite-coverage policy, `scripts/cipolicy`, `scripts/prwatchdog`, and focused static-scope contracts).
- `make check-gomod-replace`: **PASS**.
- `make check-native-dependency-surface`: **PASS** (`173149092` native binary bytes, below the enforced cap).
- `make check-eventexport-isolation`: **PASS**.
- `make check-core-boundary`: **PASS**.
- `make test-native-doltlite-beads`: **PASS**.
- Fresh-cache `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected`: **PASS**, `0 issues`; selected package: `./cmd/gc`.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed`: **PASS**.
- `make check-docs`: **PASS**.
- `make vet`: **PASS**.
- Static log: `/var/tmp/ga-343l2s-pr-static-lane.log` (`sha256:4e37799f8e29fa7f82bf7c147b2e6c24d914c05c29755cdfbb4a54dacdc1aee8`).

A preliminary, non-gating `lint-full` diagnostic used the ambient shared
golangci-lint cache and surfaced stale findings for a deleted
`/var/tmp/gc-maintainer-fix.*` worktree plus existing generated/dashboard
dependency findings. It reported nothing in either changed file and is not the
ordinary-PR lane selected by `.github/workflows/ci.yml`; the required
fresh-cache affected-package lane above is the authoritative result. The
original diagnostic remains preserved at
`/var/tmp/ga-343l2s-policy-lane.log`.

## Disposition

Gate PASS. Cut `deploy/ga-343l2s-gate` from the exact reviewed source, commit
this checklist there, push the isolated branch, and open a pull request. Merge
authority remains with mayor/mpr; the deployer does not merge.
