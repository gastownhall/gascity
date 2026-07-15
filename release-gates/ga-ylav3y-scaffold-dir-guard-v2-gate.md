# Release Gate: scaffold-dir guard fix v2

- Bead: `ga-ylav3y`
- Branch: `builder/ga-5vzfgb-scaffold-dir-guard-v2`
- Candidate before gate commit: `b38cb54935e578d8aac31b27cc4ec28ea98050e0`
- Base: `origin/main` at `7052648f9de0bf254aa132a6a73f3cdfd3ed5a76`
- Evaluated: `2026-07-15T22:01:14Z`

`docs/PROJECT_MANIFEST.md` is not present in this repository at the evaluated
commit, so this gate uses the deployer release criteria from the role contract.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Deploy bead description records `Reviewed + PASSED by reviewer gascity/reviewer` after re-review; labels include `source:actual-reviewer`. |
| 2 | Acceptance criteria met | PASS | Diff is limited to the scaffold-dir guard hardening and matching resource-census ledger update: `TESTING.md`, `internal/beadmeta/guard_test.go`, `internal/testenv/lint_test.go`, `internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`. The dedicated guard tests and ledger test pass. |
| 3 | Tests pass | PASS | `gofmt -l internal/beadmeta/guard_test.go internal/testenv/lint_test.go internal/testpolicy/resourcecensus/census.go` produced no output; `git diff --check origin/main...HEAD` passed; `go build ./...` passed; `go vet ./...` passed; `go test -count=1 ./internal/beadmeta ./internal/testenv ./internal/testpolicy/resourcecensus` passed; `go test -count=1 ./internal/testpolicy/resourcecensus -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$'` passed; `TMPDIR=/var/tmp/gf-ylav3y LOCAL_TEST_JOBS=6 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel` passed all fast jobs. A first fast run with long `TMPDIR=/var/tmp/gc-deployer-ga-ylav3y-tmp-fast` failed in supervisor/controller Unix-socket tests, including `bind: invalid argument`; this matches the known long-TMPDIR socket failure mode and passed after rerun with a short `/var/tmp` path. |
| 4 | No high-severity review findings open | PASS | Notes scan found no unresolved HIGH/CRITICAL findings; deploy bead records `Security: ... No OWASP-relevant issues (test-only code)`. |
| 5 | Final branch is clean | PASS | Scratch checkout was clean before adding this gate file (`git status --short --branch` showed only `## HEAD (no branch)`). This gate file is the only release commit added by deployer before the final status/push verification. |
| 6 | Branch diverges cleanly from main | PASS | Checked before and after tests: `git rev-list --left-right --count origin/main...origin/builder/ga-5vzfgb-scaffold-dir-guard-v2` returned `0 2`; `git merge-base` returned `7052648f9de0bf254aa132a6a73f3cdfd3ed5a76`; `git merge-tree --write-tree origin/main origin/builder/ga-5vzfgb-scaffold-dir-guard-v2` returned tree `82be71b66c83c2ba75c3fd2bbdbee9f6f490812b`. |
| 7 | Single feature theme | PASS | Both commits serve one test-infrastructure theme: ignore scaffold-only `ga-*`/worktree-stage directories in repo lint/guard walks and update the resource-census ledger for the resulting subprocess baseline. |
