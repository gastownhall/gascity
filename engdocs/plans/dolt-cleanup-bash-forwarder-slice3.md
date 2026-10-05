# Plan: bash forwarder shim + doctor advisory + denylist test (ga-nw4z6 slice 3/5)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-nw4z6` (closed) — P0 DATA LOSS:
> `gc dolt cleanup` classifier marks the live `beads` DB as orphan.
> **Designer spec:** `ga-78stvc` — 576-line design body pins:
> 27-line POSIX forwarder body verbatim, argv translation table,
> `--server-down-ok` deprecation stderr line, `mol-dog-doctor.toml`
> inspect-step advisory rewrite verbatim, `mol-dog-stale-db.toml`
> verify-from-HEAD step, `TestNoBashCleanupDestructiveLogic` body
> with 10 denylisted substrings, 4 required substrings, 2048-byte
> / 50-line cap, POSIX-shebang assertion.
> **Branch:** `local/integration-2026-04-30` (rig root
> `/home/jaword/projects/gascity`). Slice 1 (`ga-0txff0`, CLOSED
> 2026-05-11) confirmed Go runner already on this branch — no
> cherry-pick needed.
> **Sibling slices:** `ga-9h05hk` (slice 2 builder bead,
> live-session probe — landed/landing), `ga-endmgy` (slice 4,
> audit log — not yet designed), `ga-qg23tv` (slice 5, regression
> test — not yet designed).
> **Decomposed into:** 1 builder bead (see Children below).

## Context

The original bug (`ga-nw4z6`) is that `gc dolt cleanup --force`
deletes the live `beads` database because the bash classifier
misclassifies it as an orphan. The architect's 5-slice plan
restructures the cleanup pipeline:

- **Slice 1** (`ga-0txff0`, CLOSED) — Get the Go runner on the
  integration branch. Was a cherry-pick task; the designer
  verified the runner was already present byte-identically on
  `local/integration-2026-04-30`, so the slice closed
  superseded.
- **Slice 2** (`ga-9h05hk`) — Live-session `SHOW PROCESSLIST`
  cross-check + `--force` fail-closed. Independent of slice 3.
- **Slice 3 (this plan)** — Replace the bash classifier with a
  thin forwarder to the Go runner; update doctor formula
  advisory; verify stale-db formula parity; add a permanent
  denylist test.
- **Slice 4** (`ga-endmgy`) — `cleanup-audit.log` append. Not
  yet designed.
- **Slice 5** (`ga-qg23tv`) — `TestDoltCleanupRefusesLiveBeadsDatabase`
  regression test that proves FR-10 (the original bug) is fixed.
  Not yet designed.

Slice 3's job is structural: **delete the destructive bash logic.**
After this slice, `examples/dolt/commands/cleanup/run.sh` is a
27-line POSIX `/bin/sh` argv translator that `exec`s
`gc dolt-cleanup`. No `rm`, no `DROP`, no `SHOW DATABASES`, no
classification. The denylist test enforces this permanently —
any future regression that puts classification logic back into
bash fails CI.

The designer (ga-78stvc) pinned every byte the builder needs:

- **Forwarder body** — full 27-line POSIX `/bin/sh` script,
  including the `case` block for argv translation (`--force`,
  `--max N`, `--server-down-ok`, `--probe`, `--json`, `-h`, `*`)
  and the `GC_CLEANUP_JSON` env-to-flag bridge. (§4.1)
- **Verbatim deprecation stderr** for `--server-down-ok` —
  `gc dolt cleanup: --server-down-ok is no longer supported; the
  SQL DROP path is the sole deletion mechanism`. (§4.2)
- **Doctor advisory replacement** — the inspect step's
  recommendation changes from `gc dolt cleanup` to
  `gc dolt-cleanup --json` with a sentence about the JSON
  envelope. (§5.2)
- **Stale-db reconcile** — the HEAD version of
  `examples/dolt/formulas/mol-dog-stale-db.toml` already
  implements FR-07 (calls `gc dolt-cleanup --json --probe` and
  parses the envelope). The working tree is `M` per `git status`
  — a 228-line revert. Builder runs `git checkout HEAD --
  examples/dolt/formulas/mol-dog-stale-db.toml` and verifies
  `TestStaleDBFormulaRuntimeContract` still passes. (§6.2)
- **Test name + body** — `TestNoBashCleanupDestructiveLogic`
  lives next to `TestMaterializeBuiltinPacks` in
  `cmd/gc/embed_builtin_packs_test.go`. Full body in §7.2.

## Why a single builder bead

The four deliverables ship in one PR:

1. The forwarder replaces the 378-line bash classifier.
2. The doctor formula advisory tells operators to use the new
   command instead.
3. The stale-db formula reconcile keeps `mol-dog-stale-db` in
   sync (it already references `gc dolt-cleanup --json --probe`
   in HEAD).
4. The denylist test enforces the structural property in CI.

Splitting them would force one to land in an inconsistent state
(e.g., new forwarder shipped but the doctor formula still
recommends the old command; or the denylist test added before
the forwarder is replaced, immediately failing CI). The design
body is fully verbatim — no judgment calls to spread across
multiple builders. Mirroring slice 2's decomposition (`ga-9h05hk`),
this is one bead's worth of work.

## Children

| ID            | Title                                                                                                | Routing label    | Routes to         | Depends on |
|---------------|------------------------------------------------------------------------------------------------------|------------------|-------------------|------------|
| `ga-set4vz`   | feat(packs/dolt): bash forwarder shim + doctor advisory + denylist test (ga-nw4z6 slice 3/5)         | `ready-to-build` | `gascity/builder` | (none — slice 1 closed; slice 2 independent) |

## Acceptance for the slice (ga-78stvc design §9)

Met when `ga-set4vz` closes and all of the following hold:

- [ ] `examples/dolt/commands/cleanup/run.sh` matches ga-78stvc
      §4.1 byte-for-byte. Mode `0755`. Shebang `#!/bin/sh` (POSIX,
      not bash).
- [ ] `examples/dolt/formulas/mol-dog-doctor.toml` updated per
      ga-78stvc §5.2; inspect step `id`/`title`/`needs` unchanged;
      variable schema (`port`, `latency_threshold`, `conn_max`)
      unchanged; `probe` and `report` steps untouched.
- [ ] `examples/dolt/formulas/mol-dog-stale-db.toml` matches HEAD
      (working-tree revert reconciled via `git checkout HEAD --`).
- [ ] `cmd/gc/embed_builtin_packs_test.go::TestNoBashCleanupDestructiveLogic`
      added verbatim per ga-78stvc §7.2; passes.
- [ ] Existing `TestMaterializeBuiltinPacks` continues to pass
      (round-trip and executable mode preserved).
- [ ] Existing `TestBuiltinDatabaseEnumeratorsSkipManagedProbeDatabase`
      continues to pass.
- [ ] Existing `examples/dolt/stale_db_formula_test.go::TestStaleDBFormulaRuntimeContract`
      continues to pass after the reconcile.
- [ ] `go test ./cmd/gc/... ./examples/dolt/... -count=1` green.
- [ ] `go vet ./...` clean. `golangci-lint run` clean.
- [ ] PR titled `feat(packs/dolt): bash forwarder shim + doctor
      advisory + denylist test (slice 3/5 of ga-nw4z6)`.

## Notes for the builder

- **Read ga-78stvc in full.** The 576-line design body is the
  contract; the bead body in `ga-set4vz` is a high-level summary,
  not a substitute. The forwarder body, the deprecation stderr
  line, the doctor advisory replacement, and the denylist test
  are all byte-pinned.
- **Edit `examples/dolt/`, not `.gc/system/packs/dolt/`.** The
  runtime path under `.gc/system/packs/dolt/commands/cleanup/run.sh`
  is materialized from the embed (`MaterializeBuiltinPacks` in
  `cmd/gc/embed_builtin_packs.go:47-58`) on every `gc start` /
  `gc init`. Hand-edits to the runtime path are blown away on
  the next start. Sole source of truth is
  `examples/dolt/commands/cleanup/run.sh`.
- **POSIX, not bash.** Shebang `#!/bin/sh`. The forwarder body
  uses only `set -e`, `while/case`, `shift`, parameter expansion,
  and `exec`. No `[[ ]]`, no arrays, no `local`, no `read -r`.
  ga-78stvc §4.3 explains why NFR-03 of ga-nw4z6 pins POSIX.
- **`exec`, not subshell.** `exec gc dolt-cleanup …` replaces
  the current process so stdin/stdout/stderr/exit-code pass
  through verbatim. A subshell would introduce a wrapper PID
  and mask signal propagation. Pinned by ga-nw4z6 §"Action flow"
  and ga-78stvc §4.4.
- **Unknown flags pass through to `cobra`.** Don't reproduce
  cobra's error wording in shell — that's the ZFC anti-pattern
  this project explicitly rejects. The Go runner handles unknown
  flags (ga-78stvc §4.5).
- **Reconciling stale-db is mechanical.** The HEAD version of
  `mol-dog-stale-db.toml` already implements FR-07. The working
  tree drift (228 lines smaller) is a pre-existing revert.
  `git checkout HEAD -- examples/dolt/formulas/mol-dog-stale-db.toml`
  + verify `TestStaleDBFormulaRuntimeContract` still passes.
- **Pre-commit hook + TDD RED commits.** Per the project memory
  `pre-commit-hook-tdd-conflict`: the `.githooks/pre-commit`
  golangci-lint rejects compile-error RED commits. The default
  beads-only hooks at `.beads/hooks` let them through. For this
  slice, the changes don't involve a compile-error RED step —
  the test body compiles standalone and just fails until the
  forwarder is in place. Default `.githooks` hook path works.

## Out of scope

These belong to siblings of `ga-nw4z6` and must not creep into
this slice:

- **Slice 1**: Go runner cherry-pick (`ga-0txff0`) — CLOSED;
  no work needed.
- **Slice 2**: Live-session `SHOW PROCESSLIST` cross-check
  (`ga-9h05hk`) — independent; landing in parallel.
- **Slice 4**: `cleanup-audit.log` append (`ga-endmgy`) — not
  yet designed.
- **Slice 5**: `TestDoltCleanupRefusesLiveBeadsDatabase`
  regression test (`ga-qg23tv`) — not yet designed.
- **The `gc dolt cleanup` (with-space) bash surface** — keeps
  existing as the forwarder. Removing the bash surface entirely
  is future cleanup.
- **Other steps of `mol-dog-doctor.toml`** (`probe`, `report`)
  — unchanged.
- **The Go runner itself** (`cmd/gc/cmd_dolt_cleanup.go`) —
  already on the integration branch; no edits.

## Validation gates

- `go test ./cmd/gc/... ./examples/dolt/... -count=1` green
  (new and existing tests).
- `go vet ./...` clean.
- `golangci-lint run` clean (`.githooks/pre-commit` hook).
- `git diff --stat` confined to: `examples/dolt/commands/cleanup/run.sh`
  (replaced), `examples/dolt/formulas/mol-dog-doctor.toml`
  (one prompt block edited), `examples/dolt/formulas/mol-dog-stale-db.toml`
  (reconciled from HEAD, expect ~+228 LOC restoration), and
  `cmd/gc/embed_builtin_packs_test.go` (test appended). No other
  files modified.
- ZFC: no role names in the diff.
- No new third-party Go modules; no new env vars; no Go runner
  edits.
- POSIX `/bin/sh` discipline in the forwarder.

## Risks and unknowns

- **Stale-db reconcile drift.** The working tree currently has
  a 228-line revert of `mol-dog-stale-db.toml`. Reconciling from
  HEAD restores the integrated `gc dolt-cleanup --json --probe`
  invocation, but if there's been recent design work on that
  formula in another worktree, the reconcile may conflict. The
  builder should diff the working tree vs HEAD before running
  `git checkout` and confirm the working-tree version is the
  pre-#1548 form (designer flagged this as the expected case in
  §6.1).
- **Denylist substring false positives.** The size cap (2048
  bytes / 50 lines) is intentionally coarse and gives ~2.7×
  headroom over the current forwarder body (~750 bytes / 27
  lines). If a future contributor adds extensive comments, the
  cap may fire benignly; the test message instructs them to
  either trim or update the cap with explicit justification.
  This is the designer's intended second-layer guard.
- **Embed round-trip preservation.** The existing
  `TestMaterializeBuiltinPacks` checks content and executable
  mode on the runtime path. The new forwarder must preserve
  both. Builder should run this test after the replacement and
  confirm mode `0755`.

## References

- **Architecture:** `ga-nw4z6` (closed) — 5-slice plan, FR/NFR
  list, root-cause analysis.
- **Design:** `ga-78stvc` — 576-line verbatim contract.
- **Sibling beads:** `ga-0txff0` (slice 1, CLOSED), `ga-9h05hk`
  (slice 2 builder), `ga-endmgy` (slice 4, not designed),
  `ga-qg23tv` (slice 5, not designed).
- **Builder bead:** `ga-set4vz`.
- **Source surfaces:**
  - `examples/dolt/commands/cleanup/run.sh` (378 → 27 lines).
  - `examples/dolt/formulas/mol-dog-doctor.toml` (one prompt
    block).
  - `examples/dolt/formulas/mol-dog-stale-db.toml` (reconcile
    from HEAD).
  - `cmd/gc/embed_builtin_packs_test.go` (test appended).
- **Read-only references (no edits):**
  - `examples/dolt/embed.go` (`PackFS`).
  - `cmd/gc/embed_builtin_packs.go::MaterializeBuiltinPacks`.
  - `cmd/gc/cmd_dolt_cleanup.go` (Go runner; delegation target).
  - `examples/dolt/stale_db_formula_test.go` (existing test;
    must continue passing).
- **Design visuals:**
  - `/home/jaword/projects/gc-management/.gc/worktrees/gascity/designer/ga-78stvc/forwarder-flow.dot`
  - `/home/jaword/projects/gc-management/.gc/worktrees/gascity/designer/ga-78stvc/forwarder-flow.png`
