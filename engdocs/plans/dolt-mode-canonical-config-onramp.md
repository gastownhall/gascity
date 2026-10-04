# Dolt Mode Canonical Config On-Ramp Plan

Owner: `gascity/pm`
Created: 2026-06-24
Root beads: `ga-yqn5py.1`, `ga-yqn5py.2`
Source: designer contracts for #3702 Slice 1 and Slice 2

## Goal

Make Gas City write `dolt.mode: server` when the SDK knows a Dolt server is in
play, including managed city Dolt on Darwin where host and port may be empty in
the canonical state. This prevents `bd context` from falling back to embedded
mode and lets the native store preflight see the intended server-mode setup.

Tracker import was a no-op in this session because no visible tracker import
helper was installed.

## Children

| ID | Parent | Target | Purpose | Depends on |
| --- | --- | --- | --- | --- |
| `ga-yqn5py.1.1` | `ga-yqn5py.1` | `gascity/builder` | Verify canonical config writes and preserves `dolt.mode`. | - |
| `ga-yqn5py.1.2` | `ga-yqn5py.1` | `gascity/builder` | Add `ConfigState.DoltMode` and write non-empty values through `EnsureCanonicalConfig`. | `ga-yqn5py.1.1` |
| `ga-yqn5py.2.1` | `ga-yqn5py.2` | `gascity/builder` | Ensure managed, external, and rig endpoint states set server mode when appropriate. | `ga-yqn5py.1.2` |
| `ga-yqn5py.2.2` | `ga-yqn5py.2` | `gascity/builder` | Verify the managed-Dolt preflight chain sees `dolt_mode == "server"` end to end. | `ga-yqn5py.2.1` |

All child beads are labeled `ready-to-build` and `source:actual-pm`, with
`gc.routed_to` set to `gascity/builder`.

## Acceptance Rollup

The package is complete when:

- `ConfigState` has a documented `DoltMode string` field grouped with the
  other Dolt endpoint fields.
- `EnsureCanonicalConfig` writes non-empty `DoltMode` values to
  `dolt.mode`, trims surrounding whitespace, and preserves existing
  `dolt.mode` when `DoltMode` is empty.
- Contract tests cover first write, idempotent server write, empty-value
  preservation, and the difference between config key `dolt.mode` and metadata
  key `dolt_mode`.
- Managed city Dolt sets `DoltMode: "server"` even when `DoltHost` and
  `DoltPort` are empty.
- External or explicit Dolt server state sets server mode when host or port is
  present.
- Rig endpoint state propagates or derives server mode consistently with the
  city endpoint state.
- A focused end-to-end regression observes `bd context --json` reporting
  `dolt_mode == "server"` and `PreflightChecker.Check` remaining eligible for
  the managed server-mode case when other eligibility inputs are satisfied.
- A negative case keeps `DoltMode` empty and does not force `dolt.mode` for a
  scope with no managed Dolt and no host or port.

## Dependency Graph

```text
ga-yqn5py.1.1
  -> ga-yqn5py.1.2
      -> ga-yqn5py.2.1
          -> ga-yqn5py.2.2
```

Slice 2 depends on Slice 1 because endpoint-state constructors or call sites
cannot populate `DoltMode` until the canonical config contract exists.

## Risks

- If builder finds that the constructor-vs-call-site placement needs a new
  architectural rule, builder should stop and route a `needs-architecture`
  bead instead of widening the implementation.
- End-to-end coverage may need a live managed-Dolt fixture. Builder should use
  existing test helpers and avoid unmanaged background processes.
- The negative case is load-bearing: empty `DoltMode` means caller does not
  know the mode, not that existing `dolt.mode` should be deleted.

## Out of Scope

- Do not add a new storage backend.
- Do not change native embedded build-tag behavior.
- Do not add a new exported API unless required by existing package patterns.
- Do not change role or agent behavior.
