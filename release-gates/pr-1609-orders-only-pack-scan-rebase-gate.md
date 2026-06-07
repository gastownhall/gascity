# Release Gate: PR 1609 orders-only pack scan rebase

Bead: ga-9304b
Source bead: ga-cp6j
Branch: builder/ga-cp6j-1
Commit under review: 196cb8629
Existing PR: https://github.com/gastownhall/gascity/pull/1609

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-9304b` notes contain `VERDICT: pass`; findings: none. |
| 2 | Acceptance criteria met | PASS | Orders-only pack discovery is present for city and rig scope; diff includes `cmd/gc/cmd_order.go` and `cmd/gc/pack_import_formula_order_test.go`; `TestPackV2OrdersOnlyPackVisibleToCity` and `TestPackV2OrdersOnlyPackVisibleToRig` both pass. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run TestPackV2OrdersOnly -v -count=1` PASS; `go test ./cmd/gc ./internal/orders/... -count=1` PASS; `go vet ./...` PASS; `make test-fast-parallel` PASS. |
| 4 | No high-severity review findings open | PASS | Review notes list `Findings: none`; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was clean before writing this gate artifact; the gate artifact is committed as the final branch change. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` completed with no conflicts; GitHub reports PR #1609 `mergeable=MERGEABLE`, `mergeStateStatus=CLEAN`. |

## Acceptance Evidence

- `cityOrderRoots` and rig scan roots build roots from topo-ordered pack dirs, using a synthesized `<packDir>/formulas` layer so `PACK_DIR` resolution remains stable for orders-only packs.
- Rig scan roots include the full configured `cfg.RigPackDirs[rig]` set; packs imported at both city and rig scope can contribute orders at both scopes.
- Tests cover city-level and rig-level orders-only pack discovery, same city/rig scope visibility, local override precedence, and mixed formula-pack / orders-only-pack / formula-pack precedence.
- PR #1609 already existed for this branch, so deployment updates the existing PR rather than opening a duplicate.

## Test Evidence

- `go test ./cmd/gc -run TestPackV2OrdersOnly -v -count=1`: PASS.
- `go test ./cmd/gc ./internal/orders/... -count=1`: PASS.
- `go vet ./...`: PASS.
- `make test-fast-parallel`: PASS.
- `git diff --check origin/main...HEAD`: PASS.
