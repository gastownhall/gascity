# Release gate: ga-qlyszz mail cache refresh

**Deploy bead:** `ga-qlyszz` — needs-deploy: cached mail session route refresh
**Source review bead:** `ga-dqlij2` — Review: cached mail session route refresh
**Source branch:** `builder/ga-inf8lf.2-mail-cache-refresh`
**Deploy branch:** `deploy/ga-qlyszz-mail-cache-refresh`
**Reviewed feature HEAD:** `290d58d0a`
**Verdict:** **PASS**

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead `ga-dqlij2` is closed with `REVIEWER VERDICT: PASS`; deploy bead `ga-qlyszz` records reviewer PASS for `290d58d0a`. Single-pass review is accepted while gemini second-pass is disabled. |
| 2 | Acceptance criteria met | PASS | Acceptance coverage verified in `internal/mail/beadmail`: cached providers refresh live/historical session routes after the bounded interval, remove closed-session routes, keep steady-state reads cached, and serialize concurrent refreshes. Focused acceptance tests pass; see test runs. |
| 3 | Tests pass | PASS | `make test-fast-parallel`, `go vet ./...`, and focused beadmail acceptance tests all pass on the deploy branch. |
| 4 | No high-severity review findings open | PASS | Review notes list informational findings only: broad scan trade-off, zero refresh interval behavior for non-cached provider, serialized refresh mutex, and optional `MultiRecipientInboxer` dispatch. 0 HIGH findings. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file; after the gate commit the deployer rechecks clean status before PR creation. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --quiet HEAD origin/main` exits 0 before the gate commit, with no merge conflicts. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem: cached session-route discovery for beadmail providers in `internal/mail/beadmail`. The only files changed are `beadmail.go` and `beadmail_test.go`. |

## Test Runs

Commands run by deployer on `deploy/ga-qlyszz-mail-cache-refresh` at reviewed feature HEAD `290d58d0a`:

```text
$ make test-fast-parallel
All fast jobs passed

$ go vet ./...
(clean)

$ go test ./internal/mail/beadmail -run 'TestProviderCached_(RefreshSeesNewHistoricalAliasSession|RefreshRemovesClosedSessionFromLiveHistoricalMatch|ExpiredRefreshConcurrentAccessScansOnce|BroadSessionListCacheConcurrentAccess)$'
ok  	github.com/gastownhall/gascity/internal/mail/beadmail	0.006s
```

## Commits In Scope

```text
290d58d0a fix: refresh cached mail session routes (refs ga-inf8lf.2)
4cd12f8a9 test: red cached mail session refresh (refs ga-inf8lf.2)
```

## Files In Scope

```text
internal/mail/beadmail/beadmail.go
internal/mail/beadmail/beadmail_test.go
```
