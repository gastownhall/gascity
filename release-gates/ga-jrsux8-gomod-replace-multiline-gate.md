# Release Gate: go.mod replace guard multi-line block detection

Deploy bead: ga-jrsux8
Feature branch: builder/ga-xttrv5-gomod-replace-multiline
Tip under gate: 95b1634d3
Base: origin/main 322dc987b

Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses the deployer release criteria and the repo testing guidance in `TESTING.md`.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Base guard review `ga-1rc90l` contains `REVIEW VERDICT: PASS`; follow-up parser review `ga-jwsz0i` contains `Reviewer verdict: PASS`. |
| 2 | Acceptance criteria met | PASS | Base guard acceptance is covered: `scripts/check-gomod-replace.sh` rejects pseudo-version, local-path, git-ref, and prerelease replace targets while allowing released semver targets; `make check-gomod-replace` is wired through the Makefile and CI. Follow-up acceptance is covered: grouped `replace (...)` blocks now track `in_replace_block` state and apply the same RHS checks to inner lines; subtests cover pseudo-version failure, local-path failure, and released-version pass in block form. |
| 3 | Tests pass | PASS | `go test ./scripts/ -v -run TestCheckGomodReplaceGuard` PASS; `make check-gomod-replace` PASS; `go vet ./...` PASS; `make test` PASS via observable runner; `git diff --check origin/main...HEAD` PASS. |
| 4 | No high-severity review findings open | PASS | `ga-1rc90l` had one MEDIUM non-blocking parser gap, fixed by this branch and reviewed in `ga-jwsz0i`; `ga-jwsz0i` lists only minor non-blocking observations. Unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was clean before writing this gate artifact; the gate artifact is the only deployer-authored branch change and is committed as the final branch tip. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base origin/main builder/ga-xttrv5-gomod-replace-multiline` equals `origin/main` (`322dc987b`); `git merge-tree origin/main builder/ga-xttrv5-gomod-replace-multiline` exited 0 with no conflicts. |
| 7 | Single feature theme | PASS | Diff is one CI/developer-tooling feature: a `go.mod` replace guard in `.github/workflows/ci.yml`, `Makefile`, `scripts/check-gomod-replace.sh`, and `scripts/gomod_replace_guard_test.go`. No independent subsystem or user-facing behavior is bundled. |

## Review And Acceptance Evidence

| Bead | Scope | Review verdict | Evidence |
|---|---|---|---|
| ga-z0fyli | Add required guard for unreleased `go.mod` replace targets | PASS via ga-1rc90l | Current branch includes the rebased base guard commits `0aa5582cd` and `fac44259f`; tests cover pseudo-version, local-path, git-ref, prerelease, clean go.mod, released semver, policy message, Makefile wiring, and CI wiring. |
| ga-xttrv5 | Fix grouped multi-line `replace (...)` parser gap | PASS via ga-jwsz0i | Current branch includes follow-up commit `95b1634d3`; state-machine parsing checks inner block lines and adds block-form failure/pass coverage. |

## Test Evidence

- `go test ./scripts/ -v -run TestCheckGomodReplaceGuard`: PASS.
- `make check-gomod-replace`: PASS.
- `go vet ./...`: PASS.
- `make test`: PASS (`observable go test: PASS`, log `/tmp/gascity-test.jsonl.wfrNtV`).
- `git diff --check origin/main...HEAD`: PASS.

## Branch Evidence

- `git log --oneline origin/main..builder/ga-xttrv5-gomod-replace-multiline`:
  - `95b1634d3 fix(check-gomod-replace): handle multi-line replace blocks`
  - `fac44259f fix(check-gomod-replace): block git-ref and prerelease version tokens`
  - `0aa5582cd ci(deps): block outbound PRs adding unreleased go.mod replace directives`
- Changed files: `.github/workflows/ci.yml`, `Makefile`, `scripts/check-gomod-replace.sh`, `scripts/gomod_replace_guard_test.go`.
