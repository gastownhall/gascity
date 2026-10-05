# Plan: postgres-server doctor check + systemd-user linger probe (ga-46yyd)

> **Status:** decomposing — 2026-05-11
> **Source architecture:** `ga-vkjp` (closed) — mayor → architect
> contract for the postgres-server check.
> **Designer spec:** `ga-46yyd` (this bead) — full body pins verbatim
> `Message`/`FixHint`/`Details` strings, helper signatures, exact
> `t.Run` sub-test names, registration order, and the coordination
> CI guard.
> **Superseded ancestors (already closed by architect 2026-05-11):**
> `ga-8em0`, `ga-gq0h` — same scope; ga-46yyd is canonical.
> **Sibling slice (postgres-auth):** `ga-5c4x` (designer-complete) →
> `ga-yih2` (builder bead, open). The two slices share the
> `cityHasPostgresScope` predicate and registration order; whichever
> PR lands second consumes the existing predicate definition.
> **Local-only stack status:** slice-1 PG-auth (`ga-pnqg`, the
> `MetadataState.PostgresHost/Port/User/Database` fields) is **landed**
> via PR #1727 / b7015a05 — so this slice's prerequisite "slice 1
> MetadataState fields present at HEAD" is satisfied.
> **Decomposed into:** 1 builder bead (see Children below)

## Context

The architect's contract on `ga-vkjp` settled the policy: read-only
TCP probe to the configured PG endpoint, **no auto-start**, no
`Fix()`, registered as a sibling of `postgres-auth` and listed
*before* it. The designer's body pins the verbatim wording for every
status branch — operator-facing strings are the contract, asserted
literal-equal in tests so a copy-edit drift breaks the build.

This is the doctor-side observability for PG-backed rigs. Slices 1-3
of PG-auth ensure `gc bd` can connect; slice 4 (sibling, `ga-yih2`)
explains *credential resolution*; this slice (effectively slice 5 of
the PG-auth chain) explains *server reachability + boot-survival*.

## Why a single builder bead

The design is ~470 LOC of source + tests in a single file
(`internal/doctor/checks_postgres.go`), one constant in `cmd/gc/cmd_doctor.go`
(`newDoctorPostgresServerCheck`), one shared predicate
(`cityHasPostgresScope`) coordinated with slice-4, and two integration
tests. Splitting would force the second PR to either fork the
verbatim contract strings or retrofit the registration gate
mid-flight. Slice-4 (`ga-yih2`) followed the same shape — one bead.

## Children

| ID            | Title                                                                                       | Routing label    | Routes to         | Depends on |
|---------------|---------------------------------------------------------------------------------------------|------------------|-------------------|------------|
| `ga-46yyd.1`  | feat(doctor): add postgres-server check + systemd-user linger probe (slice 5 of PG-auth)    | `ready-to-build` | `gascity/builder` | — (slice-1 landed; slice-4 ga-yih2 may land before or after) |

## Acceptance for the parent (ga-46yyd)

Met when `ga-46yyd.1` closes and all of the following hold (rolled
up from designer's §12 acceptance hints):

- [ ] `internal/doctor/checks_postgres.go` defines `PostgresServerCheck`
      with the four-method `Check` interface verbatim per §5.4.
- [ ] Per-scope sub-messages match §2.1 verbatim (`reachable at ...`,
      `server not reachable at ...`, `metadata missing postgres
      host/port; cannot probe`).
- [ ] Top-level Message aggregation matches §2.2 verbatim across all
      seven rows (single-OK, single-Error, single-linger-Warning,
      multi-OK, multi-Warning, multi-Error, zero-scope).
- [ ] `Details[]` sort order matches §2.3 (severity desc, then scope
      path lex asc; linger row sorts last among Warnings).
- [ ] Per-scope Detail row format matches §3.1 verbatim (status glyph,
      em-dash separator, scope-display token).
- [ ] Global linger Detail row matches §3.2 verbatim (`⚠ systemd-user
      linger is not enabled — PG will not start at boot`).
- [ ] Verbose-only linger-probe-failed Detail row matches §3.3 verbatim.
- [ ] `postgresServerFixHint(host, port, goos, lingerNeeded)` matches
      §5.1 signature; FixHint text matches §4.1 / §4.2 / §4.3 verbatim
      for every GOOS × loopback × linger combination.
- [ ] `systemdUserLingerEnabled` helper matches §5.2 verbatim
      (function signature, constants, function-variable seams).
- [ ] `cityHasPostgresScope(cityPath, cfg)` matches §5.3 verbatim;
      guards nil `cfg` inside the helper.
- [ ] Check registered in `cmd/gc/cmd_doctor.go` inside
      `if cityHasPostgresScope(cityPath, cfg) { ... }`, BEFORE
      slice-4's `postgres-auth` registration (ordering comment
      verbatim).
- [ ] All `t.Run` sub-test names from §8.1, §8.2, §8.3, §8.4 present
      verbatim.
- [ ] `TestCityHasPostgresScopeDefinedExactlyOnce` passes (§9).
- [ ] `go test ./internal/doctor/ -run "TestPostgresServer|TestSystemdUserLingerEnabled" -count=1` green.
- [ ] `go test ./cmd/gc/ -run "TestPostgresChecks|TestCityHasPostgresScope" -count=1` green.
- [ ] `go vet ./internal/doctor/ ./cmd/gc/` clean.
- [ ] ZFC: no role names in the diff.

## Notes for the builder

- **Read the designer's bead in full before starting.** Every
  `Message` / `FixHint` / `Details` string is asserted literal-equal
  in tests. Paraphrasing breaks the build.
- **Verbatim Unicode codepoints matter.** §3.2 uses `⚠` (U+26A0
  without variation selector). §4.1 Windows row uses `→` (U+2192).
  §3.1 em-dash separator is U+2014 with single ASCII spaces. Tests
  assert these as literals.
- **Test seams are function-variables, not interfaces.** §5.2
  pins `systemdUserLingerStatProbe`, `systemdUserLingerCurrentUser`,
  `systemdUserLingerGOOS` as package-level `var`s. The helper
  internally uses these, NOT `os.Stat` / `user.Current` / `runtime.GOOS`
  directly. Tests override and restore in `t.Cleanup`.
- **Coordinate with slice-4 (`ga-yih2`) on `cityHasPostgresScope`.**
  Whichever PR lands second consumes the existing definition.
  `TestCityHasPostgresScopeDefinedExactlyOnce` (§9) surfaces a
  duplicate definition as a test failure with a helpful message
  before it becomes a linker error. If `ga-yih2` has merged before
  this slice, use its definition; otherwise introduce it here.
- **File layout: single file at top of `internal/doctor/`.** §6 pins
  `internal/doctor/checks_postgres.go`. Designer explicitly rejects
  the sub-package layout slice-4's design floated, on the grounds
  that no sub-package exists today and creating one as a side effect
  is scope creep. If slice-4 has *already* landed and created
  `internal/doctor/checks/`, switch to `internal/doctor/checks/postgres_server.go`
  to follow it. Either layout is OK; the binding constraint is
  **both PG checks live in the same package**.
- **Registration order is load-bearing.** Per §7, the call site
  comment must include the phrase "ORDER IS LOAD-BEARING for
  operator UX" and the test name `TestPostgresChecksRegistrationOrder`.
  Slice-4's `postgres-auth` is registered *after* `postgres-server`.

## Out of scope (do NOT touch — explicitly rejected by architect)

These are pinned in §10. The builder must reject any review comment
suggesting these belong in the same PR:

- Auto-start of PG (`os/exec`, `systemctl start`, etc.).
- A `--start-pg` flag on `gc doctor` (deferred — `ga-igomxo`).
- Boot-time service unit installation by `gc init` (deferred — `ga-5holj3`).
- A `--explain-postgres-server` flag (no resolution chain to explain).
- A libpq / pgx connection (reachability ≠ auth — slice-4's job).
- An RDS DescribeDBInstances cloud probe.
- Auto-restart on transport error in bd recovery hooks.
- Warm-up alerts during `gc start` for postgres-server
  (slice-4 may do this for postgres-auth; slice-5 inherits the
  decision).
- Per-rig `RigPostgresServerCheck` (aggregated single `CheckResult`
  per `Run` is the architect contract; per-scope info goes in
  `Details[]` only).
- Configurable timeout (hard-pin 2 seconds, matches Dolt).
- Including the password in the FixHint.

## Validation gates

- `go test ./internal/doctor/ -run TestPostgresServerCheck -count=1` green.
- `go test ./internal/doctor/ -run TestPostgresServerCheck_Linger -count=1` green.
- `go test ./internal/doctor/ -run "TestPostgresServerFixHint|TestSystemdUserLingerEnabled" -count=1` green.
- `go test ./cmd/gc/ -run "TestPostgresChecksRegistrationOrder|TestPostgresChecksNotRegisteredForPureDoltCity|TestCityHasPostgresScopeDefinedExactlyOnce" -count=1` green.
- `go vet ./internal/doctor/ ./cmd/gc/` clean.
- `grep -rn "exec\." internal/doctor/checks_postgres.go` returns zero
  matches.
- `grep -rn "internal/pgauth" internal/doctor/checks_postgres.go`
  returns zero matches.
- ZFC: no role names in the diff.
- Typed wire: no `map[string]any` / `json.RawMessage` introduced.
- Manual: stop local PG; run `gc doctor` against a PG-backed rig →
  `✗ postgres-server — server not reachable at <host>:<port>` with
  the per-GOOS hint. Start PG; re-run → `✓ postgres-server — reachable
  at <host>:<port>`.
- Manual: pure-Dolt city → no `postgres-server` line at all
  (registration gate, NOT a "skipped" line).
- Manual on Linux+systemd, loopback PG, after `loginctl disable-linger
  $USER` → `⚠ postgres-server — reachable at 127.0.0.1:<port>;
  boot-survival is not configured` with the literal `loginctl
  enable-linger` amendment in `FixHint`.

## Risks and unknowns

- **`internal/doctor/checks/` sub-package may exist by merge time.**
  If `ga-yih2` (slice-4 builder bead) lands first and introduces
  the sub-package, this slice follows it — `internal/doctor/checks/postgres_server.go`
  instead of `internal/doctor/checks_postgres.go`. Either layout is
  acceptable per §6.
- **`/run/systemd/system` presence on container-based test runners.**
  The §5.2 helper guards with both the runtime-dir stat AND the
  linger-file stat; CI runners without systemd should land in the
  "non-Linux or no-systemd" branch (`(false, nil)`) — verify locally
  if the CI matrix includes Alpine / busybox.
- **Slice-4 may have already introduced `cityHasPostgresScope`.**
  If so, the §9 coordination test passes trivially with `n == 1`
  and this slice consumes the existing definition. Builder runs
  `grep -rn 'func cityHasPostgresScope(' cmd/gc/` at start of work
  to decide.
- **PR remains LOCAL-ONLY until both slice-4 (`ga-yih2`) and this
  slice are ready to ship together** — the architect mail flagged
  this. The builder lands the work on a local branch; PM (or
  architect) coordinates the joint PR open when both are green.

## Cross-references

- `ga-vkjp` — closed mayor/architect contract carrying the original
  decision tree.
- `ga-8em0`, `ga-gq0h` — closed older parallel design beads
  (superseded 2026-05-11 by architect).
- `ga-yih2` — open slice-4 builder bead (sibling). Shares
  `cityHasPostgresScope` and registration order with this slice.
- `ga-5c4x` — closed slice-4 design (verbatim vocabulary parent for
  the em-dash separator + status-glyph set).
- `ga-amb2` — boot-time installer follow-on (the linger probe in
  this slice is the diagnostic that points operators at
  `loginctl enable-linger` before they reach the installer).
- `ga-igomxo`, `ga-5holj3` — Phase 2 / Phase 3 follow-on beads
  (deferred until mayor green-lights).
