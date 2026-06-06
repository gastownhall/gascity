# Release gate — identity contract lint guard (ga-w8nugs / ga-b4gug)

**Verdict:** PASS

- Bead: `ga-w8nugs` (review of `ga-b4gug`, closed)
- Branch: `quad341:builder/ga-b4gug-1` (original slice 3 head at `5cbf1d75`)
- Original reviewed HEAD: `5cbf1d75` (stacked on slice 2 `e89f19d4`)
- Slice-3 delta: 1 commit, +119 lines (test-only — lint guard via subtest)

## Stack dependency

This PR was originally **stacked on PR #1793 (slice 2)** which was **stacked on
PR #1791 (slice 1)**. Both lower slices have since merged into `main`; the
final branch was rebased onto the PR #1793 merge commit and now carries only
the slice-3 lint guard, this gate note, and the merge-repair fixup.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Reviewer PASS verdict in bead notes | PASS | `gascity/reviewer` PASS at HEAD `5cbf1d75` (per gm-w7ta0o). |
| 2 | Acceptance criteria met | PASS | New test `TestNoExternalIdentityWriters` greps the codebase for `identity.toml` writers outside the contract package; merge repair allowlists the non-writer `.gitignore` negation template. |
| 3 | Tests pass on final branch | PASS | `go test ./internal/beads/contract` — PASS after merge repair. |
| 4 | No high-severity review findings open | PASS | Reviewer routing message indicates clean PASS; no findings. |
| 5 | Working tree clean | PASS | `git status` clean before gate-file commit. |
| 6 | Branch diverges cleanly from main | PASS | Rebased onto current `main`; lower stacked slices are no longer carried by this PR. |

## Original validation (deployer re-run on `deploy/ga-w8nugs` at HEAD `5cbf1d75`)

- `go build ./...` — clean
- `go vet ./...` — clean
- `golangci-lint run ./internal/beads/contract` — 0 issues
- `go test ./internal/beads/contract -count=1` — PASS

## Push target

Pushing to fork (`quad341/gascity`); PR cross-repo. No longer stacked after
PR #1793 merged into `main`.

## Merge repair validation

- PR #1793 merged into `main`; PR #1794 was rebased onto that merge.
- `TestNoExternalIdentityWriters` initially failed on `cmd/gc/gitignore.go`,
  which writes `.gitignore` negation patterns rather than the identity file.
- Added an explicit allowlist entry for that non-writer path.
- `go test ./internal/beads/contract` — PASS.
