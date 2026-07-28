# Release gate: formula `GC_RIG` scope resolution

- Deploy bead: `ga-djfr2g`
- Build bead: `ga-fstubn`
- Reviewed source: `8dcf51a1821596ec2aa79a016b2457adb62b4c9e`
- Gate base: `origin/main@682a0726f5ad20cedd39e3b97e0f9d6f7fa7b919`
- Evaluation date: 2026-07-28
- Disposition: **PASS**

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-djfr2g` records `verdict: pass` after an independent review at the reviewed source SHA. |
| 2 | Acceptance criteria met | **PASS** | Focused tests pass for valid `GC_RIG` routing outside a registered rig path, explicit `--rig` precedence, invalid/unbound `GC_RIG` warning plus cwd/city fallback, unchanged behavior when `GC_RIG` is unset, and rig-scoped formula variables. The implementation is shared by formula show, catalog, cook, and version-check call sites. |
| 3 | Tests pass | **PASS** | `go build ./...` passed; `go test -count=1 ./cmd/gc/... -run 'TestResolveFormulaScope\|TestRigFormulaVarsForScope' -v` passed; `make test-fast-parallel` passed all 9 jobs; `go vet ./...` passed. All commands ran at the reviewed source SHA. |
| 4 | No high-severity review findings open | **PASS** | Reviewer notes report no style, security, or specification findings and no blocking findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain` was empty at the reviewed source SHA before this gate record was created. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first and rechecked after tests. `git merge-tree --write-tree origin/main 8dcf51a1821596ec2aa79a016b2457adb62b4c9e` exited 0 against the gate base and produced tree `cccf7c4176560777c13c6cfcd733fafb10885d8c`; no self-rebase was required. |
| 7 | Single feature theme | **PASS** | The two-commit diff is confined to `cmd/gc/cmd_formula.go` and `cmd/gc/cmd_formula_test.go`, implementing and testing one formula scope-resolution behavior. |

## Acceptance evidence

- `GC_RIG` is consulted after explicit `--rig` and before cwd-based discovery.
- A valid bound rig selects its store root, formula layers, and formula variables even when the agent worktree is outside the rig path.
- An unknown or unbound `GC_RIG` does not make formula commands unusable: resolution falls through and emits a warning naming the discarded value and selected scope.
- Existing cwd and city fallback behavior remains in place when `GC_RIG` is unset.
- No configuration schema, API wire shape, migration, or new dependency is introduced.
