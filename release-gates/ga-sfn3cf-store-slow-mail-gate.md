# Release gate: ga-sfn3cf store-slow mail handling

**Deploy bead:** `ga-sfn3cf` — needs-deploy: store-slow mail read handling
**Source review bead:** `ga-e1kvn6` — Review: store-slow mail read handling
**Source branch:** `builder/ga-bqldr7.1-store-slow-mail`
**Deploy branch:** `deploy/ga-sfn3cf-store-slow-mail`
**Reviewed feature HEAD:** `eb31b2303`
**Verdict:** **PASS**

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead `ga-e1kvn6` is closed with `REVIEW VERDICT: PASS`; deploy bead `ga-sfn3cf` records reviewer PASS for `eb31b2303`. Single-pass review is accepted while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | Store-slow mail reads are deadline-wrapped for list/count, return typed `503 store_slow` responses, map to exported non-fallbackable `IsStoreSlowError`, degrade `gc mail check --inject` without failing, and surface non-inject errors. Focused API/CLI tests pass. |
| 3 | Tests pass | PASS | `make test-fast-parallel`, `go vet ./...`, focused store-slow API/CLI tests, `make dashboard-check`, and dashboard preview smoke all pass on the deploy branch. |
| 4 | No high-severity review findings open | PASS | Review notes contain one low/non-blocking future-interface note about deadline goroutine lifetime with buffered channel protection. 0 HIGH findings. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file; after the gate commit the deployer rechecks clean status before PR creation. `make dashboard-check` regenerated files without leaving a diff. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --quiet HEAD origin/main` exits 0 before the gate commit, with no merge conflicts. |
| 7 | Single feature theme | PASS | Commit set is one mail-read error-handling theme across the API adapter and CLI mail command. The files changed are `cmd/gc/cmd_mail.go`, `internal/api/client.go`, `internal/api/huma_handlers_mail.go`, and their focused tests. |

## Test Runs

Commands run by deployer on `deploy/ga-sfn3cf-store-slow-mail` at reviewed feature HEAD `eb31b2303`:

```text
$ make test-fast-parallel
All fast jobs passed

$ go vet ./...
(clean)

$ go test ./internal/api ./cmd/gc -run 'Test(MailListRigStoreSlowReturnsTyped503|MailCountRigStoreSlowReturnsTyped503|ClientListMailInbox_StoreSlowDoesNotFallback|ClientCountMail_StoreSlowDoesNotFallback|RouteMailCheckInjectStoreSlowEmitsDegradedNotice|RouteMailCheckStoreSlowNonInjectReturnsError|InboxUsesSingleIssuesTierMessageScanAcrossRoutes|ProviderCached_BroadSessionListCacheConcurrentAccess|NewMailProviderUsesCachedBeadmailProvider)$'
ok  	github.com/gastownhall/gascity/internal/api	0.066s
ok  	github.com/gastownhall/gascity/cmd/gc	0.336s

$ make dashboard-check
openapi-ts generation, Vite build, TypeScript typecheck, and go test ./cmd/gc/dashboard/... pass

$ npm run preview -- --host 127.0.0.1 --port 4177
served http://127.0.0.1:4177/; curl -fsS returned 0
```

## Commits In Scope

```text
eb31b2303 fix: handle store-slow mail reads (refs ga-bqldr7.1)
8e95e6849 test: red store-slow mail handling (refs ga-bqldr7.1)
```

## Files In Scope

```text
cmd/gc/cmd_mail.go
cmd/gc/cmd_mail_test.go
internal/api/client.go
internal/api/client_test.go
internal/api/handler_mail_test.go
internal/api/huma_handlers_mail.go
```
