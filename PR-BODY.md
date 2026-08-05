## Summary

Applies the live re-verify guard proven in gascity#5000
(`cmd/gc/work_assignment.go`'s `ReleaseWorkBead`) to the two worst
state-destroying cached-read→write paths flagged by ra-559vsp's audit —
both are irreversible DELETEs driven by a possibly-stale enumeration:

1. **`internal/mail/beadmail/beadmail.go`'s `PurgeReadMessageWisps`** — its
   candidate list answers from a `read:true` metadata snapshot. A message the
   user un-read (or that was deleted by another path) inside the cache window
   was previously deleted anyway on the strength of that stale snapshot.
2. **`cmd/gc/wisp_gc.go`'s `deleteExpiredBeadClosure`** (the closed-root
   descendant-closure purge) — `closedWispGCEntries` can answer from a stale
   cached snapshot. A root reopened (live work resumed) inside the cache
   window would previously have its full descendant closure destructively
   deleted anyway.

## Fix

Both sites now re-read the candidate through the live handle
(`beads.HandlesFor(store).Live.Get`) immediately before the destructive
delete and skip when:
- the live read errors (bead already gone — nothing to delete), or
- the live state disagrees with the stale snapshot (message no longer
  `read:true`; root no longer `closed`).

For the wisp_gc closure-purge path, `purgeExpiredBeads`'s generic delete-loop
counted any nil-error return as "purged," which would have miscounted a
guard-skip as a successful purge — introduced a sentinel error
(`errBeadNoLongerEligible`) so a live-recheck skip is neither counted as
purged nor surfaced as a delete failure.

## Tests

`internal/mail/beadmail/beadmail_retention_test.go` (new, using a
`staleListStore` double that lets `List` answer stale while `Get`/`Delete`
stay live):
- `TestPurgeReadMessageWisps_SkipsMessageUnreadAfterSnapshot`
- `TestPurgeReadMessageWisps_SkipsMessageGoneAfterSnapshot`

`cmd/gc/wisp_gc_test.go` (new):
- `TestWispGC_ClosureSkipsRootReopenedAfterSnapshot`

All three proven fail-before (deleted/purged the stale-snapshot candidate
anyway) / pass-after.

```
go test ./internal/mail/beadmail/...
ok  	github.com/gastownhall/gascity/internal/mail/beadmail

go test ./cmd/gc/ -run TestWispGC -v
... (all PASS)
```

Full `cmd/gc` package suite also run; the only failure is a pre-existing,
unrelated infra flake (a leaked `dolt sql-server` subprocess from
`TestInitFromWithoutHostedPreservesTemplate`, not any `--- FAIL: TestXxx`
assertion) — reproduces identically on the sibling
`fix/nil-priority-normalize` clone, so it predates this change.

Source bead: ra-nxppyo (depends on ra-559vsp's audit).
