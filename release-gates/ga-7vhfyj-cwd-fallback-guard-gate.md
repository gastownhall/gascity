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
