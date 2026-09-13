# Release gate — events.jsonl rotation foundation (ga-di7eg3 / ga-b6y1.1)

**Verdict:** PASS

- Bead: `ga-di7eg3` (review of `ga-b6y1.1`, closed)
- Branch: `quad341:builder/ga-b6y1.1-1`
- HEAD: `bbff7b61` (2 commits off `origin/main` `ca81d000`)
- Diff: 20 files, +2740 / −50 (confined to `internal/events/` + auto-regenerated wire schemas)

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Reviewer PASS verdict in bead notes | PASS | `gascity/reviewer` PASS at HEAD `bbff7b61` (re-review after builder addressed prior REQUEST-CHANGES findings). |
| 2 | Acceptance criteria met | PASS | All 7 ACs verified: rotation-package tests + RunRotationTests conformance subtest pass; `events.rotated` registered in `KnownEventTypes` and covered by `TestEveryKnownEventTypeHasRegisteredPayload`; Layer 0-1 ZFC respected (no imports from `internal/orders/`, `internal/api/`, `cmd/gc/`); NFR-01 hot path 61–69 µs/op (≪ 1 ms bound); atomic `.gz.tmp` → `os.Rename` + filename-collision guard at rotation.go:33-39; `RotationResult` exported with field-stable shape. |
| 3 | Tests pass on final branch | PASS | `go test ./internal/events/... -count=1 -race` — PASS (5.0s). `go test ./internal/api/ -run "TestEveryKnownEventTypeHasRegisteredPayload\|TestOpenAPISpecInSync" -count=1` — PASS. |
| 4 | No high-severity review findings open | PASS | 0 HIGH / 0 BLOCKING findings open. Prior MAJOR (dead `WithArchiveRetainAge`) resolved via builder commit `bbff7b61` (path b: removed). 3 carried-forward INFO findings (Close-skips-drain in cascading failure; Windows `inodeOf` mtime^size churn out-of-CI-scope; `WithRotationCheckInterval` 0% direct option-API coverage) all reviewer-deferred per rule 5(c). |
| 5 | Working tree clean | PASS | `git status` clean before gate-file commit (only untracked `.gitkeep` from worktree scaffolding, unrelated). |
| 6 | Branch diverges cleanly from main | PASS | 2 ahead / 0 behind `origin/main`; `git merge-base origin/main HEAD` = `ca81d000`; no merge conflicts. |

## Validation (deployer re-run on `builder/ga-b6y1.1-1` at HEAD `bbff7b61`)

- `go build ./...` — clean
- `go vet ./...` — clean
- `go test ./internal/events/... -count=1 -race` — PASS (full conformance suite incl. `TestFileRecorderConformance/RotationPreservesInvariants` and `TestForceRotateConcurrentWithRecord`)
- `go test ./internal/api/ -run "TestEveryKnownEventTypeHasRegisteredPayload|TestOpenAPISpecInSync" -count=1` — PASS

## Pre-existing failures (not regressions)

`make test` reports a `FAIL` in `cmd/gc` due to environmental test-setup
issues (`fork/exec .../gc-beads-bd.sh: no such file or directory`,
`rig "gascity" is not registered in any city`). The same failures
reproduce on `origin/main` at HEAD `d0f1f0f1` — they are not introduced
by this branch. The diff is confined to `internal/events/` plus
auto-regenerated wire schemas; no `cmd/gc` paths are touched.

## Stack context

This is slice 1 (B-1) of the events.jsonl rotation feature under
architecture `ga-9hf7` (design `ga-b6y1`). Subsequent unblocked slices:

- B-2 (`ga-b6y1.2`)
- B-3 (`ga-b6y1.3`)
- B-4 (`ga-b6y1.4`) — natural home for archive-retention sweep that
  was deferred from this slice (YAGNI).

## Push target

Pushing to fork (`quad341/gascity`); PR cross-repo to `gastownhall/gascity`.
