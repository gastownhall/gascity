# Release gate: explicit `--city` formula scope pin

- Deploy bead: `ga-vcj2vo`
- Build bead: `ga-61cxkw`
- Review bead: `ga-4qlsxg`
- Reviewed source: `e25f6e9df1a7b50059c11a0448a12c24aae00b4a`
- Gate base: `origin/main@e6135a435098a70f20081d1d88a03b6742002d9a`
- Evaluation date: 2026-07-30
- Disposition: **PASS**

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Independent review bead `ga-4qlsxg` records `verdict: pass` for reviewed source `e25f6e9df1a7b50059c11a0448a12c24aae00b4a`. |
| 2 | Acceptance criteria met | **PASS** | `resolveFormulaScope` and `rigFormulaVarsForScope` both honor explicit `--city` after explicit `--rig` and before ambient `GC_RIG` or cwd discovery. Focused tests cover city-over-`GC_RIG`, city-over-cwd, `GC_RIG`-over-cwd, and unbound-`GC_RIG` fallthrough with the selected-rig warning. |
| 3 | Tests pass | **PASS** | At the reviewed source SHA, `go build ./...` and `go vet ./...` passed. `go test ./cmd/gc/ -run 'TestResolveFormulaScope\|TestRigFormulaVarsForScope' -count=1 -v` reported 14 PASS, 0 FAIL, 0 SKIP. `make test-fast-parallel` passed all 10 jobs. The required `make test-cmd-gc-process-parallel` coverage passed all six `GC_FAST_UNIT=0` shards plus `productmetrics-testhook`, reporting 15,247 PASS, 0 FAIL, and 11 intentional skips; `TestTutorial01` ran and passed. The skips are existing helper-only, opt-in live-canary, unsupported-OS, unavailable optional prompt-fixture, or ambient-cwd cases explicitly disabled inside test binaries; none bears on formula scope precedence. |
| 4 | No high-severity review findings open | **PASS** | The independent review reports no security, style, specification, blocker, or major findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain` was empty after testing at the reviewed source SHA; only these gate-record edits were then added. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first and rechecked after tests. `git merge-tree --write-tree origin/main e25f6e9df1a7b50059c11a0448a12c24aae00b4a` exited 0 against the gate base and produced tree `06495988b3b266e76e96f99fdac35647b81abc94`; no self-rebase was required. |
| 7 | Single feature theme | **PASS** | The reviewed three-commit set changes `cmd/gc` formula scope resolution, its tests, and the corresponding gate evidence only. It restores one precedence rule: explicit `--city` pins city scope ahead of ambient rig discovery. |

## Acceptance evidence

- Explicit `--rig` remains the highest-priority scope selector.
- Explicit `--city` now pins formula operations to city storage and city formula layers ahead of `GC_RIG` and cwd-based rig discovery.
- City scope supplies no rig-scoped formula variables.
- Existing `GC_RIG` precedence and unbound-rig fallthrough behavior remain covered.
- No configuration schema, API wire shape, migration, dependency, or unrelated subsystem changes.
