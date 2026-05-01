# Product Requirements Document: gc selfhost UX hardening (1.1.0)

Source bead: `ga-r8hs`
Generated: 2026-05-01 by `gascity/planner`
Milestone: 1.1.0
Type: umbrella PRD (synthesises seven per-bug PRDs)

## Problem Statement

On 2026-04-26, a mayor session was lost for hours debugging a cascading
series of `gc start` and `gc stop` failures during the pack v1 → v2
transition window. Each individual failure took 5–30 minutes to
root-cause; many error messages were misleading, under-informative, or
buried under retry-induced warning spam. The bug bundle (filed under
meta-tracker `ga-r8hs`) decomposes into seven concrete defects across
three clusters:

**gc start error UX**
- `ga-qpbe` — pack v1 → v2 silent duplicate (emits a generic
  duplicate-name error with no migration guidance)
- `ga-ytx2` — duplicate-name error with empty `SourceDir` for
  auto-imported system packs (forces a 30-minute scavenger hunt)
- `ga-7zi8` — N×retries of identical `deprecated order path` warnings
  drown the fatal cause at the bottom of the output

**Supervisor lifecycle robustness**
- `ga-9gdd` — `gc stop` runs the same validation pipeline as `gc
  start` and refuses to terminate when validation fails (firefighter
  needs a permit to enter the burning building)
- `ga-7kwr` — running `gascity-supervisor` keeps in-memory copies of
  the previous binary and parsed packs across `go install` rebuilds
  (no drift detection, no auto-restart)
- `ga-sn06` — `bd config set` calls under bd ≥ 1.0.3 trigger a
  schema auto-migration that exceeds the 30 s `op_init` provider
  timeout, putting the supervisor in a `starting_bead_store` retry
  loop with no actionable error

**Pack v2 documentation**
- `ga-fli0` — no published guide for migrating `[[agent]]` blocks
  from `pack.toml` (v1) to `agents/<name>/agent.toml` +
  `prompt.template.md` (v2); operators reverse-engineer the layout,
  the auto-import map, and `fallback = true` semantics from errors

The unifying user impact: a contributor who upgrades the SDK and
tries to run their existing city against new packs hits a 30+ minute
cascade of cryptic failures before they can even diagnose the
problem. First-time tutorial users see ANSI-yellow walls of repeated
warnings, miss the fatal cause entirely, and conclude the SDK is
broken.

## Goals & Non-Goals

### Goals
- A first-time `gc start` against a working tutorial city produces no
  warning spam; a failing `gc start` produces a single visually
  distinct `FATAL:` line at the bottom of the output.
- Every duplicate-name error names both contributing sources in a
  way an operator can act on **without grepping the codebase**, and
  v1/v2 collisions emit a distinct migration-guidance error linking
  to a published migration page.
- `gc stop` always reaches its core action (terminate supervisor,
  clean up sockets/locks) regardless of pack/agent config validity.
- After a `go install ./cmd/gc` rebuild, the next `gc start` brings
  the running supervisor in line with the new binary and on-disk
  packs without requiring the operator to know about systemd.
- `op_init` is idempotent and fast (< 5 s p95) regardless of bd
  minor version.
- A pack maintainer can migrate a v1 `[[agent]]` block to the v2
  layout in < 10 minutes using a published Mintlify guide, with the
  v1/v2 collision error linking directly to that guide.

### Non-Goals
- Building a `gc fix --packs` auto-migration tool (separate PRD if
  desired; the fixes here document the manual procedure first).
- Deprecating v1 `[[agent]]` syntax (v1 remains valid; v2 is the
  recommended layout).
- Restructuring the validation pipeline globally — fixes are local.
- Solving binary-drift detection for non-systemd supervisor
  launches (out of scope for 1.1.0; tracked separately).
- Auto-migrating deprecated order paths (`formulas/orders/<name>/`)
  — out of scope; the warning surface is the focus.

## User Stories

- As a **first-time tutorial user**, I want my city to come up in
  under 30 s on first start, with errors that point me at the exact
  files I need to edit so I don't conclude the SDK is broken.
- As a **gc contributor**, I want my freshly rebuilt `gc` binary to
  take effect on the next `gc start` so I can iterate on changes
  without learning systemd internals.
- As a **city operator** in a config-driven outage, I want `gc stop`
  to terminate my city so I can repair config offline.
- As a **pack maintainer** wrapping a system pack, I want my users
  to get clear migration prompts when they hit the v1/v2 mismatch so
  I don't field repeated support tickets.
- As an **on-call engineer**, I want a stable `FATAL:` marker in
  `gc start` logs so I can grep paged-alert output programmatically.

## Functional Requirements (cross-cutting)

The seven detailed FR sets are owned by their per-bug PRDs (see
References §). The umbrella adds three cross-cutting requirements:

| ID | Requirement | Priority | Acceptance Criteria |
|----|-------------|----------|---------------------|
| FR-X1 | The seven fixes compose end-to-end. A `gc start` against a v1 pack colliding with a v2 system pack produces: a single distinct `FATAL:` line at the bottom, both source paths named, and a link to the migration guide — within the supervisor's existing 30 s `providerOpTimeout`. | Must | Integration test under `test/` (build tag) exercises the matrix end-to-end. |
| FR-X2 | A `docs/troubleshooting/gc-start-walkthrough.md` (or equivalent Mintlify page) collects the seven failure modes and their resolutions, cross-linked from the FATAL output and from `docs/getting-started/troubleshooting.md`. | Should | Page exists, is in `docs.json` navigation, and is referenced from at least the v1/v2-collision and op_init-timeout error paths. |
| FR-X3 | The 1.1.0 release notes / CHANGELOG include a "selfhost UX upgrades" section listing the seven shipped fixes with one-line summaries and bead links. | Must | Release notes entry exists at the milestone cut. |

## Non-Functional Requirements (cross-cutting)

| ID | Requirement | Metric |
|----|-------------|--------|
| NFR-X1 | First-time `gc start` time for a tutorial city. | < 30 s p95 (down from "hangs / never reaches ready" on bd ≥ 1.0.3). |
| NFR-X2 | Operator time-to-spot the fatal cause on a failed `gc start`. | < 5 min p95 (down from ~30 min anecdotally). |
| NFR-X3 | Output line count for a typical failed `gc start`. | < 1/3 of pre-1.1.0 output (warning de-dup × visual FATAL distinction). |
| NFR-X4 | Cross-cutting integration test wall-clock duration. | < 60 s under `go test -tags=integration ./test/...`. |

## Technical Constraints

Aligned across all seven per-bug PRDs (each PRD also lists its own
constraints from `CLAUDE.md`):

- **No status files — query live state.** No fix introduces a
  sentinel file ("init done", supervisor PID file, drift cache).
  State is queried from the process table, `/proc/<pid>/exe`,
  on-disk mtimes, and the supervisor's own API.
- **No premature abstractions.** Each fix is a localized change at
  the relevant call site. The umbrella does not introduce a shared
  "gc-start-error-format" library, "supervisor-lifecycle" framework,
  or "validation-severity" subsystem.
- **Tests next to code.** Per-bug regression tests live alongside
  their fixes. The cross-cutting integration test (FR-X1) lives
  under `test/` with the `integration` build tag.
- **Layering invariants.** All fixes stay in their declared layers
  (`internal/config`, `internal/supervisor`, `cmd/gc`). The
  validation-bypass policy on `gc stop` is a CLI-layer decision, not
  a validator-library policy.
- **Bitter Lesson.** Fixes are policy choices encoded in code, not
  configurable knobs. As error volume / supervisor complexity grow,
  the fixes remain useful without further tuning.
- **Tutorial harness compatibility.** Every fix preserves the Actual
  tutorial acceptance harness behavior (see `isolated-tutorial-
  harness` skill).

## Dependencies

- Each per-bug PRD owns its own dependency list. Cross-cutting:
  - **FR-X1** depends on all seven child fixes shipping or being
    available on a coordinated branch (four are already closed; three
    are ready-to-build — see Status §).
  - **FR-X2** (troubleshooting walkthrough) depends on
    `pack-v1-to-v2-migration-guide` (`ga-fli0`) URL stability and on
    the published `FATAL:` output format from
    `gc-start-warning-suppression` (`ga-7zi8`).
  - **FR-X3** (release notes) depends on the milestone cut date
    being scheduled — coordinate with PM for the 1.1.0 release.
- Mintlify docs build pipeline (`./mint.sh dev`, `make check-docs`).
- bd ≥ 1.0.3 (the version that exposed `ga-sn06`); the fast-path
  `op_init` (now shipped) keeps lower versions working too.

## Open Questions

These are cross-cutting questions beyond the per-bug PRDs' open
items. Architect / designer to resolve.

1. **Architect** — Should `gc start` produce a structured
   machine-readable summary line at the end (PID, binary path, drift
   status, warnings emitted, fatal cause)? The seven per-bug PRDs
   each touch this output surface; consolidating the output schema
   now avoids re-formatting later.
2. **Architect** — FR-X1's integration test harness: does the
   existing `test/` suite already have a "fresh-city-from-scratch"
   fixture, or does this need a new harness? If the latter, the
   harness work may exceed FR-X1's scope and want its own bead.
3. **Designer** — Where in the docs sidebar does FR-X2's
   troubleshooting walkthrough live: extend
   `docs/getting-started/troubleshooting.md`, sit under
   `docs/troubleshooting/` (where `dolt-bloat-recovery.md` already
   lives), or top-level `docs/walkthroughs/`? The choice affects
   FATAL-line link strings.
4. **Designer** — Should the FATAL-line ANSI style be a new style
   (e.g., bold-red on red-tint) distinct from existing error
   formatting, or a re-skin of the current style? Touches accessibility
   review (no-color mode, screen readers).
5. **PM** — Is the 1.1.0 release notes section (FR-X3) authored by
   the PM after work decomposition, or does it want a designer pass
   for tone? Recommend PM-owned with a designer review.

## Status

Children of `ga-r8hs` (per-bug PRDs and their downstream
implementation beads):

| Source bead | PRD slug | Architecture bead | Implementation bead | State |
|-------------|---------|-------------------|---------------------|-------|
| `ga-qpbe` | `pack-v1-v2-collision-detection` | `ga-9ogb` | `ga-9ogb.1` | shipped |
| `ga-ytx2` | `duplicate-name-error-source-paths` | `ga-tpfc` | `ga-tpfc.1` | shipped |
| `ga-fli0` | `pack-v1-to-v2-migration-guide` | `ga-6wrr` | `ga-6wrr.1` | designer pass / build-ready |
| `ga-7zi8` | `gc-start-warning-suppression` | `ga-q0bf` | `ga-q0bf.1` | ready-to-build |
| `ga-9gdd` | `gc-stop-bypass-validation` | `ga-r8iz` | `ga-r8iz.1` | ready-to-build |
| `ga-7kwr` | `supervisor-binary-stale-detection` | `ga-a3ry` | `ga-a3ry.1` (closed); `ga-xxqx` (review) | shipped (phase 2 in review) |
| `ga-sn06` | `gc-beads-bd-op-init-timeout` | `ga-5mym` | `ga-5mym.1` | shipped |

**Cross-cutting work unrouted as of 2026-05-01** (this PRD adds):

- FR-X1 — cross-cutting integration test → `needs-architecture`
- FR-X2 — `gc start` troubleshooting walkthrough page → `needs-design`

Cross-cutting handoff beads will be created by the planner after
this PRD is committed.

## References

- Source bead: `ga-r8hs`
- Per-bug PRD drafts (rig root, currently untracked working copy):
  - `docs/prd/duplicate-name-error-source-paths.md` (`ga-ytx2`)
  - `docs/prd/gc-beads-bd-op-init-timeout.md` (`ga-sn06`)
  - `docs/prd/gc-start-warning-suppression.md` (`ga-7zi8`)
  - `docs/prd/gc-stop-bypass-validation.md` (`ga-9gdd`)
  - `docs/prd/pack-v1-to-v2-migration-guide.md` (`ga-fli0`)
  - `docs/prd/pack-v1-v2-collision-detection.md` (`ga-qpbe`)
  - `docs/prd/supervisor-binary-stale-detection.md` (`ga-7kwr`)
- Existing pack v2 documentation: `docs/packv2/`
- Existing troubleshooting docs: `docs/troubleshooting/`,
  `docs/getting-started/troubleshooting.md`
- Source location for auto-import map (referenced by `ga-ytx2` and
  `ga-qpbe`): `internal/config/config.go:2679`
- Validator entry (referenced by both gc-start-error-UX PRDs):
  `internal/config/config.go validateAgents` (~line 2374)
- Build hash encoded in version output (referenced by `ga-7kwr`):
  commit `acc19d24`
- Hot patch and upstream PR for `op_init` (referenced by `ga-sn06`):
  commit `e98fda07`, bd PR #1264

## Cross-cutting

The seven per-bug PRDs already self-classify into two clusters
("gc start error UX" and "supervisor lifecycle robustness") plus one
cross-cluster doc deliverable (`pack-v1-to-v2-migration-guide`). The
umbrella treats them as a single 1.1.0 selfhost-UX initiative for
release-notes purposes (FR-X3) and for the cross-cutting integration
test (FR-X1).

Architect should review FR-X1 and FR-X2 against the per-bug PRDs to
confirm no duplicate work, and to decide whether the structured
end-of-output summary (Open Question 1) wants its own bead or rides
along with `ga-q0bf` (warning de-dup is the closest existing surface).
