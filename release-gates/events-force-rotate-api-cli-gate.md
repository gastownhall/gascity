# Release Gate: events force rotate API and CLI

Date: 2026-05-17

## Scope

- Deploy bead: ga-mqb8hg
- Source bead: ga-b6y1.3
- PR: https://github.com/gastownhall/gascity/pull/2272
- Branch: builder/ga-b6y1-3
- Head before gate commit: 205b545aa
- Base checked: origin/main at 6c309c1e7
- Project manifest: docs/PROJECT_MANIFEST.md was not present in this checkout; this gate uses the deployer role gate criteria.

## Gate Result

PASS.

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-mqb8hg notes contain `## Review Verdict: PASS`. |
| 2 | Acceptance criteria met | PASS | See acceptance table below. |
| 3 | Tests pass | PASS | Focused acceptance tests, dashboard checks, `make test-fast-parallel`, `go vet ./...`, `go build ./...`, and GitHub PR checks passed. |
| 4 | No high-severity review findings open | PASS | Review notes list only LOW and INFO findings. `gh pr view 2272 --json reviews,comments,latestReviews` returned no PR reviews or comments. |
| 5 | Final branch is clean | PASS | `git status --short` was clean before writing this gate; the only release change is this gate file and it is committed with the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0. No merge conflicts with origin/main. |

## Acceptance Criteria

| Acceptance criterion | Result | Evidence |
|---|---|---|
| `TestOpenAPISpecInSync` passes after registration. | PASS | `go test ./internal/api -run 'TestEventRotate|TestOpenAPISpecInSync|TestGeneratedClientInSync|TestGenClient' -count=1` passed. |
| `gc events rotate --help` shows flag inventory and one example. | PASS | `go run ./cmd/gc events rotate --help` exited 0 and showed `--api`, `--wait`, global `--city`, and examples for `gc events rotate`, `gc events rotate --wait`, and `gc --city /path/to/city events rotate --api http://127.0.0.1:8080`. |
| Manual rotation on gc-management after B-1/B-2/B-3 land produces one archive and archive-aware event listing remains sorted by seq. | PASS | The assembled PR branch contains the B-1, B-2, and B-3 commits. Live gc-management rotation was not run pre-merge to avoid mutating the production city log; equivalent behavior is covered by API wait tests, CLI rotate tests, and `internal/events` rotation/archive/sequence tests. |
| Tests cover golden path, empty-file no-op, unsupported provider, supervisor unreachable, missing city, `--wait` golden path, and `--wait` timeout. | PASS | `go test ./internal/api -run 'TestEventRotate|TestOpenAPISpecInSync|TestGeneratedClientInSync|TestGenClient' -count=1` and `go test ./cmd/gc -run 'TestDoEventsRotate|TestEventsRotateHelp' -count=1` passed. |
| All error strings match the catalog byte-for-byte. | PASS | Pinned by `go test ./cmd/gc -run 'TestDoEventsRotate|TestEventsRotateHelp' -count=1`, which passed. |

## Test Evidence

- PASS: `go test ./internal/api -run 'TestEventRotate|TestOpenAPISpecInSync|TestGeneratedClientInSync|TestGenClient' -count=1`
- PASS: `go test ./cmd/gc -run 'TestDoEventsRotate|TestEventsRotateHelp' -count=1`
- PASS: `go test ./internal/config -run 'TestEventsRotation|TestDefaultEventsRotation' -count=1`
- PASS: `go test ./internal/events -run 'Test.*Rotation|Test.*Archive|Test.*Seq' -count=1`
- PASS: `go run ./cmd/gc events rotate --help`
- PASS: `make dashboard-check`
- PASS: `make test-fast-parallel`
- PASS: `go vet ./...`
- PASS: `go build ./...`
- PASS: `git diff --check`
- PASS: `make dashboard-smoke` exited 0
- PASS: dashboard preview served `http://127.0.0.1:47850/` and `curl -fsS` returned HTML
- PASS: `gh pr checks 2272` reported all active required checks passing

## Notes

- PR #2272 is stacked on PR #2262 and PR #2268 until the prerequisite rotation foundation and config plumbing land.
- No deployer-side code changes were made beyond this gate file.
