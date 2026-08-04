# Release Gate: attempt_bounded_self_rebase force-with-lease refspec fix

- Bead: `ga-dl57j2`
- Source bead: `ga-ggsux3`
- Reviewed deploy commit: `e42ef16c0d9c8d445529e520637968aef43aa3b7`
- Deploy branch: `deploy/ga-dl57j2-gate`
- Source branch: `builder/ga-ggsux3-force-with-lease-refspec-fix` (provenance only; not used as a deploy push target)
- Gate worktree: `/var/tmp/codex-gc-deployer-ga-dl57j2-20260721140719`
- Base: `origin/main` at `c2538d42bfa4b10444764a4695736cdc5340ed1a`
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate applies the active deployer prompt release criteria plus the repo guidance in `TESTING.md`.

## Summary

PASS on 2026-07-22.

This gate evaluated the reviewed source commit directly. The change is a single shared script fix plus its direct shell regression coverage:

- `scripts/rebase-resolve-lib.sh`
- `scripts/test-rebase-resolve.sh`

## Criterion 6: Branch Diverges Cleanly From Main

PASS. Evaluated first.

Evidence:

- `git merge-tree --write-tree origin/main e42ef16c0`: `7bec7fdd0aeb79e803abf43bceef262d862cb5f2` (exit 0)
- `git diff --name-only origin/main...e42ef16c0`: only `scripts/rebase-resolve-lib.sh` and `scripts/test-rebase-resolve.sh`
- No bounded self-rebase was needed for this gate.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source bead `ga-ggsux3` is closed with notes containing `REVIEWER VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | The library now captures `refs/remotes/origin/$branch` before rebase and uses `--force-with-lease=$branch:$expected_remote_sha` when pushing the rebased branch. The smoke suite includes and passes `bounded/restricted-fetch-refspec-still-pushes`, reproducing the restricted-fetch-refspec production failure and confirming rc=0 with the fix. Static guard `push/bounded-rebase-force-with-lease` also passes. |
| 3 | Tests pass | PASS | `go build ./cmd/gc/` passed. `go vet ./...` passed. `make test-fast-parallel` passed. `shellcheck scripts/rebase-resolve-lib.sh scripts/test-rebase-resolve.sh` passed. `scripts/test-rebase-resolve.sh` passed with `pass=23 fail=0`. `go test ./scripts/... -run RebaseResolve -v` passed. `git diff --check origin/main...HEAD` passed. |
| 4 | No high-severity review findings open | PASS | Source bead review notes contain only two low-severity, non-blocking awareness notes and no HIGH or CRITICAL findings. |
| 5 | Final branch is clean | PASS | Gate worktrees were clean before writing this checklist. This gate file is the only deploy-branch delta on top of the reviewed source commit and is committed separately. |
| 6 | Branch diverges cleanly from main | PASS | See dedicated criterion 6 section above. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem: deployer bounded self-rebase push safety and its direct regression tests. No independent feature theme is bundled. |

## Test Log Summary

- `go build ./cmd/gc/`: PASS
- `go vet ./...`: PASS
- `make test-fast-parallel`: PASS (`All fast jobs passed`)
- `shellcheck scripts/rebase-resolve-lib.sh scripts/test-rebase-resolve.sh`: PASS
- `scripts/test-rebase-resolve.sh`: PASS (`pass=23 fail=0`)
- `go test ./scripts/... -run RebaseResolve -v`: PASS
- `git diff --check origin/main...HEAD`: PASS
