# Runtime & Provider Packs

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-06-12 |
| Author(s) | Julian, Claude |
| Issue | `ga-1symz6` (epic); PoC PRs `ga-fse3es` → `ga-ghbts9` → `ga-h504e5` → `ga-6qwfkb` |
| Related | [packv2/doc-pack-v2.md](packv2/doc-pack-v2.md), [provider-inheritance.md](provider-inheritance.md), PR #3335 (gastown pack as Go module) |

Plan for moving runtime providers and builtin agent-provider definitions
out of the gascity core and into packs consumed as external dependencies,
plus the scoped first PoC.

## Problem

Three things called "provider" are compiled into the `gc` binary today,
and all three grow by editing core:

1. **Runtime providers** (`internal/runtime/{tmux,k8s,subprocess,exec,
   acp,t3bridge,cloudflare,hybrid,auto}`) implement `runtime.Provider`
   and are selected by a hardcoded string switch in
   [`cmd/gc/providers.go`](../../cmd/gc/providers.go) (`newSessionProviderByName`).
   There is no registry; adding a runtime means editing the switch and
   shipping a new `gc`.
2. **Builtin agent providers** (claude, codex, gemini, kiro, mimo,
   opencode, …) are ~780 LOC of declarative Go structs in
   [`internal/worker/builtin/profiles.go`](../../internal/worker/builtin/profiles.go).
   This is pure data trapped in Go — every new agent CLI (see the mimo
   commits, `ga-9jg4fq`) requires a core PR, even though `[providers.x]`
   TOML with inheritance (`base = "builtin:claude"`) already exists as a
   user-facing surface.
3. **Service/bridge providers** (discord, telegram — `ga-aiefhz`) are
   already moving to packs via `[[service]]`; they are prior art, not
   part of this plan.

For this fork specifically, `internal/runtime/t3bridge` (~2.8k LOC) and
`cmd/gc/template_resolve_t3bridge.go` are fork-owned behavior living
inside upstream-owned trees — exactly what the upstream-alignment rules
say to isolate behind small ownership boundaries.

## Survey findings (June 2026)

- **The runtime contract is already self-contained.** The root
  `internal/runtime` package (~7k LOC: `runtime.go`, `provider_core.go`,
  `dialog.go`, `liveness.go`, `probe.go`, `fingerprint.go`, `beacon.go`,
  `mcp.go`, `process_control.go`, `fake.go`, …) imports **only stdlib**,
  with one exception: `staging.go` imports `internal/overlay`.
- **A conformance suite exists.** `internal/runtime/runtimetest`
  (`RunProviderTests` / `RunLifecycleTests` / `RunSessionTests`) imports
  only the contract package — external implementations can run it as-is
  once it is importable.
- **Coupling gradient across providers** (non-test LOC):
  `hybrid` 218 and `cloudflare` 503 import *only* the contract;
  `exec` 595, `subprocess` 659, `auto` 356, `acp` 1711 are middling;
  `tmux` 6187, `k8s` 2089, `t3bridge` 2839 also pull in
  `internal/{beads,events,citylayout}`.
- **The external-module precedent already shipped.** PR #3335
  (`5a23df317`) consumes `github.com/gastownhall/gascity-packs` as a
  pinned Go module (embedded pack bytes via `gascitypacks.Gastown()`),
  with a pin-bump script and hermetic integration tests. That repo
  already hosts nested adapter modules (slack-*).
- **Pack registry prior art**: `work/default-pack-registry` (commit
  `052164dcf`) moves bundled default packs into the import registry —
  the loading mechanism Track B needs.
- **Pack v2** ([doc-pack-v2.md](packv2/doc-pack-v2.md)) defines packs as
  declarative trees (agents, formulas, commands, services, doctor,
  `[providers.x]` settings). Packs do not carry compiled Go code; a
  "runtime pack" therefore means *a Go module that may sit alongside
  pack content*, consumed like gascity-packs is today.

## Goals

- Runtime providers become external Go modules implementing a stable
  contract, registered by name, conformance-tested against the shared
  suite, and pinned in `go.mod` like gascity-packs.
- Builtin agent-provider specs become pack-delivered TOML resolved
  through the existing provider-inheritance chain.
- Core keeps only: the contract, the registry, the composition/routing
  layers (`auto`, transport selection), and whatever providers we
  deliberately keep builtin (at minimum `fake`/`fail` for tests).
- Fork-specific providers (t3bridge) end up outside upstream-owned
  trees, making `upstream/main` merges cheap.

## Non-goals

- No `go plugin` / dlopen-style dynamic loading. Linkage stays
  compile-time; "external" means *external module*, not runtime-loaded
  binary. (`exec:<script>` remains the escape hatch for truly dynamic
  runtimes.)
- No change to how cities *select* a runtime (`session = "tmux"` etc.).
- Service packs (discord/telegram) — already covered by `[[service]]`.

## Target architecture

```
github.com/gastownhall/gascity/runtimeapi      (nested module, contract)
    ├── runtime contract types + Provider interface + fake
    └── runtimetest/ conformance suite
            ▲                        ▲
            │                        │
github.com/gastownhall/gascity      provider modules
  internal/runtime → aliases          (e.g. gascity-packs/runtime-cloudflare,
  registry + builtins                  later: runtime-t3bridge)
  cmd/gc registers external                  ▲
  providers via pinned imports ──────────────┘
```

Dependency graph is acyclic: `runtimeapi` has no deps; provider modules
depend only on `runtimeapi`; gascity depends on both. This avoids the
module cycle that would arise if providers imported the gascity module
itself.

### Track A — runtime providers as modules

1. **Registry seam.** `Register(name, factory)` keyed by the existing
   selection names, plus prefix handling for `exec:`. The
   `newSessionProviderByName` switch becomes a lookup; each builtin
   registers from its own file so a later module split is mechanical.
2. **Contract module.** Promote the root `internal/runtime` package +
   `runtimetest` to a nested module `runtimeapi/` (own `go.mod`, no
   dependencies). `internal/runtime` becomes type aliases re-exporting
   it, so the ~hundreds of existing import sites (and upstream diffs)
   stay untouched. `staging.go` (the one `internal/overlay` dependent)
   stays behind in `internal/runtime`.
3. **Provider modules.** Each extracted provider is a Go module
   depending only on `runtimeapi`, running `runtimetest` conformance in
   its own CI, optionally paired with pack content (docs, doctor
   checks). Hosted in gascity-packs as nested modules
   (`runtime-<name>/`), following the slack-* precedent.
4. **Consumption.** gascity pins provider modules in `go.mod` and
   registers them in one small file (`cmd/gc/runtime_packs.go`) — the
   single fork-owned divergence point for fork-only providers.

### Track B — agent provider specs as pack TOML

Convert `internal/worker/builtin/profiles.go` into `[providers.x]` TOML
shipped in gascity-packs and loaded through the bundled-import registry
(`work/default-pack-registry` mechanism). Bootstrap constraint: provider
resolution must work offline before pack composition, so the embedded
module bytes (not network fetch) are the source. This track has a much
larger blast radius (every resolution path in `internal/config/resolve.go`)
and ships **after** Track A proves the consumption pattern.

## PoC scope (first slice, Track A)

**Extracted provider: `cloudflare`.** Rationale: zero coupling beyond
the contract, ~500 LOC, genuinely remote semantics (HTTP to a Worker),
and low blast radius — it only activates when explicitly selected.
`hybrid` is smaller but is composition logic whose constructor wiring
lives in core anyway; `t3bridge` is the strategic payoff but carries
beads/events coupling and is load-bearing for the T3 integration — it
graduates second, once the mechanism is proven.

Four PRs, each independently green:

| PR | Repo | Content | Risk |
|---|---|---|---|
| 1 | gascity | Registry: `internal/runtime/registry.go` (Register/Lookup, `exec:` prefix hook), builtins re-register, switch in `cmd/gc/providers.go` becomes lookup. Boundary test pins the contract package to stdlib-only (mirroring `worker_boundary_import_test.go`). Behavior-preserving. | Low |
| 2 | gascity | Contract split: move root `internal/runtime` (minus `staging.go`) + `runtimetest` to nested module `runtimeapi/`; `internal/runtime` becomes aliases. `go.work` for local dev; parent `go.mod` requires `runtimeapi` via relative `replace` until first tag. | Medium (mechanical but wide) |
| 3 | gascity-packs | `runtime-cloudflare/` nested module: provider code moved over, depends on `runtimeapi`, runs `runtimetest` conformance in packs CI. | Low |
| 4 | gascity | Delete `internal/runtime/cloudflare`; pin `runtime-cloudflare` in `go.mod`; register in `cmd/gc/runtime_packs.go`. Existing cloudflare selection tests pass unchanged. | Low |

PoC exit criteria:

- `gc` builds with cloudflare resolved from the external module; a city
  with `session = "cloudflare"` behaves identically (existing tests).
- `runtimetest` conformance runs green inside the provider module's CI,
  outside the gascity module.
- `go vet ./...`, fast unit baseline, and the sharded suites pass at
  each PR boundary.
- A written t3bridge extraction checklist derived from the PoC (what
  beads/events surfaces it needs from the contract or via injection).

## Alternatives considered

- **`pkg/runtime` in the gascity module, providers depend on gascity.**
  Creates a module cycle (gascity ⇄ provider) the moment `gc` links a
  provider. Go tolerates module-graph cycles but tagging/MVS hygiene is
  painful; rejected.
- **Standalone `gascity-runtime` repo for the contract.** Cleanest
  release engineering, but a third repo to coordinate during the
  fast-iteration phase. The nested `runtimeapi/` module keeps contract
  changes and their gascity consumers in one PR; we can graduate it to
  its own repo later without changing the import path strategy
  (path would change, aliases confine the blast radius).
- **`exec:` script packs only.** Already works today with zero new
  mechanism, but abandons type safety, conformance testing, and the
  in-process providers we actually need to maintain (tmux, k8s).
- **Go `plugin` package.** Platform-fragile, version-locked, rejected.

## Risks

- **Interface stability.** `runtime.Provider` is 18 methods plus 7
  optional capability interfaces; promoting it to a public module makes
  changes breaking. Mitigation: the optional-interface pattern already
  in place is the extension mechanism; `runtimeapi` stays v0 during the
  PoC window.
- **Nested-module tagging.** Same-repo nested modules need
  `runtimeapi/vX.Y.Z`-style tags and a pin-bump flow; reuse the
  `scripts/update-bundled-gastown-pack` pattern.
- **Upstream drift.** The alias shim in `internal/runtime` is a wide but
  mechanical diff against upstream. If upstreaming the registry+contract
  split is viable, propose it early — it is role-free SDK infrastructure
  and passes the Primitive Test.

## Follow-ups after the PoC

1. **t3bridge extraction** (fork payoff): provider module outside
   upstream-owned trees + `cmd/gc/runtime_packs.go` registration;
   `template_resolve_t3bridge.go` and the legacy `exec:` alias move
   behind the same boundary.
2. `hybrid`, then `k8s` extraction; `tmux`/`subprocess`/`acp` stay
   builtin until there is a concrete reason to move them.
3. Track B: builtin agent-provider TOML via the bundled-import registry.
