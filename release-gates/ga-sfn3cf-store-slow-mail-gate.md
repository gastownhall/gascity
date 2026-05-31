# Release gate: ga-sfn3cf store-slow mail handling

**Deploy bead:** `ga-sfn3cf` - needs-deploy: store-slow mail read handling
**Source review bead:** `ga-e1kvn6` - Review: store-slow mail read handling
**Source branch:** `builder/ga-bqldr7.1-store-slow-mail`
**Deploy branch:** `deploy/ga-sfn3cf-store-slow-mail`
**Reviewed code HEAD:** `01c871aa6` - `fix: address store-slow mail review findings`
**Evidence refresh:** 2026-05-30, adopt-pr review loop attempt 5
**Verdict:** **PASS**

This gate was refreshed after review attempt 5 found stale evidence from
`eb31b2303`. The commands below were rerun in the adopted PR worktree at code
HEAD `01c871aa6`; this artifact correction changes only this release-gate file.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Earlier source review bead `ga-e1kvn6` is closed with `REVIEW VERDICT: PASS`; the post-review fix commit `01c871aa6` resolves the later review-loop blocker/major findings. |
| 2 | Acceptance criteria met | PASS | Store-slow mail reads are deadline-wrapped for list/count/thread/get, return typed `503 store_slow` responses, map to exported non-fallbackable `IsStoreSlowError`, degrade `gc mail check --inject` without failing, and surface non-inject errors. Focused API/CLI tests pass against `01c871aa6`. |
| 3 | Tests pass | PASS | `git diff --check`, focused store-slow API/CLI tests, focused aggregate mail `go test -race`, `make test-fast-parallel`, `go vet ./...`, `make dashboard-check`, and dashboard preview smoke all pass in the adopted PR worktree. |
| 4 | No high-severity review findings open | PASS | Attempt 5 validated the previous blocker and majors as resolved. The only gating attempt-5 finding was stale release-gate evidence, corrected here. Minor/non-gating follow-ups remain optional. |
| 5 | Final branch is clean | PASS | `git status --short` was clean after validation and before this gate-file update; `make dashboard-check` regenerated files without leaving a diff. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exits 0 on the adopted PR worktree before this gate-file update. |
| 7 | Single feature theme | PASS | Commit set is one mail-read error-handling theme across the API adapter, mail decode/client surfaces, CLI mail command, focused tests, and this release gate. |

## Test Runs

Commands run in `/data/projects/gascity/worktrees/ga-qr612` at reviewed code
HEAD `01c871aa6`:

```text
$ git diff --check 9cc93c7b44225e3ceeee3efcfc81d6047fc4deb0..HEAD
(clean)

$ go test ./internal/api ./cmd/gc -run 'Test(MailGetRigStoreSlowReturnsTyped503|MailListRigProviderPanicReturns500|MailListAllRigsProviderPanicReturnsPartial|ClientGetMail_StoreSlowDoesNotFallback|RouteMailPeekStoreSlowDoesNotFallback|RouteMailCheckPartialProviderErrorInjectEmitsDegradedNotice|RouteMailCheckPartialProviderErrorNonInjectReturnsError|MailCountAllRigsStoreSlowAllFailedReturnsTyped503|MailListAllRigsStoreSlowAllFailedReturnsTyped503|MailListAllStatusStoreSlowAllFailedReturnsTyped503|MailCountRigStoreSlowReturnsTyped503|MailListRigStoreSlowReturnsTyped503|MailThreadRigStoreSlowReturnsTyped503)$' -count=1
ok  	github.com/gastownhall/gascity/internal/api	0.286s
ok  	github.com/gastownhall/gascity/cmd/gc	2.221s

$ go test -race ./internal/api -run 'Test(MailCountAllRigsStoreSlowAllFailedReturnsTyped503|MailListAllRigsProviderPanicReturnsPartial|MailListRigProviderPanicReturns500|MailGetRigStoreSlowReturnsTyped503|UniqueMailRecipientsDoesNotMutateInput|UniqueNonEmptyMailRecipientsDoesNotMutateInput)$' -count=1
ok  	github.com/gastownhall/gascity/internal/api	1.514s

$ make test-fast-parallel
All fast jobs passed

$ go vet ./...
(clean)

$ make dashboard-check
openapi-ts generation, Vite build, TypeScript typecheck, and go test ./cmd/gc/dashboard/... pass

$ npm run preview -- --host 127.0.0.1 --port 4177
served http://127.0.0.1:4177/; curl -fsS returned 0

$ git merge-tree --write-tree HEAD origin/main
(exits 0)
```

## Commits In Scope

These are the evidence-time hashes at reviewed code HEAD `01c871aa6`.

```text
f80d06e9b test: red store-slow mail handling (refs ga-bqldr7.1)
32899eb00 fix: handle store-slow mail reads (refs ga-bqldr7.1)
122ecaa36 chore: release gate PASS for ga-sfn3cf
01c871aa6 fix: address store-slow mail review findings
```

## Files In Scope

```text
cmd/gc/cmd_mail.go
cmd/gc/cmd_mail_test.go
internal/api/client.go
internal/api/client_test.go
internal/api/decode_mail.go
internal/api/decode_mail_test.go
internal/api/handler_mail.go
internal/api/handler_mail_test.go
internal/api/huma_handlers_mail.go
release-gates/ga-sfn3cf-store-slow-mail-gate.md
```
