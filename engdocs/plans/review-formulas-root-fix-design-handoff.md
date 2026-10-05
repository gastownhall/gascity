# Plan: review-formulas root-fix design handoff

> Owner: `gascity/pm` - Created: 2026-06-17
> Sources: design beads `ga-hm6t73`, `ga-8yv2fi`, `ga-lqy77a`;
> architecture root `ga-jtjaiy`

## Goal

Convert the completed review-formulas root-fix design work into builder
packages that reduce hook/reconciler latency under busy CI runner load.

The work has two root fixes and one temporary unblock marker:

- WP-A adds an opt-in `GC_BD_PROBE_TIMEOUT` override so integration tests
  can use a shorter bd subprocess probe timeout without changing the
  production default.
- WP-B changes the synthesized default `scale_check` to use
  `bd ready --limit=1`, preserving the yes/no semantics while avoiding a
  full ready-query scan.
- WP-C adds a temporary `repo-policy.py` known-flake marker for
  `Integration / review-formulas`, tied directly to WP-A/WP-B removal
  conditions.

## Work breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-hm6t73.1` | Build GC_BD_PROBE_TIMEOUT override for bdProbeTimeout | `gascity/builder` | `ready-to-build` |
| `ga-8yv2fi.1` | Build default scale_check bd ready limit | `gascity/builder` | `ready-to-build` |
| `ga-lqy77a.1` | Build temporary review-formulas known-flake marker | `gascity/builder` | `ready-to-build` |

## Relationship graph

```text
ga-hm6t73.1 (WP-A root fix)
  <relates-to> ga-lqy77a.1 (WP-C temporary marker)

ga-8yv2fi.1 (WP-B root fix)
  <relates-to> ga-lqy77a.1 (WP-C temporary marker)
```

WP-C is intentionally related rather than blocked on WP-A/WP-B. The marker
is useful only while the root fixes are active; blocking it until root fixes
close would defeat the temporary-unblock purpose.

## Acceptance summary

### `ga-hm6t73.1`

1. TDD covers unset, valid, below-floor, and invalid
   `GC_BD_PROBE_TIMEOUT` values.
2. The production default remains `180 * time.Second`.
3. Valid Go duration strings are accepted.
4. Values below 5s are floored to 5s with the specified stderr warning.
5. `test/integration/review_formula_test.go` sets
   `GC_BD_PROBE_TIMEOUT=30s` for test cities.
6. `go test ./...` and `go vet ./...` pass.

### `ga-8yv2fi.1`

1. The synthesized default pool `scale_check` appends `--limit=1` to
   `bd ready`.
2. Custom operator-provided `scale_check` commands are unchanged.
3. The consuming code still treats non-zero output as "work present",
   not an exact worker count.
4. Existing scale-check tests pass, including the current equivalent of
   `go test ./cmd/gc/... -run TestScaleCheck`.
5. `go test ./...` and `go vet ./...` pass.

### `ga-lqy77a.1`

1. `repo-policy.py` is created at repo root.
2. It contains exactly one `known_flake_check_prefixes` entry:
   `Integration / review-formulas`.
3. The comment above the entry includes `root-fix: ga-jtjaiy` and the
   removal condition referencing `ga-hm6t73.1` or `ga-8yv2fi.1`.
4. The PR description references `ga-jtjaiy`, states the removal
   condition, and includes or links the removal PR plan.
5. The work does not wire `repo-policy.py` into CI gate scripts.

## Handoff notes

- No additional design, architecture, or validator bead is needed from
  this handoff.
- WP-A and WP-B are root fixes; WP-C is a temporary marker and must not
  become permanent policy.
- The marker must not land as a standalone process patch without active
  WP-A/WP-B tracking.

## Risks

- Treating `scale_check` output as an exact count after `--limit=1` would
  under-scale pools. Builder must verify the yes/no contract at the
  consumer.
- A too-low `GC_BD_PROBE_TIMEOUT` could create noisy false negatives, so
  the 5s floor and stderr warning are part of the requirement.
- A known-flake marker without a removal condition would permanently mask
  CI signal. The root-fix and removal comments are mandatory.
