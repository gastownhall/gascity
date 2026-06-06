# Release Gate: ga-fyk7mj

Generated: 2026-05-17T05:09:11-07:00

## Scope

- Deploy bead: `ga-r8rmtj` - Review: superseded hook molecule tier follow-up
- Source bead: `ga-fyk7mj` - fix(config): default hook molecule tier should use bd ready semantics
- Release branch: `builder/ga-fyk7mj-1`
- Source branch: `fork/builder/ga-fyk7mj-1`
- Source commit: `d5662fbd5` - `chore(config): confirm hook molecule tier superseded`
- Base checked: `origin/main`
- Resolution: no-code/superseded; the reviewed branch carries an empty audit commit and no source diff from `origin/main`.

`docs/PROJECT_MANIFEST.md` is not present in this repository at this branch. This gate applies the deployer release-gate criteria plus the acceptance criteria recorded on `ga-fyk7mj`.

## Gate Criteria

| # | Criterion | Status | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-r8rmtj` notes contain `Review Verdict: PASS (no-code/superseded)` from `gascity/reviewer`, naming branch `fork/builder/ga-fyk7mj-1` at `d5662fbd5`. |
| 2 | Acceptance criteria met | PASS | Acceptance trace below. The requested default `EffectiveWorkQuery` molecule tier is absent on current `origin/main`; existing code and tests enforce that molecule containers are not routable demand. |
| 3 | Tests pass | PASS | `make test` passed with `observable go test: PASS log=/tmp/gascity-test.jsonl`; `go vet ./...` passed; `git diff --check origin/main...HEAD` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes report PASS, no source changes, and "Security / OWASP: No new code, no findings." `bd list --label high -n 50` returned no issues. |
| 5 | Final branch is clean | PASS | `git status --short --branch` returned only `## builder/ga-fyk7mj-1...fork/builder/ga-fyk7mj-1` before this gate file was added. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 after final fetch. Current `origin/main` is `b472d0c32`; `git rev-list --left-right --count origin/main...HEAD` returned `1 2`. |

## Acceptance Trace

| Acceptance item | Status | Evidence |
|---|---|---|
| Default `EffectiveWorkQuery` uses ready semantics for molecule tier. | PASS | Superseded by current contract: no default molecule tier exists. `internal/config/config.go:2446` states molecule containers are not routable demand, and `EffectiveWorkQuery` defaults route ready executable work instead. |
| Existing work query tests updated/enforce current contract. | PASS | `internal/config/config_test.go:1486`, `:1539`, and `:1564` assert default `EffectiveWorkQuery` output does not contain `--type=molecule`. |
| Deferred/blocked molecule roots do not appear in default hook output. | PASS | Hook-side defense remains covered by `cmd/gc/hook_defer_blocked_test.go:9`, `:32`, and `:54`; repo-wide `make test` passed on this branch. |
| Scale checks do not count molecule containers as demand. | PASS | `internal/config/config_test.go:1744`, `:1928`, and `:1950` assert default `EffectiveScaleCheck` does not count `--type=molecule` demand. |
| Diff is scoped to the superseded/no-code resolution. | PASS | `git diff --stat origin/main...fork/builder/ga-fyk7mj-1` and `git diff --name-status origin/main...fork/builder/ga-fyk7mj-1` produced no output before this gate file was added. |

## Test Evidence

| Command | Result |
|---|---|
| `make test` | PASS: `observable go test: PASS log=/tmp/gascity-test.jsonl` |
| `go vet ./...` | PASS |
| `git diff --check origin/main...HEAD` | PASS |
| `git merge-tree --write-tree origin/main HEAD` | PASS |
| `git rev-list --left-right --count origin/main...HEAD` | PASS: `1 2` after final fetch |

## Push Target

Pushing to fork (`quad341/gascity`); PR is cross-repo.
