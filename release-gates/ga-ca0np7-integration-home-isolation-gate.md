# Release Gate: integration HOME isolation ancestry-fix retry

- Deploy bead: `ga-ca0np7`
- Review bead: `ga-20x17h`
- Reviewed source: `254dbabb5b3295ce800870576cdef4a6791ab2d5`
- Evaluated source after bounded self-rebase: `c0358b4fadb06fea51301c1ab4a3bb119656d9ba`
- Base: `origin/main@7c817e0640fae801631043005f1d54b17ce3e97c`
- Deploy branch: `deploy/ga-20x17h-gate`
- Verdict: **PASS — RAW FAILURES PRESERVED AND WAIVED**

The ancestry-fix retry resolves the prior traceability rejection: both commits
in the evaluated range cite accepted source beads. The mayor's standing
`ga-lpfjhc` authorization waives the nine exact beads#4566 failures after a
mechanism trace established that this diff cannot alter Dolt's explicitly set
config/schema root. The tenth integration failure,
`TestDoltConfigWiringExternalHost`, has a different signature and is preserved
as a raw FAIL, but the mayor granted the narrow waiver
`mayor-2026-08-20-ga-ca0np7-c3`. That waiver rests on empirical
non-causality, not on mechanism separation: `dolt_config_test.go:34` obtains
its env from `newIsolatedToolEnv` -> `newIsolatedEnvRoot` ->
`integrationEnvFor`, so the changed function is on this test's call path. A
matched-pair causality probe re-run at the gate head found the test clean both
with and without `pinRealHomeEnv` applied (see "Causality probe" below). The
release may proceed; none of these failures is rewritten green.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-20x17h` records `verdict: pass` for reviewed source `254dbabb5b`; it independently verified the identical reviewed tree, the ancestry-fix message, style, security, and targeted tests. |
| 2 | Acceptance criteria met | **PASS** | The combined change pins real HOME only for `gc`/supervisor subprocess fixtures, re-isolates standalone `bd` subprocess HOME to the caller-owned directory, and tightens the empty-home guard. The two changed tests and three directly related tests all passed by name with zero skips. The evaluated commits cite `ga-vvnov6` and `ga-9lh4m0`, resolving the prior ancestry-scope failure. |
| 3 | Tests pass | **PASS — FAILURES WAIVED** | The required 40-job local CI union completed **27 PASS / 13 FAIL jobs**. Nine `test/integration` failures carry the exact beads#4566 signature and are preserved as **FAIL — WAIVED** under the mayor's `ga-lpfjhc` standing authorization: the builder's recorded mechanism trace shows the diff replaces only `HOME`, while every affected fixture explicitly sets unchanged `GC_HOME`/`DOLT_ROOT_PATH`, so it cannot alter schema migration or store bootstrap. `TestDoltConfigWiringExternalHost` is also preserved as **FAIL — WAIVED** under `mayor-2026-08-20-ga-ca0np7-c3`. That waiver rests on empirical non-causality, not on mechanism separation: the changed `integrationEnvFor` is on this test's call path (`dolt_config_test.go:34` -> `newIsolatedToolEnv` -> `newIsolatedEnvRoot` -> `integrationEnvFor`), and a matched-pair probe re-run at the gate head found the test clean both with and without `pinRealHomeEnv` applied (see "Causality probe"). Five additional failures are attributed to the independently tracked, structurally unrelated causes listed below. `waiver_ref: ga-lpfjhc mayor ruling 2026-08-18 for nine exact beads#4566 failures; mayor-2026-08-20-ga-ca0np7-c3 for TestDoltConfigWiringExternalHost only`. |
| 4 | No high-severity review findings open | **PASS** | `ga-20x17h` reports no style or security findings. One pre-existing low-severity fail-open branch coverage gap is explicitly non-blocking. Unresolved HIGH count: `0`. |
| 5 | Final branch is clean | **PASS** | Before adding this record, the branch was clean and tracked `origin/deploy/ga-20x17h-gate`. `git diff --check`, affected formatting, `go build ./...`, `go vet ./...`, and integration-tagged vet all passed. Repository hooks resolve to `.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | Reviewed source `254dbabb5b` rebased without conflict onto `origin/main@7c817e0640`, producing `c0358b4fad`. An initial zsh source of the bash helper failed to load its push guard only after completing the clean local rebase and therefore did not push; the correct bash invocation then returned the documented no-op result because current main was already an ancestor. The exact candidate was subsequently pushed under the recorded non-diff fast-failure attribution. |
| 7 | Single feature theme | **PASS** | Three files, one theme: `test/integration/integration_test.go` carries the harness HOME-isolation change and its tests, `scripts/test-integration-shard` wires those two tests into the per-PR `rest-smoke` manifest, and this gate record documents the evaluation. No independent feature is bundled. |

## Test evidence

Environment for the required local CI union:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`

Commands and results:

- `GC_SESSION=subprocess go test -tags integration -run '^(TestStandaloneBDEnvForDirIsolatesHome|TestIntegrationEnvForPinsRealHome|TestStandaloneBDEnvAllowsBDAutoStart|TestStandaloneBdEnvIsolatesAmbientDoltConfig|TestUsesStandaloneBDWorkspaceKeepsFileProviderOnShim)$' -v ./test/integration/... -count=1 -timeout 5m`: **5 PASS / 0 FAIL / 0 SKIP**.
- `EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true" make test-local-full-parallel`: **27 PASS jobs / 13 FAIL jobs**. Logs: `/var/tmp/gc-local-tests.6i2mXO`.
- Guarded pre-push fast gate: **9 PASS jobs / 1 FAIL — ATTRIBUTED** (`TestProviderLiveClaudeKindPath`, `ga-nqlb8q`, no `test/integration` path or mechanism overlap). Log: `/var/tmp/gc-local-tests.i4POu1/unit-core.log`.
- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `go vet -tags integration ./test/integration/...`: PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected`: PASS, 0 issues in `./test/integration`.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed`: PASS.
- `git diff --check origin/main...HEAD`: PASS.

`diff_tests_executed`:

- `TestIntegrationEnvForPinsRealHome`: PASS.
- `TestStandaloneBDEnvForDirIsolatesHome`: PASS.

Both diff-owned tests are registered in `rest_smoke_tests`
(`scripts/test-integration-shard`) and therefore execute on every PR via
`Integration / rest-smoke-{1,2}-of-2`, not only in the local focused run
above.

Related unchanged tests executed in the same focused command:

- `TestStandaloneBDEnvAllowsBDAutoStart`: PASS.
- `TestStandaloneBdEnvIsolatesAmbientDoltConfig`: PASS.
- `TestUsesStandaloneBDWorkspaceKeepsFileProviderOnShim`: PASS.

`test_counts`: required union `27 PASS jobs / 13 FAIL`; diff-owned tests
`2 PASS / 0 FAIL / 0 SKIP`.

`skip_justification`: not applicable — zero diff-owned skips.

`waiver_ref`: `ga-lpfjhc` mayor ruling 2026-08-18 covers the nine exact
beads#4566 failures after the recorded condition-(b) mechanism trace. It does
not cover `TestDoltConfigWiringExternalHost`; that distinct timeout is covered
only by the narrow mayor waiver `mayor-2026-08-20-ga-ca0np7-c3`, recorded in
audited event `ga-ca0np7.2` and mail `gm-wisp-ba846n`.

## Causality probe

Matched-pair re-run of the waived test at the gate head
(`bf6afc7f801d4729e7eb1da3bdac0cf085826c52`, which contains the gate-record
commit `ad8e7f036`), executed by the gate executor rather than copied from
review. Identical command both ways, varying only whether
`integrationEnvFor` calls `pinRealHomeEnv`:

```
GC_SESSION=subprocess go test -tags integration \
  -run '^TestDoltConfigWiringExternalHost$' ./test/integration/... -count=1 -v -timeout 5m
```

| Variant | Result | Test time | Package time |
|---|---|---|---|
| With `pinRealHomeEnv` applied (head) | **PASS** | 7.47s | 74.071s |
| With the `pinRealHomeEnv` call removed | **PASS** | 25.13s | 41.191s |

Both runs exited `0` with all three in-test phases passing (hostname
reachability, config-port wiring, cross-workspace sharing) and zero skips;
`dolt` and `bd` were both resolvable, so `requireDoltIntegration` did not
skip. Neither variant approached the 15-second `runBDInitCompat` deadline.
The change is therefore on the test's call path but empirically not causal
for the recorded timeout, which is the basis for
`mayor-2026-08-20-ga-ca0np7-c3`.

## Preserved full-union failures

The following exact beads#4566 failures are preserved as **FAIL — WAIVED**
under `ga-lpfjhc`; they are not rewritten green:

- `TestPersonalWorkFormulaCompileAndRun`
- `TestAdoptPRFormulaRetriesTransientReviewerStep`
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`
- `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries`
- `TestGraphWorkflowSuccessPath`
- `TestHumaBinary_SessionMessageAsync`
- `TestCleanInstallTutorialPath`
- `TestGCLiveContract_BeadsAndEvents`
- `TestHumaBinary_CityCreateAsync`

`TestDoltConfigWiringExternalHost`, tracked independently by `ga-gajll3`, is
also preserved as **FAIL — WAIVED** under
`mayor-2026-08-20-ga-ca0np7-c3`. The waiver is scoped to this one test and this
deploy bead. The candidate changes `test/integration/integration_test.go`
while the recorded failure surfaces at the unchanged hardcoded 15-second
`runBDInitCompat` deadline in `test/integration/dolt_config_test.go`, but the
two are not mechanically separate: `dolt_config_test.go:34` calls
`newIsolatedToolEnv`, which reaches the changed `integrationEnvFor` via
`newIsolatedEnvRoot`. Package, file-boundary and env-construction overlap are
therefore all recorded. The waiver rests instead on the empirical matched-pair
result in "Causality probe".

Five additional failures are structurally outside the candidate and tracked:

- `TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox` — `ga-uswva7`.
- `TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore` — `ga-227zz7`.
- `TestBdFlagManifestCurrent` — `ga-f0uceo`.
- `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` — `ga-afqddr` / `ga-k3fxvj`.

The raw failures above remain visible in the gate record. They are not a green
test claim; the two named mayor authorizations make them non-blocking for this
specific release evaluation.
