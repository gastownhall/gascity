# Coordstore Steady-State Workload Design Handoff Plan

Source bead: `ga-sftyt`
Design source: `source:actual-designer`
Existing build bead: `ga-w08fz`
Target agent: `gascity/investigator`
Priority: P1 gate for the R4 coordstore evaluation

## Goal

Feed the completed steady-state workload design into the already in-progress
coordstore lifecycle build without creating a parallel implementation lane. The
design changes the benchmark from create-only data growth to realistic churn:
main records close, wisps delete, and expired records purge during the soak.

## Work Package

1. `ga-w08fz` - Investigator: apply steady-state workload lifecycle design
   - Route: `gascity/investigator`
   - Label: existing in-progress implementation bead
   - Source design: `ga-sftyt`
   - Acceptance: `WorkloadConfig` adds `CloseRate`, `WispDeleteRate`, and
     `PurgeExpiredRate`; `RealWorldWorkload` and `SmokeWorkload` define churn
     rates while `StressWorkload` leaves them zero; runner schedule includes
     guarded close, wisp delete, and purge ops; `execOp` implements each churn
     op with seed locks, runtime floors, swap-remove, and expected
     `IsNotFound` race handling; `opName` reports the new operations; smoke or
     focused workload verification shows wisp population remains bounded; `go
     vet ./...` and focused coordstore tests pass.

## Dependency Graph

- `ga-sftyt` is the design handoff feeding `ga-w08fz`.
- No new builder bead was created because `ga-w08fz` is already assigned and
  in progress.

## Guardrails

- Keep all workload changes inside `internal/benchmarks/coordstore/` unless the
  in-progress implementation proves a directly necessary adjacent change.
- Do not edit the workload design from PM; implementation questions belong on
  `ga-w08fz` or back to architecture.
- Keep seed mutations under `seedMu` and do not expose mutable seed state
  directly.
- Preserve the zero-hardcoded-role invariant.
