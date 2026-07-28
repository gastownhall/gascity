# Release gate: non-interactive cwd fallback guard

**Deploy bead:** `ga-7vhfyj`
**Build bead:** `ga-81d3x5`
**Review bead:** `ga-hrc5gx`
**Reviewed commit:** `02b568c035d308eb40c31123430aa9a20f0fb419`
**Base checked:** `origin/main` at `af42a94245a547a0c47ec26054afa5fd1347b567`
**Isolated branch:** `deploy/ga-7vhfyj-gate`
**Verdict:** **PASS**

`docs/PROJECT_MANIFEST.md` is absent from both the reviewed commit and current
`origin/main`, so there are no additional repository-local release criteria to
apply beyond the seven deployer gate criteria below.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-hrc5gx` contains `REVIEW VERDICT: PASS`, is closed with reason `pass`, and records independent review at `02b568c035d308eb40c31123430aa9a20f0fb419`. Reviewer mail `gm-wisp-mij4nfi` confirms the deploy handoff. |
| 2 | Acceptance criteria met | PASS | `resolveImplicitCWD` uses `term.IsTerminal` and fails closed for non-interactive stdin. All five implicit-path call sites across `gc init` and `gc start` route through it; no bare `os.Getwd()` remains in `cmd_init.go` or `cmd_start.go`. The targeted test matrix passes. Compiled-binary smoke confirms no-argument `gc init` with `/dev/null` or piped stdin and no-argument `gc start` all refuse with exit 1 before creating a city, while explicit-path `gc init --no-start` succeeds. |
| 3 | Tests pass | PASS | `go build ./...` passes in 20.63s; `go vet ./...` passes in 17.48s; targeted guard tests pass in 3.606s; `make test-fast-parallel` passes all 9 jobs in 193.65s; `make lint-new` reports 0 issues. The reviewer independently ran the full `cmd/gc` package: 8,030 PASS, 0 FAIL, 96 SKIP in 343.911s. |
| 4 | No high-severity review findings open | PASS | Zero unresolved HIGH findings. The only reviewer observation is the non-blocking, pre-existing wizard-trigger use of `isTerminalFunc`, explicitly outside this bead's scope. |
| 5 | Final branch is clean | PASS | The reviewed tree was clean before gate creation; after committing this checklist on the isolated deploy branch, `git status --porcelain` is empty. |
| 6 | Branch diverges cleanly from main | PASS | Checked first. `git merge-tree --write-tree origin/main 02b568c035d308eb40c31123430aa9a20f0fb419` succeeded with tree `578a714c6962d3fca18d7a19cdcbbd759891e61a`. The reviewed history is two commits behind and two ahead, with no conflicts; no bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | Both reviewed commits are the RED/GREEN pair for one `cmd/gc` behavior: refusing unsafe implicit-current-directory fallback under non-interactive stdin. The small internal parameter cleanup in `cmdInitWithOptions` removes newly exposed dead parameters in the same call path and is not an independent feature. |

## Reviewed history

```text
9262373ab test(cmd/gc): red — refuse implicit cwd fallback on non-tty stdin
02b568c03 feat: green — refuse implicit cwd fallback on non-tty stdin
```

The commit set touches seven files under `cmd/gc`: two command implementations,
the new shared guard and its tests, and three affected test call sites. It does
not change configuration, HTTP/API schemas, generated assets, or dashboard
code.

## Test evidence

```text
go test ./cmd/gc \
  -run '^(TestResolveImplicitCWD_|TestCmdInit_NoArgs|TestCmdInit_ExplicitPath|TestCmdInitFromFile_NoArgs|TestCmdInitFromDir_NoArgs|TestResolveStartDir_)' \
  -count=1
ok github.com/gastownhall/gascity/cmd/gc 3.606s

go build ./...
PASS (20.63s)

go vet ./...
PASS (17.48s)

make test-fast-parallel
All fast jobs passed (9/9, 193.65s)

make lint-new
0 issues
```

Compiled-binary smoke:

```text
gc init              </dev/null  -> exit 1, explicit non-interactive error
printf ... | gc init             -> exit 1, explicit non-interactive error
gc start             </dev/null  -> exit 1, explicit non-interactive error
gc init <path> --no-start </dev/null
                                -> exit 0, city.toml created in scratch path
```

## Post-gate amendment — guard narrowed to gc init (ga-w3rhto)

CI on PR #4738 failed after this gate recorded PASS. `cmd/gc process / shard 7
of 12` failed `TestTutorial01/01-hello-gas-city` and `TestTutorial01/session-fail`,
both at a bare `exec gc start`. Reproduced locally on the gate branch and
confirmed green on `origin/main`, so it is a regression from this change, not a
flake.

**Correction to criterion 2.** Applying the guard to `gc start` was not
required by the stated hazard and is now reverted. `resolveStartDir` feeds
`requireBootstrappedCity` (`cmd/gc/cmd_start.go`), which resolves through
`findCity` — an upward walk for an existing `city.toml`/`.gc` — and returns an
error *before any side effect* when there is none. `gc start` therefore cannot
bootstrap or leak state in an arbitrary checkout; only `gc init` can. The guard
now covers the three `gc init` implicit-path branches only, and criterion 2's
"no bare `os.Getwd()` remains in `cmd_start.go`" no longer holds by design.

The guard on `gc start` also reached two commands outside the stated scope:
`gc restart` (via the shared `restartTarget` → `resolveStartDir`) and
`gc start --foreground`, the documented foreground/container controller entry
point. Neither is mentioned in the PR description.

**Gap in criterion 3.** Every suite cited under criterion 3 is structurally
unable to reach the failing tests. `TestTutorial01` is gated by
`skipSlowCmdGCTest`, which skips unless `GC_FAST_UNIT=0`
(`cmd/gc/fast_loop_helpers_test.go:17`). `make test-fast-parallel` sets
`GC_FAST_UNIT=1`, and a bare `go test ./cmd/gc` leaves it unset — so the
reviewer's "8,030 PASS, 0 FAIL, 96 SKIP" full-package run skipped these
scenarios rather than passing them. Only `make test-cmd-gc-process`
(`GC_FAST_UNIT=0`) runs them. A change to a command's path-resolution behavior
should be gated on a suite that executes the CLI end to end.

**Verification after narrowing:** `TestTutorial01` (full) passes; all `gc init`
guard tests still pass unchanged.
