# Plan: cmd/gc managed Dolt test teardown

> Status: decomposed - 2026-06-17
> Source bead: `ga-xxp9cd` (P1, bug)
> Prior art: `engdocs/plans/dolt-test-process-leak-cleanup.md`

## Context

On 2026-06-17 around 10:56 PT, the host hit a large OOM after 1,106
orphaned `dolt sql-server` processes accumulated over 5-6 days. The
orphan processes used configs under
`/var/tmp/gc-build-tmp/Test*/001/.gc/runtime/packs/dolt/dolt-config.yaml`,
which points to cmd/gc tests that correctly isolate Dolt state in temp
directories but do not reliably tear down the managed server process.

This is distinct from the closed `ga-w2kh1r` production-port leak: these
tests are not hitting production Dolt. The failure mode is process lifetime,
not endpoint isolation.

## PM Split

| ID | Title | Route | Depends on |
| --- | --- | --- | --- |
| `ga-xxp9cd.1` | test: Pin managed Dolt sql-server teardown for cmd/gc process-backed tests | `gascity/validator` | parent only |
| `ga-xxp9cd.2` | fix: Deterministically tear down managed Dolt servers spawned by cmd/gc tests | `gascity/builder` | `ga-xxp9cd.1` |
| `ga-xxp9cd.3` | test: Verify cmd/gc managed-Dolt leak fix across impacted test families | `gascity/validator` | `ga-xxp9cd.2` |

The dependency order enforces the repo's TDD rule: regression coverage first,
remediation second, final validation third.

## Acceptance

`ga-xxp9cd.1` is accepted when a regression test or helper check detects
leftover `dolt sql-server` processes scoped to the test run's temp config
path, follows the slow-process test boundaries in `TESTING.md`, and cannot
match production city or rig Dolt servers.

`ga-xxp9cd.2` is accepted when every cmd/gc test path that starts a managed
Dolt server under `t.TempDir()` registers deterministic cleanup tied to the
test lifetime; cleanup runs on success, failure, and panic; the process is
terminated and reaped; and the known impacted families complete without
leaving test-temp Dolt servers behind.

`ga-xxp9cd.3` is accepted when the validator records the exact verification
commands, confirms zero post-run test-temp `dolt sql-server` processes, and
confirms production/current city Dolt was not matched or terminated.

## Risks

- Slow process-backed tests must stay out of the fast unit path. `TESTING.md`
  routes this kind of coverage to the cmd/gc process-backed shard rather than
  the default fast `make test` loop.
- Cleanup must be scoped by spawned process ownership or test-temp config
  paths. Broad process-family kills are not acceptable.
- The older May plan includes defense-in-depth janitor work. This PM split
  does not expand `ga-xxp9cd` into a janitor/reaper feature; any such work
  should remain a separate product or architecture decision.

## Handoff

Validator owns the first and third beads. Builder owns the remediation bead.
All three children carry `source:actual-pm`, `dolt-test-leak`, and
`pm.root=ga-xxp9cd`; each child has `gc.routed_to` set to its target agent.
