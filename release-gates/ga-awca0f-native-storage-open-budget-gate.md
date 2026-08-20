# Release Gate: native storage open budget under fleet load

- Deploy bead: `ga-awca0f`
- Review bead: `ga-2y6ihp`
- Reviewed source: `2523b8d4e6955d3f3fe1fc0650b1731dd3477bdb`
- Evaluated source after bounded self-rebase: `171cbed134573e8de76eb14e783c2711194bfeb2`
- Base: `origin/main@7c817e0640fae801631043005f1d54b17ce3e97c`
- Deploy branch: `deploy/ga-awca0f-gate`
- Verdict: **PASS**

The four-file diff is test-only. It centralizes the two direct native Dolt
schema-open contexts used by `cmd/gc` fixtures and replaces their duplicated
15-second deadlines with the existing 90-second native-read fleet-load budget.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-2y6ihp` independently reproduced the RED-to-GREEN transition, ran all three diff-owned tests, verified the 90-second value against `nativeReadRetryBudget`, and recorded `verdict: pass` for reviewed source `2523b8d4e6`. |
| 2 | Acceptance criteria met | **PASS** | The two direct fixture call sites now share `nativeStorageOpenContext`; its bounded 90-second deadline is above the old 15-second failure threshold and matches the existing Dolt contention budget. Independent targeted execution passed all three acceptance tests with zero skips. The change is confined to `_test.go` files. |
| 3 | Tests pass | **PASS** | The required 40-job local CI union, including all six `cmd/gc` process shards, completed 32 PASS / 8 FAIL. Every failure was non-diff-owned, tracked, structurally unreachable from this test-only diff, and had no changed-path overlap. The exact failures are preserved below as **FAIL — ATTRIBUTED**, with the beads#4566 subset **FAIL — WAIVED** under `ga-lpfjhc`'s mayor standing authorization. The three diff-owned tests passed by name with 0 FAIL / 0 SKIP. Pre-push fast coverage was 9 PASS / 1 attributed FAIL (`ga-nqlb8q`). Policy, affected lint/format, native DoltLite, build, and vet all passed. `waiver_ref: ga-lpfjhc` for the three beads#4566 signatures; no waiver was needed for the remaining structurally attributable failures. |
| 4 | No high-severity review findings open | **PASS** | `ga-2y6ihp` reports no style or security findings. The test-only diff adds no production, wire, dependency, auth, or user-input surface. Unresolved HIGH count: `0`. |
| 5 | Final branch is clean | **PASS** | Before adding this record, `git status --short --branch` reported only `## deploy/ga-awca0f-gate...origin/deploy/ga-awca0f-gate`. `git diff --check` and `gofmt -l` over all changed Go files produced no output. Repository hooks resolve to `.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | The canonical bounded helper rebased reviewed source `2523b8d4e6` onto `origin/main@7c817e0640` without conflict, producing `171cbed134`. The isolated deploy branch was pushed at that exact candidate SHA; the first guarded push's sole fast failure was attributed to `ga-nqlb8q` before the exact head was pushed once under the standing non-diff-owned protocol. |
| 7 | Single feature theme | **PASS** | Four `cmd/gc` test files implement one reliability change: one shared native-storage-open budget, its two consumers, and its regression test. No independent behavior is bundled. |

## Test evidence

Environment for the required local CI union:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`

Commands and results:

- `GC_FAST_UNIT=0 go test ./cmd/gc/ -run '^(TestNativeStorageOpenContextUsesFleetLoadBudget|TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore|TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox)$' -v -count=1 -timeout 5m`: **3 PASS / 0 FAIL / 0 SKIP**.
- `EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true" make test-local-full-parallel`: **32 PASS jobs / 8 FAIL jobs**; all six `cmd/gc` process shards passed. Logs: `/var/tmp/gc-local-tests.UAWkgQ`.
- `make test-ci-policy`: PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected`: PASS, 0 issues in `./cmd/gc`.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed`: PASS.
- `make check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary`: PASS.
- `make test-native-doltlite-beads`: PASS.
- `go build ./...`: PASS.
- `go vet ./...`: PASS.
- `git diff --check origin/main...HEAD`: PASS.

`diff_tests_executed`:

- `TestNativeStorageOpenContextUsesFleetLoadBudget`: PASS.
- `TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore`: PASS.
- `TestCmdMailInbox_NormalizesCanonicalManagedProviderEnvAndReadsInbox`: PASS.

`test_counts`: required union `32 PASS jobs / 8 FAIL — ATTRIBUTED`; diff-owned
tests `3 PASS / 0 FAIL / 0 SKIP`.

`skip_justification`: not applicable — zero diff-owned skips.

## Preserved full-union failures

All four attribution requirements are explicit here: each test is outside the
diff, has an existing tracker, has no plausible mechanism from a test-only
`cmd/gc` change, and has no changed-path overlap.

| Test(s) | Recorded result | Tracker and attribution |
|---|---|---|
| `TestBdStoreMailWispInsert` | **FAIL — ATTRIBUTED** | `ga-sxtkmu`; Dolt connection/readiness timeout in `test/integration/bdstore_test.go`. The pending readiness fix is absent from current main. |
| `TestBdFlagManifestCurrent` | **FAIL — ATTRIBUTED** | `ga-f0uceo`; installed-`bd` manifest drift in `internal/bdflags/freshness_test.go`. |
| `TestGetKeyBinding_CapturesDefaultBinding`, `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | **FAIL — ATTRIBUTED** | `ga-afqddr` / `ga-k3fxvj`; host tmux 3.7b returned empty default bindings in `internal/runtime/tmux/tmux_test.go`. |
| `TestDoltConfigWiringExternalHost` | **FAIL — ATTRIBUTED** | `ga-gajll3`; the existing hard `bd init` deadline elapsed after initialization output reported success. Its fix remains absent from current main. |
| `TestAdoptPRFormulaCompileAndRun`, `TestAdoptPRFormulaRetriesTransientReviewerStep`, `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | **FAIL — WAIVED** | `ga-lpfjhc`; exact gastownhall/beads#4566 dirty-table schema-migration signature during fixture bootstrap. This occurrence was logged on `ga-lpfjhc`; the candidate has no production code or store-bootstrap/schema-migration path. |

The earlier pre-push `TestProviderLiveClaudeKindPath` failure is likewise
preserved in `ga-awca0f` notes and tracked by `ga-nqlb8q`; the candidate changes
only `cmd/gc` tests while the failure is in `internal/runtime/herdr`.
