# Plan: PG-auth as first warmup producer (ga-perl18 slice 4/4)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-perl18` — `gc start` warm-up alert
> mechanism for doctor failures (PG-auth and beyond).
> **Designer spec:** `ga-xextj6` pins the `WarmupEligible()` opt-in
> for `PostgresAuthCheck`, the new `CustomWarmupMail` sub-interface
> in `internal/warmup/mail.go`, the verbatim §7.2 mail subject + body
> via the exported `WarmupMailSubject` const, the
> `tryCustomSoleFailureMail` runner helper, and 5 test names with
> assertion text.
> Architect builder estimate: ~150 LOC, 1 PR.
> **Designer handoff mail:** `gm-s7sgke` (2026-05-11).
> **Decomposed into:** 1 builder bead — `ga-uslskt`.

## Context

`ga-perl18` is a 4-slice architecture for the `gc start` warm-up
alert mechanism. Three slices are already in flight:

- **Slice 1** (`ga-r1iqmy` — builder open) — `WarmupEligible()`
  interface + pack manifest warmup field. All in-tree checks default
  to `false`.
- **Slice 2** (`ga-wgsv3t` — builder open) — `RunWarmupChecks` runner
  + `WarmupReport` types. Generic mail-render path:
  `<icon> <scope> — <message>` body format + indented `fix:` lines +
  generic footer. The runner is content-agnostic (`TestRunWarmupChecks_
  MailBody_ExcludesSecretsByDefault` locks the producer as the trust
  boundary, not the runner).
- **Slice 3** (`ga-hewclh` — builder open) — duplicate-suppression +
  all-clear recovery mail + `--no-warmup-alerts` CLI / `[startup]
  warmup_alerts` city.toml opt-out. Wraps the slice-2 runner; does
  NOT change the mail-render path.

Slice 4 is the first concrete producer of warm-up alerts. It wires
the postgres-auth doctor check from `ga-5c4x` (PG-auth design) into
the slice-1 eligibility opt-in, the slice-2 runner's mail-render
path, and pins the verbatim §7.2 subject + body.

Slice 2's generic body format cannot reproduce §7.2's structure
(§7.2 has no `fix:` indent; per-scope summary + PG-specific footer
diverge from the generic template). Slice 2's design note already
flagged "slice 4 specializes" the footer. Slice 4 introduces the
minimal extension point: an optional `CustomWarmupMail` sub-interface
the runner type-asserts to override subject + body when exactly one
check name owns every failure.

PG-auth is the test driver. Once slice 4 lands, the full pipeline
becomes: broken PG password → mail to mayor with §7.2 body → operator
runs `gc doctor --explain-postgres-auth` → fixes → next `gc start`
sends the slice-3 all-clear → quiet thereafter.

## Plan

One builder bead. The designer's pin is exhaustive: verbatim Go for
`WarmupEligible`, the `CustomWarmupMail` interface, the
`WarmupMailSubject` const, the §7.2 body template (header + per-scope
lines + two-line footer), the `tryCustomSoleFailureMail` helper with
its three fall-back conditions, and 5 test names with assertion text.

| Builder bead | PR shape | Files (key) |
|---|---|---|
| **WarmupEligible opt-in + CustomWarmupMail interface + PG-auth impl + runner amendment + 5 tests** | One PR, ~150 LOC | `internal/doctor/checks/postgres_auth.go` (extend), `internal/warmup/mail.go` (new), slice-2's mail-render file (extend with `tryCustomSoleFailureMail`), `internal/doctor/checks/postgres_auth_test.go` (extend), `internal/warmup/mail_test.go` (new) |

### `ga-uslskt` — PG-auth as first warmup producer (P2, `ready-to-build`)

Implements slice 4/4 of `ga-perl18` per the design pin in
`ga-xextj6`. Routed to `gascity/builder` via `gc.routed_to`;
`gc.design_parent=ga-xextj6` records the back-link.

**Blocked by:**

- `ga-r1iqmy` — slice 1/4 builder (`WarmupEligible()` interface).
  Wired via `bd dep add ga-uslskt ga-r1iqmy`.
- `ga-wgsv3t` — slice 2/4 builder (`RunWarmupChecks` runner +
  `WarmupReport` types). Wired via `bd dep add ga-uslskt ga-wgsv3t`.
- `ga-yih2` — PG-auth check itself (slice 4/4 of PG-auth chain;
  builder open). Wired via `bd dep add ga-uslskt ga-yih2`.

Slice 3 (`ga-hewclh`) is **not** a prerequisite. Suppression runs
after mail-render via the slice-3 state file; slice 4 hooks the
mail-render path itself.

**Acceptance criteria summary** (full list in the bead body):

- `(*PostgresAuthCheck).WarmupEligible() bool` returns `true` (first
  non-default-false eligibility opt-in across in-tree checks).
- `CustomWarmupMail` interface declared in `internal/warmup/mail.go`
  with the exact signature `SoleFailureMail(report WarmupReport)
  (subject, body string)`. Interface lives in package `warmup`
  (NOT `doctor/checks`) so the dependency is one-way.
- `WarmupMailSubject = "postgres-auth alert during city warm-up"`
  exported const in `postgres_auth.go`.
- `(*PostgresAuthCheck).SoleFailureMail` returns the verbatim
  §7.2 body per design §4.2: header line + per-scope ✗/⚠ lines + the
  two-line footer.
- `tryCustomSoleFailureMail(report, checks) (subject, body string,
  ok bool)` package-private helper in slice-2's mail-render file.
  O(N) scan over `checks` — no registry map.
- Helper returns `false` when `len(report.Failures) == 0` (defensive
  guard).
- Helper returns `false` when more than one distinct check name
  appears in `report.Failures`.
- Helper returns `false` when the sole-failing check does not
  implement `CustomWarmupMail`.
- Returned body truncated to slice-2's FR-06 4096-byte cap with the
  slice-2 truncation marker if longer.
- All 5 tests from design §5 implemented and passing:
  - `TestPostgresAuthCheck_WarmupEligibleReturnsTrue`
  - `TestPostgresAuthCheck_SoleFailureMail_Subject`
  - `TestPostgresAuthCheck_SoleFailureMail_Body` (3 sub-cases:
    header / per-scope / footer)
  - `TestWarmupRunner_PostgresAuthSoleFailure_UsesCustomBody`
    (includes `MixedFailures` fallback sub-test)
  - `TestWarmupMailBodyExcludesSecrets`
- Slice-2's `TestRunWarmupChecks_MailBody_ExcludesSecretsByDefault`
  still passes (cross-slice regression).

**HARD RULES carried from design (bead body §"HARD RULES" — 9 items):**

- `CustomWarmupMail` lives in package `warmup` (one-way dependency).
- Sub-interface is the minimal extension point; slice-2 generic
  body remains the fallback for every other producer.
- `SoleFailureMail` receives a defensive copy of the report.
- ASCII-safe strings only.
- Subject is fixed via exported const — no per-scope interpolation.
- Per-scope `FixHint` (lives in `gc doctor` output, ga-5c4x §3.3)
  is DIFFERENT from the mail footer (lives in §7.2 body only).
- Multiple distinct check names → fallback to generic body.
- Helper iterates `checks` to find the implementation (O(N), no
  registry map).
- `WarmupEligible` doc-comment wording diverges deliberately from
  slice-1's default-false grep pattern.

## Routing

- Builder bead `ga-uslskt` carries `gc.routed_to=gascity/builder`
  and label `ready-to-build`. Will appear on builder's hook only
  after all three blockers close.
- `gc sling gascity/builder ga-uslskt` wakes the builder session
  for visibility — the bead is dep-gated so builder cannot start
  it yet, but the sling primes context for when the gate opens.
- Mail to builder via `gc mail send gascity/builder` with the bead
  ID and a one-line note about the three-way dep gate.

## Risk / non-goals

- The slice does NOT refactor slice-2's generic body format. The
  fallback path stays exactly as slice-2 ships it.
- The slice does NOT generalize `CustomWarmupMail` beyond
  single-check sole-failure scenarios. A multi-check custom-body
  generalization is reserved for a future slice if other producers
  ever need it.
- The slice does NOT add warm-up-eligible checks beyond PG-auth.
  Future opt-ins land as separate beads.
- The slice does NOT change `gc doctor --explain-postgres-auth`
  (lives in `ga-5c4x`).

Once `ga-r1iqmy`, `ga-wgsv3t`, and `ga-yih2` all close, `bd ready`
will surface `ga-uslskt` on the builder's queue.
