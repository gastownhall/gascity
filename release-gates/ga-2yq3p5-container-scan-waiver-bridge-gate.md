# Release gate: time-boxed container-scan waiver bridge

- Deploy bead: `ga-2yq3p5`
- Build bead: `ga-wb9e3a`
- Review bead: `ga-56p0rf`
- Reviewed source: `a774fee25bc0ecfb2e38c9936a53d58eb76e1e34`
- Base evaluated: `origin/main@615f5b7942220ee02f6825b9d3d52b7b4b9e9224`
- Deploy mode: `remote`
- Push remote: `origin`
- Overall verdict: **PASS with attributed non-diff-owned failures**

`docs/PROJECT_MANIFEST.md` and `work-packages/` are not present at the reviewed
commit, so this record uses the seven criteria embedded in
`mol-deployer-gate` plus the acceptance contract on the build bead. The
pre-flight query found no pull request associated with the reviewed source;
the normal deploy path applies.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-56p0rf` is closed `pass` on the exact reviewed source. It records no style, security, specification, or high-severity finding. |
| 2 | Acceptance criteria met | **PASS** | The executable acceptance test confirms 45 existing entries moved from `2026-08-07` to the time-boxed `2026-09-21` horizon, exactly one `CVE-2026-46600` entry was added for `usr/bin/gh` and `usr/local/bin/dolt`, no waiver was removed, and every entry has a durable-fix statement. The base-to-head diff contains only `.trivyignore.yaml` and its owning test; it does not touch bundled-tool refs, Dockerfiles, or module patches. The live Container Scan rerun is a post-PR confirmation and must be green before the merge request is routed. |
| 3 | Tests pass | **PASS with attributed raw failures** | The required CI jobs for this path set are mapped below. Their local equivalents passed, including acceptance A, the minimum-supported `bd` contract, generated artifacts, GoReleaser config, the complete dashboard lane, and the diff-owned integration/security tests. Gas City's documented full local matrix, `make test-local-full-parallel`, scheduled all 40 jobs; its verbose logs contain **48,294 PASS / 4 attributed FAIL / 208 SKIP** top-level test results. The four failures are non-diff-owned and satisfy the attribution protocol below. |
| 3a | Non-diff-owned failures attributed | **PASS** | `TestBdFlagManifestCurrent` is tracked by `ga-f0uceo`; the two dirty-schema review-formula failures are tracked by `ga-esyijp`; the `citysus.report` timeout is tracked by `ga-dqd7gf`, created under the same-run tracker escape after a structural mechanism proof landed and clauses 1 and 4 were clear. |
| 3b | Policy/lint lane | **PASS** | CI-policy tests, affected-package lint and format, `go vet ./...`, docs sync, native dependency, open-core, event-export, and native DoltLite checks all passed. |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, job matrix, timeout, required-check list, runner policy, or other CI configuration changed. `.trivyignore.yaml` is workflow input rather than workflow configuration. |
| 4 | No high-severity review findings open | **PASS** | The reviewer recorded `style_findings: none`, `security_findings: none`, `uncovered_criteria: none`, and `verdict: pass`. |
| 5 | Final branch is clean | **PASS** | The detached checkout of the exact reviewed source remained clean after the full matrix and static lanes. This checklist is the deployer's sole addition and is committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main a774fee25bc0ecfb2e38c9936a53d58eb76e1e34` exited 0 and produced tree `99e5a0cdc4069726489d76e0b34fa4c430e9e3af`. The reviewed source is based directly on the evaluated `origin/main`; no self-rebase was required. |
| 7 | Single feature theme | **PASS** | Two TDD commits change one security-policy surface: a short-lived waiver-horizon refresh plus the one newly observed vendored-tool CVE, with an owning regression test. |

## Acceptance evidence

- `.trivyignore.yaml` contains 46 entries: 45 pre-existing entries and one new
  `CVE-2026-46600` entry.
- Every entry expires on `2026-09-21` and has a non-empty statement naming its
  durable fix path.
- The new entry is scoped to `usr/bin/gh` and `usr/local/bin/dolt`; it does not
  include `kubectl`.
- No prior waiver ID was removed.
- The cumulative base-to-head diff is exactly:

  ```text
  .trivyignore.yaml                       | 96 +++++++++++++++++----------------
  scripts/container_tool_security_test.go | 92 ++++++++++++++++++++++++++++---
  2 files changed, 137 insertions(+), 51 deletions(-)
  ```

- No `GH_SOURCE_REF`, `DOLT_SOURCE_REF`, Dockerfile, `go.mod`, or module-patch
  path changed.

## Build and test evidence

- `make build`: **PASS**
- `./bin/gc version`: **PASS** (`dev`)
- `test_cmd`: `GOFLAGS=-v make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: **48,294 PASS / 4 attributed FAIL / 208 SKIP** top-level test results
- `shard_logs`: `/var/tmp/ga-2yq3p5-testlogs.hoREok`
- `skip_justification`: the 208 skips are pre-existing platform guards,
  helper-process sentinels, optional live-provider/registry cases, and opt-in
  persistence or infrastructure tests. None is diff-owned; all diff-owned tests
  executed and passed.
- `waiver_ref`: none
- `ci_lane_run`: n/a; no CI-config change

Diff-owned tests executed in the full-suite `unit-core` job:

- `TestTrivyIgnoreRefreshesBridgeHorizonAndWaivesXNetDNSMessageCVE`: **PASS**
- `TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools`: **PASS**
- `TestRebuiltToolsAssertPatchedGRPCArtifact`: **PASS**

### Required CI job mapping

The changed Go test makes the workflow's `integration` path filter true. The
other conditional path filters are false for this candidate; their summary
jobs still run and accept the documented skips. The following are the actual
unconditional or activated jobs that feed `CI / required`, with the local
equivalent executed against the reviewed source:

| Required CI job | Local equivalent and result |
|---|---|
| `Preflight / static checks` | **PASS** — `make test-ci-policy`, all five boundary/dependency guards, affected-package lint and format, and `make check-docs` passed. `make vet` was also run and passed. |
| `Preflight / acceptance A` | **PASS** — `make test-acceptance`. |
| `Preflight / generated artifacts` | **PASS** — `make dashboard-ci`, `make spec-ci`, and `./scripts/check-generated-docs-drift.sh`; no generated file drift remained. |
| `Contract / bd CLI (minimum supported)` | **PASS** — installed the pinned `BD_PREV_VERSION=v1.0.4` into an isolated directory and ran `make test-bd-cli-contract`. |
| `Release config` | **PASS** — GoReleaser v2.18.0 `check` validated `.goreleaser.yml`. |
| `Dashboard SPA` | **PASS** — bundle build, source/test/e2e typechecks, 899 Vitest cases, embedded Go projection tests, fakesupervisor build, and 19 Playwright render checks passed. |
| `CI / integration` | **PASS with attributed non-diff-owned failures in the broader sweep** — the activated integration packages and the owning security tests ran within `make test-local-full-parallel`; the only four failures are attributed below, with no diff-owned failure. The live PR integration shards remain authoritative. |
| `Check`, `CI / preflight`, `CI / required` | **Pending remote composition** — all locally reproducible dependencies above passed. These fan-in statuses must be green on the PR before deploy clearance. |
| `Container Scan` / `Image vulnerabilities` | **Pending live acceptance** — this is the diff's purpose and cannot be replaced by a local assertion. It must be green on the PR before deploy clearance. |

The remaining conditional jobs (`cmd/gc` process and product-metrics rows,
credential-provider Windows, worker phase rows, pack gate, Docker session, K8s
session, and OpenClaw bridge) have no matching changed path and are expected to
skip or resolve through their skip-tolerant summary jobs. The remote workflow
is authoritative for both the path classification and every fan-in result.

### Raw failure attribution

- `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a), mechanism — attributed`
  - The installed `bd` binary exposes flags absent from the reviewed source's
    manifest. The tracker predates this run and contains repeated independent
    candidate/base reproductions.
  - This candidate changes only `.trivyignore.yaml` and
    `scripts/container_tool_security_test.go`; neither can alter
    `internal/bdflags` or the installed executable, and there is no path overlap.
  - Current log: `integration-packages-core-4-of-4.log`.

- `failure_attribution: TestAdoptPRFormulaRetriesTransientReviewerStep -> ga-esyijp | clause 3(a), mechanism — attributed`
- `failure_attribution: TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-esyijp | clause 3(a), mechanism — attributed`
  - Both fixtures failed during `gc init` because beads migration safety
    rejected dirty pre-existing Dolt schema tables under the full-suite run.
    The consolidated beads#4566 tracker predates this run.
  - The candidate cannot affect the review-formula fixture, Dolt schema, or
    migration path; there is no path overlap.
  - Current logs: `integration-review-formulas-retries-1-of-2.log` and
    `integration-review-formulas-recovery.log`.

- `failure_attribution: TestE2E_SuspendResume_City -> ga-dqd7gf | clause 3(a), mechanism — attributed under the same-run tracker escape`
  - The test timed out after 93.73 seconds waiting for `citysus.report`, matching
    prior exact candidate/base evidence on closed bug `ga-yc0e3a`.
  - No open tracker covered the condition, so `ga-dqd7gf` was created during
    this run only after the structural mechanism proof landed: security waiver
    data and a package-local test cannot participate in session suspend/resume
    or report production. Clauses 1 and 4 are independently clear.
  - Current log: `integration-rest-full-2-of-8.log`.

## Policy and static evidence

```text
make test-ci-policy                                      PASS
make check-gomod-replace                                 PASS
make check-native-dependency-surface                     PASS
make check-eventexport-isolation                         PASS
make check-core-boundary                                 PASS
make test-native-doltlite-beads                          PASS
make test-acceptance                                     PASS
BD_PREV_VERSION=v1.0.4 make test-bd-cli-contract        PASS
make dashboard-ci                                       PASS
npm run --workspace gas-city-dashboard-frontend test    PASS (899 tests)
make spec-ci                                             PASS
./scripts/check-generated-docs-drift.sh                  PASS
make dashboard-e2e                                      PASS (19 Playwright tests)
goreleaser v2.18.0 check                                 PASS
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected  PASS (0 issues)
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed  PASS
make check-docs                                          PASS
make vet                                                 PASS
git diff --check origin/main...HEAD                      PASS
```

The configured pre-push hook at `.githooks` also completed its sharded fast
suite successfully during the origin push dry-run.

## Release disposition

**Gate PASS.** Cut `deploy/ga-2yq3p5-gate` from the exact reviewed source,
commit this checklist, push the isolated branch, open the pull request, and
require the PR's Container Scan job to pass before publishing deploy clearance
and routing the merge request. Merge authority remains with mayor/mpr; the
deployer does not merge.
