# Plan: PG-auth slice 3/4 — wire `pgauth` into `cmd/gc/bd_env.go`

> Owner: `gascity/pm` · Created: 2026-05-05
> Source architecture: `ga-dga2` (closed) — *gc bd subprocess strips
> BEADS_POSTGRES_PASSWORD env (blocks PG-backed rigs)*
> Source design: `ga-4qvs` (designer-complete, this PM session closes it)
> Sibling chain: `ga-0nmb` (1d, closed) → `ga-pnqg` (1b, closed via PR #1727)
> · `ga-3hay` (2d, closed) → `ga-vt6q` (2b, routed to builder)
> · **`ga-4qvs` (3d, this slice)** → ga-5c4x (4d)

## Why this work exists

Slices 1 + 2 deliver the foundation: `MetadataState.Backend == "postgres"`
parses cleanly with PG host/port/user/database fields (`ga-pnqg`), and a
new `internal/pgauth` package resolves PG passwords through a
seven-tier chain identical to the doltauth resolver (`ga-vt6q`). Neither
slice projects credentials into the `bd` subprocess env, so a PG-backed
rig still fails at the wrapper boundary — the mayor's repro `gc bd
--rig <pg-rig> list` still exits 1 with "no password" because nothing
calls `pgauth.ResolveFromEnv` on the wrapper side.

Slice 3 closes that hole. After this slice lands, `cmd/gc/bd_env.go`
dispatches on `MetadataState.Backend`: Dolt-backed scopes flow through
the existing path bit-for-bit, PG-backed scopes flow through a new
projection helper that calls the slice-2 resolver and writes
`BEADS_POSTGRES_*` keys into the merged subprocess env. Slice 4
(`ga-5c4x`) adds doctor checks and resolver-source events on top.

## Goal

Land a single PR that:

- Renames `applyCanonicalScopeDoltEnv` → `applyCanonicalScopeBackendEnv`
  and dispatches on `MetadataState.Backend` (empty/`"dolt"` → existing
  Dolt path, `"postgres"` → new path, anything else → typed error).
- Adds `applyResolvedScopePostgresEnv`, `mirrorBeadsPostgresEnv`, and
  package-scope `projectedPostgresEnvKeys` to `cmd/gc/bd_env.go`.
- Extends `mergeRuntimeEnv`'s `keys` strip-list with the six PG keys so
  inherited stale parent values cannot survive the strip-then-project
  cycle.
- Adds the PG-aware branch to `bdCommandRunnerWithManagedRetry`: detect
  PG transport errors via `pgTransportError`, wrap with the
  "unreachable; gc does not manage external PG endpoints" hint, and
  skip the managed-recovery branch entirely (PG is externally
  managed).
- Ships table-driven tests covering every new code path (happy path,
  resolver exhaustion, mixed-backend per-scope dispatch, parent-env
  scrub, transport-error wrap with no managed retry) plus a CI symmetry
  guard asserting `projectedPostgresEnvKeys` matches the strip-list
  segment.

## Work breakdown

| Bead       | Title                                                                    | Priority | Routes to | Gate           |
|------------|--------------------------------------------------------------------------|----------|-----------|----------------|
| `ga-4qvs.1` | feat(bd_env): wire pgauth into gc bd subprocess env (slice 3/4 of PG-auth) | P1       | builder   | ready-to-build |

The designer's notes (`ga-4qvs` §13.1) explicitly call for one PR:
"The dispatcher rename is atomic with the dispatch arm — splitting them
creates a tree where Dolt-backed cities run on a renamed function with
no PG arm, then a follow-up adds the arm. Both PRs pass tests
independently but the intermediate state is architecturally
meaningless. One PR = one reviewable design step." The pm
decomposition honours that — one builder bead, ~120 LOC source +
~250 LOC tests + ~3 call-site updates for the rename ≈ 370 LOC total.

## Dependency graph

```
ga-pnqg (slice 1 build, closed) ──┐
                                  ├─► ga-4qvs.1 (slice 3 build) ─► ga-5c4x (slice 4 design)
ga-vt6q (slice 2 build, open)  ───┘
```

`ga-4qvs.1` declares hard `depends_on` edges from both `ga-pnqg` and
`ga-vt6q`. Slice 1 is already merged; slice 2 is open at the builder
and must land before slice 3 starts (slice 3 imports
`internal/pgauth`'s public surface). The reconciler will not surface
`ga-4qvs.1` as ready until both deps close.

## Routing rationale

Slice 3 has been through architect (`ga-dga2`) and designer (`ga-4qvs`).
The designer's notes pin: the verbatim error wording for all three new
operator-facing errors (§4); the dispatcher rename rationale and the
two rejected alternatives (§9.2); the seven PG transport-marker
substrings (§6.4 + §9.3); the no-`PostgresConnectionTarget` decision
(§9.4); the empty-backend-defaults-to-Dolt back-compat invariant
(§9.1); the implementation bodies for `applyResolvedScopePostgresEnv`
and `mirrorBeadsPostgresEnv` verbatim (§6.1, §6.2); the strip-list
addition with all six keys verbatim (§6.3); the recovery-hook branch
verbatim (§6.4); the test fixture sketch with six tests + symmetry
guard (§10); the slice-4 seam non-mutation (§11); and the 8-step
reviewer checklist (§12). There is nothing left to design — only
build.

Routed to **builder** with `ready-to-build`. No validator hop because
the designer's fixture sketch (§10 of the design notes) is the test
plan; the builder authors fixtures + tests as part of the PR per
package convention (`bd_env_test.go` colocated with `bd_env.go` —
~2498 lines today).

## Acceptance criteria (rolled up)

The full criteria live in the builder bead's notes and in `ga-4qvs`'s
design (§3, §4, §5, §6, §10, §12). Roll-up for stakeholder visibility:

1. **Public surface matches the design.** `applyCanonicalScopeDoltEnv`
   no longer exists; every grep hit is replaced with
   `applyCanonicalScopeBackendEnv`. `applyResolvedScopePostgresEnv`,
   `mirrorBeadsPostgresEnv`, and `projectedPostgresEnvKeys` exist with
   the exact signatures from design §3. Helpers `pgTransportError`,
   `scopeBackendIsPostgres`, and `scopePostgresEndpoint` exist with the
   shapes from design §6.4.
2. **Dolt path is bit-for-bit unchanged.** Empty-backend and
   `"dolt"`-backend scopes flow through the same logic as today
   (`canonicalScopeDoltTarget` → `applyCanonicalDoltTargetEnv` →
   `applyCanonicalDoltAuthEnv` → `mirrorBeadsDoltEnv`). The `case "",
   "dolt":` arm preserves back-compat for every existing city's
   `metadata.json`.
3. **PG path projects the six keys.** A PG-backed scope sets
   `GC_POSTGRES_PASSWORD`, `BEADS_POSTGRES_PASSWORD`,
   `BEADS_POSTGRES_HOST`, `BEADS_POSTGRES_PORT`, `BEADS_POSTGRES_USER`,
   `BEADS_POSTGRES_DATABASE` — values from
   `pgauth.ResolveFromEnv` for the password and from `MetadataState`
   directly for the rest.
4. **Strip-list symmetry holds.** All six PG keys appear in
   `mergeRuntimeEnv`'s `keys` slice. The CI guard test
   (`TestProjectedKeysCoverage` or equivalent) asserts every entry in
   `projectedPostgresEnvKeys` is also stripped — no drift possible.
5. **Error wording is verbatim.** Three new error texts match design
   §4 exactly: `unsupported backend %q for scope %s`, `resolving
   postgres credentials for %s: %w` (wrap
   `pgauth.ErrNoPasswordResolvable`), `postgres at %s:%s is
   unreachable; gc does not manage external PG endpoints: %w` (wrap
   transport error). Tests assert literal substring presence.
6. **Recovery-hook asymmetry holds.** PG-backed scopes never invoke
   `recoverManagedBDCommand`. The new branch in
   `bdCommandRunnerWithManagedRetry` returns the wrapped error
   directly. `bdTransportRetryableError` is unchanged (Dolt-only).
7. **Mayor's repro succeeds.** With a PG-backed rig and a password in
   `<scope>/.beads/.env` (chmod 600), `gc bd --rig <rig> list` exits
   0. The same with the password in `$BEADS_CREDENTIALS_FILE`. The
   same with the password as a parent shell env var (resolver consults
   tier 5; strip step ensures no cross-contamination).
8. **Coverage and hygiene.** `go test ./cmd/gc/ -run "TestBd|TestPG|TestMergeRuntimeEnv|TestApply|TestMixedBackend|TestProjectedKeysCoverage" -count=1` passes; `go vet ./cmd/gc/` clean; new helpers carry godoc per design §8; no panics; no role names in the diff.

## Risks and unknowns

- **Slice 1 loader name not pinned.** Design §9.5 explicitly defers
  the `MetadataState` read function name (likely `LoadMetadataState`,
  `ReadMetadataState`, or `ResolveMetadataState`) to whatever slice 1
  shipped. Builder must grep `internal/beads/contract/files.go` for
  the actual loader before wiring the dispatcher. The two helpers
  `scopeBackendIsPostgres` and `scopePostgresEndpoint` use the same
  loader.
- **Strip-list ordering is sorted.** `mergeRuntimeEnv` calls
  `sort.Strings(keys)` after assembly, so insertion order is
  immaterial at runtime — but for diff-readability the design pins
  lexical placement (design §6.3). A reviewer who diffs against the
  Dolt block expects PG keys to land between `BEADS_DOLT_*` and
  `GC_*`. Builder must not append to the end.
- **`mirrorBeadsPostgresEnv` is structurally near-noop today.** The
  design (§6.2) makes it deliberate so a future `bd` rename of
  `BEADS_POSTGRES_PASSWORD` to `BEADS_PG_PASSWORD` (or similar)
  becomes a one-line patch. Builder may be tempted to delete it as
  dead code; design §9 explicitly forbids that.
- **PG transport markers are substring matches, not exact.** Design
  §6.4 + §9.3 pin seven lower-cased substrings for `pgTransportError`
  detection. Builder must implement substring-match (not exact-match)
  semantics; case-folding is part of the contract.
- **Slice-4 seam is "do nothing".** The `resolved` local in
  `applyResolvedScopePostgresEnv` is the slice-4 seam (design §11).
  Builder must NOT log `resolved.Source` or emit any event from this
  slice — that is slice 4's contract. A premature log line creates
  inconsistent surface that slice 4 has to undo.

## Out of scope (explicit)

- `gc doctor` PG checks, `--explain-postgres-auth` flag, and the
  resolver-source event payload — slice 4 (`ga-5c4x`).
- Integration test against a real Postgres instance — file as a
  follow-on bead labelled `needs-design` if the existing test matrix
  doesn't already cover it via podman/test-containers in CI.
- Changes to the `bd` binary itself — out of repo entirely.
- Managed-PG provisioning (gc bringing up a PG instance) — rejected
  in parent design (`ga-dga2 §risk-2`).
- A new `setProjectedPostgresEnvEmpty` / `clearProjectedPostgresEnv`
  helper pair. The Dolt analogues exist for managed-recovery flows
  that have no PG counterpart. Add only when a future feature needs
  them.
- Renaming or splitting `bdTransportRetryableError`. The PG branch
  exits before that function is consulted; the function stays
  Dolt-only by purpose.
- Multi-PG fallback / read-replica routing / connection pooling at
  the gc layer. None of these are in the architecture; PG concerns
  beyond credential projection live in `bd` upstream.
- New `PostgresConnectionTarget` struct. Design §9.4 pins the decision
  to take `meta contract.MetadataState` directly — no wrapper struct.

## Validation gates

- `go test ./cmd/gc/... -count=1` green.
- `go vet ./cmd/gc/` clean.
- `git diff` shows changes confined to `cmd/gc/bd_env.go`,
  `cmd/gc/bd_env_test.go`, and the ~3 call sites that reference
  `applyCanonicalScopeDoltEnv` (which become
  `applyCanonicalScopeBackendEnv`). No other files modified.
- Every error-message substring in design §4 appears verbatim in
  production code (literal-string grep).
- `grep -rn 'os.Getenv.*BEADS_POSTGRES\|os.Getenv.*GC_POSTGRES' cmd/ internal/`
  returns hits **only** under `internal/pgauth/` (slice 2's guard;
  this slice must not regress it).
- `grep -n applyCanonicalScopeDoltEnv cmd/gc/` returns no hits.
- `len(projectedPostgresEnvKeys) == 6` and every entry appears in
  `mergeRuntimeEnv`'s `keys` slice (CI guard test asserts the
  symmetry).
- New helpers (`applyCanonicalScopeBackendEnv`,
  `applyResolvedScopePostgresEnv`, `mirrorBeadsPostgresEnv`,
  `pgTransportError`, `scopeBackendIsPostgres`,
  `scopePostgresEndpoint`) carry godoc matching design §8 verbatim.
- ZFC: no role names in the diff.
- Typed wire: no `map[string]any` or `json.RawMessage` introduced on
  any wire boundary.
- Mayor's manual repro succeeds: PG-backed rig, password in
  `<scope>/.beads/.env`, `gc bd --rig <rig> list` exits 0.
