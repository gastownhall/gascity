# Plan: RunWarmupChecks runner + WarmupReport types (ga-perl18 slice 2/4)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-perl18` — `gc start` warm-up alert
> mechanism for doctor failures (PG-auth and beyond).
> **Designer spec:** `ga-f932ei` (~776-line design body) pins the
> `RunWarmupChecks` signature, the `WarmupOpts` / `WarmupReport` /
> `ScopeWarmupResult` / `WarmupCheckResult` structs (verbatim Go),
> default deadlines (`5s` per-check, `30s` total) exported as
> `DefaultWarmupPerCheckDeadline` / `DefaultWarmupTotalDeadline`,
> sha256-hex `FailureSetHash` with a deterministic canonical input,
> injectable `mail.Provider`, the extraction of `buildDoctorChecks`
> from `doDoctor`, the 6-line wire-up after the `healthBeadsProvider`
> block in `cmd_start.go`, the mail subject + body templates with the
> 4096-byte cap, the stderr summary line format, and 16 test names
> with assertions.
> Architect builder estimate: ~250 LOC source + ~250 LOC tests, 1 PR.
> **Designer handoff mail:** `gm-kuinvp` (2026-05-11).
> **Decomposed into:** 1 builder bead — `ga-wgsv3t`.

## Context

Slice 1 (`ga-r1iqmy`, builder open) landed the `WarmupEligible()`
opt-in on the `doctor.Check` interface and the `warmup` field on the
pack/doctor manifest. Slice 2 is the runner that consumes that opt-in:
during `gc start`, after the health-beads provider initializes, scan
the warm-up-eligible subset of the doctor registry in parallel, build
a `WarmupReport`, and emit ONE mail-to-mayor + ONE stderr line when
any check returns non-OK. Fail-open: panics, mailer errors,
registry-build failures, and total-deadline expiry never propagate.

The slice ships:

- A new file `cmd/gc/cmd_start_warmup.go` with the runner and the
  four exported types pinned by the designer.
- A refactor of `cmd/gc/cmd_doctor.go`: extract the inline
  `~125 LOC` registry build (`doDoctor` lines 127–255 today) into
  `buildDoctorChecks(cityPath, cfg, cfgErr, buildDoctorChecksOpts)`
  so `RunWarmupChecks` reuses the same registry. No behavior change
  to `gc doctor`.
- A 6-line block in `cmd/gc/cmd_start.go` after `healthBeadsProvider`
  (line 432 today). Return values discarded (fail-open per NFR-04).
- 16 new tests in `cmd_start_warmup_test.go` plus one golden-file
  regression test that the extracted `buildDoctorChecks` produces the
  same check-name set as the previous inline build.

The slice does NOT ship the suppression state file, the all-clear
mail, the `--no-warmup-alerts` flag, the `[startup] warmup_alerts`
config key, or any check actually opting in. Those are slices 3
(`ga-bt6b13`, designer open) and 4 (`ga-xextj6`, designer open).

## Plan

One builder bead. Architect's estimate is one PR; the designer's
pinned contract is exhaustive enough that no further design tier is
needed.

| Builder bead         | PR shape | Files (key)                                                                                       |
|----------------------|----------|---------------------------------------------------------------------------------------------------|
| **Runner + types**   | PR       | `cmd/gc/cmd_start_warmup.go` (new), `cmd/gc/cmd_start_warmup_test.go` (new), `cmd/gc/cmd_doctor.go` (extract `buildDoctorChecks`), `cmd/gc/cmd_start.go` (6-line wire-up), `cmd/gc/cmd_doctor_test.go` (or new `cmd_doctor_extract_test.go`) + `cmd/gc/testdata/doctor_check_names.golden` |

### `ga-wgsv3t` — RunWarmupChecks runner + types + wire-up + tests (P2, `ready-to-build`)

Implements FR-03, FR-04, FR-05, FR-07, NFR-01, NFR-04, NFR-09 from
`ga-perl18`. Routed to `gascity/builder` via `gc.routed_to`;
`gc.design_parent=ga-f932ei` records the back-link.

**Blocked by:** `ga-r1iqmy` (slice 1 builder) — the runner imports
the `WarmupEligible()` method on `doctor.Check`, so slice 1's PR must
land first.

**Acceptance criteria (verbatim from `ga-f932ei` §"Acceptance criteria"):**

- `cmd/gc/cmd_start_warmup.go` exists with `WarmupOpts`,
  `WarmupReport`, `ScopeWarmupResult`, `WarmupCheckResult`,
  `RunWarmupChecks`, and exported constants
  `DefaultWarmupPerCheckDeadline` (`5 * time.Second`) and
  `DefaultWarmupTotalDeadline` (`30 * time.Second`).
- `buildDoctorChecks` and `buildDoctorChecksOpts` exist in
  `cmd/gc/cmd_doctor.go`; `doDoctor` calls into them.
- `cmd_start.go` calls `RunWarmupChecks` after the
  `healthBeadsProvider` block (immediately after the `}` that closes
  the err-handling `if`); return values are discarded.
- Every `TestRunWarmupChecks_*` from `ga-f932ei` §"Test contracts"
  exists and passes (16 named tests; full list in HARD RULES below).
- `TestBuildDoctorChecks_NameSetUnchanged` exists and passes; the
  golden file `cmd/gc/testdata/doctor_check_names.golden` is checked
  in.
- `go test ./...` passes.
- `go vet ./...` clean.
- `gc doctor` produces the same output as before the change for a
  representative city (manual smoke; not test-coded).
- No new third-party Go modules (NFR-02).
- No status-file writes from inside `RunWarmupChecks` (NFR-06,
  slice 3's job).
- No role names in the new file. The `MailTo` default is `"mayor"` —
  routing handle, not a hardcoded role.

**HARD RULES carried from design:**

- **Signature is verbatim from `ga-f932ei` §1–§5.** Do not rename,
  reorder, or pluralize any field. `Mailer` is `mail.Provider`;
  `Stderr` is `io.Writer` (nil → `os.Stderr`); `Now` is
  `func() time.Time` (nil → `time.Now`); `PerCheckDeadline` and
  `TotalDeadline` zero-values fall back to the exported defaults;
  `MailFrom` defaults to `"gc-start-warmup"`; `MailTo` defaults to
  `"mayor"`.
- **`WarmupReport` shape is final for slices 2 and 3.** The
  `SuppressedFromMayor` and `SuppressionReason` fields land in slice
  2 even though they are always zero-valued here — slice 3 fills them
  without changing the struct shape. Do not omit them.
- **`FailureSetHash` is sha256-hex of the canonical failure-set input
  exactly as pinned in `ga-f932ei` §"Canonical failure-set hash
  input":** sort `Failures` by `(Scope, Check)` ASCII, then concat
  `"<scope>\t<check>\t<severity>\n"` per failure (severity is
  `doctor.CheckStatus.String()`), then `crypto/sha256` →
  `hex.EncodeToString`. Empty when `Failures` is empty.
- **Mail subject (FR-05) has two forms:**
  - When ALL failures come from one check name:
    `"<scope-or-pack>-<check> alert during city warm-up"` —
    matches FR-05 verbatim.
  - When >1 distinct check names fail: `"city warm-up: <N> doctor
    check(s) failed"`.
  Slice 4 overrides for the PG-auth single-check case with the
  verbatim §7.2 string; do not encode that override here.
- **Mail body is markdown-ish plain text capped at 4096 bytes
  (FR-06).** First line is a one-line summary; blank line; per-scope
  per-check section with `<icon> <scope> — <message>` and an optional
  `\n  fix: <FixHint>` indent. Footer
  `\n\n— see \`gc doctor\` for full details.\n`. Over-cap content
  truncated with `\n(truncated, see gc doctor for full output)\n`.
- **Stderr summary line (FR-07) has two forms:**
  - Mail succeeded: `"gc start: warmup: <n> check(s) failed
    (<highest-severity>); see mail to mayor and `gc doctor` for
    details\n"`
  - Mail failed: `"gc start: warmup: <n> check(s) failed
    (<highest-severity>); mail send error: <err>\n"`
  - Status OK: no stderr line.
- **Concurrency is `errgroup.Group` with `SetLimit(0)`** over the
  filtered eligible set, parent context bounded by `TotalDeadline`,
  per-check goroutine wrapping `c.Run` in
  `context.WithTimeout(parent, PerCheckDeadline)` with a deferred
  `recover()` that converts panics into `WarmupCheckResult{Panic,
  Status: doctor.StatusError}`. No panic escapes the runner.
- **Outer-level deferred `recover()` in `RunWarmupChecks`** turns any
  runner-level panic (e.g., nil-mailer dereference) into a stderr log
  line + `WarmupReport{HighestSeverity: StatusOK}` so the caller is
  told nothing (NFR-04). The function still returns nil error.
- **`buildDoctorChecks` extraction is mechanical** — cut-and-paste of
  the existing `d.Register(...)` lines from `doDoctor` (lines
  127–255) into a single helper that returns
  `[]doctor.Check`. `doDoctor` then builds the same registry by
  iterating the return slice and calling `d.Register(...)` on each.
  No behavior change. Order preserved.
- **`TestBuildDoctorChecks_NameSetUnchanged` uses a golden file** at
  `cmd/gc/testdata/doctor_check_names.golden` — one check name per
  line. Builder bootstraps by capturing the pre-extraction name set.
  Standard `go test -update` pattern for future legitimate changes.
- **Scope attribution heuristic:** the substring of
  `WarmupCheckResult.Check` before the first `:` is the scope display
  when present; otherwise `"city"`. A check whose prefix matches a
  rig's canonicalized name is attributed to that rig. Comments in the
  runner explain. If brittle in practice, the follow-on bead adds
  `Check.Scope() string` — explicitly OUT of scope here.
- **Test-only `checksOverride` field on `WarmupOpts` is
  un-exported** (`checksOverride []doctor.Check`). Production wire-up
  never touches it; tests in the same package (`cmd/gc/`) use it to
  bypass `buildDoctorChecks`. Do not export.
- **Wire-up in `cmd_start.go` is exactly the 6-line block in
  `ga-f932ei` §"Pinned contract — Go signatures" §7,** inserted
  immediately after the `}` closing the `healthBeadsProvider`
  err-handling `if`. Return values discarded with `_, _ =`.
  Wrapped in `if cfg != nil` is unnecessary — `cfg` is non-nil at
  that point (load errors return earlier).
- **`defaultMailProvider` factory:** if `cmd/gc/cmd_mail.go` already
  exposes a public factory used by `gc mail send`, call it directly
  and skip adding a new helper. Otherwise add a 6-line
  `cmd/gc/mail_provider.go` factory wrapping the same `beadmail`
  construction. Builder confirms by reading `cmd_mail.go` at impl
  time.

**The 16 tests (verbatim names from `ga-f932ei` §"Test contracts"):**

1. `TestRunWarmupChecks_ParallelExecution`
2. `TestRunWarmupChecks_PerCheckDeadline`
3. `TestRunWarmupChecks_TotalDeadline`
4. `TestRunWarmupChecks_FailOpen_PanicInCheck`
5. `TestRunWarmupChecks_FailOpen_MailerError`
6. `TestRunWarmupChecks_FailOpen_RunnerPanic`
7. `TestRunWarmupChecks_AllOK_Silent`
8. `TestRunWarmupChecks_NoEligibleChecks`
9. `TestRunWarmupChecks_MailSubject_SingleCheck`
10. `TestRunWarmupChecks_MailSubject_MultipleChecks`
11. `TestRunWarmupChecks_MailBody_BoundedTo4KB`
12. `TestRunWarmupChecks_MailBody_ExcludesSecretsByDefault` (documents
    that slice 2 is content-agnostic; slice 4's `PostgresAuthCheck`
    enforces exclusion)
13. `TestRunWarmupChecks_FailureSetHash_Deterministic`
14. `TestRunWarmupChecks_FailureSetHash_DiffersOnSeverityEscalation`
15. `TestRunWarmupChecks_StderrSummaryLineFormat`
16. `TestRunWarmupChecks_StderrSilentOnOK`
17. `TestRunWarmupChecks_NilCfg_Reported`
18. `TestRunWarmupChecks_ContextCancellation`

(Plus `TestBuildDoctorChecks_NameSetUnchanged` for the extraction
regression — total 18 tests with the runner asserting fail-open
guarantees, deadlines, content-agnostic mail, and deterministic hash.)

## Sequence

`bd dep add <builder-id> ga-r1iqmy` so `bd ready` gates the builder
bead until slice 1's PR lands. Slices 3 (`ga-bt6b13`) and 4
(`ga-xextj6`) of ga-perl18 are blocked-by this slice's PR landing —
designer's open beads already carry that fact, so PM does not need a
new edge there. Once this bead closes, PM surfaces the unblock for
slices 3 and 4 to the designer.

## Out of scope (slices 3-4 of ga-perl18 — separate beads)

- Suppression-state file (`warmup-last.json`) — slice 3 (FR-08).
- All-clear mail (FR-09) — slice 3.
- `--no-warmup-alerts` CLI flag (FR-10) — slice 3.
- `[startup] warmup_alerts = false` in `city.toml` (FR-11) — slice 3.
- PG-auth opting in (`return true` from its `WarmupEligible()`) and
  the verbatim slice-4 §7.2 mail body — slice 4 (FR-12).
- Any non-PG warm-up producer (postgres-server, dolt-runtime-
  discoverable, mode-permissive credentials, etc.).
- Per-severity routing.
- Web/dashboard surface for warm-up results.

## Verification (after PR lands)

```bash
# New file exists and exports the pinned API.
grep -E '^func (RunWarmupChecks|defaultMailProvider)' cmd/gc/cmd_start_warmup.go
grep -E '^const (DefaultWarmupPerCheckDeadline|DefaultWarmupTotalDeadline)' cmd/gc/cmd_start_warmup.go
grep -E '^type (WarmupOpts|WarmupReport|ScopeWarmupResult|WarmupCheckResult)' cmd/gc/cmd_start_warmup.go
# Expect: one match per name.

# Extraction landed.
grep -n 'func buildDoctorChecks' cmd/gc/cmd_doctor.go
# Expect: 1 match.

# Wire-up exists.
grep -n 'RunWarmupChecks' cmd/gc/cmd_start.go
# Expect: 1 match in the gc start path.

# Tests landed.
grep -c '^func TestRunWarmupChecks_' cmd/gc/cmd_start_warmup_test.go
# Expect: 16+ (the named tests + any builder-added regressions).
grep -c '^func TestBuildDoctorChecks_NameSetUnchanged' cmd/gc/cmd_doctor*_test.go
# Expect: 1.

# Golden file checked in.
ls cmd/gc/testdata/doctor_check_names.golden
# Expect: file exists.
```

## Builder bead

- **`ga-wgsv3t`** — RunWarmupChecks runner + types + wire-up + tests
  (P2, `ready-to-build`, `source:actual-pm`, `backend:postgres`).
  Routed to `gascity/builder`. Blocked by `ga-r1iqmy` (slice 1
  builder).
