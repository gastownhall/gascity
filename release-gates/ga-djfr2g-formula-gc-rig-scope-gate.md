# Release gate: formula `GC_RIG` scope resolution

- Deploy bead: `ga-djfr2g`
- Build bead: `ga-fstubn`
- Reviewed source: `e25f6e9df1a7b50059c11a0448a12c24aae00b4a`
- Gate base: `origin/main@e6135a435098a70f20081d1d88a03b6742002d9a`
- Evaluation date: 2026-07-30
- Disposition: **PASS**

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Independent review bead `ga-4qlsxg` records `verdict: pass` at the reviewed source SHA. |
| 2 | Acceptance criteria met | **PASS** | Focused tests pass for valid `GC_RIG` routing outside a registered rig path, explicit `--rig` precedence, invalid/unbound `GC_RIG` warning plus cwd/city fallback, unchanged behavior when `GC_RIG` is unset, and rig-scoped formula variables. The implementation is shared by formula show, catalog, cook, and version-check call sites. |
| 3 | Tests pass | **PASS** | At the reviewed source SHA: `go build ./...` and `go vet ./...` passed; the focused formula-scope command passed 14 PASS, 0 FAIL, 0 SKIP; `make test-fast-parallel` passed 10/10 jobs; and the required `make test-cmd-gc-process-parallel` coverage passed all six `GC_FAST_UNIT=0` shards plus `productmetrics-testhook`, with 15,247 PASS, 0 FAIL, and 11 intentional skips. `TestTutorial01` ran and passed. The skips are existing helper-only, opt-in live-canary, unsupported-OS, unavailable optional prompt-fixture, or ambient-cwd cases explicitly disabled inside test binaries; none bears on formula scope precedence. |
| 4 | No high-severity review findings open | **PASS** | Reviewer notes report no style, security, or specification findings and no blocking findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain` was empty at the reviewed source SHA before this gate record was created. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first and rechecked after tests. `git merge-tree --write-tree origin/main e25f6e9df1a7b50059c11a0448a12c24aae00b4a` exited 0 against the gate base and produced tree `06495988b3b266e76e96f99fdac35647b81abc94`; no self-rebase was required. |
| 7 | Single feature theme | **PASS** | The three-commit diff (RED `62d4260e0`, GREEN `6c5712c0f`, and this gate-doc refresh) is confined to `cmd/gc/cmd_formula.go`, `cmd/gc/cmd_formula_test.go`, and this gate doc, implementing, testing, and recording one formula scope-resolution behavior — including the restored `--city` scope pin. |

## Acceptance evidence

- `GC_RIG` is consulted after explicit `--rig` and before cwd-based discovery.
- A valid bound rig selects its store root, formula layers, and formula variables even when the agent worktree is outside the rig path.
- An unknown or unbound `GC_RIG` does not make formula commands unusable: resolution falls through and emits a warning naming the discarded value and selected scope.
- Existing cwd and city fallback behavior remains in place when `GC_RIG` is unset.
- An explicit `--city` pins city scope ahead of `GC_RIG` and cwd discovery.
- No configuration schema, API wire shape, migration, or new dependency is introduced.
