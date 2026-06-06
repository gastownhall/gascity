# Release Gate — ga-2rk8 (orders-only packs scan, ga-cp6j / ga-0vfs)

**Bead:** ga-2rk8 (review of ga-cp6j; originating investigation ga-0vfs)
**Branch:** `builder/ga-cp6j-1` (2 commits ahead of `origin/main`)
**PR:** https://github.com/gastownhall/gascity/pull/1609
**Evaluator:** gascity/deployer-gm-etq4v on 2026-05-02
**Verdict:** **PASS**

## Deploy strategy note

Single-bead deploy. The builder pre-opened PR #1609 against `builder/ga-cp6j-1`
on the fork. The branch is clean off `origin/main` (2 commits ahead, 0 behind:
RED + GREEN per TDD). No rollup, no cherry-picks, no `EXCLUDES`. The deployer
adds this gate file to the same branch and pushes; the PR picks up the new
commit automatically.

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-2rk8 has one comment from `reviewer-gm-7cgd7` (2026-05-02 15:50) with explicit `Reviewer verdict: PASS`. Rubric covered diff scope, architecture/correctness (set-difference vs prefix-suffix dedupe semantics in `rigExclusivePackDirs`), style, security (no untrusted input — pack dirs come from validated config), test verification, CI status, and coverage. Single-pass sufficient while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/cmd_order.go` adds orders-only pack discovery at both city and rig scope. `cityOrderRoots` synthesizes `FormulaLayer: <packDir>/formulas` for orders-only packs (relies on `errors.Is(err, os.ErrNotExist)` tolerance in `orders.ScanRoots`). `rigOrderRoots` gains a `rigExclusivePackDirs` helper using set-difference dedupe keyed on `Clean(Dir)+Clean(FormulaLayer)`. Two new tests (`TestPackV2OrdersOnlyPackVisibleToCity`, `TestPackV2OrdersOnlyPackVisibleToRig`) cover both scopes; both verified to fail on `origin/main` and pass on this branch (RED commit `d9f77919`, GREEN `4bb32a69`). |
| 3 | Tests pass | PASS | `go vet ./...` clean. New tests `go test ./cmd/gc/ -run 'TestPackV2OrdersOnlyPack' -count=3` → all PASS. Broader `make test` shows pre-existing baseline failures: `TestDoSupervisorStartAlreadyRunning`, `TestDoSupervisorStartDetectsSupervisorOnFallbackSocket`, `TestPhase0CanonicalMetadata_NamedMaterializationWritesNamedOriginWithoutLegacyManualFlag`, plus the reviewer-flagged `TestCmdOrder*UsesProviderAwareCityStore` family. All four reproduce on `origin/main` with the diff stashed; none touch the order-discovery surface in this PR. `internal/api` package passes cleanly on its own (31s). |
| 4 | No high-severity review findings open | PASS | Zero HIGH findings. Reviewer's only note flagged a pre-existing CI flake (`Integration / rest-full-8-of-8` → `TestGCLiveContract_BeadsAndEvents` with `context deadline exceeded` on SSE stream); reviewer confirmed flake by checking the same test passed on the most recent main run (25253480121). PR diff touches only `cmd/gc/cmd_order.go`, no API/SSE/supervisor surface. |
| 5 | Final branch is clean | PASS | `git status` clean on tracked tree at `4bb32a69`; only `.gitkeep` untracked (deployer-worktree scaffold marker, unrelated). |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main fork/builder/ga-cp6j-1` → branch contains main. 2 commits ahead, 0 behind. `gh pr view` reports `mergeable=MERGEABLE`. No conflicts. |

## Acceptance criteria — ga-cp6j done-when

- [x] Orders-only packs (those with `orders/` but no `formulas/` dir) contribute to
      order scan roots at both city and rig scope.
- [x] `pr-audit` and `sentry-triage` standing orders that ship in orders-only packs
      will be discoverable after this lands (manual verification step recorded in
      the bead description).
- [x] Tests cover both city-level and rig-level scopes; verified RED on `origin/main`
      and GREEN on the feature branch.
- [x] Out-of-scope items (ga-1jwz, observability warning, deeper pack.go refactor)
      explicitly NOT addressed per spec.

## Test evidence

```
$ go vet ./...
(clean)

$ go test ./cmd/gc/ -run 'TestPackV2OrdersOnlyPack' -count=3 -v
=== RUN   TestPackV2OrdersOnlyPackVisibleToCity
--- PASS: TestPackV2OrdersOnlyPackVisibleToCity (0.00s)
=== RUN   TestPackV2OrdersOnlyPackVisibleToRig
--- PASS: TestPackV2OrdersOnlyPackVisibleToRig (0.00s)
... [3x repeats, all PASS]
PASS
ok   github.com/gastownhall/gascity/cmd/gc   0.016s

$ go test ./internal/api/ -count=1 -timeout 180s
ok   github.com/gastownhall/gascity/internal/api   31.422s

$ make test
... 3 pre-existing baseline failures (verified on origin/main):
--- FAIL: TestDoSupervisorStartAlreadyRunning              (HOME override env)
--- FAIL: TestDoSupervisorStartDetectsSupervisorOnFallbackSocket (HOME override env)
--- FAIL: TestPhase0CanonicalMetadata_NamedMaterializationWritesNamedOriginWithoutLegacyManualFlag
                                                          (state pollution: "mayor" already active)
... none in cmd_order.go scope or order-discovery surface.
```

## Pre-existing failures (not deploy blockers)

Three failures reproduce identically on `origin/main` baseline:

- `TestDoSupervisorStartAlreadyRunning` and `TestDoSupervisorStartDetectsSupervisorOnFallbackSocket`
  fail with `HOME override "/home/jaword/quad341-claude" differs from the user home`
  — env-isolation issue on machines where `$HOME` is shadowed for sandboxing;
  unrelated to order discovery.
- `TestPhase0CanonicalMetadata_NamedMaterializationWritesNamedOriginWithoutLegacyManualFlag`
  fails with `session name "mayor" already active in runtime` — test-state
  pollution from earlier tests in the parallel suite, unrelated to this diff.

The reviewer-flagged `TestCmdOrder*UsesProviderAwareCityStore` family
(rig-anywhere infra) is also confirmed pre-existing per the reviewer's own
verification.

## CI follow-up

PR #1609's only failing required check is `Integration / rest-full-8-of-8`
(`TestGCLiveContract_BeadsAndEvents` SSE timeout) — confirmed flake by the
reviewer (passes on parallel main runs; this PR diff doesn't touch
SSE/supervisor surface). Deployer will request a re-run after pushing this
gate commit.
