# Plan: Config-drift hash upgrade-compatibility (`ga-s760` family)

> Owner: `gascity/pm-1` · Created: 2026-04-27
> Source: architecture decision `ga-s760` (closed)
> Investigator handoff: `ga-40qz`

## Why this work exists

Production saw 160 false-positive config-drift drains in one city in one
period. Investigation (`ga-40qz`) confirmed Mode A: `runtime.CoreFingerprint`
output is unstable across binary versions, but the reconciler treats
old-format and new-format hashes as the same domain. A binary upgrade that
adds a new hash input mass-restarts every existing session.

Architecture (`ga-s760`) adopted **proposal #1: version the stored hash**
(`v1:<hex>`). The reconciler treats unversioned and version-mismatched
stored hashes as silent rebaseline — no drain, no event. Forward-compat
(proposal #3) was rejected as too subtle; migration command (proposal #2)
is subsumed by silent rebaseline.

Mode B (non-converging drift; mayor's `gm-8r7cd5y` observed three distinct
hashes in ~30 min) is **not gating**. We ship the Mode A fix and rely on
richer diagnostics (MF-A) and a cross-tick regression test (MF-B) for
ongoing visibility.

## Goal

Eliminate false-positive drift drains on binary upgrade and give operators
the diagnostic signal needed to characterize any remaining drift in one
log read.

## Work breakdown

| Bead       | Title                                                  | Priority | Routes to | Gate          |
|------------|--------------------------------------------------------|----------|-----------|---------------|
| `ga-s760.1` | Hash versioning + drift-check upgrade-compat           | P0       | validator | needs-tests   |
| `ga-s760.2` | MF-A: Per-entry `CopyFiles` diff in drift dump          | P1       | validator | needs-tests   |
| `ga-s760.3` | MF-B: Cross-tick deterministic hash regression test    | P1       | validator | needs-tests   |
| `ga-s760.4` | MF-D: Engdocs Fingerprint Versioning section           | P2       | builder   | ready-to-build|

The architecture decision already broke the work down to MF-A / MF-B / MF-D
grain, plus the load-bearing versioning fix. No further decomposition is
needed at PM level. Each bead has both an architecture spec (in description)
and a designer operator-UX analysis (in notes).

## Dependency graph

```
ga-s760.1 (versioning)  ─────┐
                              └──> ga-s760.4 (engdocs describe versioning)
ga-s760.2 (MF-A diag)    ──────────  independent
ga-s760.3 (MF-B test)    ──────────  independent
```

`ga-s760.4` blocks on `ga-s760.1` because the engdocs section describes
behavior the runtime must already exhibit. `.2` and `.3` are independent
of `.1` and of each other; the architecture explicitly notes either ordering
is fine for those three. `.3`'s designer note observes that `.3` can reuse
`.2`'s `LogCoreFingerprintDrift` renderer if available, with an in-bead
fallback if not — this is a soft preference, not a hard dep.

## Routing rationale

All four beads have already been through architect (`source:actual-architect`)
and designer (`source:actual-designer`). They do not need another design hop.

- **`.1`, `.2`** — code changes with detailed test specifications already
  in the bead body. Routed to **validator** with `needs-tests` so tests
  are authored first; validator re-routes to builder when tests are in.
- **`.3`** — the test itself is the deliverable. Routed to **validator**
  end-to-end; no builder hop required (one new test file plus one new
  fixture file).
- **`.4`** — pure documentation, no test surface. Routed directly to
  **builder** with `ready-to-build`.

## Acceptance criteria (rolled up)

The architecture decision and per-bead specs carry the full criteria.
Roll-up for stakeholder visibility:

1. **No drain on binary upgrade.** A session started by an older binary
   without a `vN:` prefix on its stored hash MUST silently rebaseline on
   the first reconciler tick after the new binary boots. No
   `SessionDraining` event, no restart.
2. **No drain on version bump.** A session whose stored hash carries a
   different `vN:` prefix from the running binary's
   `runtime.FingerprintVersion` MUST silently rebaseline. Same guarantees.
3. **Real drift still drains.** A session whose stored hash matches the
   current version but differs in hex MUST drain on the existing path,
   including all current deferral predicates (named-session-active,
   pending interaction, attached, etc.).
4. **Operator diagnosis in one log read.** When real drift fires, the
   supervisor log shows per-`CopyFiles`-entry diff (RelDst, ContentHash,
   Probed) marked `[ ]` / `[~]` / `[+]` / `[-]`. Operator does not need
   to read the source to identify which entry diverged.
5. **CI gate against Mode B.** `TestBuildDesiredStateHashStableAcrossTicks`
   passes on every PR; failure prints a per-agent breakdown diff that
   identifies the divergent field and (for `CopyFiles`) the divergent
   entry.
6. **Maintainer awareness.** `engdocs/architecture/agent-protocol.md`
   carries a "Fingerprint Versioning" section with the rule, the
   silent-rebaseline contract, the PR-review checklist, and a version
   changelog. PRs that touch hash inputs face a deterministic checklist
   (item 1 in `.4`'s checklist points reviewers at `.3`'s test).

## Risks and unknowns

- **Mode B remains unconfirmed.** If MF-B's cross-tick test is green and
  Mode B reappears in production after `.1` lands, the divergence is
  cross-tick over a longer interval (filesystem state drift, skill
  catalog snapshot non-determinism, etc.). MF-A's per-entry diff is the
  diagnostic; we file a follow-up if it appears.
- **Mixed-format supervisor logs during rollout.** For one upgrade
  cycle, sessions in the same city may emit legacy-format and v1-format
  drift dumps interleaved. Designer note for `.2` confirms the two
  formats are unambiguously distinguishable by line shape.
- **Test fixture maintenance.** `.3`'s 50-agent `testdata/` fixture must
  cover the cross-product matrix (provider × scope × deps × wake_mode ×
  overlay × pre-start × session-setup × skill-stage × copy-files).
  Designer note recommends header-comment matrix + named cells (no
  sequence numbers) to keep coverage auditable.

## Out of scope (explicit)

- Mode B root-cause investigation (file follow-up if MF-B catches it).
- Migration command `gc session rebaseline` — deferred per architecture
  decision; silent rebaseline subsumes the operator need.
- Forward-compat hash function — rejected per architecture decision.
- Other engdocs files; only `agent-protocol.md` is touched by `.4`.

## Validation gates

- `go test ./...` green (includes `.3`'s new test).
- `go vet ./...` clean.
- New test runs in <30s on commodity hardware (per `.3` spec).
- No ANSI color codes anywhere in the new log output (per designer
  notes on `.1` and `.2`).
- One `bd remember` entry per bead from the builder when each lands so
  future maintainers learn the format from `bd prime`, not from
  archaeology.
