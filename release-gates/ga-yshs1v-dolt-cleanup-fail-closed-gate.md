**Verdict:** **PASS**

# Release Gate: fail-closed Dolt cleanup registry enumeration

- Deploy bead: `ga-yshs1v`
- Reviewed source: `da38d7dc04363aff37c9b0f140ceee0f6f78f7cc`
- Source merge base: `44bba224c32382f6dd058237012d145690e98640`
- Base evaluated: `origin/main@849cb1a3978baf3eac0cfce5258c5598b2d0eecd`
- Deploy mode: remote
- Date: 2026-09-21

The already-merged preflight found no base-repository pull request carrying the
reviewed source. Criterion 6 passed first, so no bounded self-rebase was needed.
`docs/PROJECT_MANIFEST.md` is absent at this source; this gate uses the deployer
release criteria together with `TESTING.md`, the Makefile, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | The bead records a round-5 exact-SHA reviewer PASS for `da38d7dc04363aff37c9b0f140ceee0f6f78f7cc`. The reviewer independently verified the functional diff, the additive census reconciliation, and all six exit-contract items. |
| 2 | Acceptance criteria met | PASS | `metadata_files` consumes the same validated allowlist produced by `compute_allowlist_file` and does not issue a second registry query; failed/unparseable registry enumeration cannot fall back to a partial local scan while `gc` is present; `--force` exits before destructive work when the allowlist is unverified; dry-run marks every row `unverified`; the two new regression tests prove the dry-run and destructive cases; max safety, SQL-vs-filesystem deletion selection, identifier safety, and exit-code aggregation remain intact. |
| 3 | Tests pass | PASS WITH ATTRIBUTED RAW FAILURES | The documented full-scope `make test-local-full-parallel` command ran all 40 jobs through the isolation wrapper with rootless Podman: **34 PASS jobs, 6 attributed raw FAIL jobs, 0 omitted jobs**. Five managed-Dolt readiness timeouts and one provider-owned Dolt reachability failure satisfy criteria 3a's four clauses under tracker `ga-vkhfnj` and an exact cross-PR reproduction on unrelated candidate `ga-bequ8d`; details are below. The entire diff-owned `examples/bd/dolt` package and both new tests passed by name, with 0 FAIL and 0 SKIP. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `make lint-new`, `make vet`, `make check-docs`, `make check-hooks`, changed-file `gofmt`, and `git diff --check` all passed. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI job, matrix, timeout, workflow, or required-check configuration changed)`. |
| 4 | No high-severity review findings open | PASS | The round-5 exact-SHA review found no issues. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | Before this checklist was created, `git status --porcelain=v1` was empty; `git diff --check` passed; changed Go files were `gofmt`-clean. |
| 6 | Branch diverges cleanly from main | PASS | After a final fresh fetch, `git merge-tree --write-tree origin/main da38d7dc04363aff37c9b0f140ceee0f6f78f7cc` exited 0 and produced tree `da0f6e4519e533fe1928e6a7f364e474c43f2cfe` against unchanged `origin/main@849cb1a3978baf3eac0cfce5258c5598b2d0eecd`. |
| 7 | Single feature theme | PASS | The five-file change makes one cleanup safety path fail closed, adds its regression coverage, and synchronizes the checked resource-census declarations required by that test. No independent feature is bundled. |

## Acceptance evidence

1. `compute_allowlist_file` runs once before metadata enumeration and writes
   one non-HQ rig path per line. `metadata_files` reads this file when the
   allowlist is valid, so both name and path protection use one registry view.
2. If `gc` exists but `gc rig list --json` fails or is unparseable,
   `allowlist_ready=false`; `metadata_files` returns after the HQ metadata path
   and cannot substitute the incomplete local `find` scan. The local scan is
   retained only for the documented no-`gc` fallback.
3. `--force` checks `allowlist_ready` and exits before the deletion strategy or
   removal loop.
4. Dry-run assigns `unverified: rig registry query failed; not confirmed
   orphan` to every printed row when enumeration was not verified.
5. `TestCleanupDryRunAnnotatesUnverifiedOnRegistryFailure` and
   `TestCleanupForceRefusesOnRegistryFailure` both passed by name. The second
   confirms the candidate database remains on disk after refusal.
6. Inspection of the remaining script confirms no substantive change to
   `--max`, the four-state SQL/filesystem deletion decision, database-name
   validation, or aggregate removal failure handling.
7. `TestRepositoryLedgerMatchesCensusAndDocumentation` and the full
   `internal/testpolicy/resourcecensus` package passed, proving the synchronized
   `+2 calls / +1 file` subprocess deltas match the live source census.

## Full-suite evidence

Environment and command:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
test_cmd: make test-local-full-parallel
test_cmd_scope: full-suite
```

The command ran through the gate's isolation wrapper after confirming the
rootless Podman socket and cached Dolt images. Job logs are preserved at
`/var/tmp/gc-local-tests.JAX7X5`.

- `test_counts: 34 PASS jobs, 6 attributed raw FAIL jobs, 0 skipped/omitted jobs`
- all six `cmd/gc` process shards: PASS
- all four core integration shards: PASS
- all six integration `cmd/gc` shards: PASS
- all three runtime/tmux shards: PASS
- all REST shards except the one attributed tutorial-path failure: PASS
- `diff_tests_executed: TestCleanupDryRunAnnotatesUnverifiedOnRegistryFailure PASS; TestCleanupForceRefusesOnRegistryFailure PASS; 0 FAIL; 0 SKIP`
- `go test -v ./examples/bd/dolt/... -count=1`: PASS
- `go test -v ./internal/testpolicy/resourcecensus/... -count=1`: PASS
- `skip_justification: none; zero full-suite jobs and zero diff-owned tests skipped`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`

### Criterion 3a: attributed raw failures

All six failures are not diff-owned. Their files are under `test/integration`;
the candidate changes the cleanup script and test under `examples/bd/dolt`
plus synchronized resource-census declarations. A direct search found no
reference from `test/integration` to the changed cleanup script, satisfying the
no-path-overlap and reachability checks.

The pre-existing gate tracker `ga-vkhfnj` covers whole-suite/host-load Dolt
contention. It was opened and predates this run; this run's complete sighting
was appended and read back. Clause 3 has a landed cross-PR proof: the immediately
preceding `ga-bequ8d` gate at unrelated candidate
`c15cfdd19228c3ff7cc2e29857ab4d95ffeca060` reproduced all six exact tests and
signatures in `/var/tmp/gc-local-tests.95ZqDM`. That candidate changed
`cmd/gc/build_desired_state.go` and `internal/session`, sharing no path with
this candidate.

- `TestAdoptPRFormulaCompileAndRun` -> `ga-vkhfnj` | clause 3: CROSS-PR — exact 20-second managed-Dolt readiness timeout on unrelated `ga-bequ8d` candidate.
- `TestPersonalWorkFormulaCompileAndRun` -> `ga-vkhfnj` | clause 3: CROSS-PR — same proof.
- `TestAdoptPRFormulaRetriesTransientReviewerStep` -> `ga-vkhfnj` | clause 3: CROSS-PR — same proof.
- `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` -> `ga-vkhfnj` | clause 3: CROSS-PR — same proof.
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` -> `ga-vkhfnj` | clause 3: CROSS-PR — same proof.
- `TestCleanInstallTutorialPath` -> `ga-vkhfnj` | clause 3: CROSS-PR — exact provider-owned Dolt server reachability failure on the unrelated `ga-bequ8d` candidate.

`clause-4-guard: same_package=no; failing test files and packages do not overlap
the diff. The candidate adds test load, but clause 3 is conclusively established
by cross-PR reproduction rather than the inconclusive attribution path.`

## Policy and static evidence

- `make test-ci-policy` — PASS.
- `make lint-new` — PASS (`0 issues.`; one stale external-temp-file filter warning only).
- `make vet` — PASS.
- `make check-docs` — PASS.
- `make check-hooks` — PASS (`core.hooksPath` is `.githooks`).
- changed-file `gofmt -l` — no output.
- `git diff --check origin/main...HEAD` — PASS.

## Decision

**Gate PASS.** Commit this checklist on the isolated
`deploy/ga-yshs1v-gate` branch, push it without force, open a pull request
against `gastownhall/gascity:main`, publish deploy clearance on the exact PR
head, and route the merge request to the merge authority. The deployer does
not merge.
