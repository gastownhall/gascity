# Managed Dolt Telemetry Suppression Plan

Source bead: `ga-um72h`
Architecture parent: `ga-6g6z7`
Priority: P2

## Goal

Stop gc-managed Dolt SQL server processes from accumulating telemetry
events under `~/.dolt/eventsData/` during long-running city operation.

## Work Packages

1. `ga-um72h.1` — Tests: managed Dolt telemetry suppression is covered
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Acceptance: cover managed provider env injection, provider guard
     boundaries, and best-effort persistent `metrics.disabled` setup.

2. `ga-um72h.2` — As an operator, managed Dolt processes inherit telemetry-flush suppression
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-um72h.1`
   - Acceptance: append `DOLT_DISABLE_EVENT_FLUSH=1` inside the managed
     bd provider env path without changing external/file provider behavior.

3. `ga-um72h.3` — As an operator, managed Dolt startup persists metrics.disabled best-effort
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Depends on: `ga-um72h.2`
   - Acceptance: run `dolt config --global --add metrics.disabled true`
     before managed bd provider startup, guarded to the managed path and
     non-fatal on failure.

## Dependency Graph

`ga-um72h.1` blocks `ga-um72h.2`, which blocks `ga-um72h.3`.

## Guardrails

- Do not affect external or file-based beads providers.
- Do not fail provider startup if the persistent Dolt config write fails.
- Keep the change inside controller/provider lifecycle infrastructure.
- Do not introduce role-name logic or status-file tracking.
