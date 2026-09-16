# Plan: PG-auth slice 2/4 — `internal/pgauth` resolver package

> Owner: `gascity/pm` · Created: 2026-05-05
> Source architecture: `ga-dga2` (closed) — *gc bd subprocess strips
> BEADS_POSTGRES_PASSWORD env (blocks PG-backed rigs)*
> Source design: `ga-3hay` (designer-complete, ready to close)
> Sibling chain: `ga-0nmb` → `ga-pnqg` (slice 1 build, closed via PR #1727)
> · **`ga-3hay` (this slice)** · `ga-4qvs` (bd_env wiring) · `ga-5c4x` (doctor + observability)

## Why this work exists

Slice 1 (`ga-pnqg`) shipped the schema half: `MetadataState` can now declare
`backend: "postgres"` with host/port/user/database, and the loader rejects
mixed or malformed configs. That alone does nothing — gc still has no way
to *resolve a password* for a PG endpoint when it's about to spawn a `bd`
subprocess. Slice 2 closes that hole by adding a new package
`internal/pgauth` that mirrors the doltauth resolver one-for-one: same
seven-tier resolution chain shape, same chmod posture, same typed errors.

After this slice lands, slice 3 (`ga-4qvs`) can wire the resolver into
`cmd/gc/bd_env.go`'s subprocess-env path; slice 4 (`ga-5c4x`) can call
`Source.String()` from a `gc doctor` check and from a resolver-source
event payload. Both of those slices are blocked on the public surface this
slice produces.

## Goal

Land a single PR that:

- Adds a new package `internal/pgauth` with the public surface pinned in
  the design (`Endpoint`, `Resolved`, `Source`, `ResolveFromEnv`,
  `ReadStoreLocalPassword`, `PermissivePermissionError`,
  `CredentialsParseError`, `ErrNoPasswordResolvable`).
- Implements the seven-tier resolution chain with deterministic
  short-circuit on first non-empty value, and chain-termination on
  permission/parse errors.
- Ships table-driven tests covering every tier (positive + negative),
  every error path, and the `Source.String()` enum.
- Adds a CI guard test that asserts `os.Getenv("BEADS_POSTGRES_*")` and
  `os.Getenv("GC_POSTGRES_*")` are read **only** inside this package.

## Work breakdown

| Bead       | Title                                                              | Priority | Routes to | Gate           |
|------------|--------------------------------------------------------------------|----------|-----------|----------------|
| `ga-3hay.1` | feat(pgauth): add Postgres credential resolver mirroring doltauth | P1       | builder   | ready-to-build |

The designer's notes (`ga-3hay` §13) explicitly call for a single PR:
"This slice produces one PR. The package, its tests, and the CI guard
form a tightly-coupled unit; splitting them creates false sync points."
The pm decomposition honours that — one builder bead, ~200 LOC of code +
~400 LOC of tests + ~50 LOC of CI-guard.

## Dependency graph

```
ga-pnqg (slice 1 build, closed) ─► ga-3hay.1 (slice 2 build)
                                      │
                                      ├─► ga-4qvs (slice 3 design)
                                      └─► ga-5c4x (slice 4 design)
```

`ga-3hay.1` declares a `blocks` edge from `ga-pnqg` for lineage; in
practice slice 1 is already merged on `main`, so the build can start
immediately. The slice-3 and slice-4 designer beads are open and will
reference slice 2's public surface; their builder beads (when pm
decomposes them) will get hard `blocks` edges on `ga-3hay.1`.

## Routing rationale

Slice 2 has been through architect (`ga-dga2`) and designer (`ga-3hay`).
The designer's notes pin: every `Source.String()` output (the eventing
contract for slice 4); the verbatim error text for `ErrNoPasswordResolvable`,
`PermissivePermissionError`, `CredentialsParseError`; the resolution-chain
reading order including chain-termination semantics on permission/parse
errors; the test fixture sketch; and five implementation-level decisions
the architect left implicit (chmod predicate, `ReadStoreLocalPassword`
posture, whitespace handling, `user=` override scope, process-env test
hygiene). There is nothing left to design — only build.

Routed to **builder** with `ready-to-build`. No validator hop because the
designer's fixture sketch (§10 of the design notes) is the test plan; the
builder authors fixtures + tests as part of the PR per package convention
(`auth_test.go` colocated with `auth.go`).

## Acceptance criteria (rolled up)

The full criteria live in the builder bead's notes and in `ga-3hay`'s
design (§3, §4, §5, §6, §7, §8, §9, §10, §12). Roll-up for stakeholder
visibility:

1. **Public surface mirrors the design.** Every type, function, and
   constant pinned in design §3 exists with the exact name, parameter
   shape, and return shape. No additional exported symbols, no missing
   ones.
2. **`Source.String()` is the eventing contract.** All eight strings
   from design §4 appear verbatim in production code. A table-driven
   test asserts each enum value's `String()` output exactly. Slice 4
   reads these values; renaming any is a breaking change.
3. **Resolution order is deterministic.** A test for each of the seven
   tiers asserts both the correct `Source` is reported and that
   higher-priority tiers short-circuit. Whitespace-only values are
   treated as empty (per design §9.3, mirroring `doltauth`).
4. **Permission and parse errors terminate the chain.** Design §5 pins
   that on a permissive-mode or malformed-credentials encounter at
   tier 4/6/7, the chain stops at that tier rather than falling
   through. Tests assert this exactly.
5. **Error wording is verbatim.** Every substring in design §6 (table
   for `ErrNoPasswordResolvable`, `PermissivePermissionError`,
   `CredentialsParseError`) appears literal in production code. Tests
   assert substring matches; runbooks may grep on these strings.
6. **CI guard prevents drift.** A test (`TestNoOsGetenvOutsidePgauth`
   or equivalent) walks `cmd/` and `internal/` and asserts that
   `os.Getenv("BEADS_POSTGRES_*")` and `os.Getenv("GC_POSTGRES_*")`
   appear **only** in `internal/pgauth/`. This mirrors the architect's
   guardrail and hardens it against future drift.
7. **Coverage and hygiene.** `go test ./internal/pgauth/ -count=1`
   passes; `go vet ./internal/pgauth/` clean; coverage ≥ 90%; new
   exported symbols carry godoc; no panics.

## Risks and unknowns

- **Wording stability is load-bearing.** Slice 4 (`ga-5c4x`) wires
  `Source.String()` into an event payload field tested by
  `TestEveryKnownEventTypeHasRegisteredPayload`. The eight strings in
  design §4 must be locked before slice 4 starts. Builder must not
  paraphrase.
- **Chain-termination semantics are subtle.** The intuitive choice is
  to fall through on a permission error to the next tier; the design
  pins the opposite — stop the chain, surface the error. The
  rationale is in §5 (operator put creds at this tier expecting them
  honoured; silent fall-through is a hidden config divergence).
  Builder must implement and test the chain-termination case for
  tier 4, 6, and 7 explicitly.
- **`doltauth` is the reference, not the future spec.** Design §9.4
  pins that `user=` override in credentials sections is **out of
  scope** for this slice — mirror `doltauth`'s actual code, not the
  architect's forward-looking "MAY override" language. If a builder
  reads the architect bead and sees the optional override, they must
  not implement it; the design pins it as a future bead.
- **Permissive-mode threshold edge cases.** Design §9.1 pins the
  predicate `mode & 0177 != 0` (rejects any group/other rwx bit), with
  the error wording recommending only `0600` or `0400` to operators.
  Builder must not narrow the predicate (e.g. "exactly 0600/0400"); a
  legitimately strict file at `0500` should still be accepted.

## Out of scope (explicit)

- Wiring `pgauth.ResolveFromEnv` into `cmd/gc/bd_env.go` — slice 3
  (`ga-4qvs`).
- `gc doctor` PG checks, `--explain-postgres-auth` flag, and the
  resolver-source event payload — slice 4 (`ga-5c4x`).
- Per-section `user=` override in credentials files (design §9.4).
- `Resolved.CredentialsFileOverride` field (`doltauth` has it; pgauth
  doesn't need it because no consumer reads it).
- Any change to the doltauth package — this slice is additive.
- Changes to `MetadataState` (slice 1 territory, closed).
- The bd binary's PG implementation — out of repo entirely.

## Validation gates

- `go test ./internal/pgauth/... -count=1` green; coverage ≥ 90%.
- `go vet ./internal/pgauth/` clean.
- `git diff` shows changes confined to `internal/pgauth/` plus the
  one CI-guard test file (which may sit in a higher-up package; see
  builder bead for placement).
- Every `Source.String()` value in design §4 appears verbatim in
  `auth.go`'s `String()` method (literal-string grep).
- Every error-message substring in design §6 appears verbatim in
  production code.
- `grep -rn 'os.Getenv.*BEADS_POSTGRES\|os.Getenv.*GC_POSTGRES' cmd/ internal/`
  returns hits **only** under `internal/pgauth/`.
- New exported symbols (`Endpoint`, `Resolved`, `Source`,
  `ResolveFromEnv`, `ReadStoreLocalPassword`,
  `PermissivePermissionError`, `CredentialsParseError`,
  `ErrNoPasswordResolvable`) carry godoc per design §8.
- ZFC: no role names in the diff.
- Typed wire: no `map[string]any` or `json.RawMessage` introduced on
  any wire boundary.
