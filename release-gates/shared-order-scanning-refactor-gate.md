# Release Gate: shared order scanning refactor

Bead: ga-34btiu
Source bead: ga-gse1pe.2
Branch: builder/ga-gse1pe-2
Commit under review: 96549baf3

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-34btiu` notes contain `VERDICT: pass` and `RE-REVIEW VERDICT (post-rebase): pass`; findings: none. |
| 2 | Acceptance criteria met | PASS | `cmd/gc` order/API dispatch surfaces and `internal/doctor` now use `internal/orderdiscovery`; `internal/doctor` does not import `cmd/gc`; order scan contract tests cover city roots, rig-exclusive layers, overrides, skip filtering, Rig stamping, manual visibility, and auto-dispatch filtering. |
| 3 | Tests pass | PASS | `go test ./cmd/gc/... -run TestOrderScanContract` PASS; `go test ./internal/orderdiscovery/...` PASS; `go test ./cmd/gc ./internal/orders/... ./internal/doctor/...` PASS; `go vet ./...` PASS; `make test-fast-parallel` PASS. |
| 4 | No high-severity review findings open | PASS | Review notes list `Findings: none`; unresolved HIGH findings count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was clean before writing this gate artifact; the gate artifact is committed as the final branch change. |
| 6 | Branch diverges cleanly from main | PASS | Fresh branch was based from `fork/builder/ga-gse1pe-2` after the builder rebase; `git merge-tree $(git merge-base HEAD origin/main) HEAD origin/main` completed with no conflicts. |

## Acceptance Evidence

- Changed files: `cmd/gc/api_state.go`, `cmd/gc/cmd_order.go`, `cmd/gc/order_dispatch.go`, `cmd/gc/order_scan_contract_test.go`, `internal/doctor/checks_order_firing.go`, `internal/orderdiscovery/discovery.go`.
- `internal/orderdiscovery` imports only lower-level packages and is consumed by both `cmd/gc` and `internal/doctor`.
- The final branch includes the post-rebase commits `f85768f5a` and `96549baf3`; the earlier local FAIL gate commit is not part of this branch.

## Test Evidence

- `go test ./cmd/gc/... -run TestOrderScanContract`: PASS.
- `go test ./internal/orderdiscovery/...`: PASS.
- `go test ./cmd/gc ./internal/orders/... ./internal/doctor/...`: PASS.
- `go vet ./...`: PASS.
- `make test-fast-parallel`: PASS.
- `git diff --check origin/main...HEAD`: PASS.
