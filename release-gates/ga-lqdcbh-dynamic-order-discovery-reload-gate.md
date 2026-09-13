# Release Gate: Dynamic order discovery reload

- Deploy bead: ga-5oxuay
- Source bead: ga-lqdcbh
- Branch: builder/ga-lqdcbh-1
- Source remote branch: fork/builder/ga-lqdcbh-1
- Implementation commit: 7527ef815
- Base checked: origin/main at 0c60ee793
- Gate date: 2026-05-17 PDT
- Manifest note: docs/PROJECT_MANIFEST.md is not present in this repo snapshot; this gate uses the deployer release criteria and the source bead acceptance criteria.

## Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-5oxuay` contains `PASS - claude reviewer verdict` for the implementation, with no blocking findings. |
| 2 | Acceptance criteria met | PASS | The controller stores a signed order snapshot, rescans orders during the controller loop, and swaps the dispatcher when the signature changes. `gc reload` rescans orders even when the city config revision is unchanged, reports applied reloads for order changes, keeps the quiet no-change path, and handles removed order files. `internal/orders` now matches `*/N` cron fields, covering the `*/1 * * * *` acceptance schedule. |
| 3 | Tests pass | PASS | Focused unit tests, focused integration test, `make test-fast-parallel`, and `go vet ./...` passed on this branch. |
| 4 | No high-severity review findings open | PASS | Review notes list PASS findings for thread safety, signature correctness, cron step matching, reload behavior, spec compliance, and security; there are no HIGH or blocking findings. |
| 5 | Final branch is clean | PASS | After committing the gate file, `git status --short --branch` showed only `builder/ga-lqdcbh-1...fork/builder/ga-lqdcbh-1 [ahead 1]`; there were no unstaged, staged, or untracked worktree changes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 with no conflict diagnostics. |

## Acceptance Details

The feature makes order discovery dynamic for a running controller. At startup,
the controller records the current scanned order set and a stable signature.
During controller ticks, it periodically rescans city and rig order roots. If
the signature changes, it drains the old dispatcher, builds a replacement from
the new order set, and logs added, removed, or changed orders.

Manual `gc reload` now also rescans orders when the `city.toml` revision is
unchanged. A new order file causes an applied reload response instead of a
misleading "No config changes detected" response. A second reload with no
config or order changes still reports the quiet no-change message. Removed
order files are dropped from the active dispatcher and logged.

The cron trigger parser now accepts `*/N` field syntax with a positive step,
which is required for the documented `*/1 * * * *` one-minute test order.

## Test Commands

```text
go test ./cmd/gc ./internal/orders -run 'Test(ScanOrderSetSnapshotFSTracksAddChangeRemove|ReloadConfigTracedRescansOrdersWhenConfigRevisionUnchanged|CheckTriggerCronEveryMinuteStepMatched)' -count=1
go test -tags integration ./cmd/gc -run TestControllerDiscoversAddedCronOrderWithoutRestart -count=1
git diff --check origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
make test-fast-parallel
go vet ./...
```
