# Beads ↔ Gas City Cross-Version Contract-Test System

> **Status:** Proposed (design for approval) — 2026-06-24
> **Companion to:** `engdocs/design/beads-dolt-contract-redesign.md` (defines the
> *contract*; this defines how we *test and gate* it across versions).
> **Decision owner:** integration-branch maintainers.

## Summary

`bd` (beads) and `gc` (Gas City) drift out of sync because gascity drives the
`bd` CLI as a subprocess across ~20 subcommands, parses its `--json` output into
a fixed struct, and keys runtime behavior off `bd`'s exit codes and free-text
error strings — yet **nothing tests the two products against each other across
versions.** CI pins one `bd` version (v1.0.4); code paths gated on newer `bd`
are never executed; a `bd` reword or shape change merges green and breaks at
runtime.

This spec locks the wire contract with **two mirrored, native test suites
bridged by one committed golden-JSON corpus**:

- **beads owns producer guarantees** — command/flag surface, `--json` wire
  shapes, exit codes, the `schema_version` canary — in its existing
  `cmd/bd/protocol/` package, regenerating the corpus under live-exec and
  failing on any unreviewed diff.
- **gascity owns consumer expectations** — the `bd` subset it calls, the
  substring error-string classifiers in `bdstore.go`, env projection, decoder
  behavior — asserting its decoder against the **same** committed blobs,
  offline and fast.
- A **cross-version matrix** (dolt backend only; `bd`-version × `gc`-version)
  proves **min-supported compatibility in both directions** and **blocks merge
  and release in both repos.**

The system is designed as much for **staying relevant** as for catching today's
bugs: a pin-consistency test, bidirectional corpus drift detection, an expiring
skip-ledger that forbids silent opt-outs, and a `schema_version`-anchored
cross-repo migration runbook.

## Why this is needed (root-cause evidence)

The recon catalogued **28 historical drift incidents**. The dominant class is
*version-gated code that CI never exercises*:

| Incident | What broke | Caught by |
|---|---|---|
| **#3135** | unconditional `bd list --skip-labels` broke `bd` 1.0.4 → every cache prime failed → thousands of live subprocesses, hung dashboards | `bd_prev × gc_current` cell actually runs the floor version |
| **#3948** | `bd close` exited 0 but an import-revert race rolled the close back to open | producer exit-code + persistence contract; consumer re-read defense asserted vs corpus |
| **#1726** | `bd` emitted literal `None` on a `--json` path → opaque JSON error | golden-corpus shape assertion on every `--json` command |
| **quad341 / v0.62** | `bd` *removed* commands gastown depended on | producer command-surface manifest fails when a command vanishes |
| **ga-5mym** | `bd config set` regressed to 18–50 s, overran the 30 s timeout, wedged the supervisor | cross-version behavior assertion on the exact subcommand gascity calls |

A meta-finding from building this spec underscores the need: **two independent
analysis agents disagreed on the branch's own version facts** (one claimed the
`#3135` surface was absent; direct verification proved it present). If automated
analysis drifts on version facts within one session, only an executable,
version-matrixed gate keeps the two products honest.

## Ground truth (verified on `fix/required-artifact-store-errors-ga-ksno8`)

Verified directly (not via agent report). **PRE-0 below re-verifies all of this
at implementation time** — line numbers are intentionally referenced by symbol
and job *name*, because pinning to a stale line is the exact drift this system
exists to kill.

- `internal/beads/bdstore_ready_projection.go` **exists**;
  `bdReadyProjectionMinVersion = "1.0.5"` is real and gates the ready-projection
  `bd sql` path.
- `--skip-labels` emit logic **exists**, gated by **explicit store config (not a
  version probe)** — `internal/beads/bdstore_test.go` pins exactly that rule.
- `config.go` `BDCompatibility` enum **exists**: `enum=bd-1.0.4,enum=bd-1.0.5`.
- `cmd/gc/init_provider_readiness.go` `bdMinVersion = "1.0.4"` (init floor).
- `deps.env`: `BD_VERSION=v1.0.4`, `DOLT_VERSION=2.1.7`, **no `BD_PREV_VERSION`**.
- `.github/scripts/install-bd-archive.sh` **already pins v1.0.5 SHAs** for
  linux/darwin × amd64/arm64 (plus v1.0.4, v1.0.3, v1.0.0).
- Published releases: **v1.0.4 = "Latest"**, **v1.0.5 = "Draft"** (assets exist,
  fetchable in CI with the repo token).
- `ci.yml`: aggregator jobs `check` and the terminal `ci-required` both exist;
  the **`beads` paths-filter is dead** (defined, zero consumers).
- beads CI: `pr.yml` (terminal `ci-gate` + `CI_GATE_REQUIRED`), `release.yml`
  (publishes checksums), `cross-version-smoke.yml` (a *bd-vs-bd upgrade* smoke —
  **not** a bd↔gc contract gate; keep distinct).

**Consequence:** the prerequisites shrink. The `#3135` surface is already here,
so "rebase it forward" is moot; PRE-1 is just `deps.env` pin work + confirming
v1.0.5 assets.

## Locked decisions

| # | Decision | Choice |
|---|---|---|
| **D1** | Architecture / ownership | **Mirrored suites + one golden-JSON corpus bridge.** beads = producer guarantees; gascity = consumer expectations. Each repo uses its native harness. Not a shared suite; not Pact. |
| **D2** | Fixtures | **Golden corpus + live-exec.** Committed canonical `--json` blobs are the cross-version diff anchor; beads regenerates under live-exec (fail on unreviewed diff); gascity asserts its decoder against the same blobs. |
| **D3** | Gating | **Block all matrix cells** in both repos' required fan-in. Cells are *pinned released versions*, so a release/pin bump is a deliberate, gated PR. A separate **non-required nightly radar** runs `bd@latest` so the hard gate never goes randomly red. |
| **D4** | Matrix | **dolt backend only.** Axes `bd_version` × `gc_version` over `{prev_release (=min supported), current}`. Required cells (drop prev×prev): `(bd_prev × gc_cur)`, `(bd_cur × gc_cur)`, `(bd_cur × gc_prev)`. Goal: prove min-supported compat in **both** directions. |

## Architecture

```
        ┌─────────────────────────────┐         ┌──────────────────────────────┐
        │  BEADS  (producer)          │         │  GAS CITY  (consumer)        │
        │  cmd/bd/protocol/           │         │  internal/beads/contract/    │
        │                             │         │                              │
        │  corpus.go  (generator)     │ regen   │  corpus_decoder_test.go      │
        │  corpus_test.go (diff-guard)│ ──┐     │   (offline, fast, default)   │
        │  schema_version_test.go     │   │     │  bd_contract_live_test.go    │
        │  testdata/corpus/  ◀────────┼─┐ │     │   (//go:build bd_contract)   │
        └─────────────────────────────┘ │ │     │  testdata/corpus/ (vendored) │
                 │ release.yml           │ │     └──────────────────────────────┘
                 │ corpus.tar.gz+sums    │ │ make sync-bd-corpus (release-anchored)
                 ▼                       │ └────────────▲
        signed beads release ───────────┴──────────────┘  scripts/bd_corpus_drift_test.go
                                                            (vendored bytes == release sums)
```

- **Producer (beads `cmd/bd/protocol/`):** builds on the existing harness
  (`json_contract_test.go`, `exit_codes_test.go`, isolated Dolt-per-test, builds
  `bd` from source with `gms_pure_go`). New: a deterministic, canonicalized
  corpus generator + a regenerate-and-diff guard.
- **Bridge (the corpus):** per-command JSON blobs + a provenance manifest,
  authoritative in beads, **vendored** into gascity.
- **Consumer (gascity `internal/beads/contract/`):** an **offline** decoder
  test (no `bd`, no Dolt — runs in the fast default loop) plus a build-tagged
  **live** suite that runs in the matrix against the installed release `bd`.

## Golden corpus

**Format** — a directory of per-command blobs (small reviewable diffs), not one
mega-file:

```
corpus/
  manifest.json            # {schema_version, bd_version, bd_commit, generated_*, blobs:{path:{cmd,sha256}}}
  flat/     create.json show.json list.json ready.json sql.json version.json dep_list.json count.json error.json
  envelope/ <same set>     # BD_JSON_ENVELOPE=1 → {schema_version,data} v2.0 variant
```

Blobs are **canonicalized** (bead IDs → `<ID0>`, timestamps → `<TS>`, arrays
stable-sorted) so the same logical output is byte-identical across regenerations
and the diff anchor only moves on a *real* wire change.

- **Authoritative:** `beads/cmd/bd/protocol/testdata/corpus/`.
- **Vendored:** `gascity/internal/beads/contract/testdata/corpus/`.
- **Provenance:** `bd_version`/`bd_commit` captured live from `bd version`;
  `schema_version` read live from `JSONSchemaVersion` and re-asserted.
- **Integrity anchored to the signed release, not a self-recomputed manifest:**
  beads' `release.yml` publishes `corpus.tar.gz` + a checksums file; gascity's
  `bd_corpus_drift_test.go` asserts vendored bytes match the **release-published**
  checksums for `deps.env BD_VERSION`. This closes the self-reference the
  red-team flagged.
- **Transport:** `make sync-bd-corpus` (human-run during a deliberate pin-bump
  PR) downloads `corpus.tar.gz` from the beads **release tag**, verifies against
  release checksums, copies in, commits. *Rejected:* git submodule (a second
  out-of-band pin) and download-at-test-time (non-hermetic).
- **Regeneration:** beads `make corpus-regen` builds `bd`, spins an isolated
  Dolt workspace, writes `testdata/corpus/`; `corpus_test.go` git-diffs and hard-
  fails on an unreviewed diff. A wire change **cannot** ship without regenerating
  the corpus **and** bumping `JSONSchemaVersion`. gascity never regenerates.

## Version matrix

- **Axes:** `bd_version` × `gc_version`, each `{prev_release(min supported), current}`. Backend **dolt only**.
- **Required cells** (drop prev×prev):

  | Cell | bd | gc | Runs in | Proves |
  |---|---|---|---|---|
  | 1 | **prev** (v1.0.4, Latest) | HEAD | gascity CI | new gc still supports min `bd` (the **#3135** direction) |
  | 2 | **current** (v1.0.5) | HEAD | gascity CI | new gc works with current `bd` (exercises the 1.0.5 ready-projection path) |
  | 3 | HEAD | **prev gc release** | beads CI | new `bd` doesn't break shipped gc (the direction gascity CI structurally cannot cover) |

- **Acquisition:** gascity downloads `bd` release tarballs via the existing
  SHA-pinned `install-bd-archive.sh` (API-SHA fallback **forbidden** on the
  required path). beads builds `bd` from source (HEAD) and downloads gascity
  release tarballs via a new SHA-pinned `install-gc-archive.sh`.
- **v1.0.5 note:** currently a *Draft* with assets pinned — fetchable in CI with
  the token. Promoting `deps.env BD_VERSION → v1.0.5` is the deliberate,
  gate-guarded migration that PRE-1 stages once v1.0.5 publishes.
- **Implementation note (current Phase-1 state).** The matrix above is the
  *target* shape: the upper required cell is a published `BD_VERSION` release
  installed via the `install-bd-archive.sh` tarball. Today's shipped `deps.env`
  and `ci.yml` realize the bleeding-edge `current` cell with a third, distinct
  anchor instead — `BD_CURRENT_VERSION` (`v1.1.0-rc.1`, a release-candidate with
  **no** release tarball) built from beads source at the SHA-pinned
  `BD_CURRENT_REF` by the `contract-acceptance-current` job, with
  `contract-radar-bd-head` as the non-required radar. So the three `deps.env`
  bd anchors map onto the matrix as: `BD_PREV_VERSION` = the min-supported floor
  cell (tarball), `BD_VERSION` = the installable default the floor tracks
  (tarball), and `BD_CURRENT_VERSION`/`BD_CURRENT_REF` = the source-built `current`
  cell. The doc's `BD_VERSION`-tarball upper cell stays the destination once
  v1.0.5 publishes; until then the source-built `BD_CURRENT_*` path stands in for
  it, and `bd_version_pin_test.go` keeps `BD_PREV_VERSION ≤ BD_VERSION` and the
  init/ready floors anchored to those roles.

## Enumerated test catalog

**67 cases across 11 domains × 3 sides** (41 P0 = would have caught a real
shipped incident; 21 P1 = untested high-risk; 5 P2). Every one of the 28
incidents is traced to a regressing case. Lives in
`internal/beads/contract/CATALOG.md` (gascity) + `cmd/bd/protocol/CATALOG.md`
(beads, producer cross-refs). P0 highlights:

- **Shapes (golden-corpus):** `bd-create-json-object-shape`,
  `bd-show-json-array-shape`, `bd-list-json-envelope-shape`,
  `bd-ready-json-shape`, `bd-sql-row-array-shape`, `bd-error-envelope-shape`,
  `bd-json-envelope-v2-variant`.
- **Classifiers (gascity-consumer):** `bd-notfound-error-strings` (keyed to the
  *actual* `bdstore.go` substrings `not found` / `no issue found` — the code does
  **not** match `issue not found` despite a comment; the catalog records this),
  `bd-claim-conflict-error-string`, `bd-silent-fallback-autoimport-string`.
- **Behavior/version:** `gc-config-set-not-required`, `gc-bd-version-projection`
  (`BD_BACKUP_ENABLED`/`BD_EXPORT_AUTO` on every call), `gc-devel-version-passes`,
  `gc-version-probe-fail-open`, `bd-version-pin-consistency`, `bd-raw-sql-issues-schema`,
  `gc-exec-store-full-crud-bridge`, `gc-runtime-state-first-required`,
  `doltlite-read-schema-parity`.
- **Cross-version cases** (run on >1 cell): all `bd-*` producer + golden-corpus
  cases on `bd_prev`/`bd_current`; on the consumer side
  `gc-ready-enrich-is-blocked-gate`, `gc-bd-prefix-vs-beads-prefix-optouts`,
  `gc-parse-bd-version-rc-boundary`, `gc-schema-version-canary-decode`. Decoder/
  classifier/resolver behavior is version-independent → single-version.

## Maintainability machinery (how it stays relevant)

| Mechanism | Prevents |
|---|---|
| **`bd_version_pin_test.go`** — collapses `BD_VERSION`, `bdMinVersion`, `bdReadyProjectionMinVersion`, the compat enum, and the install-script SHAs into one source of truth (mirrors the existing `dolt_version_pin_test.go`) | independently-edited version anchors drifting; a forgotten matrix SHA surfacing as a live install failure instead of a fast pin-test failure |
| **regenerate-and-diff + canonicalize double-run self-test + release-anchored drift-check** | a wire change shipping without a reviewed corpus diff; non-determinism eroding the anchor; a stale/hand-edited vendored blob passing the gate |
| **`schema_version` canary + bump-check + manifest equality** | `bd` shipping a wire change without bumping the canary; gascity decoding against an unmigrated corpus |
| **One expiring `ConformanceSkip` ledger** (required `BeadID` + `Expiry ≤ 90d`; past-expiry → hard `t.Fatalf`), applied uniformly to `SkipTxApplyConformance`, the corpus `IgnoredFields` allow-list, and the canonicalization allow-list | drift-laundering — every opt-out is loud at definition, loud over time, impossible to add silently |
| **Generation (retryable on Dolt-boot infra flake) separated from assertion (committed-corpus decode, never retried)** | a flaky required gate training maintainers to re-run-until-green |
| **doltlite parity decodes the *same* vendored blobs + schema floor** | doltlite's hand-rolled read schema silently rotting; the parity test freezing at authoring time |
| **Bidirectional field-coverage diffing** | a new `bd` field landing in the corpus but ignored by gc; gc decoding a field the pinned `bd` doesn't emit |

## Coordination protocol (`schema_version`-anchored)

**Principle:** `bd` is the producer and moves first; gascity follows; the
committed corpus is the handoff token; pins only advance *after* a release
exists. The gate guards **pinned releases, never HEAD-vs-HEAD**, so the two HEADs
are never simultaneously red on each other.

- **Non-wire bd change:** corpus regenerates identically, canary unchanged,
  merges normally, no gascity action. (Most changes.)
- **Standard wire change (additive, backward-readable):** (1) beads PR makes the
  change, bumps `JSONSchemaVersion`, regenerates + commits the corpus; (2) `bd`
  release publishes tarball + signed `corpus.tar.gz` + checksums; (3) **one**
  gascity pin-bump PR (gate-guarded) bumps `BD_VERSION`, `make sync-bd-corpus`,
  bumps the consumer `schema_version` pin, updates the decoder + classifiers,
  adds the new SHA — all together; `bd_version_pin_test.go` forces every anchor
  to move at once.
- **Breaking wire change (e.g. flipping `BD_JSON_ENVELOPE` default) → 3-PR
  ratchet** (a 2-PR dance deadlocks under block-all, because beads CI gates
  bd-HEAD against a *pinned gc-prev* that can't decode the new shape):
  1. **beads** ships the new shape behind the env flag only; corpus carries both
     `flat/` and `envelope/`; releasable with zero red cells.
  2. **gascity** teaches the decoder to unwrap `.data` *before* any default
     change; cuts a gc release that decodes it.
  3. **beads** advances the `gc_prev` pin to that release — *then* flips the
     default. **Gate rule:** a wire-*default* change requires `gc_prev` already
     decodes the new shape.
- **Deadlock escape hatch:** if a breaking change must land before gascity can
  follow, the gascity pin-bump PR raises `BD_PREV_VERSION` to **drop** the
  incompatible min-supported version in the same PR; the matrix shrinks to
  compatible cells. Releasing `bd` is never blocked by gascity (beads CI only
  tests *pinned gc releases*, never unreleased gc HEAD). The non-required radar
  goes red the morning after a `bd` release gascity hasn't consumed — lead time
  to stage the pin-bump before anyone turns the hard gate red.

## CI wiring

- **gascity `ci.yml`:** new path-gated `bd-contract` job (matrix = cells 1 & 2,
  gc always HEAD, `bd` pinned to `{BD_PREV_VERSION, BD_VERSION}` via
  `install-bd-archive.sh`), wired into the **terminal `ci-required` aggregator's
  `needs`** and its `allow_skipped` set (it's path-gated). Broaden the dead
  `beads` paths-filter to also watch `internal/beads/contract/testdata/**`,
  `cmd/gc/init_provider_readiness.go`, `deps.env`, `install-bd-archive.sh`. The
  **offline** `corpus_decoder_test.go` + `bd_corpus_drift_test.go` +
  `bd_version_pin_test.go` ride the existing fast unit baseline (no new job, no
  build tag) and gate *every* PR. `rc-gate.yml` needs **zero** new wiring — it
  calls `ci.yml`, so `bd-contract` rides in via `ci-required`.
- **beads `pr.yml`:** new `contract-corpus` job (build `bd`, regenerate, hard-
  fail on diff, schema-version bump check) + `gc-contract` job (cell 3:
  bd-HEAD × pinned gc-prev via `install-gc-archive.sh`). Both wired into the
  terminal `ci-gate.needs` + `CI_GATE_REQUIRED`; neither in any skipped-ok list.
  `cross-version-smoke.yml` stays untouched and distinctly named.
- **beads `release.yml`:** package `corpus.tar.gz` + checksums into the signed
  release.
- **Nightly radar (non-required):** `bd@latest-tag` against gc HEAD, on a cron
  off the `:00` mark; never in any required `needs`; surfaces failures via issue/
  Slack. Tests the *future* pin; the hard gate tests the *current* pin.

## Phased plan

| Phase | Goal | Deliverables | Acceptance | Depends |
|---|---|---|---|---|
| **0 — Re-ground + prereqs** | kill phantom-anchor / unrunnable-cell failures before any test exists | PRE-0 grep-anchored inventory; confirm v1.0.5 publishable + assets; `deps.env` `BD_PREV_VERSION=v1.0.4` (+ promote `BD_VERSION→v1.0.5` once published) | every asserted literal verified present; v1.0.5 installs in CI without API fallback | — |
| **1 — Cheap precursors** | one source of truth for pins; give the dead filter a consumer | `scripts/bd_version_pin_test.go` (P1); broaden + wire the `beads` paths-filter (P2) | pin test passes in fast baseline, fails loudly on any anchor/SHA drift; filter triggers the contract path | 0 |
| **2 — Producer (beads)** | canonicalized corpus under live-exec with deterministic guards | `corpus.go`, `corpusgen/main.go`, `corpus_test.go`, `canonicalize_test.go`, `schema_version_test.go`, `regenerate-corpus.sh`, initial `testdata/corpus/` | byte-identical across two regens; diff = hard fail, Dolt-boot = retry; canary bump enforced; flat + envelope for all commands | 1 |
| **3 — Corpus transport** | corpus = signed release artifact, vendored with release-anchored drift-check | beads `release.yml` (corpus.tar.gz + checksums); gascity `sync-bd-corpus.sh` + `make sync-bd-corpus`; vendored `testdata/corpus/`; `bd_corpus_drift_test.go` | sync refuses on mismatch; drift-check fails on hand-edit/stale; anchored to release checksums | 2 |
| **4 — Consumer (gascity)** | decoder asserted offline + live; every opt-out governed | `corpus_decoder_test.go` (offline, bidirectional), `manifest.go`, `bd_contract_live_test.go` (`//go:build bd_contract`), `ConformanceSkip` ledger, doltlite parity + schema floor | offline test runs with no bd/Dolt; ledger rejects unledgered/expired/bead-less skip; doltlite parity decodes matrix-current blobs | 3 |
| **5 — CI wiring** | block all cells in both repos' required aggregators + radar | gascity `bd-contract` job → `ci-required`; beads `contract-corpus` + `gc-contract` → `ci-gate`/`CI_GATE_REQUIRED`; `install-gc-archive.sh` + gc pins; nightly radar | all three cells block merge in both repos; radar reachable, never required; gc pins resolve to downloadable SHA-pinned assets | 4 |

## Residual risks

- **doltlite is outside the dolt-only matrix** → covered only for corpus-
  exercised commands. *Mitigation:* single-version parity test tied to the same
  vendored corpus + a schema floor that turns silent read-corruption into a loud
  error; expand the corpus when doltlite gains a read path.
- **Corpus duplicated (authoritative + vendored)** → a skipped sync leaves
  gascity on a stale blob. *Mitigation:* `bd_corpus_drift_test.go` fails the fast
  offline gate on any mismatch; `manifest.bd_version == deps.env BD_VERSION`.
- **`gc-contract` downloads a gc tarball** → a bad `gc_prev` pin reds an
  unrelated `bd` PR. *Mitigation:* validate both gc pins resolve to SHA-pinned
  assets before marking `gc-contract` required.
- **Offline test asserts vs vendored blobs** → a gc bug only visible against a
  real release escapes it. *Mitigation:* the live cross-version cells stay
  required and are never dropped in favor of the offline test.

## Open questions (genuinely open after verification)

1. **v1.0.5 publication:** it's a Draft today. Promote `BD_VERSION→v1.0.5` only
   once it publishes (so `gc_prev/gc_current` semantics hold), or run the
   `current` cell against the draft's pinned assets in the interim?
2. **beads `release.yml` checksums:** does it already publish a checksums file
   the drift-check can anchor to, or is that added in Phase 3?
3. **Radar home:** extend `nightly.yml` vs a standalone `bd-contract-radar.yml`;
   who owns the red-radar issue/Slack surface?
4. **CODEOWNERS** for `internal/beads/contract/**`, the corpus dir, `deps.env`,
   and the pin/skip tests — so every contract evolution is reviewed by wire
   owners.
5. **gc release asset naming** (`gascity_<v>_linux_amd64.tar.gz` vs `gc_<v>_…`)
   to target `install-gc-archive.sh` before `gc-contract` is required.

## Tracking

On approval, file the work in **bd** (not markdown TODOs, per `AGENTS.md`): one
epic per phase, with the 67 catalog cases as child beads tagged by domain/side/
priority, and the open questions as blocking beads on Phase 0.
