# Release Gate: loop.count brace-contrast error and tests

Result: PASS

Date: 2026-06-25

## Candidate

- Deploy bead: `ga-zpwiew`
- Source review beads: `ga-acfcqc`, `ga-vpfqjb`
- Source implementation beads: `ga-sdv68f.1`, `ga-sdv68f.2`, `ga-sdv68f.3`, `ga-wnuc8r.1`, `ga-wnuc8r.2`
- Branch: `builder/ga-sdv68f-loop-count-friendly-error`
- Reviewed tip commit: `7dde2a78fb903ecc5f207ec9559970e20cb8b653`
- Base: `origin/main` at `bddffa20caee`
- Merge-tree result: `fb3316b9616d988210b51b90bc74428139771c26`

## Release Criteria Source

`docs/PROJECT_MANIFEST.md` and `PROJECT_MANIFEST.md` are not present in this
checkout. This gate applies the deployer release criteria from the active role
instructions plus the repository testing guidance in `TESTING.md`. Because this
branch touches `docs/tutorials/05-formulas.md`, the docs verification floor from
the Gas City docs conventions was also applied.

## Changed Paths

- `docs/tutorials/05-formulas.md`
- `internal/formula/compile_test.go`
- `internal/formula/parser_test.go`
- `internal/formula/types.go`
- `release-gates/ga-zpwiew-loop-count-brace-contrast-gate.md`

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-acfcqc` records `Review Verdict: PASS` for the original loop.count friendly error UX and tutorial note package. `ga-vpfqjb` records `VERDICT: PASS` for tip commit `7dde2a78fb903ecc5f207ec9559970e20cb8b653`, including the brace-contrast refinement and regression test. |
| 2 | Acceptance criteria met | PASS | The branch rejects string-valued `loop.count` before the opaque JSON decode path, keeps valid integer counts unchanged, points variable-driven iteration to `range = "1..{n}"` with `var = "n"`, explicitly contrasts single-brace `{n}` with double-brace `{{n}}`, pins the error anchors in parser and compile tests, and adds the tutorial note immediately after the count/range explanation. |
| 3 | Tests pass | PASS | Focused formula acceptance test passed. `make check-docs` passed. `make test-fast-parallel` passed all fast jobs. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | `ga-acfcqc` lists one LOW style finding and no blockers. `ga-vpfqjb` lists `Blockers: none`; no HIGH findings are present in either review bead. |
| 5 | Final branch is clean | PASS | Clean detached gate worktree was used. Before writing this checklist, `git status --short --branch` returned only `## HEAD (no branch)`. After the gate commit, the release branch contains only committed changes. `git config core.hooksPath` reports `.githooks`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `fb3316b9616d988210b51b90bc74428139771c26`. |
| 7 | Single feature theme | PASS | The commit set has one user-facing theme: make formula `loop.count` failures actionable and document the variable-count alternative. The code and docs changes are coupled around the same parser behavior and tutorial guidance. |

## Test Evidence

```text
git diff --check origin/main...HEAD
PASS

git merge-tree --write-tree origin/main HEAD
fb3316b9616d988210b51b90bc74428139771c26

go test ./internal/formula -run 'Test(ParseTOML_LoopCountStringRejectsTemplateVar|Compile_LoopCountStringParseError)$' -count=1
ok  	github.com/gastownhall/gascity/internal/formula	0.003s

make check-docs
ok  	github.com/gastownhall/gascity/test/docsync	2.053s

make test-fast-parallel
All fast jobs passed

go vet ./...
PASS
```

## Review Notes

- No TOML schema, OpenAPI schema, or generated reference docs changed.
- No diagram sources changed, so `make diagrams-excalidraw` was not applicable.
- No dashboard/API files changed, so `make dashboard-check` was not applicable.
- The normal deployer worktree is in an unrelated interrupted rebase; this gate
  was evaluated in `/tmp/gascity-deploy-ga-zpwiew-1782348101` to avoid touching
  that state.
