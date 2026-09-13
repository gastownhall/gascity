# Release gate - minimal Store.Tx contract (ga-vpry3m / ga-xqsgb2.1)

**Verdict:** PASS

- Deploy bead: `ga-vpry3m`
- Source bead: `ga-xqsgb2.1`
- Parent design bead: `ga-xqsgb2`
- Branch: `builder/ga-xqsgb2-1`
- Implementation commit: `d6628305c66648736b4a73bd15e9d6e7b0370d21`
- Base checked: `origin/main` at `3ef98456b154fbf32f83c09c2e547fda1df286f7`
- Push target: `origin` (`git push --dry-run origin HEAD` succeeded)
- Manifest note: `docs/PROJECT_MANIFEST.md` is not present in this repo; this gate uses the deployer prompt criteria and the source bead acceptance criteria.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | Deploy bead `ga-vpry3m` includes `Review Verdict: PASS` from `gascity/reviewer` for commit `d6628305c`. |
| 2 | Acceptance criteria met | PASS | `beads.Store` exposes `Tx(commitMsg string, fn func(tx Tx) error) error`; `beads.Tx` includes only `Update`, `SetMetadataBatch`, and `Close`; BdStore, MemStore, FileStore, CachingStore, exec.Store, and unavailableStore satisfy the new Store method; conformance tests verify callback invocation, error propagation, nil callback rejection, and all three Tx write methods; no role names or new primitives appear in the diff. |
| 3 | Tests pass | PASS | Deployer ran focused bead-store tests, `make test-fast-parallel`, `go vet ./...`, and `make dashboard-check`; all passed. |
| 4 | No high-severity review findings open | PASS | Review notes list INFO-only findings and state no blockers/security concerns. No HIGH findings are present. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before this markdown-only gate commit; `.githooks` is active via `core.hooksPath=.githooks`. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded and produced tree `fe7c75ad723b852aadf1b896278f6c31691bc79e`; PR-style diff is limited to the Store.Tx contract, sequential adapters, conformance coverage, and compile/test helper updates. |

## Acceptance Evidence

- Minimal Tx surface: `internal/beads/beads.go` defines `type Tx interface` with `Update`, `SetMetadataBatch`, and `Close` only.
- Store contract: `internal/beads/beads.go` adds `Tx(commitMsg string, fn func(tx Tx) error) error` to `Store`.
- Sequential adapters:
  - `internal/beads/bdstore.go`
  - `internal/beads/memstore.go`
  - `internal/beads/filestore.go`
  - `internal/beads/caching_store_writes.go`
  - `internal/beads/exec/exec.go`
  - `cmd/gc/error_store.go`
- Store conformance coverage in `internal/beads/beadstest/conformance.go` verifies:
  - callback is invoked
  - `Update`, `SetMetadataBatch`, and `Close` all work through `beads.Tx`
  - callback errors propagate
  - nil callback is rejected
- Interface compile assertions in `internal/beads/beads_test.go` cover primary in-package store implementations.
- API test helper `internal/api/handler_beads_test.go` was updated to preserve the Store contract.

## Deployer Validation

- `go test ./internal/beads/...` - PASS
- `make test-fast-parallel` - PASS
- `go vet ./...` - PASS
- `make dashboard-check` - PASS
- `git diff --check origin/main...HEAD` - PASS
- `git diff --unified=0 origin/main...HEAD | rg -n 'mayor|deacon|polecat|role ==|agent ==|hardcoded' || true` - no matches
- `gh pr list --repo gastownhall/gascity --head builder/ga-xqsgb2-1 --state all` - no existing PR found before deployer push
- `gh pr list --repo gastownhall/gascity --head quad341:builder/ga-xqsgb2-1 --state all` - no existing PR found before deployer push
