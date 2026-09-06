# Plan: PG-auth slice 4/4 — doctor check + resolver-source observability

> Owner: `gascity/pm` · Created: 2026-05-06
> Source architecture: `ga-dga2` (closed) — *gc bd subprocess strips
> BEADS_POSTGRES_PASSWORD env (blocks PG-backed rigs)*
> Source design: `ga-5c4x` (designer-complete, this PM session closes it)
> Sibling chain: `ga-0nmb` (1d, closed) → `ga-pnqg` (1b, closed via PR #1727)
> · `ga-3hay` (2d, closed) → `ga-vt6q` (2b, routed to builder)
> · `ga-4qvs` (3d, closed) → `ga-wvka` (3b, routed to builder)
> · **`ga-5c4x` (4d, this slice)** → ga-5c4x.1 (4b, this PM creates)

## Why this work exists

Slices 1 + 2 + 3 deliver the PG-auth foundation: `MetadataState`
parses Postgres host/port/user/database (`ga-pnqg`); a typed
`internal/pgauth` package resolves passwords through a seven-tier
chain (`ga-vt6q`); and `cmd/gc/bd_env.go` dispatches on
`MetadataState.Backend` to project credentials into the bd
subprocess env (`ga-wvka`). After slice 3 lands, the mayor's repro
`gc bd --rig <pg-rig> list` exits 0 — but an operator has no way to
*verify* PG-backed rigs are well-configured before agents start
trying to bd-write, and an auditor reading `gc trace` cannot see
which resolver tier supplied the password.

Slice 4 closes that observability gap. It adds: a `gc doctor`
check (`postgres-auth`) that reports per-scope resolution status with
exact wording for five branches; a `gc doctor --explain-postgres-auth`
flag rendering the seven-tier resolution table per scope (never the
password value); a typed event payload
(`pg.credential_resolved`) emitted on every successful resolve; and
a regression test that asserts no observation surface — event JSON,
event envelope, redacted-text helper — leaks the password literal.
An optional warm-up alert routes the same diagnostic to the mayor's
inbox at `gc start` time, contingent on the existing dolt-side path.

## Goal

Land a single PR that:

- Adds `internal/doctor/checks/postgres_auth.go` (+ tests) with the
  five-branch status mapping from design §3.3 and `<human-source-label>`
  table from §3.4. Aggregated `CheckResult` shape per §3.5.
- Adds the `--explain-postgres-auth` flag to `cmd/gc/cmd_doctor.go`
  with the layout from design §4.2 (`[YES]/[no]/[skip]/[ERR]` semantics
  per §4.3, error rendering per §4.4, multi-scope rules per §4.5,
  empty case per §4.6, color/NO_COLOR per §8).
- Adds `internal/pgauth/events.go` with the
  `PostgresCredentialResolvedPayload` typed struct (design §5.3),
  registers `events.PostgresCredentialResolved =
  "pg.credential_resolved"` in `KnownEventTypes`, and emits the event
  from slice-3's `applyResolvedScopePostgresEnv` per §5.4.
- Adds `TestPostgresEventOmitsPassword` in
  `internal/pgauth/events_redact_test.go` with the four required
  sub-tests (§6.3) plus the negative-control sub-test (§6.4).
- Optionally wires the warm-up mail-to-mayor alert per §7 if the
  dolt-side path exists today; otherwise files a follow-on
  `needs-design` bead and proceeds.

## Work breakdown

| Bead         | Title                                                                                  | Priority | Routes to | Gate           |
|--------------|----------------------------------------------------------------------------------------|----------|-----------|----------------|
| `ga-5c4x.1`  | feat(doctor): add postgres-auth check + pg.credential_resolved event (slice 4/4 PG-auth) | P1       | builder   | ready-to-build |

The designer's notes (`ga-5c4x` §13) explicitly call for one PR:
"This design is **one PR's worth** of work… Recommended decomposition:
one builder bead, single PR. Splitting would create cross-PR
coordination overhead with no review benefit (every file references
the others). Mirror slice-3 pm decomposition (`ga-wvka` — single
bead)." The pm decomposition honours that — one builder bead, one
new package + one new file in `internal/pgauth/` + one new constant
in `internal/events/events.go` + one new flag on `cmd_doctor.go` +
one new emit call in `bd_env.go` ≈ 250-350 LOC total.

## Dependency graph

```
ga-pnqg (1b, closed) ─┐
                      ├─► ga-vt6q (2b, open)
                      │           │
                      │           ▼
                      └─► ga-wvka (3b, open) ─► ga-5c4x.1 (4b, this slice)
```

`ga-5c4x.1` declares a hard `depends_on` edge from `ga-wvka` (slice 3
builder). Slice 3 transitively depends on slices 1 + 2. The reconciler
will not surface `ga-5c4x.1` as ready until slice 3 closes — which is
correct, because slice 4 imports both `internal/pgauth`'s public
surface (slice 2) AND modifies slice 3's `applyResolvedScopePostgresEnv`
to add the emit call.

## Routing rationale

Slice 4 has been through architect (`ga-dga2`) and designer
(`ga-5c4x`). The designer's notes pin: the verbatim wording for all
five doctor-check status branches with exact `Message`/`Details`/
`FixHint` text (§3.3); the eight `<human-source-label>` strings (§3.4);
the explain-table column widths and three-token status vocabulary
(§4.2-4.3); the error-tier `[ERR]` rendering (§4.4); the multi-scope
sort order (§4.5); the empty-PG-backed-scopes case (§4.6); the typed
event constant name (`pg.credential_resolved`) and full payload
struct with field types and JSON keys (§5.2-5.3); the four required
test sub-tests with their exact assertions (§6.3); the negative-
control sub-test (§6.4); the mail-to-mayor body template (§7.2); the
seven CLI accessibility pins (§8); and the eight acceptance items
(§11). There is nothing left to design — only build.

Routed to **builder** with `ready-to-build`. No validator hop because
the designer's test sketch (§6 + §11 of the design notes) is the test
plan; the builder authors fixtures + tests as part of the PR per
package convention.

## Acceptance criteria (rolled up)

The full criteria live in the builder bead's notes and in `ga-5c4x`'s
design (§3, §4, §5, §6, §7, §8, §11). Roll-up for stakeholder visibility:

1. **Doctor check exists with the exact five-branch wording.**
   `internal/doctor/checks/postgres_auth.go` implements the `Check`
   interface; the per-scope branches (StatusOK, StatusWarning,
   StatusError–no-creds, StatusError–permissive, StatusError–parse,
   StatusError–unknown) emit the verbatim `Message` and `FixHint`
   strings from design §3.3.1 through §3.3.6. `CanFix()` returns
   false (§3.6).
2. **Check registration is gated on PG presence.** The check is
   registered in `cmd/gc/cmd_doctor.go` only when at least one scope
   (city or rig) has `MetadataState.Backend == "postgres"`. Pure-Dolt
   cities never see a "skipped postgres-auth" line.
3. **`--explain-postgres-auth` flag prints the table.** The flag is
   defined on the existing `doctor` cobra command. Output matches the
   layout in design §4.2 — three-column row format, right-aligned
   `[YES]/[no]/[skip]` at column 70, footer line with
   `Source identifier:` + `Source position:`. The empty-PG case
   (§4.6) prints exactly one line and exits 0.
4. **`[YES]`/`[no]`/`[skip]`/`[ERR]` semantics are honest.** Tiers
   after the winner are `[skip]`, never `[no]`. Permission/parse
   errors render the failing tier with `[ERR]` plus inline reason
   (§4.4); subsequent tiers are `[skip]`.
5. **Typed event payload is registered.**
   `events.PostgresCredentialResolved = "pg.credential_resolved"`
   added to `KnownEventTypes`. `PostgresCredentialResolvedPayload`
   in `internal/pgauth/events.go` has six string fields with the JSON
   keys from §5.3. **No `password` or `password_present` field.**
   Registered via `events.RegisterPayload` in `init()`.
   `TestEveryKnownEventTypeHasRegisteredPayload` does not regress.
6. **Event emission lives in slice-3's helper.**
   `applyResolvedScopePostgresEnv` (`cmd/gc/bd_env.go`) calls a
   small `emitPostgresCredentialResolved(...)` helper after the
   resolver returns success. Best-effort — a recorder failure does
   NOT propagate as a returned error.
7. **Redaction regression test passes.**
   `TestPostgresEventOmitsPassword` in
   `internal/pgauth/events_redact_test.go` runs the four required
   sub-tests (`EventPayloadOmitsPassword`,
   `EventEnvelopeOmitsPassword`, `RedactTextScrubsPassword`,
   `EventCarriesExpectedSource`) plus the negative-control
   `EventEmitsForResolvedSource`. The test uses a unique
   `redaction-canary-<hex>` password literal — appears nowhere else
   in the codebase.
8. **Warm-up alert ships if dolt-side exists.** If the dolt-side
   doctor check has a warm-up alert path today, slice 4 reuses it
   verbatim with the §7.2 mail body. If not, builder files a
   `needs-design` follow-on bead and ships slices 1-4 without the
   alert (architect's spec marks §5 of slice 4 as optional).
9. **Coverage and hygiene.**
   `go test ./internal/doctor/checks/ -run TestPostgresAuth -count=1`,
   `go test ./internal/pgauth/ -run TestPostgresEventOmitsPassword -count=1`,
   `go test ./internal/events/ -count=1`, all pass.
   `go vet ./internal/doctor/checks/ ./internal/pgauth/ ./internal/events/`
   clean. ZFC: no role names in the diff. Typed wire: no
   `map[string]any` or `json.RawMessage`.

## Risks and unknowns

- **`pgauth.PermissivePermissionError` may lack a `Source` field.**
  Design §3.3.4 notes the slice-2 error today carries `Path` + `Mode`
  but not `Source`. The doctor must reconstruct the failing tier by
  comparing `permErr.Path` against `<scope>/.beads/.env`,
  `os.Getenv("BEADS_CREDENTIALS_FILE")`, and
  `pgauth.DefaultCredentialsPath()` (if exported). If slice 2 ships
  without that helper exported, the builder files a follow-on bead to
  add it; the interim fallback is `tier=credentials_file` (less
  specific, still actionable).
- **`Render(ctx)` vs `RenderExtras(io.Writer)` interface choice.**
  Design §4.1 explicitly leaves the integration shape to the builder:
  either extend `CheckContext` with an `ExplainPostgresAuth bool` and
  a writer, or add a `RenderExtras(io.Writer)` method to the `Check`
  interface that `PostgresAuthCheck` opt-in implements. Both are
  isomorphic; the builder picks whichever is the smaller diff.
- **Where `events.go` lives.** Design §5.3 recommends
  `internal/pgauth/events.go` (payload sits with the package whose
  semantics it describes). If the builder finds this creates an
  import cycle with `internal/events`, fall back to
  `internal/extmsg/events.go` style. The recommendation is preference,
  not a hard pin.
- **The trace renderer is owned elsewhere.** Slice 4 only registers
  the payload + emits the event. The `cmd_trace` command's renderer
  is out of scope. As long as the payload's JSON keys are snake_case
  and the field types are stable, the trace renderer needs no
  changes (it iterates registered types).
- **Mail-to-mayor warm-up path may not exist.** Design §7.3 makes
  this conditional: search `gc start` for any
  `Notify`/`SendMail`/`Mayor` send used by existing doctor checks
  during warm-up. If absent, defer the warm-up alert to a follow-on
  `needs-design` bead and ship the four core deliverables (check,
  flag, event, regression test) without it.
- **Cross-binary redaction acceptance test is deferred.** Design
  §6.5 names `TestPostgresEnvDumpRedacted` (fork-exec `gc bd` and
  grep stdout/stderr) as out of scope. File as `needs-design` if
  not already covered. The current `IsSensitiveKey` already redacts
  `*PASSWORD*` keys; the test would lock the regression door.
- **Status icon vocabulary is fixed (`✓ ⚠ ✗`).** Design §3.7 + §8.1
  + §8.3 pin Unicode glyphs from the existing `printResult`
  formatter. Builder must NOT introduce new glyphs (e.g. `🔑` for
  credentials) — the existing vocabulary is the contract operators
  rely on for `grep`-ability (§8.5).

## Out of scope (explicit)

- A JSON output mode for `gc doctor` — requires a global formatter
  refactor; file as `needs-design` (§10).
- `gc bd doctor` (bd-side credentials check) — lives in the bd
  binary's repo, per architect.
- Integration test against a real PG instance — file as `needs-design`
  if not already covered by podman/test-containers in CI.
- Auto-fix for chmod-on-credentials-file — would prompt; out of scope
  for non-interactive `--fix` (§10).
- `database` field on the event payload — additive follow-up if a
  consumer needs it; file as `needs-design`.
- Cross-binary redaction acceptance test — see §6.5; file as
  `needs-design`.
- Wider terminal width detection / re-flow in the explain table —
  design §8.4 explicitly forbids it. Long values wrap naturally.
- New constants for `[YES]`/`[no]`/`[skip]`/`[ERR]` — keep them as
  literal strings in the explain renderer; no premature abstraction.

## Validation gates

- `go test ./internal/doctor/checks/ -run TestPostgresAuth -count=1` green.
- `go test ./internal/pgauth/ -run TestPostgresEventOmitsPassword -count=1` green.
- `go test ./internal/events/ -count=1` green
  (`TestEveryKnownEventTypeHasRegisteredPayload` does not regress).
- `go vet ./internal/doctor/checks/ ./internal/pgauth/ ./internal/events/` clean.
- `git diff` shows changes confined to `internal/doctor/checks/`
  (new package), `internal/pgauth/events.go` + redaction test (new
  files), `internal/events/events.go` (one constant), `cmd/gc/cmd_doctor.go`
  (one flag + one registration call), `cmd/gc/bd_env.go` (one emit
  call site in slice-3's helper). No other files modified.
- `grep -rn '"password"' internal/pgauth/events.go` returns zero
  matches (the field literally does not exist in the struct).
- `grep -rn 'password' internal/pgauth/events.go` returns matches
  ONLY in identifier names (`PostgresCredentialResolvedPayload`,
  comments referring to the contract, etc.) — never as a JSON key.
- The five status branches' verbatim `Message` and `FixHint` strings
  appear as exact-match assertions in `postgres_auth_test.go`.
- The `[YES]`/`[no]`/`[skip]`/`[ERR]` token vocabulary is
  exact-match-asserted in the explain-table renderer test.
- ZFC: no role names in the diff.
- Typed wire: no `map[string]any` or `json.RawMessage` introduced on
  any wire boundary.
- Manual: `gc doctor --explain-postgres-auth` against a city with one
  PG-backed rig (mode 0600 scope file) prints the §4.2 table with
  `[YES]` at tier 4 and `[skip]` at tiers 5-7; the password value
  appears nowhere.
- Manual: `gc trace --rig <r> --since=1m` after a `gc bd` call
  surfaces a `pg.credential_resolved` event with all six payload
  fields (no password).
