# Plan — Pool session isolation hardening

**Bead:** `ga-ighomh`
**Rig:** `gascity`
**Source:** `ga-onhj2l`

The pooled-session binding defect caused multiple live agents to operate in one
working directory and contributed to confirmed loss of uncommitted work. The
narrow identity fix shipped in PR #4659; this plan adds independent safety and
observability layers so the same hazard cannot return silently.

## Outcome

An operator can enable a session pool only when each instance resolves to an
isolated working directory. Session start and respawn refuse an occupied
directory, and session views report the directory used by the live process.

The behavior applies to every agent and rig without role-specific logic. Gas
City and Tincan pool configurations provide the live acceptance evidence.

## Work packages

| Bead | User outcome | Route | Blockers |
| --- | --- | --- | --- |
| `ga-ighomh.1` | A live session cannot start or respawn in another live session's working directory. | `gascity/builder` | None |
| `ga-ighomh.2` | An unsafe pooled-agent configuration is rejected before replicas start. | `gascity/builder` | None |
| `ga-ighomh.3` | CLI and API session views show each running process's actual working directory. | `gascity/builder` | None |

The three packages are intentionally parallel and independently shippable.
Parent-child links connect them to `ga-ighomh`; no artificial blocker edges are
added.

Each bead contains its full measurable acceptance criteria. In summary:

- `ga-ighomh.1` covers collision detection, fail-closed process-state errors,
  registered typed events, reuse of the existing bounded cwd scan, and
  start/respawn regression tests.
- `ga-ighomh.2` covers effective-config validation, constant and
  per-instance-varying templates, layered configuration, actionable errors,
  and Gas City/Tincan pool evidence.
- `ga-ighomh.3` covers live cwd precedence, stored-metadata fallback, CLI/API
  consistency, pooled-session display regressions, and Gas City/Tincan live
  evidence.

## Delivery constraints

- Preserve the merged per-slot identity fix from PR #4659. These packages
  harden and expose that behavior; they do not rebuild it.
- Rebase-check before integration because the in-flight PR #4501/#4551 stack
  touches nearby session lifecycle and CLI code. That stack addresses
  work-item target scope and does not satisfy these packages.
- Keep process inspection in the side-effect-owning session/runtime layer and
  route session lifecycle operations through the worker boundary.
- Reuse the fail-closed live-cwd scan introduced by `fd63422bd`; do not add a
  second host-process enumeration mechanism.
- Register payloads for every new event and keep CLI, HTTP, SSE, OpenAPI, and
  generated dashboard types on typed wire paths.
- Do not repair already-poisoned live session metadata or recycle active
  sessions as part of these code packages. Fleet remediation remains an
  operator action after the fixed binary is deployed.

## Verification

Builders follow test-first development and record happy-path and edge-case
coverage in each child bead. Every package runs its relevant fast test shards
and `go vet ./...`. Changes to API, OpenAPI, event, or dashboard surfaces also
run `make dashboard-check` and serve the dashboard locally.

Cross-rig acceptance is not complete from unit tests alone. After deployment,
the evidence must show that:

1. Gas City and Tincan pooled instances resolve to distinct live process cwd
   values.
2. `gc session list` and any corresponding API view match those live values.
3. A deliberately colliding configuration or start attempt is rejected and
   produces the typed event required by `ga-ighomh.1`.

## Risks and coordination

Fail-closed validation may expose existing unsafe pack or city configuration
immediately. The error must identify the affected agent configuration so the
operator can correct it before restoring pool capacity.

The live cwd guard runs on lifecycle paths, so its work must remain bounded to
known session PIDs. A host-wide process walk would turn a safety check into a
reconcile-time scalability risk.

Cross-rig verification depends on the fixed binary reaching both rigs. A code
merge without deployment evidence does not satisfy the Tincan acceptance
criteria.
