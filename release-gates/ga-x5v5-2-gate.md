# Release Gate: ga-x5v5.2 - gc start URL contract coordination

**Deploy bead:** ga-569cdn
**Source bead:** ga-x5v5.2 (closed)
**Parent design:** ga-x5v5
**Branch:** `builder/ga-x5v5.2`
**Commit:** `9a156bbc8`
**Verdict:** PASS

Note: `docs/PROJECT_MANIFEST.md` is not present on this branch. This gate uses the deployer role's release criteria plus the repository testing guidance in `TESTING.md`.

## Criteria

| # | Criterion | Status | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-569cdn` notes contain `VERDICT: pass` and `Verdict: PASS - no blockers` for commit `9a156bbc`. |
| 2 | Acceptance criteria met | PASS | At gate time, `docs/packv2/migration.mdx` had the required final "See also" callout linking to `/troubleshooting/gc-start-walkthrough`. `internal/logutil/walkthrough_urls.go` exports the eight canonical docs URLs from the design contract. `internal/logutil/walkthrough_urls_test.go` table-tests every URL and includes a tree scan that fails if production Go hardcodes those URLs outside the contract file. `rg` over Go files found the URL strings only in `walkthrough_urls.go` and `walkthrough_urls_test.go`. Existing memory `gc-start-fatal-url-contract` records the lock-step docs/code pattern. |
| 3 | Tests pass | PASS | Post-fix evidence: `make check-docs` PASS (`github.com/gastownhall/gascity/test/docsync` 2.442s). `make test-fast-parallel` PASS (`All fast jobs passed`). `go vet ./...` PASS. Focused unit regression PASS: `go test -count=1 -run 'TestStartOutputProxyDedupsWarningsAndDefersFatal|TestStartOutputProxyVerboseDoesNotDedupWarnings|TestFatalFormatting|TestWalkthroughURLs' ./cmd/gc ./internal/logutil`. Focused integration regression PASS: `go test -count=1 -tags integration -run 'TestStartDrift_DirectLaunch_RestartsToNewBuildID\|TestStartDrift_RestartLoopGuard_RefusesFourthInWindow\|TestStartDrift_RestartDoesNotTriggerStandaloneControllerConflict' ./test/integration/...` (78.103s). |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-569cdn` contain no HIGH, CRITICAL, BLOCKER, or request-changes findings; the verdict is PASS with no blockers. |
| 5 | Final branch is clean | PASS | After committing this gate file, `git status --short --branch` reports a clean tree on `builder/ga-x5v5.2...origin/builder/ga-x5v5.2 [ahead 1]`. |
| 6 | Branch diverges cleanly from main | PASS | After committing this gate file, `git merge-tree --write-tree origin/main HEAD` exited 0 with no merge conflicts against `origin/main`. |

Migration note: on 2026-05-20, the detailed PackV2 migration note moved from
`docs/packv2/migration.mdx` to `engdocs/design/packv2/migration.mdx`; current
user-facing migration guidance lives in `docs/guides/migrating-to-pack-vnext.md`.
The evidence row above preserves the point-in-time gate record.

## Stacked Sources

The branch intentionally includes closed prerequisite work because `ga-x5v5.2` coordinates the docs page, migration guide, and fatal-output URL contract.

| Bead | Status | Commit evidence | Role in branch |
|------|--------|-----------------|----------------|
| ga-x5v5.1 | closed | `2d3601312` plus gate commit `41bac4989` | Adds the `gc start` troubleshooting walkthrough page and image. |
| ga-6wrr.1 | closed | `b7995cc42` via merge `d3e073555` | Adds the pack v1 to v2 migration guide. |
| ga-q0bf.1 | closed | `473182406` via merge `0888899de` | Adds warning dedup, fatal formatting, and `gc-start:` trailer. |
| ga-x5v5.2 | closed | `9a156bbc8` | Adds the migration-guide cross-link and centralizes fatal docs URLs. |

## Diagnostic Notes

A raw, non-sharded `go test -count=1 ./internal/logutil ./internal/orders ./cmd/gc` was attempted and failed in the broad `cmd/gc` package after its 10 minute timeout with environment-sensitive failures unrelated to this patch, including missing rig registration, unavailable Dolt test ports, and a test-local `bd` binary missing the `update` command. `TESTING.md` directs broad local sweeps through the sharded wrappers, and the documented `make test-fast-parallel` gate passed.
