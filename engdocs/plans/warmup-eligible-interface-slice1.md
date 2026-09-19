# Plan: WarmupEligible() interface + pack manifest warmup field (ga-perl18 slice 1/4)

> **Status:** decomposing — 2026-05-11
> **Parent architecture:** `ga-perl18` — `gc start` warm-up alert
> mechanism for doctor failures (PG-auth and beyond).
> **Designer spec:** `ga-3onbqh` (~458-line design body) pins:
> `Check.WarmupEligible() bool` as the LAST interface method, the
> verbatim one-line default-false implementation with doc comment for
> grep-ability, `PackScriptCheck.Warmup` field + special-case
> `WarmupEligible()` returning `c.Warmup`, `pack.toml [[doctor]]
> warmup = true` and `doctor.toml warmup = true` TOML names,
> three new test functions with 3-case TOML parse matrices,
> registry-of-record test `TestCheckWarmupEligibleDefaultsFalse`.
> Architect builder estimate: ~80 LOC source + ~120 LOC tests, 1 PR.
> **Designer handoff mail:** `gm-rnx0uu` (2026-05-11).
> **Decomposed into:** 1 builder bead — `ga-r1iqmy`.

## Context

`gc start` today does not run any diagnostic check before deciding the
city is up. The PG-auth child of ga-perl18 establishes a warmup
pattern: opt-in checks run during `gc start`, and a non-OK result
mails the mayor instead of failing the city. Slice 1 ships the typed
eligibility opt-in only — the API surface that slices 2-4 will read
from. Nothing reads `WarmupEligible()` in this slice; that's slice 2
(`ga-f932ei`, design in flight).

The pinned shape per designer:

- **Method on `Check`, not an optional sub-interface.** Architect
  literal wording and the named test
  `TestCheckWarmupEligibleDefaultsFalse` both imply the method is on
  the main interface, and opt-in via type-assertion would push the
  eligibility statement into the runner's type-assertion logic
  (a ZFC violation — eligibility lives on the check, not in framework
  glue).
- **Manifest field is `warmup`, not `warmup_eligible`.** Shorter,
  reads naturally as `warmup = true`, matches existing terse style of
  `fix = "..."`, `script = "..."`.
- **Exposed in BOTH the legacy `[[doctor]]` block AND
  convention-discovered `doctor.toml`.** Both pathways synthesize
  `DiscoveredDoctor` → `PackScriptCheck`. The flag must survive both.

Landing this first lets slices 2-4 reference a stable contract — they
don't have to flop between API revisions while design work is in
flight in parallel.

## Plan

One builder bead. The whole slice is a single PR shape: an interface
extension, a forest of one-line method additions (estimated ~12
concrete types plus 3 test mocks), two new struct fields, and four
new test functions.

| Builder bead              | PR shape | Files (key)                                                                                |
|---------------------------|----------|--------------------------------------------------------------------------------------------|
| **Interface + manifest**  | PR       | `internal/doctor/types.go`, every concrete `Check` impl, `internal/doctor/pack_checks.go`, `internal/config/config.go`, `internal/config/doctor_discovery.go`, `internal/config/pack.go`, `cmd/gc/cmd_doctor.go`, plus 4 test files |

### `ga-r1iqmy` — interface + manifest field + tests (P2, `ready-to-build`)

Implements FR-01 and FR-02 from `ga-perl18`. Routed to
`gascity/builder` via `gc.routed_to` metadata;
`gc.design_parent=ga-3onbqh` records the back-link.

**Acceptance criteria (verbatim from `ga-3onbqh` §"Acceptance criteria"):**

- `doctor.Check` interface has `WarmupEligible() bool` as its LAST
  method (after `Fix`).
- Every concrete in-tree `Check` implementation has the one-line
  method returning `false` with the verbatim doc comment:
  `// WarmupEligible returns false; this check is not part of the
  // 'gc start' warm-up scan.` (Verbatim — preserves grep-ability.)
- `PackScriptCheck.WarmupEligible()` returns `c.Warmup` (the one
  in-tree check whose `WarmupEligible` is variable).
- `PackScriptCheck` has new `Warmup bool` field, placed LAST after
  `PackName`. Populated from the manifest at the call site in
  `cmd/gc/cmd_doctor.go:247`.
- `PackDoctorEntry`, `DiscoveredDoctor`, `doctorManifest` all have
  `Warmup bool` fields. TOML tag is `warmup` (lowercase,
  `omitempty` on the legacy entry only).
- `legacyPackDoctors` (`internal/config/pack.go:~2050`) copies the
  field through into the synthesized `DiscoveredDoctor`.
- `TestCheckWarmupEligibleDefaultsFalse` exists with one subtest per
  concrete in-tree `Check` type (the slice is the registry-of-record
  for the safe-default invariant) plus the
  `pack_script_check_default_false` and `pack_script_check_opted_in`
  subtests. All pass.
- `TestPackDoctorWarmupFlagParses` exists with 3 subtests
  (`explicit_true`, `explicit_false`, `default_omitted`).
- `TestDoctorManifestWarmupFieldParses` exists with 3 subtests
  (mirror of above for `doctor.toml`).
- `TestPackScriptCheckWarmupEligibleReflectsField` exists with 2
  subtests (zero-value false; `Warmup: true` true).
- `go test ./...` passes; `go vet ./...` clean.
- No behavioral change to `gc doctor` or `gc start`.

**Builder enumeration step:**

```
grep -rn 'func.*CanFix() bool' internal/doctor/ cmd/gc/
```

Use the result to enumerate every concrete `Check` implementor. Design
§"Existing in-tree Check implementations" lists the expected ~12 types
(`BeadsRoleCheck`, `CustomTypesCheck`, `DurationRangeCheck`,
`EventLogSizeCheck`, `ConfigSemanticsCheck`,
`DeprecatedAttachmentFieldsCheck`, `ImplicitImportCacheCheck`,
`WorktreeCheck`, `DoltServerCheck`, and others in `checks.go`,
`skill_checks.go`, `cmd/gc/doctor_v2_checks.go`,
`cmd/gc/doctor_mcp_checks.go`). Builder confirms by grep at impl time
and adds any straggler with the same one-liner — no architect re-pin
required for stragglers.

Test mocks (`mockCheck`, `detailCheck`, `hintCheck` in
`doctor_test.go`) also need the one-liner or the test file won't
compile. Builder's first `go test ./...` surfaces every missing case.

**HARD RULES carried from design:**

- The verbatim doc comment on every default-false impl is
  load-bearing. Future audit greps `WarmupEligible returns false` to
  find every check that has opted out by default.
- Receiver pattern (pointer vs value) MUST match the rest of each
  type's methods. `BeadsRoleCheck` uses pointer receivers → its
  `WarmupEligible()` does too.
- The TOML field name is `warmup`. Do not pluralize, do not snake-case
  `warmup_eligible`.
- Place `WarmupEligible()` AFTER `Fix` in the `Check` interface —
  position matters for `go doc` readability and is part of the
  designer pin.
- Place `Warmup` LAST in `PackScriptCheck` (after `PackName`) — same
  reason: the other fields are call-site-mandatory; `Warmup` is
  optional.
- `omitempty` ONLY on the legacy `PackDoctorEntry` TOML tag. The
  `doctorManifest` tag is bare `toml:"warmup"`.
- Adding `WarmupEligible()` does NOT change any runtime behavior in
  this slice. No new flags, env vars, or city.toml keys appear.

## Sequence

No `bd dep add` edge to other ga-perl18 slices — slice 2's design
(`ga-f932ei`) is in flight and explicitly references this contract
as a stable pin. Build order is implicit: slice 1 must merge before
slice 2's PR lands. PM will surface the unblock once
`ga-r1iqmy` closes.

## Out of scope (slices 2-4 of ga-perl18 — separate beads)

- `RunWarmupChecks` runner.
- `WarmupReport` struct.
- Wire-up in `cmd_start.go`.
- Mail-to-mayor on failure.
- Suppression state file.
- `--no-warmup-alerts` flag.
- `[startup] warmup_alerts` city.toml key.
- Any check actually opting in (slice 4 flips PG-auth to `warmup =
  true`).

## Verification (after PR lands)

```bash
# Interface is extended.
grep -A1 'type Check interface' internal/doctor/types.go | tail -1
# Expect: a closing brace if WarmupEligible() is on the line above.
grep -c 'WarmupEligible() bool' internal/doctor/types.go
# Expect: 1 (interface declaration).

# Every default-false impl is present.
grep -rc 'WarmupEligible returns false' internal/doctor/ cmd/gc/ | grep -v ':0$'
# Expect: ≥12 files (the in-tree checks).

# Pack-script check reflects the field.
grep -A2 'func (c \*PackScriptCheck) WarmupEligible' internal/doctor/pack_checks.go
# Expect: a method body returning c.Warmup.

# Manifest plumbing.
grep -n 'Warmup' internal/config/config.go internal/config/doctor_discovery.go
# Expect: 3+ hits (PackDoctorEntry, DiscoveredDoctor, doctorManifest).
```

## Builder bead

- **`ga-r1iqmy`** — interface + manifest + tests (P2,
  `ready-to-build`, `source:actual-pm`, `backend:postgres`). Routed
  to `gascity/builder`.
