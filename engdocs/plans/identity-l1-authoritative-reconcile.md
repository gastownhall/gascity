# Plan: L1-authoritative reconcile flow + endpoint validator (ga-a75ro)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-3ski1` (closed; decomposed into A/B/C/D)
> **Designer spec:** `ga-a75ro` (this bead) — full design body with the
> 11-row reconcile decision table, error-message verbatim, atomic
> three-layer write helper, and the integration-test plan.
> **Child A status (contract package):** **landed** —
> `ga-401s4` / `ga-7o5mb` / `ga-b4gug` / `ga-4tg3j` all closed
> 2026-05-07. The `contract.ReadProjectIdentity` /
> `contract.WriteProjectIdentity` API surface is live in
> `internal/beads/contract/identity.go` (~83 LOC + 250 LOC of tests).
> **Sibling status:** child C (`ga-ue241`) and child D (`ga-xxgld`)
> are deferred until B (this slice) lands.
> **Decomposed into:** 1 builder bead (see Children below)

## Context

After child A landed the `internal/beads/contract` package (the typed
L1 reader + writer + path helper + lint guard + gitignore negation),
the parent architecture's three-layer model is half-built. L1
(`.beads/identity.toml`, canonical, git-tracked) exists as an API and
as a file format, but nothing in the live code path *reads* it before
deciding what `project_id` to use.

Child B closes that gap. The existing reconciler in
`cmd/gc/dolt_project_id.go::ensureManagedDoltProjectID` operates on a
4-state (L2 ↔ L3) matrix; this slice promotes it to the 11-row
(L1 ↔ L2 ↔ L3) decision table specified in the designer's body, and
updates the connect-time identity validator
(`cmd/gc/cmd_rig_endpoint.go::readCanonicalProjectID`) to consult L1
first with an L2 fallback for legacy rigs.

This is the "make it work" core of the fix: after this slice merges,
the system survives branch switches, fresh worktrees, and DB recreates
without identity drift (parent architecture FR-1 / FR-2 / FR-4
satisfied). Events (FR-5) and the doctor check (FR-7) follow in
children C and D.

## Why a single builder bead

The designer's body splits the work into four numbered deliverables —
the reconciler refactor, the "generate fresh" guard, the endpoint
validator update, and the atomic three-layer write helper — but they
share a single test surface and modify tightly-coupled call sites in
two files (`dolt_project_id.go`, `cmd_rig_endpoint.go`) plus their
test siblings. Splitting them across multiple builder beads / PRs
would force the second PR to either revert the first's error-string
contract or carry a stale fallback to L2 for a window. Mirroring the
slice-3 (`ga-wvka`) and slice-4 (`ga-5c4x.1`) PG-auth decomposition,
this is one PR's worth of work; one builder bead.

## Children

| ID            | Title                                                                                   | Routing label    | Routes to         | Depends on |
|---------------|-----------------------------------------------------------------------------------------|------------------|-------------------|------------|
| `ga-a75ro.1`  | feat(identity): L1-authoritative reconcile flow + endpoint validator (ga-3ski1 child B) | `ready-to-build` | `gascity/builder` | child A (landed) |

## Acceptance for the parent (ga-a75ro)

Met when `ga-a75ro.1` closes and all of the following hold (these
mirror the designer's "Acceptance" checklist on the bead body):

- [ ] `cmd/gc/dolt_project_id.go::ensureManagedDoltProjectID` reads
      L1 first via `contract.ReadProjectIdentity`.
- [ ] All 11 reconcile-table rows have explicit code paths and tests
      (table-driven `TestReconcileDecisionTable`).
- [ ] The "generate fresh" path
      (`generateLocalProjectID`) is reachable only when all three
      layers are empty; a test asserts this.
- [ ] `cmd_rig_endpoint.go::readCanonicalProjectID` reads L1 first
      via `contract.ReadProjectIdentity`; falls back to
      `metadata.json#project_id` for legacy rigs.
- [ ] Error wording updated as specified on the design body §3
      (PROJECT IDENTITY MISMATCH wording referencing L1 path +
      `gc bd doctor --reseed-identity` recovery hint); existing
      tests' error-substring assertions updated to match.
- [ ] `managedDoltProjectIDReport` gains `IdentityFileUpdated bool`
      and `Layer string` fields; callers updated.
- [ ] `seedDatabaseProjectID`'s L3-clobber refusal is preserved
      verbatim (parent architecture's silent-clobber guard).
- [ ] `go test ./cmd/gc -run 'Test(EnsureManagedDoltProjectID|ReconcileDecision|RigEndpoint).*' -count=1` passes.
- [ ] `go test ./...` passes overall.
- [ ] `go vet ./...` clean.

## Notes for the builder

- **Read the designer's bead in full before starting.** The body
  pins: the 11-row decision table, the new error wording, the
  `writeProjectIdentityToAllLayers` helper signature, and the four
  required integration tests with their exact names.
- **Preserve the L1 ↔ L3 mismatch refusal.** Auto-overwriting a
  non-empty L3 from L1 is the exact silent-clobber regression the
  bug bead (ga-3ski1) warns about. The fix is the new error
  message, NOT fix-on-the-fly.
- **No event emission yet.** Leave well-named TODO markers:
  `// TODO(ga-3ski1 child C / ga-ue241): emit project.identity.stamped`.
  Child C will register the event constant and wire the emit
  call.
- **No doctor check yet.** Child D (`ga-xxgld`) owns the
  out-of-band `gc doctor` identity-drift check. This slice only
  touches the connect-time path.
- **Don't change `seedDatabaseProjectID`'s low-level error.** The
  caller-side wrapping in `ensureManagedDoltProjectID` gets the
  new operator-facing wording; the helper itself retains its
  current error so callers can `errors.Is` if they want.

## Out of scope

These belong to siblings C/D of `ga-3ski1` and must not creep into
this slice:

- L2 cache regeneration on every `EnsureCanonicalMetadata` call (→ C)
- `gc-beads-bd.sh` wrapper L1-awareness (→ C)
- `project.identity.stamped` event registration / emit (→ C)
- `gc doctor` identity-drift out-of-band check (→ D)
- k8s pod-side L1 projection (→ D)

## Validation gates

- `go test ./cmd/gc -run 'TestReconcileDecisionTable' -count=1` green;
  every one of the 11 designer-pinned rows has a sub-test asserting
  (writes, error) on an in-memory `fsys` + fake DB-stamp store.
- `go test ./cmd/gc -run 'TestEnsureManagedDoltProjectIDLegacyMigration' -count=1` green
  (L2 only → L1 written, L2 unchanged, L3 seeded).
- `go test ./cmd/gc -run 'TestEnsureManagedDoltProjectIDDBReseed' -count=1` green
  (L1+L2+L3 matching, drop dolt DB, reconcile → L3 reseeded from L1,
  L1/L2 untouched).
- `go test ./cmd/gc -run 'TestEnsureManagedDoltProjectIDRefusesL1L3Mismatch' -count=1`
  green; the test must also assert *no writes happen* (L2 unchanged
  even if it disagrees with L1).
- Existing `TestEnsureManagedDoltProjectIDGeneratesLocalIdentityWhenMetadataAndDatabaseMissing`
  updated to also assert L1 written in the "all three absent" path.
- `cmd/gc/cmd_rig_endpoint_test.go` identity-mismatch tests extended
  with at least one case where L1 is the source and L2 is empty /
  mismatched.
- `git diff` confined to `cmd/gc/dolt_project_id.go`,
  `cmd/gc/dolt_project_id_test.go`, `cmd/gc/cmd_rig_endpoint.go`,
  `cmd/gc/cmd_rig_endpoint_test.go` (plus shared fixtures if
  needed). No other files modified.
- ZFC: no role names in the diff.
- Typed wire: no `map[string]any` or `json.RawMessage` introduced.

## Risks and unknowns

- **`generateLocalProjectID` callers may have implicit expectations
  the prefix is `gc-local-`.** Verify by grep before/after the
  refactor that no consumer relies on the prefix substring; the
  L1 contract package already documents the prefix as illustrative,
  not contractual.
- **`readCanonicalProjectID` is called from multiple sites in
  `cmd_rig_endpoint.go`.** Confirm all three call sites
  (`cmd_rig_endpoint.go:502 / 505 / 508`) carry the same fallback
  semantics and the same updated wording; if one diverges, the
  test surface needs an extra case.
- **Test fakes for the dolt DB-stamp store.** The existing
  `cmd/gc/dolt_project_id_test.go` (line 16) uses a live dolt
  setup. The new `TestReconcileDecisionTable` runs entirely
  in-memory; the builder may need to extract a small
  `dbStampStore` interface so the table-driven test can use a
  fake. Keep the interface unexported and confined to `cmd/gc/`.
