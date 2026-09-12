# Plan: PG-auth slice 1/4 — Postgres-aware `MetadataState` + parse validation

> Owner: `gascity/pm` · Created: 2026-05-05
> Source architecture: `ga-dga2` (closed) — *gc bd subprocess strips
> BEADS_POSTGRES_PASSWORD env (blocks PG-backed rigs)*
> Source design: `ga-0nmb` (in-progress, ready to close)
> Sibling chain: `ga-3hay` (pgauth) → `ga-4qvs` (bd_env wiring) → `ga-5c4x` (doctor + observability)

## Why this work exists

Gas City cannot drive a Postgres-backed rig today: every `gc bd` subprocess
inherits an env scrubbed of `BEADS_POSTGRES_PASSWORD` (correct, by
`mergeRuntimeEnv`'s `IsSensitiveKey` filter), and there is no resolver to
project a password back in. The architect (`ga-dga2`) settled on the
explicit-projection model — `pgauth.Resolve` produces the password, gc
writes it to the child env under the bd-expected key — and decomposed the
work into four design slices. This plan covers slice 1 of 4: extending the
canonical metadata contract so a scope can *declare* PG, with parse-time
validation that prevents silent fall-through to a wrong backend.

This slice is the foundation. Slices 2–4 (`ga-3hay`, `ga-4qvs`, `ga-5c4x`)
all depend on `MetadataState.Backend == "postgres"` being a thing. Any
delay here delays the entire chain.

## Goal

Land a single PR that:

- Adds four PG-specific fields to `internal/beads/contract.MetadataState`
  (mirrors the existing Dolt fields).
- Adds `LoadMetadataState` — a typed, validating loader for
  `.beads/metadata.json`.
- Extends `EnsureCanonicalMetadata` with backend-discriminated field
  preservation/scrubbing, symmetric to the existing Dolt behaviour.
- Ships hand-edited fixtures and tests covering every rejection case and
  the byte-for-byte round-trip property.

## Work breakdown

| Bead       | Title                                                                  | Priority | Routes to | Gate           |
|------------|------------------------------------------------------------------------|----------|-----------|----------------|
| `ga-0nmb.1` | feat(contract): add Postgres MetadataState fields with parse validation | P1       | builder   | ready-to-build |

The designer (`ga-0nmb` notes §13) explicitly recommended a single PR
shape: "Splitting it would create false sync points between the struct
definition and the parser that consumes it." The pm decomposition honours
that — one builder bead, ~150 LOC of code + ~250 LOC of fixture-driven
tests.

## Dependency graph

```
ga-0nmb (design)  ─►  ga-0nmb.1 (build) ─► closes ga-0nmb
                                              │
                              ga-3hay's eventual builder bead
                                  will depend on ga-0nmb.1
```

`ga-3hay` (slice 2) currently lists a `parent-child` style edge from
`ga-0nmb`; the slice-2 designer can proceed against slice 1's *contract*
(captured in `ga-0nmb`'s notes) without waiting on the build. Once
slice 2 is decomposed by pm, that build bead will get a hard dependency
on `ga-0nmb.1`.

## Routing rationale

Slice 1 has been through architect (`ga-dga2`) and designer (`ga-0nmb`).
The designer's notes pin the operator-facing error catalogue verbatim,
the validation reading order, the function signature, and the four
implementation-level decisions the architect left implicit (§8.1–§8.4).
There is nothing left to design — only build.

Routed to **builder** with `ready-to-build`. No validator hop because the
designer's fixture sketch (§9 of the design notes) is the test plan; the
builder authors fixtures + tests as part of the PR per package convention
(`metadata_test.go` colocated with `files.go`).

## Acceptance criteria (rolled up)

The full criteria live in the builder bead's notes and in `ga-0nmb`'s
design (§3, §4, §5, §6, §7, §8, §9). Roll-up for stakeholder visibility:

1. **Schema additive only.** Existing Dolt-backed `metadata.json` files
   round-trip byte-for-byte unchanged (regression assertion).
2. **PG declared scopes parse cleanly.** A canonical PG `metadata.json`
   round-trips byte-for-byte; unknown keys are preserved.
3. **Wrong configs fail fast with operator-grade messages.** Five
   rejection cases (E1–E5 in design §4) emit verbatim error strings on
   stderr, citing absolute file path and the offending value. Reading
   order is deterministic (§5).
4. **Loader is composable.** `LoadMetadataState` returns
   `(state, ok, err)` matching the existing reader convention; the
   typed `MetadataParseError` lets slice 4's doctor discriminate parse
   failures from I/O failures without string-matching.
5. **Writer is symmetric.** `EnsureCanonicalMetadata` writes PG keys
   when the state declares them and scrubs cross-backend keys when
   `Backend` is explicit (per §8.3); leaves keys alone when `Backend`
   is empty.
6. **Tests over the table, not the code.** Every rejection wording is
   asserted as a substring; every valid fixture is asserted byte-equal
   after canonicalisation.

## Risks and unknowns

- **Wording stability.** §4's rejection wording is the contract.
  Builder must not paraphrase. Tests assert substring matches; runbooks
  and `gc trace` post-processing may grep on the literals. Treat the
  English text as an API.
- **JSON tag introduction is additive but new.** §8.1 recommends
  tagging every field on `MetadataState`, including the legacy four.
  This is zero-regression today (no caller marshals the struct
  directly), but a reviewer unfamiliar with the design should be
  pointed at §8.1's reasoning so the tag addition is not flagged as
  scope creep.
- **No JSON struct tags exist on `MetadataState` today.** The legacy
  schema is enforced exclusively through `EnsureCanonicalMetadata`'s
  explicit `defaults` map at `internal/beads/contract/files.go:260`.
  The builder must keep that map in sync with the new fields *and* add
  the struct tags for the loader path.

## Out of scope (explicit)

- `internal/pgauth` resolver package (slice 2 — `ga-3hay`).
- Wiring `LoadMetadataState` into `cmd/gc/bd_env.go` (slice 3 —
  `ga-4qvs`).
- `gc doctor` PG checks and `Resolved.Source` event surface (slice 4 —
  `ga-5c4x`).
- The bd binary's PG implementation. We only model what gc needs to
  *see*.
- Changes to `ReadDoltDatabase` or to `IsSensitiveKey` /
  `mergeRuntimeEnv`. The architect's guardrails (`ga-dga2` § Guardrails)
  forbid widening the inheritance filter.

## Validation gates

- `go test ./internal/beads/contract/... -count=1` green.
- `go vet ./internal/beads/contract/` clean.
- `git diff` shows changes confined to `internal/beads/contract/`.
- Every rejection wording in design §4 appears verbatim in `files.go`
  (literal-string grep).
- `internal/beads/contract/testdata/metadata/` contains every fixture
  in design §9; each has a paired test entry.
- New exported symbols (`LoadMetadataState`,
  `MetadataParseError`) carry godoc.
- ZFC: no role names in the diff.
- Typed wire: no `map[string]any` or `json.RawMessage` introduced on a
  wire boundary.
