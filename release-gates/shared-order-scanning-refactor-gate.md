# Release Gate: shared order scanning refactor

Bead: ga-34btiu
Source bead: ga-gse1pe.2
PR: gastownhall/gascity#2378
Branch: builder/ga-gse1pe-2
Review workflow root: ga-nb75kag
Review attempt: 1
Reviewed range: `refs/adopt-pr/ga-tztq40p/upstream-base..refs/adopt-pr/ga-tztq40p/rebased-head`

Commit-specific claims are intentionally omitted from this artifact. Rebases
and maintainer fixup amendments change the final commit SHA; the adopt-pr
workflow records the reviewed head in `refs/adopt-pr/ga-tztq40p/rebased-head`
after the fixup commit.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Acceptance criteria met | PASS | `cmd/gc` order/API dispatch surfaces and `internal/doctor` use `internal/orderdiscovery`; `internal/doctor` does not import `cmd/gc`; caller-owned filtering still happens after discovery. |
| 2 | Direct shared-package coverage present | PASS | `internal/orderdiscovery/discovery_test.go` covers nil config/default filesystem discovery, deterministic rig ordering, `OnRigScanError` continue/abort/default behavior, `OnOverrideError` continue/abort behavior, partial-order return after a swallowed override error, `CityOrderRoots`, and `RigExclusiveLayers`; `testenv_import_test.go` provides the repo-required testenv scrubber import. |
| 3 | Command consumer contracts covered | PASS | `cmd/gc/order_scan_contract_test.go` covers city roots, rig-exclusive layers, override application before return, disabled overrides, rig-scoped overrides, manual-order visibility, and dispatcher filtering. |
| 4 | Focused tests pass | PASS | `git diff --check`; `go test ./internal/orderdiscovery`; `go test ./cmd/gc -run TestOrderScanContract`; `go test ./internal/doctor`. |
| 5 | Review evidence is current | PASS | Removed stale commit IDs and the prior vacuous `internal/orderdiscovery` no-test claim; test evidence now names the direct package tests added for this review fix. |

## Acceptance Evidence

- Changed files: `cmd/gc/api_state.go`, `cmd/gc/cmd_order.go`, `cmd/gc/order_dispatch.go`, `cmd/gc/order_scan_contract_test.go`, `internal/doctor/checks_order_firing.go`, `internal/orderdiscovery/discovery.go`, `internal/orderdiscovery/discovery_test.go`, `internal/orderdiscovery/testenv_import_test.go`, `release-gates/shared-order-scanning-refactor-gate.md`.
- `internal/orderdiscovery` imports only lower-level packages and is consumed by both `cmd/gc` and `internal/doctor`.
- `scanAllOrders` is documented as returning the post-override discovery view; direct package tests now pin override continue/abort behavior and partial return semantics.

## Test Evidence

- `git diff --check`: PASS.
- `go test ./internal/orderdiscovery`: PASS.
- `go test ./cmd/gc -run TestOrderScanContract`: PASS.
- `go test ./internal/doctor`: PASS.
