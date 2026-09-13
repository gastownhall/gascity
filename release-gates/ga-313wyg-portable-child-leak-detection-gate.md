# Release gate: portable child-process leak detection

- Deploy/review bead: `ga-313wyg`
- Build bead: `ga-gxmz9n`
- Reviewed source: `2df8be32fb7090172b86e6d3afb82d3cdd32ebdf`
- Deploy branch: `deploy/ga-313wyg-gate`
- Gate base: `origin/main@c0f633d2c18d17ca8dcd7f99d553127cb9ce0483`
- Evaluation date: 2026-07-30
- Disposition: **PASS**

`docs/PROJECT_MANIFEST.md` is not present at the reviewed commit, so this
checklist applies the deployer role's release-gate criteria and the test
evidence requirements in
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Branch diverges cleanly from main | **PASS** | Checked first and again after testing. `git merge-tree --write-tree origin/main 2df8be32fb7090172b86e6d3afb82d3cdd32ebdf` exited 0 against `origin/main@c0f633d2c18d17ca8dcd7f99d553127cb9ce0483` and produced tree `bf089bf9adf7f33726b5e25b0d95c0fa0a318a8d`. No self-rebase or source-branch mutation was required. |
| 1 | Review PASS present | **PASS** | Review bead `ga-313wyg` records `verdict: pass` for the three-commit reviewed tip, including the mandatory resource-census update. |
| 2 | Acceptance criteria met | **PASS** | `pidutil.ChildPIDs` now enumerates direct children through bounded `ps -axo pid=,ppid=` execution on Linux and macOS, excludes the enumeration helper's own PID, and returns enumeration errors. The workspace test leak guard delegates to that helper and fails closed when enumeration is unavailable instead of reporting a clean run. Tests cover a live child, helper-PID exclusion, a hung `ps`, clean/leaked/unavailable decisions, and the surviving-child regression. The production orphan-reaping path is unchanged, no external dependency was added, and the source-resource ledger acknowledges the new subprocess and fixed-sleep sites. |
| 3 | Tests pass | **PASS** | `go build ./...`, `go vet ./...`, changed/affected lint (0 issues), changed-file formatting, and `git diff --check` passed. The focused JSON run over `internal/pidutil`, `internal/workspacesvc`, and `internal/testpolicy/resourcecensus` recorded **292 PASS, 0 FAIL, 8 SKIP**. The eight skips are six existing host-subreaper cases plus the two standard self-exec helper/harness entry points; none exercises `ChildPIDs`, `livingTestChildren`, or `shouldFailForLeak`. The documented `make test-local-full-parallel` selected 40 jobs and initially recorded 35 PASS/5 environment failures. Those red results were not counted as passes: three were rerun successfully with CI's released `bd v1.1.0` binary (core package shard 4, formula recovery, and REST-full shard 7), and two unchanged tmux shards were rerun successfully with isolated tmux 3.4, matching Ubuntu CI rather than this Fedora host's tmux 3.7b default-binding behavior. Final CI-matched census: **40 PASS, 0 unresolved FAIL**. The hook-enforced `make test-fast-parallel` added **10 PASS, 0 FAIL** jobs. Preflight policy/boundary/native-DoltLite/docs checks, Tier A acceptance, the bd CLI contract, and Darwin/arm64 cross-compilation of both changed packages also passed. Generated/dashboard/release-config jobs were not locally repeated because this diff touches none of their inputs; GitHub required CI remains authoritative before merge. |
| 4 | No high-severity review findings open | **PASS** | The independent review reports no security, style, or specification findings and no uncovered acceptance criteria. Unresolved HIGH/CRITICAL findings: 0. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain` was empty after all tests and test-created schema cleanup. The configured hook path is `.githooks`; this checklist is the only deployer-authored release change. |
| 7 | Single feature theme | **PASS** | The three-commit set changes one portable child-process enumeration and leak-detection path, its regression tests, and the mechanically required resource-census baselines. No independent feature is bundled. |

## Acceptance evidence

- Direct-child enumeration no longer depends on `/proc`, so the macOS test
  leak guard performs a real check.
- Enumeration failure is distinguishable from a clean result and fails the
  package run.
- The `ps` helper is bounded to one second and cannot count itself as a leaked
  child.
- The existing production orphan-reaping behavior remains unchanged.
- No API, configuration, persistence migration, or external dependency is
  introduced.
