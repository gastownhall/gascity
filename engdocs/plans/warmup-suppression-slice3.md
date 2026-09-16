# Plan: warm-up suppression + all-clear + opt-out (ga-perl18 slice 3/4)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-perl18` — `gc start` warm-up alert
> mechanism for doctor failures (PG-auth and beyond).
> **Designer spec:** `ga-bt6b13` (~800-line design body) pins the
> `WarmupSuppressionState` JSON schema (Version=1), the 24h window
> exported as `WarmupSuppressionWindow`, the all-clear subject/body
> templates, the `--no-warmup-alerts` CLI flag, the
> `[startup] warmup_alerts *bool` config key, the
> closed `SuppressionReason` vocabulary (3 values), and 18 test names.
> Architect builder estimate: ~150 LOC source + ~300 LOC tests, 1 PR.
> **Designer handoff mail:** `gm-rfufw0` (2026-05-11).
> **Decomposed into:** 1 builder bead — `ga-hewclh`.

## Context

Slice 2 (`ga-wgsv3t`, builder open) ships the `RunWarmupChecks` runner +
`WarmupReport.FailureSetHash` (sha256-hex over the canonical (Scope, Check,
Severity) failure set). Slice 3 wraps that runner with:

- **FR-08 — duplicate suppression.** A 24h window. If the current failure-
  set hash equals the last-emitted hash AND less than 24h has elapsed, the
  mayor mail is withheld. Different hash or expired window → re-emit.
- **FR-09 — all-clear recovery mail.** When previously-failing (Scope, Check)
  pairs drop out of the current failure set, ONE recovery mail per cycle
  bundles all drops. Bypasses the 24h window (recovery is a one-time event
  per drop).
- **FR-10 — `--no-warmup-alerts` CLI flag.** Skips mail dispatch AND state-
  file writes. Stderr summary still emitted.
- **FR-11 — `[startup] warmup_alerts = false` city.toml opt-out.** Same
  effect as the CLI flag. Pointer type (`*bool`) so absence = default-on.
  CLI flag wins over config; config wins over default.

The persistent surface is `<cityPath>/.gc/runtime/warmup-last.json`, atomic
writes via `fsys.WriteFileAtomic`, mode `0o644`. Parse errors / unknown
`Version` values / missing file are all treated as "no prior state" (the
next cycle re-emits and overwrites). Mirrors the `dolt-state.json` pattern
in `cmd/gc/beads_provider_lifecycle.go:835`.

The slice remains fail-open: state-IO errors, mail-send errors, and unknown-
version state files NEVER propagate to `gc start`. A state-write failure
surfaces an extra stderr error line so the operator knows storage is sick,
but the start path continues.

After slice 3 lands, slice 4 (`ga-xextj6`, designer open) flips PG-auth's
`WarmupEligible()` to `true` and asserts the verbatim §7.2 mail body. The
full pipeline becomes: broken PG password → mail → operator fixes → next
`gc start` sends all-clear → quiet thereafter.

## Plan

One builder bead. Architect's estimate is one PR; the designer's pinned
contract is exhaustive enough that no further design tier is needed. The
slice-2 builder bead (`ga-wgsv3t`) blocks this slice — slice 3 extends
slice-2's `WarmupOpts` struct (adds `NoAlerts`, `NoAlertsReason`,
`StatePath`) and reads slice-2's `WarmupReport.FailureSetHash`.

| Builder bead | PR shape | Files (key) |
|---|---|---|
| **Runner extension + state IO + CLI/config opt-out** | One PR, ~150 LOC source + ~300 LOC tests | `cmd/gc/cmd_start_warmup.go` (extend), `cmd/gc/cmd_start_warmup_state.go` (new), `cmd/gc/cmd_start_warmup_state_test.go` (new), `cmd/gc/cmd_start_warmup_test.go` (extend), `cmd/gc/cmd_start.go` (~10 LOC), `internal/config/config.go` (~12 LOC), `internal/config/config_test.go` (~40 LOC) |

### `ga-hewclh` — warmup suppression + all-clear + opt-out (P2, `ready-to-build`)

Implements FR-08, FR-09, FR-10, FR-11, NFR-03, NFR-06 from `ga-perl18`.
Routed to `gascity/builder` via `gc.routed_to`; `gc.design_parent=ga-bt6b13`
records the back-link.

**Blocked by:** `ga-wgsv3t` (slice 2 builder). Wired via `bd dep add
ga-hewclh ga-wgsv3t`.

**Acceptance criteria summary** (full list in the bead body):

- `WarmupSuppressionWindow = 24 * time.Hour` exported.
- `WarmupOpts` extended with `NoAlerts bool`, `NoAlertsReason string`,
  `StatePath string`.
- `cmd/gc/cmd_start_warmup_state.go` exists with `WarmupSuppressionState`,
  `SuppressedFailure`, `readWarmupState`, `writeWarmupState`,
  `defaultWarmupStatePath`.
- `readWarmupState` returns `(nil, nil)` for missing files, parse errors,
  AND unknown `Version` values (NFR-03 "treat as missing").
- `writeWarmupState` creates parent dir (`0o755`) when absent and writes
  atomically via `fsys.WriteFileAtomic` (mode `0o644`).
- `computeDroppedPairs(prev []SuppressedFailure, now []WarmupCheckResult)
  []SuppressedFailure` and `sendAllClearMail(opts WarmupOpts, dropped
  []SuppressedFailure) error` exist with the design signatures.
- `sendAllClearMail` is a no-op when `NoAlerts==true` OR `len(dropped)==0`.
- `resolveNoAlerts(cfg *config.City, cliFlag bool) (noAlerts bool, reason
  string)` exists in `cmd_start.go` with the closed reason vocabulary
  (`"no-warmup-alerts-flag"`, `"warmup-alerts-disabled-in-config"`).
- `--no-warmup-alerts` flag declared in `cmd_start.go`, NOT hidden.
- `internal/config/config.go` has `StartupConfig.WarmupAlerts *bool` and
  `City.Startup StartupConfig`.
- All 18 tests from `ga-bt6b13` §"Test contracts" exist and pass.

**HARD RULES carried from design:**

- **Closed `SuppressionReason` vocabulary, exactly three values:**
  `"duplicate-within-24h"`, `"no-warmup-alerts-flag"`,
  `"warmup-alerts-disabled-in-config"`. Other cycles carry empty string.
  Tests assert exact strings.
- **State file path is canonical** — `<cityPath>/.gc/runtime/warmup-last.json`.
- **Unknown `Version` field is treated as ABSENT.** A v999 file from a
  future binary causes ONE re-emission and an overwrite. No silent
  forward-compat magic.
- **Both opt-outs suppress all-clear mails AND failure mails AND state
  writes.** Per architecture Branch C. Stderr line still emits the cleared
  count.
- **All-clears BYPASS the 24h window** (FR-09). The 24h window dampens
  failure re-pages; recovery is a one-time event per drop.
- **One all-clear mail per cycle** (bundles all (Scope, Check) drops).
- **Fail-open** — state-IO, mail, parse errors NEVER propagate. State-write
  failure surfaces an extra stderr line; does not return an error.
- **No `--force-warmup-alerts`.** Operators remove the config key to re-enable.
- **24h is the ONLY suppression mechanism.** No configurable window in
  this slice.

## Sequence

`bd dep add ga-hewclh ga-wgsv3t` is in place so `bd ready` gates this
builder bead until slice 2's PR lands. After this PR merges, slice 4
(`ga-xextj6`) unblocks; PM surfaces that unblock to the designer.

## Out of scope (slice 4 or deferred follow-ons)

- PG-auth's `WarmupEligible() bool { return true }` — slice 4 (`ga-xextj6`).
- Verbatim slice-4 §7.2 mail body — slice 4.
- A configurable suppression window (`[startup] warmup_suppression_window`).
- A `--force-warmup-alerts` flag to override config.
- Per-severity routing.
- Web/dashboard surface for warm-up history.
- Cross-city suppression history sharing.

## Verification (after PR lands)

```bash
# Constant + types exported.
grep -E '^const WarmupSuppressionWindow' cmd/gc/cmd_start_warmup.go
grep -E '^type (WarmupSuppressionState|SuppressedFailure)' cmd/gc/cmd_start_warmup_state.go
# Expect: 1 match each.

# Helpers landed.
grep -E '^func (readWarmupState|writeWarmupState|defaultWarmupStatePath|computeDroppedPairs|sendAllClearMail|resolveNoAlerts)' cmd/gc/cmd_start_warmup*.go cmd/gc/cmd_start.go
# Expect: 6 matches total.

# CLI flag exists.
grep -n -- '--no-warmup-alerts' cmd/gc/cmd_start.go
# Expect: 1 match.

# Config field exists.
grep -n 'StartupConfig' internal/config/config.go
# Expect: type definition + field on City.

# Test landed.
grep -c '^func TestWarmup' cmd/gc/cmd_start_warmup_test.go cmd/gc/cmd_start_warmup_state_test.go
# Expect: 16+ across the two files.

grep -c '^func TestStartupWarmupAlertsParses' internal/config/config_test.go
# Expect: 1.
```

## Builder bead

- **`ga-hewclh`** — warmup suppression + all-clear + opt-out (P2,
  `ready-to-build`, `source:actual-pm`, `backend:postgres`). Routed to
  `gascity/builder`. Blocked by `ga-wgsv3t` (slice 2 builder).
