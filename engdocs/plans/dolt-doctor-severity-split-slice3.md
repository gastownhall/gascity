# Plan: doctor severity split + dolt-runtime-discoverable check (ga-lsois slice 3/3)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-lsois` (closed) — *Dog/maintenance
> scripts default `GC_DOLT_PORT=3307` → CRITICAL alarm fatigue.*
> **Designer spec:** `ga-kylssb` — full design body (926 lines) pinning
> verbatim mail subjects, dog classification prose, new doctor check
> struct/Name/Run/FixHint, registration order, and 15 test names.
> **Sibling slices (independent):** `ga-u0lx9p` (slice 1, shared
> `port_resolve.sh` helper — builder `ga-rq2e5a`); `ga-nptxjv` (slice 2,
> formula prompt rewrites — builder `ga-15x7eb`).
> **Decomposed into:** 1 builder bead (see Children below)

## Context

Slice 3 of architect `ga-lsois` lands two wire-coupled contracts:

1. **`mol-dog-doctor.toml` probe step** classifies its failure by exit
   code — exit 78 (EX_CONFIG) → **WARNING** (`Dolt runtime not
   initialized [WARNING]`), any other non-zero → **CRITICAL** (`Dolt
   server unreachable [CRITICAL]`). The dog reads the prose and picks
   the mail subject verbatim.

2. **A new doctor check `dolt-runtime-discoverable`** in
   `internal/doctor/checks.go` (plus a per-rig variant
   `rig:<name>:dolt-runtime-discoverable`), registered immediately
   BEFORE `NewDoltServerCheck` at both city and rig scopes. It reads
   `dolt-state.json` and reports `StatusError` with the verbatim
   FixHint `start the city (gc start) or check $GC_CITY_RUNTIME_DIR
   for a stale or wrong-scope state file` when the file is missing,
   malformed, marks the server stopped, or names a dead PID.

The designer (`ga-kylssb`) pinned every byte the builder needs:

- Two load-bearing mail subjects (§3.1 of the design).
- 25-line replacement prose for `mol-dog-doctor.toml` lines 73–78
  (§3.2 of the design — the dog's classification rule, exit-code primary).
- `DoltRuntimeDiscoverableCheck` and `RigDoltRuntimeDiscoverableCheck`
  type, constructor, `Name()`, full `Run()` matrix, `CanFix()`/`Fix()`
  no-ops, and shared `doltRuntimeDiscoverableFixHint` constant (§4–§5).
- Registration diff at `cmd_doctor.go:204-205` (+1 line) and `:235`
  (+1 line) with the same `skip` predicate as `DoltServerCheck` (§4.5, §5.3).
- 15 test names across `internal/doctor/checks_test.go` (13),
  `cmd/gc/cmd_doctor_test.go` (1), and `cmd/gc/embed_builtin_packs_test.go`
  (1) with verbatim assertions per §6.1–§6.3.
- Pinned answers to architect's deferred decisions (§8).
- Out-of-scope deferrals to PG-side analogue, auto-fix, and the
  `report` step's `[CRITICAL]` subject (§10).

## Why a single builder bead

Per the design's §12: ~140 LOC Go (city + rig check + FixHint) +
~390 LOC Go tests (13 doctor + 1 registration + 1 lint) +
~+19 LOC formula prose × up to 4 copies = **~590 LOC, one PR**.

The work is tightly coupled — the registration-order lint
(`TestDoctorRegistersDoltRuntimeBeforeServer`) and the formula-prose
lint (`TestMolDogDoctorSubjectsPinned`) both fail until the Go check
and the formula edits land together, and the dog's classification
prose names the doctor check by its `Name()` string so renames would
desync the prose from the Go. The design body is fully verbatim with
no judgment calls to spread across multiple builders. Mirrors slice 1
(`ga-u0lx9p` → `ga-rq2e5a`) and slice 2 (`ga-nptxjv` → `ga-15x7eb`).

## Children

| ID | Title | Routing label | Routes to | Depends on |
|---|---|---|---|---|
| `ga-frcthm` | feat(doctor): dolt-runtime-discoverable check + dog severity split (ga-lsois slice 3/3) | `ready-to-build` | `gascity/builder` | (none; design closed) |

## Acceptance for the parent (ga-kylssb)

Met when the child builder bead closes and all of the following hold
(these mirror the designer's §9 verification list and §11 guardrails):

- [ ] `go test ./internal/doctor/ -run TestDoltRuntimeDiscoverableCheck` —
      all 9 city-level tests PASS.
- [ ] `go test ./internal/doctor/ -run TestRigDoltRuntimeDiscoverableCheck` —
      all 4 per-rig tests PASS.
- [ ] `go test ./cmd/gc/ -run TestDoctorRegistersDoltRuntimeBeforeServer` —
      PASS.
- [ ] `go test ./cmd/gc/ -run TestMolDogDoctorSubjectsPinned` — PASS.
- [ ] `grep -F 'Dolt runtime not initialized [WARNING]' .gc/system/packs/dolt/formulas/mol-dog-doctor.toml` —
      exits 0 (substring present).
- [ ] `grep -F 'Dolt server unreachable [CRITICAL]' .gc/system/packs/dolt/formulas/mol-dog-doctor.toml` —
      exits 0 (substring present).
- [ ] `grep -F 'ESCALATION: Dolt server unreachable [CRITICAL]' .gc/system/packs/dolt/formulas/mol-dog-doctor.toml` —
      exits non-zero in the `probe` step (the `report` step's
      `ESCALATION: Dolt health critical [CRITICAL]` MAY still appear
      and is untouched).
- [ ] All four `mol-dog-doctor.toml` copies stay in sync (in-repo
      working city, `examples/`, `cmd/gc/.gc/system/packs/`, and
      `.beads/formulas/`) — `TestMolDogDoctorSubjectsPinned` is the
      load-bearing guarantee.
- [ ] Check `Name()` returns exactly `"dolt-runtime-discoverable"`;
      per-rig variant returns exactly `"rig:<rig.Name>:dolt-runtime-discoverable"`.
- [ ] Registration order: discoverable BEFORE server-reach at BOTH
      city (`cmd_doctor.go:204-205`) and rig (`cmd_doctor.go:235`) levels.
- [ ] Single shared `doltRuntimeDiscoverableFixHint` constant; both
      city and rig checks reference it.
- [ ] `CanFix()` returns false for both variants; `Fix()` is a no-op.
- [ ] The new check does NOT open a TCP connection (that's
      `DoltServerCheck`'s job; the two checks are deliberately layered).
- [ ] `go test ./...` green; `go vet ./...` clean.
- [ ] Manual: `gc doctor` against a city with `dolt-state.json`
      removed shows `dolt-runtime-discoverable [ERROR]` BEFORE
      `dolt-server [ERROR]`, FixHint names `gc start` and
      `$GC_CITY_RUNTIME_DIR`.
- [ ] Manual: `gc doctor` against a healthy city shows
      `dolt-runtime-discoverable [OK]` with port + pid.
- [ ] No edits to `port_resolve.sh`, `runtime.sh`, `dolt-target.sh`
      (slice 1 territory) or to any other formula prompt body (slice
      2 territory).

## Notes for the builder

- **Read `ga-kylssb` in full before any edit.** The design body pins
  each byte — Go struct fields, error messages, FixHint text, the
  25-line replacement prose for `mol-dog-doctor.toml`. Treat it as
  the contract; the bead body is a summary, not a substitute.
- **Four copies of `mol-dog-doctor.toml`.** See design §13. The four
  paths are:
  1. `.gc/system/packs/dolt/formulas/mol-dog-doctor.toml`
  2. `examples/dolt/formulas/mol-dog-doctor.toml`
  3. `cmd/gc/.gc/system/packs/dolt/formulas/mol-dog-doctor.toml`
     (the one `MaterializeBuiltinPacks` reads — the lint asserts
     against this copy)
  4. `.beads/formulas/mol-dog-doctor.toml`
  If a generator script produces #2/#3/#4 from #1, edit #1 only and
  re-run the generator. Otherwise edit all four. The
  `TestMolDogDoctorSubjectsPinned` lint is load-bearing — any
  out-of-sync copy fails CI. Slice 2's builder (`ga-15x7eb`)
  encountered the same multi-copy situation; check the merged
  result for the approach used.
- **Exit-code primary classification.** The dog must classify on
  `$?` (exit code 78 → WARNING, other non-zero → CRITICAL). Do NOT
  pattern-match the stderr text from slice 1. The prose explicitly
  says "do not pattern-match" — keep that line in.
- **Same `skip` predicate as `DoltServerCheck`.** Both checks gate
  on the identical scope predicate (managed-dolt bd-backend). There
  is no scope where one should run and the other should not.
- **Registration order is load-bearing.** The runtime-discoverable
  check must register BEFORE the server-reach check at both city
  and rig levels — the order makes the more-actionable failure
  surface first. `TestDoctorRegistersDoltRuntimeBeforeServer`
  enforces this.
- **No TCP connection in the new check.** Discovery (state file)
  and connection (TCP probe) are layered tests — keep them separate.
- **No autostart.** `CanFix() == false` for both variants. The
  doctor never starts services.
- **Independent of slices 1 and 2.** This PR can land in any order
  relative to `ga-rq2e5a` (slice 1 builder) and `ga-15x7eb` (slice
  2 builder). The classification keys on exit code 78, which slice
  1's helper sets — but slice 1's source-line edits don't need to
  be in place for the new doctor check to compile or the formula
  prose to land. If slice 1 or slice 2 have already landed in
  HEAD, you'll see their changes; leave them alone.
- **PR shape:** ~140 LOC Go check + ~390 LOC Go tests + ~+19 LOC
  formula prose × up to 4 copies = ~590 LOC, single PR.

## Out of scope

These belong to siblings of `ga-lsois`, separate slice trees, or
explicit architect deferrals; must not creep into this slice:

- Slice 1 territory: `port_resolve.sh` helper edits, `runtime.sh`,
  `dolt-target.sh`. (Owned by `ga-u0lx9p` → `ga-rq2e5a`.)
- Slice 2 territory: any other formula prompt body rewrites,
  `[vars.port]` stanza deletions, `command.toml` description edits.
  (Owned by `ga-nptxjv` → `ga-15x7eb`.)
- `postgres-runtime-discoverable` analogue — separate slice in the
  PG-auth chain. (Design §10 explicit deferral.)
- The `report` step's `ESCALATION: Dolt health critical [CRITICAL]`
  subject — that's resource degradation, severity correctly CRITICAL,
  untouched. (Design §3.3 explicit.)
- Auto-fix that runs `gc start` — `CanFix() == false`. (Design §10
  explicit deferral; architect's `ga-lsois` §"Why not auto-start"
  ratifies.)
- Supervisor-side env injection of `GC_DOLT_PORT` — architect's
  fourth layer, deferred in `ga-lsois` §"Out of scope".
- New event type for discovery failure — no new event published;
  doctor stdout + exit code is sufficient. (Design §10.)
- Cross-formula severity unification for `mol-dog-stale-db.toml`,
  `mol-dog-jsonl.toml`, `mol-dog-reaper.toml` — architect §"Out of
  scope" excludes this. Slice 3 is the minimum to suppress the
  3307-alarm-fatigue bug.

## Validation gates

- All 15 new test functions PASS (9 city doctor + 4 rig doctor + 1
  registration order + 1 formula prose lint).
- `go test ./...` green; `go vet ./...` clean.
- `git diff` confined to: `internal/doctor/checks.go`,
  `internal/doctor/checks_test.go`, `cmd/gc/cmd_doctor.go`,
  `cmd/gc/cmd_doctor_test.go`, `cmd/gc/embed_builtin_packs_test.go`,
  and the four `mol-dog-doctor.toml` copies (or just #1 if a
  generator covers the rest).
- ZFC: no role names in the diff.
- The verbatim wire strings (`Dolt runtime not initialized
  [WARNING]`, `Dolt server unreachable [CRITICAL]`,
  `dolt-runtime-discoverable`, `` Exit `78` (`EX_CONFIG`) ``) are
  all enforced by `TestMolDogDoctorSubjectsPinned`.

## Risks and unknowns

- **Four-copy formula sync.** If the project's embedded-pack
  materializer expects a single source-of-truth file and a
  generator produces the copies, the builder needs only edit #1.
  If not, edit all four. Slice 2's builder bead (`ga-15x7eb`) hit
  the same situation; the merged commit shows the actual approach.
- **`pidutil` test fixture for dead PIDs.** Design §6.1 expects a
  reusable test helper for "known-dead PID". If one doesn't exist,
  the builder picks a high PID (e.g. `999999`) and verifies it's
  not alive at test time. Standard pattern in the existing doctor
  tests.
- **Slice 1 / slice 2 landing order.** Independent — design §0 and
  §11 are explicit. The builder may find slice 1 or slice 2 already
  merged; if so, the diff should NOT touch those files.

## Refs

- Design (this slice): `ga-kylssb` (will be closed by pm after decomposition)
- Parent architecture: `ga-lsois` (closed)
- Sibling slice 1 design: `ga-u0lx9p` (closed); builder: `ga-rq2e5a`
- Sibling slice 2 design: `ga-nptxjv` (closed); builder: `ga-15x7eb`
- Plan doc: `engdocs/plans/dolt-doctor-severity-split-slice3.md` (this file)
