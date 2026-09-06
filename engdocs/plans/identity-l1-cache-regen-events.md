# Plan: cache regen + bd-wrapper update + identity event (ga-3ski1 child C)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-3ski1` — three-layer identity with one
> git-tracked authority.
> **Designer spec:** `ga-ue241` (closed; 890 lines + 3 graphviz visuals)
> — full verbatim contract for three deliverables and the shared
> `--city` flag.
> **Sibling A (closed; landed):** contract package (`ga-401s4` /
> `ga-7o5mb` / `ga-b4gug` / `ga-4tg3j`).
> **Sibling B (in flight; blocker):** `ga-h0pyln` — reconcile flow +
> endpoint validator. This bead's blocked-by edge points here.
> **Sibling D (deferred):** `ga-xxgld` — doctor + k8s polish.
> **Decomposed into:** 1 builder bead (see Children below).

## Context

After child A landed the typed L1 contract package and child B
(`ga-h0pyln`, in flight) lands the 11-row reconcile flow, child C
closes the "L1-authoritative-in-practice" loop with three additive
deliverables on top of the reconciler:

1. **Cache regen in `EnsureCanonicalMetadata`** — every call to
   the canonicalizer reads L1 and silently realigns
   `meta["project_id"]` when L1 disagrees. Non-coordinated writers
   (bd, k8s patch script, manual edit) get auto-corrected on the
   next `gc bd …` invocation.
2. **`gc-beads-bd.sh` simplification** — remove the
   `metadata_has_project_id` precondition gate; rename
   `backfill_project_id_if_missing` → `ensure_project_identity`;
   always invoke the reconciler unconditionally with the new
   `--city` flag.
3. **Identity-stamped event** — every layer write emits one
   `project.identity.stamped` event on the city event bus, with a
   typed payload (ScopeRoot, Source, Layer, OldID, NewID) so drift
   is post-mortem-able.

The designer pinned every byte: signature-unchanged decision
(§3.1), Source/Layer closed enums (§5.3), payload struct shape
(§5.2), wrapper rename verbatim (§4.1), --city flag plumbing
(§6), and 7 test contracts (§7).

## Why one builder bead

The designer explicitly offered "one PR or three small ones —
sliced by deliverable — at the pm's preference." This decomposition
opts for **one builder bead** because:

- Deliverable 2 (wrapper) passes `--city "$GC_CITY_PATH"` to the
  reconciler.
- Deliverable 3 (event) consumes `--city` to compute the
  `events.jsonl` path.
- Both depend on §6's shared `--city` flag introduction.
- Splitting forces one of the three PRs to own the `--city` flag
  while the others stub it, introducing temporary state across the
  merge window.

The work is bounded (~6 files, 7 tests, ~250 LOC total) and
matches the prior decomposition pattern (`ga-rq2e5a`, `ga-9h05hk`,
`ga-cv6ome`) where one builder bead carries a tight verbatim
contract.

## Children

| ID            | Title                                                                              | Routing label    | Routes to         | Priority | Depends on |
|---------------|------------------------------------------------------------------------------------|------------------|-------------------|----------|------------|
| `ga-qku0jy`   | feat(identity): cache regen + bd-wrapper update + identity event (ga-3ski1 child C / ga-ue241) | `ready-to-build` | `gascity/builder` | P0       | `ga-h0pyln` |

## Acceptance for the parent (ga-ue241)

Met when `ga-qku0jy` closes and all of the following hold (mirroring
the designer's §8 acceptance checklist):

### Deliverable 1 — Cache regen
- [ ] `EnsureCanonicalMetadata` reads L1 via
      `contract.ReadProjectIdentity` when L1 is present and
      non-empty; updates `meta["project_id"]` from L1.
- [ ] Signature is unchanged — `(fs, path, state) (bool, error)`.
      `scopeRoot` derived internally.
- [ ] L1 parse error propagates as non-nil error from
      `EnsureCanonicalMetadata`.

### Deliverable 2 — Wrapper
- [ ] `gc-beads-bd.sh::metadata_has_project_id` removed.
- [ ] `gc-beads-bd.sh::backfill_project_id_if_missing` renamed to
      `ensure_project_identity`; both call sites (1896, 1957) updated.
- [ ] `gc-beads-bd.sh::identity_toml_present` added but NOT wired
      as a guard in `ensure_project_identity`.
- [ ] `ensure_project_identity` always invokes
      `gc dolt-state ensure-project-id` (no precondition gate);
      passes `--city "$GC_CITY_PATH"`.
- [ ] `bd migrate --update-repo-id` fallback removed.

### Deliverable 3 — Event
- [ ] `ProjectIdentityStamped` const in `internal/events/events.go`
      with value `"project.identity.stamped"` and present in
      `KnownEventTypes`.
- [ ] `ProjectIdentityStampedPayload` struct in
      `internal/api/event_payloads.go` with the five JSON-tagged
      fields per §5.2; implements `IsEventPayload()`.
- [ ] `events.RegisterPayload(events.ProjectIdentityStamped,
      ProjectIdentityStampedPayload{})` added.
- [ ] `gc dolt-state ensure-project-id` accepts required `--city`
      flag.
- [ ] `events.NewFileRecorder` opens at
      `<city>/.gc/events.jsonl`; failure degrades to
      `events.Discard` with stderr log.
- [ ] Reconciler emits one event per layer write per the §5.3 tuple
      table; `generated` row emits three events (L1, L2, L3);
      no-op and refusal rows emit zero events.
- [ ] `Source` enum closed set: `"generated"`,
      `"migrated_from_metadata"`, `"migrated_from_database"`,
      `"cache_repair"` — no others.
- [ ] `Layer` enum closed set: `"L1"`, `"L2"`, `"L3"` — no others.
- [ ] `TestEveryKnownEventTypeHasRegisteredPayload` continues to pass.

### Build hygiene
- [ ] `go vet ./...` clean.
- [ ] `go test ./...` green.

## Notes for the builder

- **Read ga-ue241 in full.** 890 lines is the contract; the bead
  body is a summary. Every literal string in the wrapper, every
  Source/Layer enum value, every event field name is pinned to
  the byte.
- **Blocked-by ga-h0pyln.** All three deliverables are additive on
  top of child B's 11-row reconciler. Wait for ga-h0pyln to land
  before starting.
- **`EnsureCanonicalMetadata` signature is deliberate.** §3.2's
  trade-off table records the decision: internal derivation
  avoids editing 25 call sites (22 of which are tests). Don't
  "improve" by adding a `scopeRoot` parameter.
- **Source/Layer enums are closed sets.** No `"unknown"`,
  no `"L0"`, no `"forced"`. The reconciler picks the tuple from
  the §5.3 row table; tests assert literal-string equality.
- **`events.Discard` for recorder fallback.** When
  `NewFileRecorder` fails, the existing package-level
  `events.Discard` provides the same `Record` shape silently.
  Don't introduce a custom no-op recorder.
- **Per-layer events, not combined.** The fresh-generate row emits
  THREE events (one per layer write). Don't collapse into one
  event with a multi-layer field — that breaks the
  "grep by scope_root in the bus" usability the architect designed for.

## Out of scope

These belong to sibling D (ga-xxgld) and must not creep into
this slice:

- `gc doctor` integration for identity drift detection.
- k8s pod-side L1 projection (python patch script update).
- Reconciler on read-only `gc bd …` paths (rejected per §4.3 /
  NFR-2; no extra round-trips on hot path).
- Removing `metadata.json#project_id` entirely (long-term follow-up
  after several months of L1 stability).

## Validation gates

- `go test ./internal/beads -run TestEnsureCanonicalMetadata -count=1`
  green (3 tests: regen, preserve, parse-error).
- `go test ./cmd/gc -run TestEnsureProjectID -count=1` green
  (3 tests: emits-stamped, emits-nothing-on-noop,
  emits-nothing-on-refusal; with 5 subtests in the emits-stamped case).
- `go test ./internal/api -run TestEveryKnownEventTypeHasRegisteredPayload -count=1` green.
- `go test ./cmd/gc -run TestNoBashCleanupProjectIDGuard -count=1`
  green (static content check on the wrapper).
- `go test ./... -count=1` green; `go vet ./...` clean.
- ZFC: no role names in the diff.
- Typed wire: `ProjectIdentityStampedPayload` is a registered typed
  payload (no `map[string]any`).

## Risks and unknowns

- **`io.Discard` parameter to `NewFileRecorder`.** §5.5 sample
  passes `io.Discard` as the second arg; builder confirms the
  signature against the existing `events.NewFileRecorder` source.
  If the signature differs, the design's intent (best-effort
  recorder) is preserved — adapt the call shape; don't reject
  best-effort.
- **`bd_wrapper_test.go` location.** §7.7 leaves the wrapper-test
  file to the builder's judgment. Place near existing shell-script
  static tests if any; otherwise create at `cmd/gc/bd_wrapper_test.go`.
- **L1 parse-error wording.** §3.3 says the error surfaces from
  `EnsureCanonicalMetadata`; the actual wording is the existing
  contract package's wording, unchanged. Tests assert non-nil
  error, not literal string.
- **Scope-root relative-path handling.** §5.3 says ScopeRoot is
  relative to city root; absolute paths "leak machine-local info."
  Builder MUST compute `relScopeRoot` via `filepath.Rel(cityPath,
  scopeRoot)`. The test (§7.4) asserts the relative form; if
  computation yields `..` (scope outside city), surface as an
  error (or empty string + warning, builder's choice — verify with
  the reconciler maintainer if ambiguous).
