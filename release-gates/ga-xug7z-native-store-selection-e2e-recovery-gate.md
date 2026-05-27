# Release Gate: Native Store Selection E2E Merge Recovery

- Deployment bead: ga-xug7z
- Source bead: ga-l2souo.6
- Feature branch: builder/ga-l2souo-6-e2e
- Evaluated feature tip: b5bc7f7bf
- Gate run: 2026-05-26 20:36 UTC
- Release criteria source: deployer release-gate criteria. `docs/PROJECT_MANIFEST.md` is not present in this worktree.

## Gate Checklist

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | `ga-xug7z` notes contain `claude-reviewer verdict: PASS`; reviewer states the three new native-store tests pass, `go vet` is clean, and there are no security, style, or architectural violations. |
| 2 | Acceptance criteria met | PASS | The native-eligible, drift-fallback, and force-fallback paths are covered by `TestOpenStoreAtForCityEligibleNativeWrapsInjectedNativeStoreInCache`, `TestOpenStoreAtForCityContextDriftFallsBackWithPreflightDiagnostic`, and `TestOpenStoreAtForCityForceFallbackSkipsPreflightAndNativeOpen`. CLI/status diagnostic agreement is covered by `TestCityStatusFormatJSONIncludesBeadsDiagnostic` and the `cmd/gc` package sweep. Preflight edge cases are covered by the new `internal/beads/contract` tests. |
| 3 | Tests pass | PASS | See command evidence below. Targeted native-store tests, the requested `cmd/gc`/`internal/beads`/`internal/events` package sweep, `go vet ./...`, `make test-fast-parallel`, `make dashboard-check`, and dashboard preview smoke all passed. |
| 4 | No high-severity review findings open | PASS | `ga-xug7z` reviewer notes report no security, style, or architectural violations. Prior `ga-ybrn4` notes list only INFO/VERY LOW/LOW items; no HIGH findings remain open. |
| 5 | Final branch is clean | PASS | After committing this gate artifact, `git status --short --branch` returned only `## HEAD (no branch)`. Generated dashboard files stayed in sync after `make dashboard-check`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` exited 0. `git merge-tree --write-tree origin/main HEAD` produced tree `775524bc430073fcb7c751e977aecd271fa5965d` with no conflicts. |
| 7 | Native dependency surface has an explicit guard | PASS | `make check-native-dependency-surface` now runs in CI and locally passed with `modules=724`, `aws=25`, `azure=9`, `dolthub=14`, `googleapi=1`, and `binary_bytes=236197920`, below the configured ceiling. |

## Acceptance Evidence

| Acceptance criterion | Evidence |
|----------------------|----------|
| Healthy Dolt server scope selects native store and wraps it in `CachingStore`. | `TestOpenStoreAtForCityEligibleNativeWrapsInjectedNativeStoreInCache` passed in `go test ./internal/beads -run 'TestOpenStoreAtForCity(EligibleNative|ContextDrift|ForceFallback)' -count=1`. |
| Current drift scenario falls back to `BdStore` and surfaces the blocking gate in diagnostics. | `TestOpenStoreAtForCityContextDriftFallsBackWithPreflightDiagnostic` passed; `internal/beads/contract` package tests passed in the broader sweep. |
| Native selection and preflight CLI/status diagnostics agree for the same fixture state. | `TestCityStatusFormatJSONIncludesBeadsDiagnostic` is included in the passing `go test ./cmd/gc/...` sweep. |
| `GC_BEADS_FORCE_FALLBACK=1` produces deterministic `BdStore` fallback with no native gate execution beyond provider contract. | `TestOpenStoreAtForCityForceFallbackSkipsPreflightAndNativeOpen` passed. |
| The intentional upstream Beads/Dolt dependency closure cannot grow silently. | `make check-native-dependency-surface` is wired into CI and caps native dependency-family module counts plus the built `gc` binary size. |
| Quality gates for touched packages pass. | `go test ./cmd/gc/... ./internal/beads/... ./internal/events/... -count=1`, `go vet ./...`, and `make test-fast-parallel` passed. |
| No hardcoded user role names introduced in Go source. | `git diff --word-diff=porcelain origin/main...HEAD \| rg '^\+.*\b(mayor\|deacon\|polecat\|quad341\|jim\|james)\b'` returned no added matches. |

## Command Evidence

| Command | Result |
|---------|--------|
| `go test ./internal/beads -run 'TestOpenStoreAtForCity(EligibleNative\|ContextDrift\|ForceFallback)' -count=1` | PASS: `ok github.com/gastownhall/gascity/internal/beads 0.069s` |
| `go test ./cmd/gc/... ./internal/beads/... ./internal/events/... -count=1` | PASS: `cmd/gc` 426.872s; dashboard, internal/beads, internal/beads/contract, internal/beads/exec, internal/events, and internal/events/exec all passed. |
| `go vet ./...` | PASS |
| `make test-fast-parallel` | PASS: all fast jobs passed. |
| `make dashboard-check` | PASS: OpenAPI client generation, Vite build, TypeScript typecheck, and `go test ./cmd/gc/dashboard/...` passed. |
| `make check-native-dependency-surface` | PASS: modules=724; aws=25; azure=9; dolthub=14; googleapi=1; binary_bytes=236197920. |
| `npm run preview -- --host 127.0.0.1 --port 4187` plus `curl -fsS http://127.0.0.1:4187/` | PASS: preview served 22,773 bytes; preview process was stopped after the smoke check. |
| `git diff --check origin/main...HEAD` | PASS |

## Final Gate Result

PASS. The branch is ready to push and open as a pull request after this gate artifact is committed to `builder/ga-l2souo-6-e2e`.
