# Release Gate: ga-qfr6ri Args Patchability

Evaluated: 2026-06-09T03:00:55Z

Feature branch: `builder/ga-0mhj1r`
Focused handoff commit: `51a9ad911a30da9fbc5f6b25410d8370090d3115`
Base: `origin/main` at `ad1dd4e25d0b985733723c31b493c1a04f4803d3`
PR: https://github.com/gastownhall/gascity/pull/3256

`docs/PROJECT_MANIFEST.md` is absent on both `origin/main` and the focused
feature commit, so this gate uses the deployer release criteria, the
repository testing guidance in `TESTING.md`, and the API/dashboard invariants
from `engdocs/architecture/api-control-plane.md` and
`engdocs/contributors/huma-usage.md`.

## Scope

This is the rerun gate for the focused args patchability candidate. The prior
deploy gate failed criterion 7 because PR #3256 bundled an unrelated overlay
bare-hook lint helper. The focused candidate at `51a9ad911` is one commit over
current `origin/main`; `git diff --name-only origin/main..51a9ad911` contains
only config structs/apply logic, field-sync/apply tests, generated API/schema
artifacts, generated dashboard types, and config docs. It does not include
`internal/overlay/lint.go` or `internal/overlay/lint_test.go`.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-1xxum0` is closed with `Review Verdict: PASS` for the args patchability feature in PR #3256. The rerun handoff bead `ga-q7y8lr.1` records that the PR branch was focused to `51a9ad911` by removing the unrelated overlay lint helper while preserving the reviewed args patchability package. |
| 2 | Acceptance criteria met | PASS | Source bead `ga-qfr6ri` acceptance path A is implemented: `AgentOverride.Args` and `AgentPatch.Args` accept `args` in `[[rigs.patches]]` and `[[patches.agent]]`; nil keeps existing args, empty list clears, populated list fully replaces. `applyAgentOverride` and `applyAgentPatchFields` deep-copy the slice. Config docs, schemas, OpenAPI, Go client, and dashboard generated types include the field. |
| 3 | Tests pass | PASS | Local release checks passed: focused config tests, OpenAPI/client sync tests, schema freshness, `make test-fast-parallel`, `go vet ./...`, `make dashboard-check`, and a local Vite preview smoke. |
| 4 | No high-severity review findings open | PASS | Review notes list no blockers or HIGH findings. The previous deploy concern was scope contamination, not a technical high-severity finding, and the focused candidate removes the unrelated overlay files. |
| 5 | Final branch is clean | PASS | Clean deploy worktree before adding this checklist: `git status --short --branch` reported only `## HEAD (no branch)`. Dashboard generation/build left no diff. The gate commit contains this checklist as the only deployer-added file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base origin/main HEAD) origin/main HEAD` produced no conflicts or conflict markers. GitHub reports PR #3256 `mergeable: MERGEABLE`. |
| 7 | Single feature theme | PASS | The candidate is one commit touching the config patchability path and its required generated/doc artifacts. `internal/overlay/lint.go` and `internal/overlay/lint_test.go` are absent from `origin/main..51a9ad911`, resolving the prior independent-feature failure. |

## Test Evidence

```text
go test ./internal/config/... -run 'TestAgentFieldSync|TestApplyAgentPatchCoversAllFields|TestApplyAgentOverrideCoversAllFields' -count=1
ok  	github.com/gastownhall/gascity/internal/config	0.036s
```

```text
go test ./internal/api/... -run 'TestOpenAPISpecInSync|TestGeneratedClientInSync' -count=1
ok  	github.com/gastownhall/gascity/internal/api	0.078s
ok  	github.com/gastownhall/gascity/internal/api/genclient	5.632s
```

```text
go test ./test/docsync -run TestSchemaFreshness -count=1
ok  	github.com/gastownhall/gascity/test/docsync	1.900s
```

```text
make test-fast-parallel
[unit-core] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-cmd-gc-6-of-6] ok
All fast jobs passed
```

```text
go vet ./...
PASS (no output)
```

```text
make dashboard-check
npm run gen: PASS
npm run build: PASS
npm run typecheck: PASS
go test ./cmd/gc/dashboard/...: PASS
```

```text
npm run preview -- --host 127.0.0.1 --strictPort --port <ephemeral>
dashboard preview served /
```

## Decision

PASS. PR #3256 is ready for merge authority review; deployer must not merge.
