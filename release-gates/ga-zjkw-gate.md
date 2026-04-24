# Release Gate — ga-zjkw (beadmail deriveReplyTitle edge-case tests)

**Bead:** ga-2uga (originating, closed) via review bead ga-zjkw
**Branch:** `release/ga-lipl` (extends PR #1170 — tests stack on the unmerged ga-guxh fix)
**Source commit:** 65f2032f on `gc-builder-1-01561d4fb9ea` → applied here as f32afc76
**Evaluator:** gascity/deployer on 2026-04-24
**Verdict:** **PASS**

## Deploy strategy note

The `deriveReplyTitle` helper being tested was introduced by ga-guxh and
is not yet on `origin/main` — it sits in the open PR #1170
(release/ga-lipl). Cutting a fresh branch off main for ga-zjkw would
leave the tests referencing a function that doesn't exist there.
Extending PR #1170 with this test commit is the natural sequence: the
fix and its follow-up coverage ship together.

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-zjkw notes: `Review verdict: PASS` from `reviewer-gm-1z52135` on commit 65f2032f (mail `gm-wisp-30ot`). Single-pass sufficient while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | Both ga-2uga tests present: `TestReplyTruncatesLongBodyTitle` (120-char body → len ≤80 with `...` suffix) and `TestReplyFallsBackToPlaceholderWhenOriginalAndBodyEmpty` (pins literal `"Re: (no subject)"`). Coverage on `deriveReplyTitle` is 100% per reviewer `go tool cover -func`. |
| 3 | Tests pass | PASS | `go test ./internal/mail/...` green (beadmail 0.006s, exec cached, mail cached); `go vet ./...` clean. |
| 4 | No high-severity review findings open | PASS | Reviewer reported `Findings: none`. Test-only code, no user-input surface. |
| 5 | Final branch is clean | PASS | Tracked tree clean; untracked `.gitkeep` and `release-gates/ga-bxq5-gate.md` are stale artifacts from a prior FAIL session (ga-bxq5, not this deploy) and are excluded from the commit. |
| 6 | Branch diverges cleanly from main | PASS | 3 commits ahead of `origin/main` (ga-guxh fix + ga-lipl gate + this ga-2uga test); 3 commits behind merged main. No content conflicts — `git diff origin/main` applies cleanly. PR #1170 will remain mergeable. |

## Cherry-pick log

| Source SHA | Branch SHA | Summary |
|------------|------------|---------|
| 65f2032f   | f32afc76   | test(beadmail): cover Reply long-body truncation + no-subject fallback (ga-2uga) |

Test-only commit — 47 insertions in `internal/mail/beadmail/beadmail_test.go`, no other files.

## Acceptance criteria — ga-2uga done-when

- [x] `TestReplyTruncatesLongBodyTitle` covers the `>80`-char truncation branch.
- [x] `TestReplyFallsBackToPlaceholderWhenOriginalAndBodyEmpty` covers the literal `"Re: (no subject)"` placeholder.
- [x] Both tests drive the public `Reply` API, not the private helper.
- [x] `go test ./internal/mail/beadmail/...` green.
- [x] `go vet ./internal/mail/beadmail/...` clean.
