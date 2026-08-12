# Release Gate: gc lint bd-flag validation check

Bead: ga-gln5rr
Source implementation bead: ga-d409bb
Review bead: ga-pins0c
Deploy branch: deploy/ga-gln5rr-bd-flag-lint-guard
Reviewed feature branch: builder/ga-d409bb-bd-flag-lint-guard
Feature head before gate commit: c0d8a1ff46b112b0c9bde74bc40b0af87859adb4
Base checked: origin/main at 95518bc3ae523962a20bd194990305fc1c58966e
Merge base: dd8730a9c30821ea7ed6555505b1524cbaa5d2fa
Release criteria source: deployer prompt criteria; docs/PROJECT_MANIFEST.md is not present on origin/main.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-pins0c is closed with close reason `pass`; notes contain `REVIEW VERDICT: PASS` for c0d8a1ff4. |
| 2 | Acceptance criteria met | PASS | See acceptance table below. The implementation covers the ga-d409bb Part 3 scope only; ga-8rwp5b pack-author/template sweep was explicitly out of scope. |
| 3 | Tests pass | PASS | `TMPDIR=/var/tmp make test-fast-parallel` completed with `All fast jobs passed`; `go vet ./...` exited 0. |
| 4 | No high-severity review findings open | PASS | ga-pins0c reports no blocking issues and no unresolved HIGH findings; security review found no injection surface, no new trust boundary, and no ReDoS concern. |
| 5 | Final branch is clean | PASS | Branch was clean before writing this checklist. After the gate commit, `git status --short --branch` must show no path changes before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree 312027111b296fc53af2fb7bee0f544fd8f00af3. |
| 7 | Single feature theme | PASS | Diff is limited to `cmd/gc` lint/bd delegation and new `internal/bdflags` support package/tests: 8 files, +760/-103. |

## Acceptance Evidence

| Acceptance item | Result | Evidence |
|-----------------|--------|----------|
| Add shared bd flag manifest for template-used bd subcommands. | PASS | `internal/bdflags/bdflags.go` defines `ValueFlags`, `BoolFlags`, and subcommand coverage; tests cover unknown subcommands, globals, and representative subcommands. |
| Reuse manifest from `cmd/gc/cmd_bd.go` instead of duplicating flag truth. | PASS | `cmd/gc/cmd_bd.go` delegates `bdSubcmdValueFlags` and `bdSubcmdBoolFlags` to `internal/bdflags`. |
| `gc lint` reports unknown bd flags from raw prompt source and fails via existing diagnostic plumbing. | PASS | `cmd/gc/cmd_lint.go` calls `bdflags.ScanUnknownFlags(data)` and emits `bd-unknown-flag`; `cmd/gc/cmd_lint_test.go` covers clean invocations, typo reporting, and out-of-scope subcommands. |
| Freshness test detects manifest drift against installed `bd --help`. | PASS | `internal/bdflags/freshness_test.go` contains `TestBdFlagManifestCurrent`; it skips clearly if `bd` is absent. The fast test gate ran with all fast jobs passing. |

## Commands Run

```text
git diff --name-status origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
TMPDIR=/var/tmp make test-fast-parallel
go vet ./...
```

## Touched Files

```text
cmd/gc/cmd_bd.go
cmd/gc/cmd_lint.go
cmd/gc/cmd_lint_test.go
internal/bdflags/bdflags.go
internal/bdflags/bdflags_test.go
internal/bdflags/freshness_test.go
internal/bdflags/scan.go
internal/bdflags/testenv_import_test.go
```
