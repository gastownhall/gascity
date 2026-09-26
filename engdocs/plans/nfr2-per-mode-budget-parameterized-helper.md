# Plan: NFR-2 per-mode restart budgets with parameterized helper

> Status: PM handoff ready, 2026-05-19
> Design bead: `ga-3jqzk`
> Architecture decision: `ga-3uzwp`
> Builder bead: `ga-06grr`

## Summary

Implement the refreshed NFR-2 integration-test budget decision for
`test/integration/start_drift_test.go`.

The previous direct-mode 5s implementation plan is stale. The binding decision
is now:

| Mode | Integration test budget |
|---|---:|
| direct | 10s |
| systemd-managed | 15s |

The builder should replace the shared restart-ready budget helper with a
parameterized helper that receives the budget and mode from each call site.

## Scope

One builder bead, one implementation file:

| Bead | Target | File |
|---|---|---|
| `ga-06grr` | `gascity/builder` | `test/integration/start_drift_test.go` |

Required changes:

- Replace `driftRestartReadyBudget = 15 * time.Second` with
  `driftDirectRestartBudget = 10 * time.Second` and
  `driftSystemdRestartBudget = 15 * time.Second`.
- Replace the direct test call with
  `assertRestartDuration(t, out, driftDirectRestartBudget, "direct")`.
- Replace the systemd test call with
  `assertRestartDuration(t, out, driftSystemdRestartBudget, "systemd-managed")`.
- Delete `assertRestartReadyDuration`.
- Add `assertRestartDuration(t, out, budget, mode)` exactly as pinned in
  `ga-3jqzk`, including the else-guarded OK log.

## Acceptance

- Only `test/integration/start_drift_test.go` changes.
- `driftReadyTimeout` remains unchanged.
- `cmd/gc/cmd_start_drift.go` remains unchanged.
- The old generic NFR-2 format strings are gone.
- New NFR-2 logs include the mode name: `direct` or `systemd-managed`.
- The direct integration-test budget is 10s, not 5s.
- The systemd integration-test budget remains 15s.

## Verification

Builder runs:

```bash
go test -tags integration -run TestStartDrift ./test/integration/...
go vet ./...
git diff --check origin/main...HEAD
git grep "NFR-2 violated:\\|NFR-2 OK:" docs/ engdocs/ test/
```

The grep confirms no stale old-format scraper references remain outside the
new helper strings.

## Risks

- The older plan `engdocs/plans/nfr2-per-mode-budget-test-patch.md` describes a
  direct 5s assertion and is superseded for this slice.
- Loaded CI may still vary, but architecture chose 10s as the current direct
  integration budget. Any further widening should go back through architecture,
  not builder-local judgment.
