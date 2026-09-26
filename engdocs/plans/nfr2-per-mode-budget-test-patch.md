# Plan: NFR-2 per-mode budget test patch (direct=5s, systemd=15s)

> **Status:** decomposing — 2026-05-11
> **Bug parent:** `ga-6wuo77` — `TestStartDrift_SystemdManaged_
> RestartsToNewBuildID` flaked under a single NFR-2 5s budget; observed
> 11.3s on a clean dev box (Fedora 44, systemd 259). Root cause: a
> systemctl `--user restart` adds DBus dispatch + unit-FSM transition +
> ExecStop wait on top of the bootstrap floor that direct-mode pays
> alone. The 5s budget tracks direct-mode restart cost end to end; it
> does not track systemd-managed restart.
> **Architect spec:** `ga-6wuo77` body — split NFR-2 into per-mode
> budgets (direct=5s, systemd=15s) and fix a co-located cosmetic
> else-branch bug at both sites.
> **Designer spec:** `ga-5n1efb` pins the verbatim Go source patch
> for `test/integration/start_drift_test.go` lines 101-110 and
> 157-164: per-mode label in format strings, `const budget` at the
> assertion site (NOT package-level), 5-line systemd-mode comment
> citing the latency floor.
> Architect builder estimate: ~25 LOC test only, 1 PR.
> **Designer handoff mail:** `gm-44j4ip` (2026-05-11).
> **Decomposed into:** 1 builder bead — `ga-ng06n5`.

## Context

NFR-2 was introduced in commit `0e04c195` (originating bead
`ga-xbgq`) with a single 5s budget covering both restart modes. The
direct-mode test happily passed: a kill+exec is bounded by kernel
signal latency + binary startup, both of which are sub-second on a
healthy dev box. The systemd-managed-mode test, on the other hand,
goes through `systemctl --user restart`, which dispatches over DBus
to the user-mode service manager, transitions the unit through the
inactive → activating → active FSM, blocks on `ExecStop`, and only
then issues `ExecStart`. On a clean dev box that floor sits around
11.3s — leaving the 5s budget routinely violated and the test
flaky.

The architect's decision (`ga-6wuo77`):

| Restart mode      | Budget | Rationale                                  |
|-------------------|--------|--------------------------------------------|
| direct (kill+spawn) | **5s** (unchanged) | kernel signal + exec; no service-manager hop |
| systemd-managed   | **15s** | DBus + unit FSM + ExecStop wait + ExecStart dispatch; ~1.3x headroom over observed 11.3s; ≥30s still trips on real regressions |

A co-located cosmetic bug: at both assertion sites (`:107` and
`:161`), `t.Logf("NFR-2 OK ...")` runs unconditionally after
`t.Errorf` — which means a failing test prints "OK" alongside the
"violated" line. Move the OK log into an `else` branch so it only
fires when the budget held. Bundle the fix in the same PR.

Production `driftReadyTimeout = 5 * time.Second` at
`cmd/gc/cmd_start_drift.go:178` is a different quantity — a
PollReady cap inside the supervisor, not the total wall-clock
asserted by the test. It stays at 5s for both modes.

## Plan

One builder bead. The designer's pin is the verbatim two-block
source patch plus a pinned format-string table and five-line systemd
comment template. Mechanical edit.

| Builder bead | PR shape | Files (key) |
|---|---|---|
| **Per-mode budget split + cosmetic else fix** | One PR, ~25 LOC test only | `test/integration/start_drift_test.go` (lines 101-110 + 157-164) |

### `ga-ng06n5` — NFR-2 per-mode budget test patch (P3, `ready-to-build`)

Implements the per-mode budget split from `ga-6wuo77` per the
verbatim source patch in `ga-5n1efb`. Routed to `gascity/builder`
via `gc.routed_to`; `gc.design_parent=ga-5n1efb` records the
back-link.

**Blocked by:** none. Independent test-only patch.

**Acceptance criteria summary** (full list in the bead body):

- Lines 101-110 replaced with the direct-mode patch (Patch 1).
- Lines 157-164 replaced with the systemd-mode patch (Patch 2).
- `cmd/gc/cmd_start_drift.go` unchanged. Verify via `git diff`.
- Format strings match the pinned per-mode-labelled forms (NOT the
  pre-existing single-format strings):
  - `"NFR-2 violated (direct): restart took %s (>%s)"`
  - `"NFR-2 violated (systemd-managed): restart took %s (>%s)"`
  - `"NFR-2 OK (direct): restart took %s (budget %s)"`
  - `"NFR-2 OK (systemd-managed): restart took %s (budget %s)"`
- No bare `>5s` / `>15s` literals; budget interpolated via `%s`.
- `const budget = N * time.Second` declared **inside** each
  assertion block — NOT package-level, NOT shared.
- Systemd comment names the latency floor (DBus + unit-FSM +
  ExecStop wait), observed 11.3s on Fedora 44 / systemd 259, ~1.3x
  headroom, ≥30s regression trip-point. Five lines. Cites
  `ga-6wuo77`.
- `requireUserSystemd(t)` skip guard unchanged.
- `pollHealthBuildID(t, ..., driftReadyTimeout)` call site
  unchanged — `driftReadyTimeout` stays 5s for both modes.
- Search-and-update: `git grep "NFR-2 violated:"` and `git grep
  "NFR-2 OK:"` across docs and engdocs. Log scrapers and triage
  prompts that grep on the old single-format strings must be
  updated as part of the same PR.
- `go vet ./...` clean.
- `go test -tags integration -run TestStartDrift
  ./test/integration/...` passes BOTH tests on a clean dev box.

**HARD RULES carried from design (bead body §"HARD RULES" — 6 items):**

- No package-level budget `const` / `var`.
- No bare `5s` / `15s` literals in format strings.
- Old strings are REPLACED, not augmented (log-scraper search is
  mandatory).
- Do NOT touch `cmd/gc/cmd_start_drift.go:178`.
- Do NOT add `t.Skip` to the systemd test on slow runners.
- Five-line systemd comment, exactly.

## Routing

- Builder bead `ga-ng06n5` carries `gc.routed_to=gascity/builder`
  and label `ready-to-build`. Builder will see it via the Tier-3
  pool-queue query.
- `gc sling gascity/builder ga-ng06n5` wakes the builder session
  immediately after handoff.
- Mail to builder via `gc mail send gascity/builder` with subject
  pointing at the bead ID for context.

## Risk / non-goals

- The slice does NOT modify `cmd/gc/cmd_start_drift.go:178`. The
  production `driftReadyTimeout` is a PollReady cap (a different
  quantity than the wall-clock asserted by the test); architect §3
  is explicit it stays 5s.
- The slice does NOT skip the systemd test on slow runners.
  Skipping removes the regression signal.
- The slice does NOT change the unit-name validator at
  `setupDriftSystemdScenario` / `requireUserSystemd`.

The 15s budget tracks the observed floor + 1.3x headroom. If
future dev-box latency drifts past 15s, the architect spec is the
right place to revisit — not the test.
