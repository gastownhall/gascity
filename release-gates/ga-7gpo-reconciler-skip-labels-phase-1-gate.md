# Release gate - reconciler skip-labels phase 1

- Review bead: `ga-tp47gr`
- Source bead: `ga-7gpo`
- Branch: `builder/ga-7gpo-1`
- Reviewed commit: `8ca3919fc8510a9835df7f662573d8a6d8f711bf`
- Gate date: 2026-05-15
- Gate source: deployer prompt release criteria. `docs/PROJECT_MANIFEST.md` is
  not present in this checkout.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-tp47gr` contains `Review Verdict: PASS` from `gascity/reviewer` on 2026-05-15. |
| 2 | Acceptance criteria met | PASS | Phase 1 ACs are satisfied: `ListQuery.SkipLabels` is added; `Prime` and `runReconciliation` request skipped-label semantics; reconciler comparisons are label-blind only when `skipLabels=true`; non-reconciler paths pass `false`; no `BdStore` argv wiring or `--no-labels` usage was added. |
| 3 | Tests pass | PASS | `go test ./internal/beads/... -count=1`; `go vet ./internal/beads/...`; `make test-fast-parallel`; and `go vet ./...` all passed on the final branch before this gate commit. |
| 4 | No high-severity review findings open | PASS | The review notes list only non-blocking observations; no HIGH or blocking findings are open. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed a clean `builder/ga-7gpo-1...fork/builder/ga-7gpo-1` branch. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` completed without conflicts and returned tree `cea5fdec2759e1c1dea76a361594b657f1344d48`. |

## Acceptance Evidence

- AC-1 (`bd list --skip-labels` argv): deferred to Phase 2 (`ga-z0pc4k`) by the source bead and review. This gate confirms Phase 1 does not add `BdStore` argv wiring.
- AC-2 (label-only change does not emit `bead.updated`): `beadChanged(old, fresh, true)` skips only the label comparison, and `TestCachingStoreRunReconciliationSkipLabelsSuppressesLabelOnlyUpdates` covers label-only suppression.
- AC-3 (status change still emits `bead.updated`): the same regression test changes status after the label-only pass and verifies one `bead.updated` notification.
- AC-4 (label-skipped response regression): the test primes a cached bead with labels, injects a backing response with labels dropped when `SkipLabels=true`, runs reconciliation, and verifies zero update events for the label-only delta.
- AC-5 (call sites updated): `rg -n "SkipLabels|beadChanged|runReconciliation|Prime\\(" internal/beads` shows reconciler paths pass `true` and event/live-read paths pass `false`.

## Test Evidence

```text
go test ./internal/beads/... -count=1
ok  	github.com/gastownhall/gascity/internal/beads	3.735s
?   	github.com/gastownhall/gascity/internal/beads/beadstest	[no test files]
?   	github.com/gastownhall/gascity/internal/beads/closeorder	[no test files]
ok  	github.com/gastownhall/gascity/internal/beads/contract	0.318s
ok  	github.com/gastownhall/gascity/internal/beads/exec	6.751s
```

```text
go vet ./internal/beads/...
PASS
```

```text
make test-fast-parallel
All fast jobs passed
```

```text
go vet ./...
PASS
```
