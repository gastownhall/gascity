# Release Gate: ga-ga91go.1 beadmail Assignees routing

Bead: ga-ga91go.1
Branch: release/ga-ga91go-1-beadmail-assignees
Base: origin/main c5bd4f713e2d194b6c602caf0d84bcbb8e5fc9eb
Code head before gate artifact: 3f71a97da71bb1e4a0f1dcfb59e18d4789fdc81e
Source commit: 7bd1edc8ffa50f77c83501da6a58fd06c308c5a3

Note: docs/PROJECT_MANIFEST.md is not present in this repository checkout. This gate uses the deployer release-gate criteria from the role instructions plus the bead acceptance criteria.

## Summary

PASS. The release branch cherry-picks only the reviewed beadmail Assignees routing commit onto current origin/main. The diff is limited to the approved beadmail and bdstore files plus this gate artifact, preserves the reviewer-approved behavior, and passes the targeted package tests, the fast sharded baseline, and go vet.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-ic4wqg is closed with `REVIEW VERDICT: PASS` for source commit 7bd1edc8f. |
| 2 | Acceptance criteria met | PASS | Prerequisite ga-tftexi.1 is closed with PR https://github.com/gastownhall/gascity/pull/2865 recorded. Only 7bd1edc8f was cherry-picked; excluded commits 8a6de11d9, d2f2e8bb9, and 64aa34ed0 are not ancestors of HEAD. Diff before this gate artifact contains only internal/beads/bdstore.go, internal/beads/bdstore_test.go, internal/mail/beadmail/beadmail.go, and internal/mail/beadmail/beadmail_test.go. |
| 3 | Tests pass | PASS | `go test ./internal/mail/beadmail ./internal/beads -run 'TestInboxUsesSingleBothTierMessageScanAcrossRoutes\|TestCheckUsesSingleAssigneeMessageScanForSlashRecipient\|TestBdStoreList(Assignees\|WispsAssignees)' -count=1` passed. `go test ./internal/mail/beadmail ./internal/beads -count=1` passed. `make test-fast-parallel` passed all 8 fast jobs. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes on ga-ic4wqg list one LOW/nit finding only; no HIGH findings are present. |
| 5 | Final branch is clean | PASS | Working tree was clean after the cherry-pick and before writing this gate artifact; final cleanliness is rechecked after committing the gate. |
| 6 | Branch diverges cleanly from main | PASS | Branch was created from origin/main c5bd4f713 and the cherry-pick applied without conflicts. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem theme: beadmail inbox routing through ListQuery.Assignees with the BdStore support and tests needed for that route. |

## Acceptance Detail

| Acceptance item | Result | Evidence |
|-----------------|--------|----------|
| ga-tftexi.1 completed first | PASS | `bd show ga-tftexi.1` reports CLOSED and records PR #2865. |
| Cherry-pick only 7bd1edc8f | PASS | `git log origin/main..HEAD` contains only `3f71a97da feat(mail): route inbox scans through assignees`, produced by cherry-picking 7bd1edc8f. |
| Exclude 8a6de11d9, d2f2e8bb9, 64aa34ed0 | PASS | `git merge-base --is-ancestor <sha> HEAD` returned non-zero for each excluded SHA. |
| Approved diff scope | PASS | `git diff --name-only origin/main...HEAD` before adding the gate artifact listed only the four approved code/test files. |
| Inbox routing uses Assignees | PASS | `messageCandidatesAll` sets `ListQuery.Assignees` when route candidates are present and only uses `AllowScan` for no-route scans. |
| Scan gate behavior not weakened | PASS | `ListQuery.Assignees` remains a filter; no-route scans still require explicit `AllowScan`. |
| DSL injection guards active | PASS | BdStore query construction continues through `appendBdQueryClause`, preserving `isBareBdQueryValue` validation for server-side query clauses. |

## Commands Run

```text
go test ./internal/mail/beadmail ./internal/beads -run 'TestInboxUsesSingleBothTierMessageScanAcrossRoutes|TestCheckUsesSingleAssigneeMessageScanForSlashRecipient|TestBdStoreList(Assignees|WispsAssignees)' -count=1
go test ./internal/mail/beadmail ./internal/beads -count=1
make test-fast-parallel
go vet ./...
```
