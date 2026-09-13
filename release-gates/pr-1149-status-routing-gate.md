# Release Gate: PR #1149 status routing fixes

**Verdict:** PASS

- Deploy bead: `ga-iix6p`
- Source bead chain: `ga-nzb0w` -> `ga-0c0xl` -> `ga-iix6p`
- PR: https://github.com/gastownhall/gascity/pull/1149
- Branch: `feat/adr-0001-status-routing`
- Head checked before gate commit: `6a029a4d244ade9450b4b343289ce2a1f731c704`
- Base checked: `origin/main` at `42a84cff81d395390cfff4bc116621c5afa332c0`
- Merge base: `03c805621f7cb6dc517ddb04ad53d0786658443a`
- Push target: `fork` because the existing open PR head is `quad341:feat/adr-0001-status-routing`.
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this worktree. This gate applies the deployer prompt's six release criteria plus the repo guidance in `TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Deploy bead `ga-iix6p` notes contain `VERDICT: pass`, with review scope `6a029a4d2` and `FINDINGS: none`. |
| 2 | Acceptance criteria met | PASS | The PR implements the scoped ADR 0001 read-path routing and StoreHealth surfaces: typed store-maintenance event payloads are registered, `/v0/status` and `gc status` expose StoreHealth, generated OpenAPI/dashboard client artifacts are updated, and the routed command matrix covers rig, order, session, convoy, mail, beads, status, and wait reads. The fix commit also resolves the prior acceptance-test compile failure and live-contract `cache_not_live` cleanup retry finding. |
| 3 | Tests pass | PASS | Local deployer gate passed: `make test-fast-parallel`, `go vet ./...`, `make dashboard-check`, and `git diff --check`. GitHub PR checks are green, including `CI / required`, `CI / preflight`, `CI / integration`, CodeQL, dashboard, acceptance A, all cmd/gc process shards, package integration shards, rest shards, and worker-core gates. |
| 4 | No high-severity review findings open | PASS | `ga-iix6p` reports `FINDINGS: none`; the prior `ga-0c0xl` P1/P2 request-changes findings are marked fixed in the review notes. |
| 5 | Final branch is clean | PASS | `git status --short --branch` showed only `## feat/adr-0001-status-routing...fork/feat/adr-0001-status-routing` before adding this markdown-only gate commit. `make dashboard-check` produced no generated-file drift. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree `e491ecc30360e86beb52044d5721f7ea80dc1890`; `gh pr view 1149` reports `mergeStateStatus: CLEAN`. |

## Acceptance Evidence

- Store maintenance event payloads are typed and registered:
  - `internal/events/events.go` declares `gc.store.maintenance.done` and `gc.store.maintenance.failed`.
  - `internal/events/payloads.go` defines `StoreMaintenanceDonePayload` and `StoreMaintenanceFailedPayload`.
  - `internal/api/event_payloads.go` registers both payload shapes for the typed event stream.
- StoreHealth is exposed through both projections:
  - `internal/storehealth/storehealth.go` computes store size, MB-per-row ratio, and latest maintenance event status.
  - `internal/api/store_health.go` caches/adapts the domain health block for `/v0/status`.
  - `cmd/gc/store_health.go` renders the CLI `gc status` block.
  - `internal/api/openapi.json`, `docs/schema/openapi.json`, and dashboard generated TypeScript include `StatusStoreHealth`.
- Read-path routing is covered by six-row route matrices and stale-cache behavior tests:
  - `TestRouteRigList_SixRowMatrix`
  - `TestRouteOrderHistory_SixRowMatrix`
  - `TestRouteSessionList_SixRowMatrix`
  - `TestRouteConvoyList_SixRowMatrix`, `TestRouteConvoyStatus_SixRowMatrix`, `TestRouteConvoyCheck_SixRowMatrix`
  - `TestRouteMailCheck_SixRowMatrix`, `TestRouteMailPeek_SixRowMatrix`, `TestRouteMailCount_SixRowMatrix`
  - `TestRouteBeadsList_SixRowMatrix`, `TestRouteBeadsShow_SixRowMatrix`
  - `TestRouteRigStatus_SixRowMatrix`
  - `TestRouteWaitList_SixRowMatrix`, `TestRouteWaitInspect_SixRowMatrix`
- Prior review findings were resolved:
  - `test/acceptance/import_named_sessions_regression_test.go` now uses the block-scoped `sessions := decodeSessionListJSON(...)` form.
  - `test/integration/gc_live_contract_test.go` uses `liveContractSessionListEventually` to retry transient `503 cache_not_live` responses during cleanup.

## Test Evidence

| Command | Result | Notes |
|---------|--------|-------|
| `make test-fast-parallel` | PASS | All fast jobs passed: `fsys-darwin-compile`, `unit-core`, and `unit-cmd-gc-1-of-6` through `unit-cmd-gc-6-of-6`. |
| `go vet ./...` | PASS | No output. |
| `make dashboard-check` | PASS | OpenAPI TS generation, Vite build, TypeScript typecheck, and `go test ./cmd/gc/dashboard/...` passed. |
| `git diff --check` | PASS | No whitespace errors. |
| `gh pr checks 1149 --watch=false` | PASS | Required CI, CodeQL, dashboard, acceptance, process, integration, rest, and worker-core checks were all passing; optional path-gated Mac/Docker/K8s/MCP lanes were skipped by policy. |

## Branch Evidence

- `git diff --stat origin/main...HEAD`: 82 files changed, 13,246 insertions, 248 deletions.
- Main changed after the feature rebase, but the branch merges cleanly with current `origin/main` and GitHub reports the PR merge state as clean.
- The existing PR is cross-repo from `quad341:feat/adr-0001-status-routing`; this gate commit should be pushed to `fork` so PR #1149 is updated directly.

## Result

PASS. The existing PR is ready for human merge review after the release-gate commit is pushed.
