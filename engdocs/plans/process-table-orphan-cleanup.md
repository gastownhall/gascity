# Process-Table Orphan Cleanup

Date: 2026-05-31

## Goal

Prevent duplicate agent runtimes for the same `GC_SESSION_ID` by adding an
optional process-table scanner capability, using it at session spawn time, and
sweeping untracked runtimes during reconciliation.

Source design beads:

- `ga-21uqyd` - test coverage and integration guard
- `ga-qmbisj` - runtime scanner contract and fake seam
- `ga-9nzf62` - `internal/runtime/proctable`
- `ga-prvthv` - `Manager.killExistingOrphans`
- `ga-r3jh5o` - reconciler orphan sweep
- `ga-gh892d` - tmux and subprocess provider scanner implementations

Tracker import: no external tracker skill was installed for this rig, so no
tracker issues were imported.

## Work Packages

| Bead | Route | Purpose | Acceptance summary |
| --- | --- | --- | --- |
| `ga-21uqyd.1` | `gascity/validator` | Author the failing guardrail tests first. | Covers fake scanner conformance, manager cleanup-before-start, non-scanner no-op, reconciler sweep cases, proctable root detection/PID safety, provider scanner behavior, and integration cleanup with `//go:build integration`. |
| `ga-qmbisj.1` | `gascity/builder` | Define `LiveRuntime`, optional `ProcessTableScanner`, and the fake seam. | Does not change `runtime.Provider`; initializes `OrphanedRuntimes`; fake scanner methods use tracked session `GC_SESSION_ID`; compile-time conformance is present. |
| `ga-9nzf62.1` | `gascity/builder` | Add `internal/runtime/proctable`. | Exposes `ScanBySessionID`, `ScanAll`, and `KillByPID`; implements root detection; handles per-process ENOENT/EACCES safely; rejects PID 0/1; stays below provider/session layers. |
| `ga-prvthv.1` | `gascity/builder` | Enforce single runtime before session starts. | Adds `Manager.killExistingOrphans`; calls it before every `m.sp.Start` in manager/chat paths with bead ID, not provider session name; scan errors do not abort spawn. |
| `ga-r3jh5o.1` | `gascity/builder` | Sweep process-table orphans during reconciliation. | Runs after `reapRuntimesBoundToClosedBeads`; terminates untracked runtimes whose bead is closed or absent; skips tracked/open-bead runtimes; logs errors to stderr and returns reaped count. |
| `ga-gh892d.1` | `gascity/builder` | Implement scanner support in tmux and subprocess providers. | Uses `proctable`; fills `ProviderName`/`IsTracked` from provider registries; avoids holding subprocess mutex across scan/kill; keeps k8s deferred and pass-through providers unchanged. |

## Dependency Order

1. `ga-21uqyd.1` is first so validator can author tests before implementation.
2. `ga-qmbisj.1` depends on `ga-21uqyd.1`.
3. `ga-9nzf62.1`, `ga-prvthv.1`, and `ga-r3jh5o.1` depend on `ga-qmbisj.1`.
4. `ga-gh892d.1` depends on `ga-9nzf62.1`.

This order keeps the optional runtime contract as the central unblocker, lets
session and reconciler work proceed after the contract exists, and waits to wire
tmux/subprocess providers until the shared scanner package exists.

## Risks

- The integration test can leave live processes behind if cleanup is not tied to
  `GC_SESSION_ID`; validator acceptance requires cleanup by session ID.
- A one-level process scan can target inherited child commands; root detection
  must compare the parent environment for the same `GC_SESSION_ID`.
- Spawn-time scan failures must not strand work in start-pending state; builder
  acceptance requires log-and-continue behavior.
