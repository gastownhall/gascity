# Release Gate: reaper Step 6 type-scope guard

- Result: **PASS**
- Deploy bead: `ga-aw7bns`
- Reviewed source commit: `2acb522886288d7d458c3f2d3108a27915df89b8`
- Source branch (provenance only): `builder/ga-t832q4.2`
- Current base: `origin/main` at `55763721614477ebc4859e0a0f0c0086acbd2f41`
- Review lineage: `ga-owoezb` (round 1 request-changes) -> `ga-vk8i0v`
  (substantive round 2 PASS) -> `ga-kys2jp` (rebase/composition PASS)

## Scope

The candidate adds a fail-closed guard immediately before the maintenance
reaper's pattern-based `bd prune`. The guard rejects unsafe pattern characters
before constructing SQL and blocks the prune whenever an old closed
non-session bead would match the configured session pattern. It preserves the
normal session-only prune path and composes with the current Dolt-native backup
freshness detection.

The reviewed range is one feature theme across four files and six TDD/fix
commits: 264 insertions and 3 deletions.

```text
internal/bootstrap/packs/core/assets/scripts/reaper.sh  +53/-0
test/reaper_prune_backup_guard_test.sh                  +28/-1
test/reaper_prune_type_scope_guard_test.sh             +179/-0
test/reaper_session_pattern_test.sh                     +4/-2
```

## Criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | **PASS** | `ga-vk8i0v` is closed with reason `pass` and records `ROUND 2 VERDICT: PASS` for the substantive security/spec review. `ga-kys2jp` is closed with reason `pass` and pins the exact deploy commit `2acb522886288d7d458c3f2d3108a27915df89b8` after independently verifying the rebase and its composition with the Dolt-native backup logic. |
| 2 | Acceptance criteria met | **PASS** | The pattern-validity gate runs before `_TYPE_GUARD_LIKE` is built and fails closed on characters outside the allowlist. The type-scope query targets closed, old rows in `issues` whose `issue_type != 'session'`; pinned rows are not `closed`, and ephemeral work is stored outside `issues`. A nonzero count records a distinct anomaly and prevents `gc bd prune`; zero preserves the existing prune. T15 proves that a fresh Dolt-native `file://` backup may open the backup gate without bypassing the type-scope guard. The type-safe empty-pattern SQL path remains unchanged. |
| 3 | Tests pass | **PASS** | The documented full-scope local CI union completed **40/40 jobs PASS**, with **79,988 PASS / 0 FAIL / 319 SKIP** verbose test events and **385 package PASS / 0 package FAIL**. `test_cmd_scope: full-suite`. The separately executed five-file reaper suite completed **33 PASS / 0 FAIL / 0 SKIP**. `waiver_ref: none`. See the test log below. |
| 3a | Pre-existing failures attributed | **PASS — n/a** | No job, package, or test failed, so no attribution or waiver was used. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, tracked-base `make lint-affected fmt-check-changed`, `bash -n` on the changed shell surface, production `shellcheck`, `make check-hooks`, `make build`, `go vet ./...`, module-replacement/native-dependency/event-export/core-boundary checks, native DoltLite beads tests, and `git diff --check` all exited 0. Affected lint reported `0 issues`; `core.hooksPath` is `.githooks`. Shellcheck emitted only the pre-existing informational SC1091 for the line-18 `dolt-target.sh` source. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI job, workflow, matrix, timeout, or required-check change)`. |
| 4 | No high-severity review findings open | **PASS** | The round-1 SQL-injection finding was fixed by the charset-validation gate and independently verified in round 2. The substantive PASS records no remaining blocker or HIGH finding; the rebase-only review found no regression or new finding. |
| 5 | Final branch is clean | **PASS** | At the exact reviewed commit, after all test and static runs and before this checklist was added, `git status --porcelain` produced no output. The checklist is the only deployer-authored file and will be committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After a final fetch, `git merge-tree --write-tree origin/main 2acb522886288d7d458c3f2d3108a27915df89b8` exited 0 against current `origin/main@55763721614477ebc4859e0a0f0c0086acbd2f41`, producing tree `c7d2f67235e08b339252edcab93c00d7d080c68d`. `git diff --check` and `assert_deploy_ancestry_scope` also passed. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | All production and test changes belong to one maintenance-reaper safety feature: constrain Step 6's pattern prune to session-only scope while retaining its backup gate and valid-pattern behavior. |

## Test evidence

The rootless Podman socket was active before the run. Podman reported rootless
operation with `crun`, Ryuk was disabled, and the cached image set included the
repository-pinned Dolt `2.1.7` image and the Testcontainers Dolt `1.32.4`
image.

```text
test_cmd:
  DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
  TESTCONTAINERS_RYUK_DISABLED=true
  EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true'
  GOFLAGS=-v GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6
  LOCAL_TEST_LOG_DIR=/var/tmp/ga-aw7bns-full.Zvz5m6/jobs
  isolated-test-run.sh -- bash -c 'make test-local-full-parallel'

test_cmd_scope: full-suite
test_job_counts: 40 PASS, 0 FAIL, 0 SKIP
test_event_counts: 79,988 PASS, 0 FAIL, 319 SKIP
test_package_counts: 385 PASS, 0 FAIL
full_log: /var/tmp/ga-aw7bns-full.Zvz5m6/full-suite.log
job_logs: /var/tmp/ga-aw7bns-full.Zvz5m6/jobs
tripwire: none
```

`skip_justification`: the 319 verbose Go SKIP events are existing
platform-specific, explicit helper-process, and opt-in live/provider cases in
unchanged packages. This candidate's tests are shell files, not Go tests, and
all diff-owned shell cases ran separately with zero skips.

The required diff-owned and adjacent reaper tests were rerun through the same
isolation wrapper:

```text
test/reaper_expired_rig_nudge_test.sh              4 PASS
test/reaper_prune_backup_guard_test.sh            15 PASS
test/reaper_prune_type_scope_guard_test.sh         5 PASS
test/reaper_rig_name_for_db_test.sh                6 PASS
test/reaper_session_pattern_test.sh                3 PASS
TOTAL                                             33 PASS, 0 FAIL, 0 SKIP
log: /var/tmp/ga-aw7bns-focused-reaper.log
```

`diff_tests_executed`: backup-guard T1-T15 PASS, including the new combined
Dolt-native-backup/type-scope case; type-scope-guard T1-T5 PASS, including the
quote-injection rejection; session-pattern T1-T3 PASS. `waiver_ref: none`.

The first attempted full-suite invocation used an explicit log directory that
had not yet been created. Every job failed before its command could start with
`No such file or directory`; that harness invocation is not scored as a test
result. The authoritative run above created the directory first, ran every job
once, and is the only product-test result used by this gate.

## Static evidence

```text
make test-ci-policy                                                     PASS
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main
  make lint-affected fmt-check-changed                                  PASS (0 issues)
bash -n reaper.sh and all three changed shell test files                PASS
shellcheck internal/bootstrap/packs/core/assets/scripts/reaper.sh       PASS (pre-existing SC1091 info only)
make check-hooks                                                        PASS (.githooks active)
make build                                                              PASS
go vet ./...                                                            PASS
make check-gomod-replace check-native-dependency-surface                PASS
make check-eventexport-isolation check-core-boundary                    PASS
make test-native-doltlite-beads                                         PASS
git diff --check origin/main...HEAD                                     PASS
```

Logs: `/var/tmp/ga-aw7bns-policy.log` and
`/var/tmp/ga-aw7bns-static.log`.
