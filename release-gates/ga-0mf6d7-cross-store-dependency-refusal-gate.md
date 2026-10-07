# Release gate: fail loudly on cross-store dependency writes

- Deploy bead: `ga-0mf6d7`
- Build bead: `ga-q5dgaz`
- Review bead: `ga-ae9jru`
- Reviewed source: `cc456bed7e2da24ba90025065eeef3466b20f0d1`
- Base: `origin/main@93e5d31b40dcf3bb31f3331a507b7bb50a4c2ed5`
- Isolated branch: `deploy/ga-0mf6d7-gate`
- Evaluated: 2026-09-10

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-ae9jru` is closed with `verdict: pass` and pins the exact reviewed commit above. No review carryover was used. |
| 2 | Acceptance criteria met | **PASS** | `TestBdStoreDepAddCrossStoreFailsLoudly` proves a well-formed cross-store pair returns an error naming both IDs and prefixes before the underlying write. `TestBdStoreDepAddSameStoreStillSucceeds`, `TestBdStoreDepAddExternalTargetIsNotCrossStore`, and the pre-existing `TestBdStoreDepAddParentChildAlreadyParentedIsNoop` prove the supported/no-op cases remain intact. All reported PASS in both the unit and integration package sweeps. The implementation adds only a fail-loudly guard; it does not introduce a cross-store dependency model. |
| 3 | Tests pass | **PASS** | The documented full-scope command `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel` completed **38/40 jobs green**, with **46,035 PASS / 2 raw FAIL / 209 SKIP** top-level test executions. Both raw failures are attributed below under criterion 3a. `test_cmd_scope: full-suite`. `diff_tests_executed: TestBdStoreDepAddCrossStoreFailsLoudly PASS; TestBdStoreDepAddSameStoreStillSucceeds PASS; TestBdStoreDepAddExternalTargetIsNotCrossStore PASS` (each twice: unit-core and integration-packages-core). `waiver_ref: none`. `ci_lane_run: n/a (no CI-config change)`. Rootless Podman 5.8.4 was active before the run; Ryuk was disabled; cached `dolthub/dolt-sql-server:1.32.4` matched the `testcontainers-go/modules/dolt@v0.43.0` default. Full log: `/var/tmp/gc-deploy-ga-0mf6d7-full.hzbNqg`; shard logs: `/var/tmp/gc-local-tests.RgLzsC`. |
| 3a | Pre-existing failures attributed | **PASS** | `failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a), mechanism — the failure is installed-bd help flags absent from the repository's bdflags manifest; this diff changes neither that manifest nor the installed binary. Tracker predates this run; no failing-test path overlap. Verified sighting comment `c094e16b-b04c-5532-b73a-270c4fb1891c`. failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn | clause 3(a), mechanism — the tracker independently proves the city-suspend desired-state/reconciler retirement path; this diff changes only dependency-prefix validation and does not touch or invoke that mechanism. Tracker predates this run; no failing-test path overlap. Verified sighting comment `49f625e4-0717-53a9-bd56-0e17cc861eca`. The diff changes neither resource census nor build targets and adds no test load outside its existing package. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` PASS. `make test-native-doltlite-beads` PASS. `make test-bd-cli-contract` PASS. `make test-bd-conditional-release-contract` PASS. On a synthetic merge of the reviewed source into current main, `GOLANGCI_LINT_CACHE=<fresh on-disk cache> LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=<merge first parent> make lint-affected` reported `0 issues`, and `make fmt-check-changed` PASS. The initial direct-branch lint attempt was not scored: current main made a base-added embed path appear deleted, forcing full scope, while the shared analyzer cache returned diagnostics for already-deleted temporary worktrees. The clean-cache synthetic-merge run matches CI's checkout and cache shape. `go build ./...` PASS; `go vet ./...` PASS; `make check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary check-docs` PASS (native guard: 727 modules, 173,201,420-byte binary). |
| 4 | No high-severity review findings open | **PASS** | Reviewer recorded no style or security findings, no uncovered acceptance criterion, and `verdict: pass`; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | Gate checklist committed on the isolated deploy branch; `git status --porcelain` was empty afterward. Hooks path is `.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | After a final fetch, `git merge-tree --write-tree origin/main cc456bed7e2da24ba90025065eeef3466b20f0d1` returned 0 against `origin/main@93e5d31b40dcf3bb31f3331a507b7bb50a4c2ed5`, producing tree `4ea52ecfb0e40adf0c6184089d64a50e367d5097`. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The reviewed range changes only `internal/beads` dependency-prefix validation and its regression tests: one cohesive behavior, making unsupported cross-store `DepAdd` calls fail visibly while preserving same-store and external-target behavior. |

## Skip justification

The 209 skips are suite-controlled opt-ins and platform/provider exclusions already present outside this diff, including live provider/Kubernetes cases, persistence tests requiring explicit compatible-bd opt-in, Darwin/root-only checks, and helper processes. None of the three diff-owned tests skipped; each reported PASS twice in the full-suite output.

## Additional audit evidence

- `git diff --stat origin/main...cc456bed7e2da24ba90025065eeef3466b20f0d1`: 3 files, 95 insertions, 5 deletions.
- Ancestry scope guard PASS for accepted bead `ga-q5dgaz`; no `.claude/**` path and no uncited commit in the source range.
- No existing PR carried the reviewed commit at final preflight.
- No `PR-DESCRIPTION` block, `gc.pr_ping`, or unhandled PR-open instruction was present.
