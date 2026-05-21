# Release Gate: gc lint pack CLI

Bead: ga-4add8
Source bead: ga-371q.6
Branch: builder/ga-371q-6
Commit under review: 3a2027c7b

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-4add8` notes contain `VERDICT: pass`; findings: none. |
| 2 | Acceptance criteria met | PASS | `gc lint <pack>` is wired into `cmd/gc/main.go`; lint implementation, tests, CLI docs, config loader support, and JSON schema are present; focused tests and CLI smoke checks pass. |
| 3 | Tests pass | PASS | `go test ./cmd/gc ./internal/config -run 'TestLint|Test(RenderPrompt|ParseWithPromptTemplate|QualifiedName|EffectiveWorkQuery|EffectiveSlingQuery|ExpandPacks)'` PASS; `go run ./cmd/gc lint examples/gastown/packs/gastown` PASS; `go run ./cmd/gc lint examples/bd --json` PASS; `go vet ./...` PASS; `make test-fast-parallel` PASS. |
| 4 | No high-severity review findings open | PASS | Review notes list `Findings: none`; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was clean before writing this gate artifact; the gate artifact is committed as the final branch change. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` completed with no conflicts. |

## Acceptance Evidence

- Changed files: `cmd/gc/cmd_lint.go`, `cmd/gc/cmd_lint_test.go`, `cmd/gc/main.go`, `docs/reference/cli.md`, `internal/config/pack.go`, `schemas/lint/result.schema.json`.
- `gc lint .` discovery, city-root packs, loader warnings, runtime-compatible missing-key behavior, malformed template action, valid pack, ignored prompt-discovery directories, missing fragment diagnostics, and schema-backed recursive JSON reports are covered by tests.
- CLI smoke checks validated both human output and JSON output.

## Test Evidence

- `go test ./cmd/gc ./internal/config -run 'TestLint|Test(RenderPrompt|ParseWithPromptTemplate|QualifiedName|EffectiveWorkQuery|EffectiveSlingQuery|ExpandPacks)'`: PASS.
- `go run ./cmd/gc lint examples/gastown/packs/gastown`: PASS.
- `go run ./cmd/gc lint examples/bd --json`: PASS.
- `go vet ./...`: PASS.
- `make test-fast-parallel`: PASS.
- `git diff --check origin/main...HEAD`: PASS.
