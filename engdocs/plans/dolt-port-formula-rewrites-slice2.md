# Plan: formula prompt rewrites off `:-3307` literal (ga-lsois slice 2/3)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-lsois` (closed) — *Dog/maintenance
> scripts default `GC_DOLT_PORT=3307` → CRITICAL alarm fatigue.*
> **Designer spec:** `ga-nptxjv` — full design body with verbatim
> rewrites for 4 formulas, lint test signature + body, and a 2-line
> `gc dolt sql` argv-passthrough prereq.
> **Sibling slices (independent):** `ga-u0lx9p` (slice 1, shared
> `port_resolve.sh` helper — builder `ga-rq2e5a`); `ga-kylssb`
> (slice 3, doctor severity split — designer working).
> **Decomposed into:** 1 builder bead (see Children below)

## Context

Slice 2 of architect `ga-lsois` rewrites the four in-scope formula
prompts (`mol-dog-jsonl`, `mol-dog-doctor`, `mol-dog-stale-db`,
`mol-dog-reaper`) to invoke `gc dolt sql -q "<query>"` instead of
embedding `--port "${GC_DOLT_PORT:-3307}"` directly. Per NFR-07
pinned to **REMOVE**, every `[vars.port]` / `[vars.dolt_port]`
stanza with `default = "3307"` is deleted from those four files
(not just `mol-dog-reaper`), and every `{{port}}` / `{{dolt_port}}`
template reference in step bodies is removed.

The designer (`ga-nptxjv`) pinned every byte the builder needs:

- Verbatim rewrites for each step body across the 4 formulas (§3.1–§3.4).
- Closure of in-scope formulas is exactly **4 files** — the other 3
  closure hits (`runtime.sh`, `dolt-target.sh` for slice 1;
  `operational-awareness.template.md:16` prose) are explicitly out
  of scope here.
- CI lint `TestFormulasUseGcDoltSqlNotRawPort` body, walk, glob,
  failure message format (§4).
- 2-line `run.sh` `"$@"` forwarding prereq + `command.toml`
  description update (§5).
- 6 verification commands the builder hands to the validator (§7).
- Out-of-scope deferrals to slices 1 and 3 (§10).

## Why a single builder bead

Per design §9: ~150 LOC of formula edits + ~50 LOC of test +
~3 LOC of `run.sh`/`command.toml` = **~200 LOC, 1 PR**. The work
is tightly coupled — the lint enforces a cross-cutting invariant
that fails until all 4 rewrites land together, and the `run.sh`
prereq is a 2-line patch that must ship in the same commit so the
formula rewrites are executable. The design body is fully verbatim
with no judgment calls to spread across multiple builders. This
mirrors slice 1's decomposition pattern (`ga-u0lx9p` → single
builder `ga-rq2e5a`).

## Children

| ID            | Title                                                                                              | Routing label    | Routes to         | Depends on            |
|---------------|----------------------------------------------------------------------------------------------------|------------------|-------------------|-----------------------|
| `ga-15x7eb`   | feat(packs/{dolt,maintenance}): rewrite formula prompts off ':-3307' literal (ga-lsois slice 2/3)   | `ready-to-build` | `gascity/builder` | (none; design closed) |

## Acceptance for the parent (ga-nptxjv)

Met when `ga-15x7eb` closes and all of the following hold (these
mirror the designer's §7 verification list plus the NFR-07 closure):

- [ ] `go test ./cmd/gc/ -run TestFormulasUseGcDoltSqlNotRawPort` PASSES.
- [ ] `grep -rln 'GC_DOLT_PORT.*3307' .gc/system/packs/maintenance/formulas/ .gc/system/packs/dolt/formulas/`
      exits non-zero (no matches).
- [ ] `grep -rln '"3307"' .gc/system/packs/maintenance/formulas/ .gc/system/packs/dolt/formulas/`
      exits non-zero (no matches).
- [ ] `grep -rn '{{port}}\|{{dolt_port}}' .gc/system/packs/maintenance/formulas/ .gc/system/packs/dolt/formulas/`
      exits non-zero (no matches).
- [ ] All four formula files' `[vars]` sections lack any `port` or
      `dolt_port` stanza with `default = "3307"`.
- [ ] Variables tables in the four formula descriptions no longer
      list `port` / `dolt_port` rows.
- [ ] `gc dolt sql -q "SELECT 1"` against a running city returns a
      single-row result and exits 0 (manual smoke test).
- [ ] `gc dolt sql` with no args still opens an interactive shell
      (backward compat verified).
- [ ] `command.toml` description matches the pinned string verbatim.
- [ ] `go test ./...` green; `go vet ./...` clean.
- [ ] No changes to `runtime.sh`, `dolt-target.sh`,
      `internal/doctor/checks.go`, or `port_resolve.sh` (slice 1
      and slice 3 territory).

## Notes for the builder

- **Read `ga-nptxjv` in full before any edit.** The design body
  pins each rewrite verbatim, including fence styles, blank-line
  positions, and backslash-continuation usage. Treat it as the
  contract.
- **Order: ship the `run.sh` prereq first.** Verify
  `gc dolt sql -q "SELECT 1"` works end-to-end before touching
  formula prompts. Otherwise the rewrites describe commands the
  dog can't actually run.
- **Per-query call sites in `mol-dog-reaper.toml`.** Every SQL
  block becomes one `gc dolt sql -q "..."` call per query (not a
  batched multi-statement `-q`). The design's §3.4 shows the
  backslash-continuation pattern.
- **Backtick-escaping in `mol-dog-stale-db.toml`.** The
  `DROP DATABASE` example uses `` \` `` for identifier-quoting
  backticks. The explanatory paragraph after the fence is
  load-bearing — do not remove it.
- **Slice 1 allowlist becomes a no-op.** Slice 1's
  `TestNoDolt3307FallbackInScripts` allowlists
  `mol-dog-reaper.formula.toml` for `default = "3307"`. Once this
  PR deletes that stanza, the allowlist still passes — it just
  matches zero files. No coordination needed.
- **Independence:** this PR is independent of slices 1
  (`ga-rq2e5a`) and 3 (TBD). Land in any order.

## Refs

- Design (slice 2): `ga-nptxjv` (PM closes after decomposition)
- Parent architecture: `ga-lsois` (closed)
- Sibling slice 1: design `ga-u0lx9p` (closed); builder `ga-rq2e5a` (open)
- Sibling slice 3: design `ga-kylssb` (open; designer working)
- Sibling plan: `engdocs/plans/dolt-port-resolve-helper.md`
