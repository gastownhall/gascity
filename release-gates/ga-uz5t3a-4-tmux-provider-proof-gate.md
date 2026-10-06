# Release Gate: tmux runtime provider conformance proof

**Verdict:** **PASS**

- Deploy bead: `ga-uz5t3a.4`
- Reviewed source: `12d4c10b568aef47fc77704d3e1a137c39b9bd4d`
- Source merge base: `48d68d13a77647eedeeabe772f362a32954a53b2`
- Base evaluated: `origin/main@1958a106f5881aea273244d9c7ab81a6b7c9f982`
- Deploy mode: remote
- Date: 2026-09-21

The already-merged preflight found no base-repository pull request carrying the
reviewed source. Criterion 6 passed first, so no bounded self-rebase was needed.
`docs/PROJECT_MANIFEST.md` is absent at this source; the gate therefore uses the
deployer release criteria together with `TESTING.md`, the Makefile, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | The bead records `REVIEWER VERDICT: PASS` for the exact source `12d4c10b568aef47fc77704d3e1a137c39b9bd4d`, with no blocking findings. |
| 2 | Acceptance criteria met | PASS | `TestTmuxConformance` directly calls `runtimetest.RunProviderTests`; its factory directly returns `NewSeamBackedWithConfig(tmuxConformanceConfig())`; the proof contains no short-mode, executable-availability, degraded-start, or other skip; `tmuxConformanceConfig` retains the isolated `testSocketName`; the provider ledger and generated `TESTING.md` now identify this exact proof. The full suite and focused confirmations below pass the affected paths. |
| 3 | Tests pass | PASS WITH ATTRIBUTED RAW FAILURES | The documented full-scope command `make test-local-full-parallel` ran all 40 jobs through the isolation wrapper with rootless Podman: **36 PASS jobs, 4 raw FAIL jobs, 0 skipped/omitted jobs**. All three runtime/tmux shards passed. The modified `TestTmuxConformance` was selected in shard 2 and the package passed; a fresh exact-name integration run also passed every subtest with 0 FAIL and 0 SKIP. All four raw failures are non-diff-owned and satisfy criteria 3a's four proofs below. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `make lint-new`, `make vet`, `make check-docs`, changed-file `gofmt`, `git diff --check`, and the configured `.githooks` ownership check all passed. The reviewed source predates the later `make check-hooks` target, so hooks ownership was verified directly with `git config --get core.hooksPath` = `.githooks`. |
| 3c | CI-config lane | PASS | `ci_lane_run: n/a (no CI job, matrix, timeout, workflow, or required-check configuration changed)`. |
| 4 | No high-severity review findings open | PASS | The reviewer reported no blocking correctness, safety, architecture, or coverage findings. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | Before this checklist was created, `git status --porcelain=v1` was empty; `git diff --check` passed; changed Go files were `gofmt`-clean. |
| 6 | Branch diverges cleanly from main | PASS | After a fresh fetch, `git merge-tree --write-tree origin/main 12d4c10b568aef47fc77704d3e1a137c39b9bd4d` exited 0 and produced tree `40d0dfd25d37b828c548f5d89e52ae55d06bb4ad` against `origin/main@1958a106f5881aea273244d9c7ab81a6b7c9f982`. |
| 7 | Single feature theme | PASS | The three-file change promotes one existing tmux conformance suite into the exact provider-ledger proof shape and synchronizes its generated documentation. No independent feature is bundled. |

## Acceptance evidence

1. `internal/runtime/tmux/adapter_test.go` calls
   `runtimetest.RunProviderTests` directly. The inline factory's first return
   value is a direct `NewSeamBackedWithConfig(tmuxConformanceConfig())` call.
2. The proof no longer checks `testing.Short`, tmux executable availability,
   `ErrServerDegraded`, or `SkipStartError`; it contains no skip path.
3. `tmuxConformanceConfig` starts from `DefaultConfig`, sets
   `SocketName = testSocketName`, and retains the existing package-owned socket
   cleanup. No changed command targets the default tmux server.
4. `runtime.builtin.tmux` changed from an expiring `waivedRuntime` entry to
   `provedRuntime`, naming
   `internal/runtime/tmux/adapter_test.go#TestTmuxConformance`.
5. The checked runtime-provider table in `TESTING.md` contains the same exact
   proof reference.
6. Fresh focused verification passed:
   - `go test -tags=integration ./internal/runtime/tmux/... -run '^TestTmuxConformance$' -v -count=1`
   - `go test ./internal/testutil/providerledger/... -run '^TestCatalogMatchesProductionWiringAndDocumentation$' -v -count=1`

## Full-suite evidence

Environment and command:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock
TESTCONTAINERS_RYUK_DISABLED=true
test_cmd: make test-local-full-parallel
test_cmd_scope: full-suite
```

The command ran through the gate's isolation wrapper after confirming the
rootless Podman socket and pinned container image. Job logs are preserved at
`/var/tmp/gc-local-tests.Tt6uJO`.

- `test_counts: 36 PASS jobs, 4 attributed raw FAIL jobs, 0 skipped/omitted jobs`
- `diff_tests_executed: TestTmuxConformance PASS, 0 FAIL, 0 SKIP`
- all three `integration-packages-runtime-tmux` shards: PASS
- `unit-core`, including the provider-ledger package: PASS
- `skip_justification: none; zero full-suite jobs and zero diff-owned tests skipped`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`

### Criterion 3a: attributed raw failures

Every tracker below was open before this run, was inspected, received this
run's sighting, and was read back. None of the failing test files overlaps the
candidate's three changed files. The candidate adds no test target or resource
census load.

- `TestGraphWorkflowSuccessPath` -> `ga-vkhfnj` (cross-PR proof). The failure
  is the tracker's fixed two-second Dolt identity-probe timeout. The same test
  and signature already occurred on unrelated candidate `ga-g0w518`.
  `go list -deps -test ./test/integration` shows the failing package imports
  neither changed Go package (`internal/runtime/tmux` or
  `internal/testutil/providerledger`), so candidate production code is
  unreachable from the failure.
- `TestHumaBinary_CityCreateAsync` -> `ga-lejnse` (cross-PR proof). The failure
  occurred during external `bd` initialization on the tracked shared-Dolt
  schema refusal (`v47 -> v66`), before feature behavior. The identical test
  and signature occurred in the immediately preceding unrelated `ga-grxrlh`
  gate.
- `TestHumaBinary_SessionMessageAsync` -> `ga-lejnse` (same root condition and
  cross-PR proof). It failed during external `bd` initialization on the same
  tracked shared-Dolt schema refusal (`v40 -> v66`), before feature behavior.
- `TestE2E_SuspendResume_City` -> `ga-dc9utn` (mechanism and cross-PR proof).
  It reproduced the tracker's exact missing `citysus.report` timeout at 93.85s.
  The tracker identifies the reconciler suspend/wake path as the root
  condition, while this candidate changes only a tmux test, provider-ledger
  catalog data, and generated test documentation.

`failure_attribution: TestGraphWorkflowSuccessPath -> ga-vkhfnj;
TestHumaBinary_CityCreateAsync + TestHumaBinary_SessionMessageAsync ->
ga-lejnse; TestE2E_SuspendResume_City -> ga-dc9utn (all clauses: not
diff-owned, predating tracker, independent mechanism/cross-PR proof, and no
path overlap)`.

`inconclusive-guard: reachable_production_code=no for the only changed Go
production packages from test/integration; added_test_load=no`.

## Policy and static evidence

- `make test-ci-policy` — PASS.
- `make lint-new` — PASS (`0 issues.`).
- `make vet` (`go vet ./...`) — PASS.
- `make check-docs` — PASS.
- changed-file `gofmt -l` — no output.
- `git diff --check origin/main...HEAD` — PASS.
- `git config --get core.hooksPath` — `.githooks`.
- `go test ./internal/testutil/providerledger/... -run '^TestCatalogMatchesProductionWiringAndDocumentation$' -v -count=1` — PASS.

## Decision

**Gate PASS.** Commit this checklist on the isolated
`deploy/ga-uz5t3a.4-gate` branch, push it without force, open a pull request
against `gastownhall/gascity:main`, publish deploy clearance on the exact PR
head, and route the merge request to the merge authority. The deployer does
not merge.
