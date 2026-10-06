# Release Gate: ga-imtb36.2 - routed_to resolver clean branch

- Bead: `ga-imtb36.2` - Deploy clean routed_to resolver release branch
- Source review bead: `ga-7fqxay`
- Branch under gate: `deploy/ga-imtb36-routed-to-resolver-clean`
- Base: `origin/main` at `dd8730a9c30821ea7ed6555505b1524cbaa5d2fa`
- Candidate head before this gate: `202a64eb0013cd5457d883060955c581909ced89`
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses the deployer release criteria from the active role prompt plus `TESTING.md`.

## Candidate Diff

`git log origin/main..HEAD --oneline` before this gate:

```text
202a64eb0 fix(config): collapse PoolName in DefaultSlingQuery (ga-7fqxay)
57991b77c fix(routing): centralize gc.routed_to derivation, make doctor --fix reconcile drift
```

`git diff --stat origin/main..HEAD` before this gate:

```text
 cmd/gc/agent_build_params.go           |   6 +-
 cmd/gc/cmd_convoy_dispatch.go          |   3 +-
 cmd/gc/cmd_hook.go                     |   9 +-
 cmd/gc/cmd_sling.go                    |   2 +-
 cmd/gc/doctor_routed_to_checks.go      | 163 +++++++++++++++++++++++----------
 cmd/gc/doctor_routed_to_checks_test.go | 145 +++++++++++++++++++++++++++++
 cmd/gc/work_query_probe.go             |   3 +-
 internal/agentutil/resolve.go          |  25 ++++-
 internal/agentutil/resolve_test.go     |  41 +++++++++
 internal/config/config.go              |   6 +-
 internal/config/config_test.go         |  16 +++-
 internal/graphroute/graphroute.go      |   6 +-
 internal/sling/sling.go                |   2 +-
 internal/sling/sling_attachment.go     |   2 +-
 internal/sling/sling_core.go           |   2 +-
 internal/sling/sling_test.go           |  34 +++++++
 16 files changed, 396 insertions(+), 69 deletions(-)
```

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-7fqxay` notes contain re-review verdict `PASS` for commit `c93d9a7bf` after the first review's medium `DefaultSlingQuery` finding was fixed. Single-pass review is sufficient while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | `ga-imtb36.1` produced the clean release branch from current `origin/main`, recorded branch/head/log/changed files, and verified no behavior drift from the reviewed range. This gate independently confirmed the release branch contains only the two routed_to resolver commits listed above. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed in a private `/tmp` namespace (`bwrap --tmpfs /tmp`) with short `/var/tmp/gf.*` `TMPDIR`, after initial host-environment attempts exposed the shared `/tmp` tmpfs at 100% and Unix socket path-length artifacts. `go vet ./...` clean. `go build ./...` clean. `gofmt -l` over all 16 changed Go files clean. `git diff --check origin/main..HEAD` clean. |
| 4 | No high-severity review findings open | PASS | `ga-7fqxay` had one medium finding in the first review; the fix was applied in `c93d9a7bf` and the re-review verdict is PASS. No unresolved HIGH findings are present in the review notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` before writing this gate reported only `## HEAD (no branch)` in the detached gate worktree. The only deployer-authored change is this release gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` passed, `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` produced no conflicts, and `git diff --check origin/main..HEAD` passed. |
| 7 | Single feature theme | PASS | The diff is one release theme: centralizing `gc.routed_to` derivation via `agentutil.RoutedToIdentity`, making `doctor --fix` reconcile v2 routed_to namespace drift, and the directly related `DefaultSlingQuery` PoolName-collapse regression fix. The unrelated pool-resume readiness commits `3ed3a448b` and `b9f3c4239` are not ancestors of this branch (`git merge-base --is-ancestor` returned non-zero for both). |

## Bead Acceptance

| Acceptance | Result | Evidence |
|------------|--------|----------|
| Wait for `ga-imtb36.1` clean branch details | PASS | `ga-imtb36.1` is closed and recorded branch `deploy/ga-imtb36-routed-to-resolver-clean`, head `202a64eb0013cd5457d883060955c581909ced89`, and fork push evidence. |
| Run standard deploy gate and verify single feature theme | PASS | Release criteria above are all PASS; criterion 7 records the theme and exclusion checks. |
| Confirm unrelated pool-resume readiness changes are excluded | PASS | Candidate log contains only `57991b77c` and `202a64eb0`; `3ed3a448b` and `b9f3c4239` are absent from the candidate ancestry. |
| Record build, smoke, vet, and test evidence | PASS | Evidence is recorded in criterion 3. |
| On PASS, open/update PR and route merge request | PENDING | To be completed after committing and pushing this gate file. |
| On FAIL, route back to PM | PASS | Not applicable: no failed gate criterion. |

## Push Target

- `git push --dry-run --no-verify origin HEAD:refs/heads/deploy/ga-imtb36-routed-to-resolver-clean` succeeded, so the push target is `origin`.
- `git push --dry-run --no-verify fork HEAD:refs/heads/deploy/ga-imtb36-routed-to-resolver-clean` reported `Everything up-to-date`; the original clean branch also exists on `fork`.
