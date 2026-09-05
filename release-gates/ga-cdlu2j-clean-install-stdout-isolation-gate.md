# Release gate: clean-install stdout isolation

- Deploy bead: `ga-cdlu2j`
- Reviewed source: `3b9824e5ae5129d5e3d4b60b885ab80e4a534278`
- Base: `origin/main@e5ed16b566302e5cd9bdbb7d75ff47bd52a8cbfc`
- Review: `ga-hash7u` (PASS, round 2)
- Gate verdict: **PASS**
- Deploy mode: remote (`gastownhall/gascity`, push through the configured GitHub remote)

The repository does not contain `docs/PROJECT_MANIFEST.md` at either the base
or reviewed commit, so this checklist applies the seven release criteria in the
active `mol-deployer-gate` formula and deployer prompt.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-hash7u` records `verdict: pass` for the resolved reviewed commit. |
| 2 | Acceptance criteria met | **PASS** | The integration harness now separates stdout from stderr for value-bearing commands, retains stderr in command errors, narrowly retries only the known Dolt dirty-table migration race, and keeps retries bounded at three attempts. `TestCleanInstallTutorialPath` passes end to end, the two stdout contract tests pass, both retry-policy tests pass, and `TestRepositoryLedgerMatchesCensusAndDocumentation` passes with no resource-census baseline increase. |
| 3 | Tests pass | **PASS** | `LOCAL_TEST_JOBS=4 EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' make test-local-full-parallel` ran all 40 documented local CI-equivalent jobs: **37 PASS, 3 attributed FAIL, 0 SKIP**. All six process-backed `cmd/gc` shards, the product-metrics testhook, unit core, all six `cmd/gc` integration-package shards, all review-formula lanes, bdstore, both REST smoke shards, and all eight REST full shards passed. The three failing test names satisfy all four criterion-3a attribution clauses below. |
| 3a | Pre-existing failures attributed | **PASS** | `TestBdFlagManifestCurrent` is tracked by `ga-f0uceo` and fails identically with `-tags integration` at the base tip because the installed `bd` exposes flags absent from the repository manifest. `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` are tracked by `ga-afqddr` and both fail identically at the base tip because host tmux 3.7b returns empty built-in default bindings. The diff touches only `test/integration/integration_test.go` and `test/integration/tutorial_path_test.go`, so neither failing package or test file is diff-owned and neither has path overlap. |
| 3b | Policy and lint lanes | **PASS** | `make test-ci-policy`; `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=e5ed16b566302e5cd9bdbb7d75ff47bd52a8cbfc make lint-affected`; matching `make fmt-check-changed`; `go vet ./...`; `go vet -tags integration ./test/integration/...`; and the preflight guards `check-gomod-replace`, `check-native-dependency-surface`, `check-eventexport-isolation`, `check-core-boundary`, `test-native-doltlite-beads`, and `check-docs` all pass. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-hash7u` records no style, security, or specification findings; unresolved HIGH findings: **0**. |
| 5 | Final branch clean | **PASS** | The reviewed source was evaluated from a clean detached checkout. The isolated deploy branch is clean after committing this checklist. |
| 6 | Branch diverges cleanly from main | **PASS** | The mandatory preflight found no PR carrying the reviewed SHA. `origin/main` is an ancestor of the reviewed source; `git merge-tree --write-tree origin/main 3b9824e5ae5129d5e3d4b60b885ab80e4a534278` completed without conflicts and produced tree `636d45d4d795ad6a853e658c9ba67e21dc5fa00f`. The ancestry-scope guard passes for the confirmed same-feature chain `ga-rsktma`, `ga-38xsx4`, and `ga-yntbkv`; no bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | All six source commits modify only the two integration-harness test files and form one correction chain: keep parsed stdout clean, preserve diagnostics, and make the affected clean-install regression test reliable under the known Dolt migration race. |

## Test evidence details

The rootless Podman socket was reachable before testing. The repository-pinned
`docker.io/dolthub/dolt-sql-server:2.1.7` image was pulled and resolved to image
ID `678aa242be2fbc6f491b3c4b36c33d50dce926f1f908fe9220620a5cb6f7c03a`.
The shell `HOME` and operating-system user home both resolved to
`/home/jaword`, avoiding the supervisor mismatch that affected earlier runs.

Focused command:

```text
go test -count=1 -tags=integration -run '^(TestDoltDirtyTableMigrationRaceRetryableMatchesKnownSignatureOnly|TestRetryOnDoltDirtyTableMigrationRaceRetriesOnlyKnownSignature|TestRunCommandStdoutExcludesStderr|TestRunCommandStdoutIncludesStderrInErrorOnFailure|TestCleanInstallTutorialPath)$' -v ./test/integration/...
```

- `test_counts`: **5 PASS, 0 FAIL, 0 SKIP** for diff-owned tests.
- `diff_tests_executed`: `TestDoltDirtyTableMigrationRaceRetryableMatchesKnownSignatureOnly` PASS; `TestRetryOnDoltDirtyTableMigrationRaceRetriesOnlyKnownSignature` PASS; `TestRunCommandStdoutExcludesStderr` PASS; `TestRunCommandStdoutIncludesStderrInErrorOnFailure` PASS; `TestCleanInstallTutorialPath` PASS.
- `skip_justification`: none; the full sweep and focused diff-owned run reported zero skips.
- `waiver_ref`: none.
- `failure_attribution`: `TestBdFlagManifestCurrent -> ga-f0uceo + identical base-ref failure`; `TestGetKeyBinding_CapturesDefaultBinding -> ga-afqddr + identical base-ref failure`; `TestGetKeyBinding_CapturesDefaultBindingWithArgs -> ga-afqddr + identical base-ref failure`.
- `policy_lane`: all commands listed in criterion 3b PASS.
