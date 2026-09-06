# Plan: gc-beads-bd op_init fast-path + perf regression test + CI guard (`ga-5mym` family)

> Owner: `gascity/pm-1` · Created: 2026-04-30
> Source: architecture decision `ga-5mym` (closed)
> Designer addendum: `gascity/designer` (in `ga-5mym.1` notes)
> Cluster siblings (independent landings): `ga-a3ry`, `ga-r8iz`
> Coordination dep: PR #1354 (`fix/issue-1295`) also edits
> `op_init` — orthogonal but shares the file

## Why this work exists

`examples/bd/assets/scripts/gc-beads-bd.sh::op_init` calls
`bd config set` per type during initialization. On bd 1.0.x that
path triggers a per-set schema migration; init regularly exceeds
the provider's 30-second timeout and `gc start` times out with no
useful diagnostic.

The hot patch (commit `4ef8ee5e` on `local/integration-2026-04-26`)
already replaced the `bd config set` loop with a direct YAML write
via `ensure_types_custom_in_yaml`. Architecture review concluded
**promote, don't redesign** — the patch is correct; we need to
land it on `origin/main` and lock in a regression test plus a CI
guard.

## Goal

Land the fast-path patch on `origin/main`. Make the regression
provably absent now, and prevent its re-introduction with a CI
guard so a future commit that re-adds `bd config set` to
`op_init` fails the build.

## Work breakdown

| Bead         | Title                                                                                              | Priority | Routes to | Gate           |
|--------------|----------------------------------------------------------------------------------------------------|----------|-----------|----------------|
| `ga-5mym.1`  | Implement gc-beads-bd op_init fast-path promotion + perf regression test + CI guard                 | P1       | builder   | ready-to-build |

The architect+designer broke this work down to a single coherent
implementation unit covering: cherry-pick of `4ef8ee5e`, atomic-
write audit of `ensure_types_custom_in_yaml`, the perf regression
test under `//go:build integration`, the Go-level CI grep guard,
the bd minimum-version pin, and a runbook docs sweep.

The audit step is mostly confirming what the designer already
verified by reading the script. Light implementation; low risk.

## Coordination

PR #1354 (`fix/issue-1295`, OPEN) also edits `op_init` — adds
`ensure_beads_role`. Orthogonal but shares the file.

Recommended sequence (per designer addendum):

1. **Wait for #1354 to land or stall.** If it lands within a day
   or two, our promotion PR rebases trivially.
2. **If #1354 stalls,** comment on the PR linking to `ga-5mym`
   describing the perf-fix promotion. Land independently; #1354
   then rebases (its change is a small addition that lifts easily
   over our patch).
3. **One PR for all three pieces** (patch + regression test + CI
   guard) — orthogonal but share the same context, easier for
   reviewers to load.

Cluster siblings under "supervisor lifecycle robustness" —
`ga-a3ry` (`supervisor-binary-stale-detection`) and `ga-r8iz`
(gc stop lenient validation). All three land independently.

## Routing rationale

Designer addendum is present in the bead notes — covers perf
budget rationale, CI grep-guard wording, regression test fixture
design (the bug only fires with an existing config to migrate;
fresh `t.TempDir()` won't reproduce), edge cases, the audit of
`ensure_types_custom_in_yaml` (already atomic per script reading),
and the validator acceptance checklist. No more design hops
needed. Routed to **builder** with `ready-to-build`.

## Acceptance criteria (rolled up)

- **Promotion PR opened** with `4ef8ee5e` rebased onto current
  `origin/main`. Coordinate with PR #1354 in the description.
- **Regression test added** at `cmd/gc/gc_beads_bd_init_perf_test.go`
  with `//go:build integration` and the recommended fixture
  (existing config + dolt + metadata; **not** a fresh `t.TempDir()`,
  which won't reproduce the migration trigger).
- **Wall-clock assertions:** fresh init `< 10 s`, short-circuit
  `< 3 s`. Production NFR targets are tighter (`< 5 s` / `< 1 s`)
  but the test budgets buy CI headroom.
- **`bd ready` follow-up assertion** within the same test as a
  free additional signal against indirect regressions.
- **CI grep guard** at `cmd/gc/gc_beads_bd_lint_test.go`. Regex:
  `^[[:space:]]*[^#].*bd[[:space:]]+config[[:space:]]+set` —
  whitespace-tolerant, skips comments. Two-line error message
  with the bead reference and the helper to use instead.
- **`ensure_types_custom_in_yaml` audited for atomic-write
  semantics.** Designer reading confirmed ✅; validator
  re-confirms by reading the file. No code change required for
  this bead beyond the audit.
- **No version-conditional code** anywhere in `op_init` or its
  helpers (`if bd_version >= "X"` is a regression).
- **`go.mod` minimum bd version** pinned to whatever ships the
  `GetCustomTypesFromYAML` fallback. Document the link in a
  comment near the helper.
- **Documentation updated** if any prose currently says "init can
  be slow" or similar (`grep -ri "30 ?s\|init.*timeout\|slow.*init" docs/`).

Full acceptance checklist is in the bead body's "Acceptance
checklist (for validator)" section.

## Risks and unknowns

- **Rebase conflict with PR #1354** — coordinate per the sequence
  above. Comment on the PR before opening the promotion PR so the
  other author isn't surprised.
- **`GetCustomTypesFromYAML` fallback removed in a future bd** —
  pin minimum version, watch CHANGELOG.
- **Regression test flakes on slow CI** — generous 10 s budget
  (vs. production NFR `< 5 s`). The slow-path bug was 60 s+; any
  regression is detectable at 10 s.
- **`ensure_types_custom_in_yaml` no-op-on-conflict semantics.**
  The helper today only writes when `types.custom` is absent;
  it doesn't merge. Designer flags this as a question for the
  builder: confirm acceptable for milestone:1.1.0 scope (the
  architect's design says "Idempotent: re-running never appends
  duplicates" — that's consistent with no-op).
- **Silent failure on `cat` in the helper** (line 319 returns 0
  on cat failure). Out of scope for this bead but builder should
  flag in the PR description for follow-up.

## Out of scope (explicit)

- Increasing the 30 s init timeout (treats symptom, not cause).
- Version-conditional code branches.
- Migrating off gc-beads-bd to a different beads provider.
- One-time migration command for already-broken cities.
- Detection of partially-initialized DBs (existing code already
  handles).
- Fixing the `cat` silent-failure path inside
  `ensure_types_custom_in_yaml` (separate follow-up).

## Validation gates

- `go test ./...` green (includes the new lint test).
- `go test -tags=integration ./cmd/gc/...` green (includes the
  new `gc_beads_bd_init_perf_test.go`).
- `go vet ./...` clean.
- Manual smoke (fresh user, existing city dir): `gc start` reaches
  ready in < 5 s.
- Manual smoke (same dir, second `gc start`): < 1 s short-circuit.
- Manual smoke (break the fix locally — re-add `bd config set
  issue_prefix`): the lint test fires with line number + bead
  reference.
- One `bd remember` entry from the builder when this lands so
  future maintainers know why `bd config set` is forbidden in
  `op_init`.
