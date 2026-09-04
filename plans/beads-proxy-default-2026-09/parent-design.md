Sol architecture contract, 2026-09-04:

- Gas City exposes only provider-neutral ephemeral init axes: transport
  `direct|proxied` and target `local|external`. Omitted axes use the selected
  provider's default; bd 1.3 defaults fresh bd/Dolt scopes to proxied-local.
- The bd adapter translates those axes to the exact bd 1.3 init contract and
  bd owns `.beads` config, metadata, client-info, proxy/server controls, and
  readiness. Gas City does not persist bd topology/mode/backend values or
  duplicate lifecycle artifacts.
- New proxied-local and direct-local resources are bd-owned. In the
  proxied-external shape bd owns only the local proxy and the external operator
  owns upstream Dolt. Direct-external is client-only. Existing GC-managed
  direct-local scopes keep their legacy owner until an explicit journaled
  handoff; no automatic handoff or conversion is part of this release.
- Persisted initialized scope state is authoritative. Fresh resolution is
  explicit CLI intent, then allowed environment/config inputs, then city
  config, then provider default, with ambient `BEADS_DOLT_*` unable to select
  transport. Credentials are runtime-only; missing credentials fail closed.
  Policy inheritance never copies database/project identity or provider state.
- Explicit backend selection preserves embedded/DoltLite unchanged. Existing
  direct/proxied scopes continue unchanged. Remote hosted proxy services,
  automatic embedded migration, and any proxy-to-direct fallback are out of
  scope.
- Provider readiness and initialization errors block before agent launch and
  never trigger a substitute topology. If a provider has already created
  resources before an error, it must expose a durable idempotent retry/repair
  contract; the scope remains uninitialized until the provider's final commit
  marker and GC performs no destructive rollback.

Promotion requires real bd/gc front-door evidence, exact text/JSON/exit and
no-mutation assertions, direct-vs-proxy parity for supported operations, active
pack coverage, focused/race/vet/sharded gates, a pushed PR labelled
`status/needs-review-auto`, and Sol review before each dependent slice.
