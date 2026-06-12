# Runtime Provider Requirements

| Field | Value |
|---|---|
| Status | Seed draft |
| Scope | Runtime provider selection, contract, and protocol behavior source of truth, stored beside `internal/runtime` |
| Related design | `engdocs/design/runtime-provider-packs.md` owns the runtime-packs direction (epic `ga-1symz6`). `docs/reference/exec-session-provider.md` is the exec/RPP protocol reference. |

This document is the reconciliation ledger for runtime provider behavior:
how a session runtime is selected, what every provider must guarantee, and
the wire protocol spoken to out-of-process providers. Future agents should
use it as the frame for runtime changes: expected behavior, implementation,
and tests must be brought back into agreement whenever any one of them
changes.

## Purpose

A runtime provider starts, stops, observes, and addresses agent sessions on
a substrate (tmux, subprocess, Kubernetes, a remote bridge, a pack-shipped
executable). Providers are transport, not policy: session lifecycle policy
is bead-backed and lives in `internal/session` (see its `REQUIREMENTS.md`);
providers report runtime facts and execute commands. The runtime-packs
initiative (`ga-1symz6`) requires that providers be updatable without a new
`gc` release, which makes the selection registry and the wire protocol —
not Go types — the load-bearing contracts.

## How To Reconcile

For every runtime provider change:

1. Read this document and the nearest `AGENTS.md`.
2. Identify the scenario rows affected by the change.
3. Update code, tests, and this document so they describe the same behavior.
4. Add a new scenario row for any new behavior or bug fix; move a row out of
   Planned only together with the tests that prove it.
5. Cite proof in the row: a test path, source path, issue, commit, or command.

If a row is wrong but tests currently enforce it, update the row and tests in
the same change. If a row describes the right product behavior but code
differs, fix code and prove the row with a test.

## Canonical Vocabulary

- **Runtime provider** — an implementation of `runtime.Provider`
  (`internal/runtime/runtime.go`). Distinct from an **agent provider**
  (claude/codex/… CLI spec, `internal/worker/builtin`) and from a
  **service** (`[[service]]`, controller-supervised process).
- **Selection name** — the string that picks a runtime: `GC_SESSION`,
  `city.toml [session].provider`, or a per-agent `session = "<name>"`
  transport override. Examples: `tmux`, `subprocess`, `acp`,
  `exec:/path/to/script`.
- **Registry** — `internal/runtime/registry`; maps selection names to
  **factories** (constructors). Resolution: exact name → longest
  registered prefix → fallback.
- **RPP (Runtime Provider Protocol)** — the versioned wire contract with
  out-of-process providers. RPP v0 is the exec contract:
  `executable <op> <args…>` with payloads on stdin/stdout.
- **Op** — one provider operation crossing the protocol boundary
  (`start`, `stop`, `is-running`, `nudge`, …).
- **Runtime pack** — a pack that ships or installs a runtime executable
  and declares it under a selection name (planned, `ga-h504e5`).

## Global Invariants

- **The contract package is stdlib-only.** The root `internal/runtime`
  package imports nothing outside the Go standard library, so providers
  and the conformance suite never drag the SDK with them. Exemption:
  `staging.go` (imports `internal/overlay`) until staging is relocated.
  Enforced by `internal/runtime/import_boundary_test.go`.
- **Selection is data.** Runtime choice is config-supplied strings
  resolved through the registry. No role names, no judgment calls in Go
  (ZFC); adding a runtime must not require editing a dispatch site.
- **Registration collisions are errors.** A later registration (e.g. a
  pack-declared runtime) must never silently shadow an earlier one.
- **Providers report observations, not durable truth.** Session state is
  bead-backed in `internal/session`; `IsRunning`/`ProcessAlive`/`Peek`
  are runtime facts that reconciliation interprets.
- **Protocol forward compatibility.** An out-of-process provider that
  does not implement an op exits 2 and the op is treated as a no-op
  success. New ops must be optional for existing executables.
- **Pack-shipped runtime executables are city-trusted code**, the same
  trust tier as pack `commands/`, `doctor/`, and services.
- **Delivery independence (governing requirement, `ga-1symz6`).**
  Updating a runtime pack must never require rebuilding or re-releasing
  `gc`. Anything that couples provider behavior to compiled-in Go code
  must either stay deliberately builtin (tmux, fake) or move behind RPP.

## Scenario Ledger

### Selection And Registry

| ID | Scenario | Required behavior | Evidence |
|---|---|---|---|
| RUNTIME-SEL-001 | Selection source order | The session provider name resolves `GC_SESSION` env var first, then `city.toml [session].provider`, then default (empty name → tmux fallback). | `cmd/gc/providers.go` (`sessionProviderContextForCity`); `cmd/gc/providers_test.go` `TestSessionProviderContextForCityUsesTargetCityAndEnvOverride` |
| RUNTIME-SEL-002 | Builtin selection names | `fake`, `fail`, `subprocess`, `acp`, `t3bridge`, `cloudflare`, `k8s`, `hybrid` are registered builtin runtimes; removing a name is a breaking config change. | `cmd/gc/runtime_registry.go`; `cmd/gc/runtime_registry_test.go` `TestRuntimeRegistryRegistersAllBuiltinNames` |
| RUNTIME-SEL-003 | Registry resolution order | Exact name beats prefix; the longest matching prefix wins; otherwise the fallback runs; with no fallback the error is `ErrUnknownRuntime` naming the selection. Factory errors propagate wrapped with the selection name. | `internal/runtime/registry/registry.go`; `internal/runtime/registry/registry_test.go` |
| RUNTIME-SEL-004 | `exec:` prefix | `exec:<script>` selects the exec provider for `<script>` (absolute, relative, or PATH-resolved). The prefix factory receives the full selection name and owns parsing. | `cmd/gc/runtime_registry.go`; `cmd/gc/runtime_registry_test.go` `TestNewSessionProviderByName_ExecPrefixUsesExecProvider` |
| RUNTIME-SEL-005 | Legacy t3bridge exec alias | An `exec:` script whose basename is `gc-session-t3` maps to the native t3bridge provider, not the exec provider. | `cmd/gc/providers.go` (`isLegacyT3BridgeExecScript`); `cmd/gc/providers_test.go` `TestNewSessionProviderByName_LegacyExecT3BridgeStillMapsNative` |
| RUNTIME-SEL-006 | Unknown name falls back to tmux | Any unregistered selection name (including empty) constructs the tmux provider with the city's session config. Pack runtimes (`ga-h504e5`) must register before resolution so they are never swallowed by this fallback. | `cmd/gc/runtime_registry_test.go` `TestNewSessionProviderByName_UnknownNameFallsBackToTmux` |
| RUNTIME-SEL-007 | Registration collisions rejected | Duplicate exact names, duplicate prefixes, blank names, malformed prefixes (no trailing `:`), and nil factories are registration errors. | `internal/runtime/registry/registry_test.go` |
| RUNTIME-SEL-008 | Per-city provider state isolation | Socket/state-based providers (`subprocess`, `acp`) derive their state directory from the provider name plus a hash of the cleaned city path, under the supervisor runtime dir. | `cmd/gc/providers.go` (`providerStateDir`) |
| RUNTIME-SEL-009 | ACP per-session routing | When the city provider is not `acp` but agents/templates select `session = "acp"`, an auto-routing provider wraps the city provider and routes those sessions to ACP. | `cmd/gc/providers.go` (`newSessionProviderFromContext`); `cmd/gc/providers_test.go` `TestNewSessionProvider_PreregistersACPBeadAndLegacyNames`, `TestConfiguredACPRouteNames_*` |
| RUNTIME-SEL-010 | Tmux socket defaults to city name | The tmux provider's socket name defaults to the city name when session config does not set one, so cities never share a default tmux server. | `cmd/gc/providers.go` (`tmuxConfigFromSession`); `cmd/gc/providers_test.go` `TestTmuxConfigFromSessionDefaultsSocketToCityName` |

### Provider Contract

| ID | Scenario | Required behavior | Evidence |
|---|---|---|---|
| RUNTIME-CONTRACT-001 | Conformance suite is the executable contract | Every `runtime.Provider` implementation must pass `runtimetest.RunProviderTests` (or the lifecycle/session sub-suites for slow substrates). New contract semantics land as conformance cases, not prose. | `internal/runtime/runtimetest/conformance.go`; `internal/runtime/fake_conformance_test.go`; per-provider conformance tests |
| RUNTIME-CONTRACT-002 | Contract package purity | The root `internal/runtime` package stays stdlib-only (staging.go exempt, see Global Invariants). | `internal/runtime/import_boundary_test.go` |
| RUNTIME-CONTRACT-003 | Absent-session semantics | `Stop` is idempotent (nil for a missing session). `Nudge` returns nil only when best-effort no-op is safe; providers that can observe but not deliver return `runtime.ErrSessionNotFound` so callers do not mistake a no-op for delivery. | `internal/runtime/runtime.go` interface docs; `internal/runtime/runtimetest/conformance.go` |
| RUNTIME-CONTRACT-004 | Optional capabilities are interface extensions | Behavior beyond the core interface (dialog handling, idle-wait, activity reporting, ACP routing, …) is expressed as optional interfaces type-asserted by callers, never as flags on the core interface. | `internal/runtime/runtime.go`; `internal/runtime/dialog.go`; `cmd/gc/providers.go` (`registerStatusProviderACPRoutes`) |

### RPP v0 (Exec Protocol)

| ID | Scenario | Required behavior | Evidence |
|---|---|---|---|
| RUNTIME-RPP-001 | Op dispatch shape | Each provider operation invokes the executable as `<executable> <op> <args…>` (git credential-helper pattern): `start <name>`, `stop <name>`, `is-running <name>`, `peek <name> <lines>`, `nudge <name>`, `set-meta <name> <key>`, `list-running <prefix>`, … | `internal/runtime/exec/exec.go`; `internal/runtime/exec/exec_test.go`; `docs/reference/exec-session-provider.md` |
| RUNTIME-RPP-002 | Start config wire format | `start` receives session config as JSON on stdin with stable field names owned by the wire struct (`startConfig`), deliberately decoupled from `runtime.Config` Go field names. | `internal/runtime/exec/json.go`; `internal/runtime/exec/json_test.go` |
| RUNTIME-RPP-003 | Exit code semantics | Exit 0 = success; exit 1 = error with the message on stderr; exit 2 = unknown op, treated as success (forward compatibility). A `start` error containing "already exists" maps to `runtime.ErrSessionExists`. | `internal/runtime/exec/exec.go` (`runWithContext`) |
| RUNTIME-RPP-004 | Payloads ride stdin | Nudge text and meta values are delivered on stdin, not argv, so payloads survive shells and length limits. | `internal/runtime/exec/exec.go` (`Nudge`, `SetMeta`, `ProcessAlive`) |
| RUNTIME-RPP-005 | Timeouts | Non-start ops default to a 30s timeout; `start` gets 120s for readiness polling; pipes are force-closed shortly after expiry even if grandchildren hold them. | `internal/runtime/exec/exec.go` (`NewProvider`, `runWithContext` WaitDelay) |
| RUNTIME-RPP-006 | Attach inherits the TTY | `attach` runs with the caller's terminal attached and blocks until detach. | `internal/runtime/exec/exec.go` (`runWithTTY`, `Attach`) |
| RUNTIME-RPP-007 | Protocol doc is canonical | `docs/reference/exec-session-provider.md` is the protocol description scripts are written against; code comments point there, and protocol changes update doc, scripts, and this ledger together. | `docs/reference/exec-session-provider.md`; `contrib/session-scripts/` |

### Planned (not yet enforced)

Rows here are intent from `engdocs/design/runtime-provider-packs.md`. They
move into the ledger above only together with the tests that prove them.

| ID | Scenario | Required behavior | Tracking |
|---|---|---|---|
| RUNTIME-PLAN-001 | Protocol handshake | A `protocol` op returns `{"version": …, "capabilities": […]}`; exit 2 (absent) means v0 with no optional capabilities, so existing `exec:` scripts stay valid. Declared capabilities map onto the optional Go interfaces. | `ga-ghbts9` |
| RUNTIME-PLAN-002 | Conformance command | `gc runtime check <name\|command>` runs the conformance suite against an arbitrary executable, so runtime-pack CIs validate without Go imports. Declared capabilities must be exercised, not trusted. | `ga-ghbts9` |
| RUNTIME-PLAN-003 | Pack-declared runtimes | `[runtimes.<name>]` in pack.toml (command, protocol version) registers a runtime during city composition; collisions with builtin names are errors; a doctor check verifies install + handshake. | `ga-h504e5` |
| RUNTIME-PLAN-004 | Delivery-independence proof | The runtime-cloudflare pack replaces `internal/runtime/cloudflare`; bumping the pack version changes provider behavior under an unchanged `gc` binary. | `ga-6qwfkb` |
| RUNTIME-PLAN-005 | Staging relocation | `staging.go` moves out of the contract package (or overlay staging becomes protocol data) and the import-boundary exemption is deleted. | `ga-1symz6` epic |
