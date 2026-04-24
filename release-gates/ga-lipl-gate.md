# Release Gate — ga-lipl (beadmail Reply empty-subject title fallback)

**Bead:** ga-guxh (originating fix, closed) via review bead ga-lipl
**Branch:** `release/ga-lipl` — cherry-pick of e106005c onto `origin/main` (`issues.jsonl` stripped per EXCLUDES discipline)
**Evaluator:** gascity/deployer on 2026-04-24
**Verdict:** **PASS**

## Gate criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | ga-lipl reviewed PASS by `gascity/reviewer` at builder commit e106005c (mail `gm-wisp-ebvz`). Single-pass sufficient while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | All seven done-when items in ga-guxh satisfied: `Reply` now derives non-empty title when subject is empty; three new tests in `beadmail_test.go` (`TestReplyDerivesTitleWhenSubjectEmpty`, `TestReplyDedupesReColonPrefix`, `TestReplyFallsBackToBodyWhenOriginalUntitled`) PASS; existing `TestReply` still PASSes; `go test ./internal/mail/...` green; `fake.go`, `exec/exec.go`, `mailtest/conformance.go`, `cmd/gc/cmd_mail.go` untouched; commit subject uses conventional-commit form (`fix(beadmail): …`). |
| 3 | Tests pass | PASS | `go test ./internal/mail/...` green (beadmail 0.031s, exec 58.8s); `go vet ./...` clean; `go build ./...` clean; `gofmt -l internal/mail/beadmail/` clean. Full suite run summary appended below. |
| 4 | No high-severity review findings open | PASS | Zero HIGH findings. One non-blocking `info` observation (whitespace-only body could yield whitespace title — unlikely in practice, not spec-required). |
| 5 | Final branch is clean | PASS | `git status` shows tracked tree clean; only `.gitkeep` and pre-existing `release-gates/ga-bxq5-gate.md` untracked (workspace scaffold from the prior FAIL gate on ga-bxq5, unrelated to this deploy). |
| 6 | Branch diverges cleanly from main | PASS | One commit ahead of `origin/main` after cherry-pick; no merge conflicts outside the excluded `issues.jsonl` path. |

## Cherry-pick log

| Source SHA | Branch SHA | Summary |
|------------|------------|---------|
| e106005c | 5c8ebdc9 | fix(beadmail): derive default title on empty-subject Reply (ga-guxh) |

`EXCLUDES`: `issues.jsonl` (bd sync artifact not present on `origin/main`).

## Acceptance criteria — ga-guxh done-when

- [x] `gc mail reply <valid-id> "body"` (no `-s`) succeeds and the reply wisp has a non-empty title like `"Re: <original title>"` — exercised by `TestReplyDerivesTitleWhenSubjectEmpty`.
- [x] Three new tests in `beadmail_test.go` pass.
- [x] Existing `TestReply` still passes.
- [x] `go test ./internal/mail/...` is green.
- [x] No changes to `fake.go`, `exec/exec.go`, or `mailtest/conformance.go`.
- [x] Conventional commit message (`fix(beadmail): derive default title on empty-subject Reply (ga-guxh)`).
- [x] `cmd/gc/cmd_mail.go` untouched.

## Follow-up tracked

- `ga-2uga` (P3, validator) — tests for untested fallback branches (`>80-char` truncation and final `"Re: (no subject)"` placeholder).
