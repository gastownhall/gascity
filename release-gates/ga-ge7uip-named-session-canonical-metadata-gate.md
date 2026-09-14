# Release gate: named-session canonical metadata

- Deploy bead: `ga-ge7uip`
- Build bead: `ga-m42o8w`
- Review bead: `ga-ci4hcs`
- Reviewed commit: `8c554d34521d341e485caee89c160cf8c66095ad`
- Base checked: `origin/main@bd84f0172a5bc91097b262d8ba102f48ec01d96f`
- Gate result: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-ci4hcs` is closed with an explicit `REVIEW VERDICT: PASS` for the exact reviewed commit. |
| 2 | Acceptance criteria met | **PASS** | When city config is available, both started and bead-only creation derive named-session identity from the configured agent table, overwrite false caller claims, and stamp or remove both canonical metadata keys from the derived result. The production API and CLI worker/session factory construction sites pass the already-loaded city config through. The documented config-unavailable fallback retains legacy behavior. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full local CI union ran all 40 jobs with rootless Podman enabled: **46,931 PASS / 8 FAIL / 189 SKIP**. The three diff-owned tests each passed in both `unit-core` and `integration-packages-core-1-of-4`; none skipped. All eight raw failures are preserved and attributed below to open trackers that predate the run. The 189 skips are pre-existing platform, explicit opt-in, credential, or integration-environment guards; no diff-owned test skipped. |
| 4 | No high-severity review findings open | **PASS** | Reviewer reported no requested changes and no unresolved HIGH findings. The separately observed API race failures were reproduced on clean main and tracked as `ga-fu58no`; they are outside this diff. |
| 5 | Final branch is clean | **PASS** | `git diff --check origin/main...HEAD` passed. Dashboard/OpenAPI regeneration left no source drift; the gate file is the only intentional deploy-only addition and is committed on the isolated branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching `origin/main@bd84f0172a5bc91097b262d8ba102f48ec01d96f`, `git merge-tree --write-tree origin/main 8c554d34521d341e485caee89c160cf8c66095ad` completed without conflict and produced tree `7f81ddfe8f6345b34af12f5f2e6773af406a7d9e`. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The two reviewed commits and six changed files form one named-session metadata ownership change: derive canonical identity at session creation and wire authoritative config to every production creation path. |

## Test evidence

`test_cmd`:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-ge7uip-full-gate make test-local-full-parallel
```

- `test_cmd_scope: full-suite`
- `test_counts: 46,931 PASS / 8 FAIL / 189 SKIP`
- `diff_tests_executed:`
  - `TestCreateStarted_DerivesConfiguredNamedSessionFromCityConfig` — PASS in `unit-core` and `integration-packages-core-1-of-4`
  - `TestCreateStarted_CityConfigOverridesFalseExtraMetaClaim` — PASS in both jobs
  - `TestCreateBeadOnly_DerivesConfiguredNamedSessionFromCityConfig` — PASS in both jobs
- `waiver_ref: none` for diff-owned tests
- `skip_justification:` all skips are existing platform/build-tag, explicit opt-in, credential, real-tmux, or unavailable-integration guards. No test added or modified by this diff skipped.

### Raw failure attribution

The suite's non-zero exit is preserved. Criterion 3 passes only because every raw failure satisfies the repository's non-diff-owned failure rule.

| Raw failure | Disposition | Evidence |
|---|---|---|
| `TestCatalogMatchesProductionWiringAndDocumentation` (two executions) | FAIL — ATTRIBUTED to `ga-1s16pf` | Clause 3(a), mechanism: the result is determined by the provider-ledger waiver catalog's date (`2026-08-26`). This diff has no providerledger path overlap. |
| `TestBdFlagManifestCurrent` | FAIL — ATTRIBUTED to `ga-f0uceo` | Clause 3(a), mechanism: installed `bd` flags exceed the checked manifest. The diff changes neither the installed binary nor `internal/bdflags`. |
| `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | FAIL — ATTRIBUTED to `ga-afqddr` | Clause 3(a), mechanism: host tmux 3.7b returns an empty default binding. The diff does not touch `internal/runtime/tmux` or host tmux configuration. |
| `TestHumaBinary_CityCreateAsync`, `TestCleanInstallTutorialPath`, and `TestGCLiveContract_BeadsAndEvents` | FAIL — WAIVED under `ga-6bnc42`, occurrence logged on `ga-lpfjhc` | Clause 3(a), mechanism: each fails during fixture city/store bootstrap with the exact `gastownhall/beads#4566` dirty-table schema-migration signature, before the tested behavior begins. This diff cannot alter Dolt schema migration or store bootstrap. The standing authorization requires the raw failures to remain visible as FAIL-WAIVED, as they do here. |

For all eight failures: clause 1 passes (not diff-owned), clause 2 passes (the cited tracker predates this run and was opened during evaluation), and clause 4 passes (no failing test package/path overlaps the candidate files). The candidate does not change a resource census or add a test target (`added_test_load=no`). Tracker occurrence notes and the complete attribution were recorded on the beads before any push.

## Additional required lanes

- `policy_lane: make test-ci-policy — PASS`
- `go vet ./...` — PASS
- `lint-affected` with a fresh on-disk cache and `LINT_CHANGED_SCOPE=tracked` — PASS, 0 issues
- `fmt-check-changed` — PASS
- `make dashboard-ci` — PASS (SPA build, TypeScript checks, dashboard Go tests, generated client check)
- `make spec-ci` — PASS (OpenAPI and generated Go client remained in sync)
- `git diff --check origin/main...HEAD` — PASS

## Acceptance audit

- Authoritative config derivation uses the existing `FindNamedSessionSpec` lookup and existing metadata constants; it does not duplicate the bead-scanning resolution path.
- A false `ExtraMeta` ownership claim is overridden when city config is present.
- Started and bead-only creation use the same resolver and metadata normalization.
- Production session-creation constructors in the API and CLI receive city config; transcript-only factories remain read-only and do not need it.
- The change stays within the existing config/session/worker boundary, introduces no dependency, endpoint, wire-shape, or migration change, and leaves config-unavailable behavior explicit.
