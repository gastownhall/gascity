# Native Store Dolt Mode On-Ramp PM Plan

Root bead: `ga-yqn5py`
Design handoffs: `ga-yqn5py.1`, `ga-yqn5py.2`

## Goal

A default `go build ./cmd/gc` user should get the native Dolt store when gc
manages a Dolt SQL server. The fix bridges gc's server-mode knowledge into
`.beads/config.yaml` so `bd context --json` reports `dolt_mode: "server"` and
the existing native-store preflight gate can pass without user configuration.

## Work Packages

| Bead | Story | Route | Depends on |
| --- | --- | --- | --- |
| `ga-yqn5py.1.3` | As a maintainer, I can verify canonical config writes and preserves `dolt.mode` | `needs-tests` -> `gascity/validator` | none |
| `ga-yqn5py.1.4` | As an operator, canonical config records Dolt server mode when supplied | `ready-to-build` -> `gascity/builder` | `ga-yqn5py.1.3` |
| `ga-yqn5py.2.3` | As an operator, I can verify managed city Dolt reaches `bd context` as server mode | `needs-tests` -> `gascity/validator` | `ga-yqn5py.1.4` |
| `ga-yqn5py.2.4` | As a city operator, gc marks managed and resolved Dolt configs as server mode | `ready-to-build` -> `gascity/builder` | `ga-yqn5py.1.4`, `ga-yqn5py.2.3` |
| `ga-yqn5py.4` | As an operator, I can diagnose a persistent `dolt_mode_safe` gate failure | `ready-to-build` -> `gascity/builder` | `ga-yqn5py.2.4` |

## Acceptance Summary

- `ConfigState` carries `DoltMode` as a peer to the Dolt host, port, and user
  fields.
- `EnsureCanonicalConfig` writes non-empty `DoltMode` to `dolt.mode` and leaves
  existing `dolt.mode` untouched when the caller does not know the mode.
- Regression coverage pins write, idempotence, preserve-on-empty, and scrub-list
  behavior.
- Managed city Dolt is covered even when host and port are empty in
  `ConfigState`; this is the designer-identified gap in the host/port-only
  condition.
- The existing `checkDoltModeSafe` gate remains authoritative. The work feeds it
  correct config data rather than weakening it.
- Troubleshooting docs explain what `dolt_mode_safe` means, what gc now writes
  automatically, and the repair path for old or drifted cities.

## Dependency Order

`ga-yqn5py.1.3` -> `ga-yqn5py.1.4` -> `ga-yqn5py.2.3` -> `ga-yqn5py.2.4` -> `ga-yqn5py.4`.

This keeps the TDD path explicit: contract coverage first, canonical config
write second, managed-city chain coverage third, caller/on-ramp fix fourth, and
docs after behavior is settled.

## Risks

- The original architecture condition mentioned non-empty host or port, but the
  design handoff found managed-city origin can require server mode while host
  and port remain empty. The implementation package states that outcome as
  acceptance criteria and leaves exact code placement to builder.
- The current checkout may already contain related `DoltMode` references. The
  downstream agents should verify current behavior and avoid duplicate edits.

## Out Of Scope

- No changes to `checkDoltModeSafe`.
- No call-rate or #3550 work.
- No platform-specific darwin-only path.
- No changes to non-Dolt, postgres, doltlite, or file-backed scope behavior.
