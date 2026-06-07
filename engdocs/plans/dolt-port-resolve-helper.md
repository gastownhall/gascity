# Plan: shared `port_resolve.sh` helper kills :=3307 fallback (ga-lsois slice 1/3)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-lsois` (closed) — *Dog/maintenance
> scripts default `GC_DOLT_PORT=3307` → CRITICAL alarm fatigue.*
> **Designer spec:** `ga-u0lx9p` — full design body (684 lines, 3
> graphviz visuals); pins function body, stderr template, exit
> code, source-line edits, lint test, and the 8 builder tests.
> **Sibling slices (independent):** `ga-nptxjv` (slice 2, formula
> rewrites), `ga-kylssb` (slice 3, doctor severity).
> **Decomposed into:** 1 builder bead (see Children below)

## Context

Slice 1 of architect ga-lsois deletes the silent `:=3307` /
`:-3307` fallback from both `runtime.sh` (dolt pack) and
`dolt-target.sh` (maintenance pack) and replaces it with a shared
`resolve_dolt_port_or_die` helper that emits a structured stderr
error and exits **78** (`EX_CONFIG`). Operator overrides via
`GC_DOLT_PORT` still take precedence.

The designer (ga-u0lx9p) pinned every byte the builder needs:

- Helper file path: `.gc/system/packs/dolt/assets/scripts/port_resolve.sh`.
- Function signature + body (POSIX `/bin/sh`, ~80 LOC).
- Stderr template: 6 lines, plain ASCII, no ANSI, ≤79 cols.
- Source-line edits at `runtime.sh:200-205` (-6/+5) and
  `dolt-target.sh:145-150` (-6/+3).
- CI lint at `test/packlint/no_dolt_3307_fallback_test.go` with
  the verbatim test body, regex `GC_DOLT_PORT.*3307`, and an
  allowlist for `mol-dog-reaper.formula.toml` (slice 2 removes it).
- 8 builder-test cases (env override, discovery success, missing
  state, present-but-not-running, runtime sources helper,
  dolt-target sources helper, exit 78 on empty state, lint
  regression).
- Pinned answer to NFR-07: `var.dolt_port.default = "3307"` →
  REMOVE (slice 2 executes; this slice allowlists the file).

## Why a single builder bead

Per the design's §13: one PR, ~+346/-12 lines across 5 files —
1 new helper + 2 source-line edits + 1 lint + 1 test driver.
The work is tightly coupled (the source-line edits depend on the
helper existing; the lint depends on the edits removing the
fallback) and the design body is fully verbatim, leaving no
judgment calls to spread across multiple builders. Mirroring the
prior single-builder-bead decompositions (ga-h0pyln,
ga-a75ro.1, ga-vt6q), this is one bead's worth of work.

## Children

| ID            | Title                                                                                       | Routing label    | Routes to         | Depends on |
|---------------|---------------------------------------------------------------------------------------------|------------------|-------------------|------------|
| `ga-rq2e5a`   | feat(packs/dolt): port_resolve.sh helper kills :=3307 fallback (ga-lsois slice 1/3)         | `ready-to-build` | `gascity/builder` | (none; design closed) |

## Acceptance for the parent (ga-u0lx9p)

Met when `ga-rq2e5a` closes and all of the following hold (these
mirror the designer's §8 acceptance checklist):

- [ ] New file `.gc/system/packs/dolt/assets/scripts/port_resolve.sh`
      with `resolve_dolt_port_or_die` body verbatim from ga-u0lx9p §1.
- [ ] `runtime.sh:200-205` replaced per ga-u0lx9p §5.1 (-6/+5 lines).
- [ ] `dolt-target.sh:145-150` replaced per ga-u0lx9p §5.2 (-6/+3 lines).
- [ ] `test/packlint/no_dolt_3307_fallback_test.go::TestNoDolt3307FallbackInScripts`
      passes, walks `.gc/system/packs/`, matches regex
      `GC_DOLT_PORT.*3307`, allowlists only
      `.gc/system/packs/maintenance/formulas/mol-dog-reaper.formula.toml`.
- [ ] All 8 tests from ga-u0lx9p §10 implemented and passing.
- [ ] `go test ./...` green; `go vet ./...` clean.
- [ ] No new env vars introduced. POSIX `/bin/sh` only in the
      helper (no `[[ ]]`, no `local`, no arrays).
- [ ] Stderr template byte-for-byte matches ga-u0lx9p §3; downstream
      substring match points (`gc dolt: cannot resolve runtime port`,
      `(missing)`, `(present but not running)`, `remediation:`)
      preserved for ga-kylssb's classifier.
- [ ] `mol-dog-reaper.formula.toml`'s `var.dolt_port.default = "3307"`
      is NOT touched in this PR (slice 2 / ga-nptxjv owns it).
- [ ] The duplicated inline `managed_runtime_port` in
      `dolt-target.sh:114-144` is preserved (follow-on bead per
      ga-u0lx9p §11).

## Notes for the builder

- **Read ga-u0lx9p in full.** The design body is the contract;
  the bead body in `ga-rq2e5a` is a high-level summary, not a
  substitute. Every byte of the helper, the stderr message, and
  the lint code is pinned in the design.
- **POSIX scoping.** Use the `_rdp_` prefix for locals — the
  helper must `exit` (not `return`) on failure, so the `( ... )`
  subshell idiom does not apply here.
- **`|| exit $?` after both call sites.** Without it, the
  command-substitution failure leaves `$?` non-zero but the
  caller continues with an empty port. This is the standard
  POSIX guard.
- **Lint regex is intentionally broad.** `GC_DOLT_PORT.*3307`
  catches every variant the operator might re-introduce. False
  positives are easier to allowlist than false negatives are to
  catch.
- **Allowlist comment.** The lint comment names slice 2
  (`ga-nptxjv`) so the dependency is searchable when slice 2
  shrinks the allowlist to empty.

## Out of scope

These belong to siblings of `ga-lsois` and must not creep into
this slice:

- Formula rewrites (`mol-dog-reaper.formula.toml`,
  `mol-doctor.formula.toml`, prompt files) → slice 2 / `ga-nptxjv`.
- Doctor WARNING-vs-CRITICAL severity classification keyed on
  exit code 78 → slice 3 / `ga-kylssb`.
- Deduplicating `managed_runtime_port` between `runtime.sh:165`
  and `dolt-target.sh:114` → P3 follow-on bead (designer noted
  the skeleton in ga-u0lx9p §11).
- Supervisor-side env injection (`gc start` exports
  `GC_DOLT_PORT` into agent envs) → architect explicit deferral.
- Migrating runtime state to a one-int-per-line format → architect
  explicit deferral.
- Localizing the stderr message → project-wide concern.

## Validation gates

- `go test ./test/packlint -run TestNoDolt3307FallbackInScripts -count=1` green.
- The 8 builder tests from ga-u0lx9p §10 all green; shell tests
  use `t.TempDir()` for state-file fixtures; stderr asserted
  byte-stable.
- `git diff` confined to: `.gc/system/packs/dolt/assets/scripts/port_resolve.sh`
  (new), `runtime.sh`, `dolt-target.sh`, `test/packlint/no_dolt_3307_fallback_test.go`
  (new), and the new helper test driver. No other files modified.
- ZFC: no role names in the diff.
- No new third-party Go modules; no new env vars.
- POSIX `/bin/sh` discipline (CI shellcheck pass).

## Risks and unknowns

- **Cross-pack source path.** `dolt-target.sh` (maintenance pack)
  sources from `dolt/assets/scripts/port_resolve.sh`. This is
  unusual but acceptable — the dolt pack owns the runtime
  concept; maintenance is a consumer. The `${GC_SYSTEM_PACKS_DIR:-...}`
  pattern matches the existing path-discipline at the top of
  `runtime.sh`.
- **Re-sourcing the helper.** `port_resolve.sh` may be sourced
  more than once per process if both consumers run in the same
  shell. Function redefinition is idempotent on identical bodies,
  so this is safe; the architect explicitly allowed it (§5.1).
- **`echo` vs `printf`.** Uniformly `printf '%s\n'` — `echo` on
  POSIX mishandles `-` prefixes and backslashes. Drift here is
  the kind of thing the design pinning catches.
