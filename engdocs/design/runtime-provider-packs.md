# Runtime & Provider Packs

| Field | Value |
|---|---|
| Status | Proposed — revised after delivery-independence decision (2026-06-12) |
| Date | 2026-06-12 |
| Author(s) | Julian, Claude |
| Issue | `ga-1symz6` (epic); PoC PRs `ga-fse3es` → `ga-ghbts9` → `ga-h504e5` → `ga-6qwfkb` |
| Related | [packv2/doc-pack-v2.md](packv2/doc-pack-v2.md), [provider-inheritance.md](provider-inheritance.md), PR #3335 (gastown pack as Go module) |

Plan for moving runtime providers and builtin agent-provider definitions
out of the gascity core and into packs, plus the scoped first PoC.

**Governing requirement (decided 2026-06-12): true delivery
independence.** Users must receive provider/runtime updates without a
new `gc` release. This rules out any in-process Go design — Go
statically links everything into the `gc` binary, so external Go
modules only decouple *source*, not *delivery* (every pin bump still
requires a gc rebuild and release). The architecture below is therefore
protocol-based: a runtime provider is an **executable shipped by a
pack**, speaking a versioned wire protocol to `gc`. An earlier revision
of this doc proposed Go-module extraction (`runtimeapi` nested module +
provider modules in gascity-packs); it is preserved under
*Alternatives considered*.

## Problem

Three things called "provider" are compiled into the `gc` binary today,
and all three grow by editing core:

1. **Runtime providers** (`internal/runtime/{tmux,k8s,subprocess,exec,
   acp,t3bridge,cloudflare,hybrid,auto}`) implement `runtime.Provider`
   and are selected by a hardcoded string switch in
   [`cmd/gc/providers.go`](../../cmd/gc/providers.go) (`newSessionProviderByName`).
   There is no registry; adding or fixing a runtime means editing the
   switch and shipping a new `gc`.
2. **Builtin agent providers** (claude, codex, gemini, kiro, mimo,
   opencode, …) are ~780 LOC of declarative Go structs in
   [`internal/worker/builtin/profiles.go`](../../internal/worker/builtin/profiles.go).
   This is pure data trapped in Go — every new agent CLI (see the mimo
   commits, `ga-9jg4fq`) requires a core PR, even though `[providers.x]`
   TOML with inheritance (`base = "builtin:claude"`) already exists as a
   user-facing surface.
3. **Service/bridge providers** (discord, telegram — `ga-aiefhz`) are
   already moving to packs via `[[service]]`; they are prior art for
   this design, not part of its scope.

For this fork specifically, `internal/runtime/t3bridge` (~2.8k LOC) and
`cmd/gc/template_resolve_t3bridge.go` are fork-owned behavior living
inside upstream-owned trees — exactly what the upstream-alignment rules
say to isolate behind small ownership boundaries.

## Survey findings (June 2026)

- **The out-of-process seam already exists.** The `exec:` provider
  ([`internal/runtime/exec`](../../internal/runtime/exec/exec.go), 595
  LOC) delegates every operation to a user-supplied executable:
  `script <op> <args...>` with session config as JSON on stdin, exit
  code 0 = success, 1 = error (stderr carries the message), **2 =
  unknown operation, treated as success for forward compatibility**
  (the git credential-helper pattern). It already handles startup-watch
  event streaming (`json.go`) and TTY-inherited attach. t3bridge was
  originally wired this way (the `gc-session-t3` legacy `exec:` alias in
  `cmd/gc/providers.go` still recognizes it).
- **Pack-shipped long-lived processes already exist.** `[[service]]`
  with `kind = "proxy_process"`
  ([`internal/config/service.go`](../../internal/config/service.go))
  gives controller-supervised processes with managed state roots
  (`.gc/services/{name}`), health paths, and pack provenance — the
  lifecycle mechanism a long-lived runtime sidecar needs.
- **Trust boundary is unchanged.** Cities already execute pack-shipped
  code (commands/, doctor/, scripts/, services). A pack-shipped runtime
  executable introduces no new trust surface.
- **A conformance suite exists.** `internal/runtime/runtimetest`
  (`RunProviderTests` / `RunLifecycleTests` / `RunSessionTests`)
  imports only the contract package; the exec provider already passes
  it, so protocol-level conformance is a matter of pointing the suite
  at a proxy provider wrapping an arbitrary executable.
- **Coupling gradient across providers** (non-test LOC):
  `hybrid` 218 and `cloudflare` 503 import *only* the contract;
  `exec` 595, `subprocess` 659, `auto` 356, `acp` 1711 are middling;
  `tmux` 6187, `k8s` 2089, `t3bridge` 2839 also pull in
  `internal/{beads,events,citylayout}` (t3bridge uses
  `beads.CachingStore` — an out-of-process t3bridge reaches the ledger
  via the `bd` CLI / gc API instead).
- **Pack delivery precedents.** PR #3335 (`5a23df317`) consumes
  gascity-packs as a pinned module; `work/default-pack-registry`
  (`052164dcf`) moves bundled packs into the import registry; the
  telegram pack plan (`ga-aiefhz`) ships a node bridge with pinned deps
  installed by a pack doctor step — the same distribution shape runtime
  packs need.
- **Pack v2** ([doc-pack-v2.md](packv2/doc-pack-v2.md)) defines packs as
  declarative trees (agents, formulas, commands, services, doctor,
  `[providers.x]` settings). Top-level pack surface is controlled;
  new sections require explicit design.

## Goals

- **A runtime provider update reaches users without a gc release**:
  bump the pack pin in the city (`packs.lock`), re-run the pack install
  step, done.
- Runtime providers become pack-shipped executables speaking a
  versioned protocol, registered by name, conformance-tested by a
  harness the pack's own CI can run.
- Builtin agent-provider specs become pack-delivered TOML resolved
  through the existing provider-inheritance chain (Track B).
- Core keeps only: the protocol client (proxy provider), the registry,
  the composition/routing layers (`auto`, transport selection), and the
  providers we deliberately keep builtin (`fake`/`fail` for tests;
  `tmux`/`subprocess`/`acp` until there is a concrete reason to move
  them).
- Fork-specific providers (t3bridge) end up outside upstream-owned
  trees — ideally owned and shipped by the T3 Code repo itself.

## Non-goals

- No Go `plugin` / dlopen dynamic loading (toolchain-locked,
  platform-fragile).
- No change to how cities *select* a runtime (`session = "<name>"`).
- Service packs (discord/telegram) — already covered by `[[service]]`.
- Moving tmux/subprocess/acp out of core (revisit after the PoC).

## Target architecture

```
city.toml: session = "cloudflare"
        │
        ▼
runtime registry (core)  ── builtin Go providers (tmux, fake, …)
        │                      registered from core
        ▼
pack-declared runtime ("cloudflare" from runtime-cloudflare pack)
        │
        ▼
proxy provider (core, evolved exec:) ── speaks RPP over fork/exec
        │                               or supervised sidecar
        ▼
pack-shipped executable (gc-runtime-cloudflare)
   installed/updated by the pack, versioned by the pack,
   never linked into gc
```

### The Runtime Provider Protocol (RPP)

RPP v0 **is** the existing exec contract, formalized and versioned:

- `executable <op> <args...>`, JSON on stdin where the op takes
  structured input, JSON or plain text on stdout per op.
- Exit codes 0/1/2 as today; 2 keeps unknown ops forward-compatible.
- One new op: `protocol` — returns
  `{"version": 0, "capabilities": [...]}` so the proxy can answer
  gc's optional-capability probes (dialog handling, idle-wait,
  activity reporting, …) without trial-and-error. A missing `protocol`
  op (exit 2) means "v0, no optional capabilities" — every existing
  `exec:` script remains valid.
- The per-op fork/exec model is v0. A long-lived sidecar mode
  (JSON-RPC over stdio, supervised via `[[service]]
  kind = "proxy_process"`) is the documented evolution for stateful or
  latency-sensitive runtimes; it is **not** in the PoC.

The protocol document is the contract. Go types stay in
`internal/runtime` — nothing is exported, no alias shims, no public Go
API commitment, and the upstream diff stays small.

### Pack surface

A pack declares runtimes by name:

```toml
# pack.toml
[runtimes.cloudflare]
command = "scripts/gc-runtime-cloudflare"   # pack-relative, or PATH name
protocol = 0
```

City composition registers pack-declared runtimes into the runtime
registry; `session = "cloudflare"` resolves through the registry to a
proxy provider bound to that executable. Name collisions with builtin
providers are errors (no silent shadowing). A pack `doctor/` check
verifies the executable is installed and `protocol`-handshakes.

### Distribution

Packs are declarative trees and must not carry per-platform compiled
binaries. A runtime pack ships one of:

- a **script** (sh/node/python) run directly — works today;
- **source + pinned install step** (`go install module@version`,
  `npm ci`) executed by the pack's install/doctor flow — the telegram
  pack precedent;
- a **pinned release-artifact fetch** (goreleaser URL + checksum) for
  compiled providers — later wave, needs a checksummed fetch helper.

The PoC uses the second form: a nested Go module in gascity-packs whose
binary is installed by the pack. The module depends on **nothing from
gascity** — the protocol is the contract.

### Conformance

`runtimetest` already validates any `runtime.Provider`; pointing it at
the proxy provider validates any executable. The PoC wraps this as
`gc runtime check <name|command>` so a runtime pack's CI conformance
step is: install gc, install the pack executable, run
`gc runtime check ./gc-runtime-cloudflare`. No Go imports required.

### Track B — agent provider specs as pack TOML

Unchanged by the pivot (it was always declarative): convert
`internal/worker/builtin/profiles.go` into `[providers.x]` TOML shipped
in gascity-packs and loaded through the bundled-import registry
(`work/default-pack-registry` mechanism). Bootstrap constraint:
provider resolution must work offline before pack composition, so the
embedded module bytes (not network fetch) are the source. Larger blast
radius (every resolution path in `internal/config/resolve.go`); ships
after Track A proves the pack-consumption pattern.

## PoC scope (first slice, Track A)

**Extracted provider: `cloudflare`.** Zero coupling beyond the
contract, ~500 LOC, already speaks HTTP to a remote Worker (so
per-op fork/exec overhead is noise), and low blast radius — it only
activates when explicitly selected. `t3bridge` is the strategic payoff
but is load-bearing for the T3 integration and needs a ledger-access
story (bd CLI / gc API) — it graduates second, once the mechanism is
proven.

Four PRs, each independently green:

| PR | Issue | Repo | Content | Risk |
|---|---|---|---|---|
| 1 | `ga-fse3es` | gascity | Registry: `internal/runtime/registry.go` (Register/Lookup, `exec:` prefix hook), builtins re-register from per-provider files, switch in `cmd/gc/providers.go` becomes lookup. Behavior-preserving. | Low |
| 2 | `ga-ghbts9` | gascity | RPP v0: protocol spec doc (`engdocs/architecture/` or `docs/`), `protocol` handshake op + capability mapping in the exec/proxy provider, `gc runtime check` conformance command (wraps `runtimetest` against an arbitrary executable). Reference executable: fake-backed test script. | Medium |
| 3 | `ga-h504e5` | gascity | Pack surface: `[runtimes.<name>]` in pack.toml, composition registers pack runtimes, collision rules, doctor handshake check. | Medium |
| 4 | `ga-6qwfkb` | gascity-packs + gascity | `runtime-cloudflare` pack: nested Go module (no gascity deps) emitting `gc-runtime-cloudflare` speaking RPP v0, installed by the pack, `gc runtime check` green in packs CI. gascity deletes `internal/runtime/cloudflare`; `session = "cloudflare"` resolves via the pack. | Low |

PoC exit criteria:

- A city with the runtime-cloudflare pack and `session = "cloudflare"`
  behaves as today (existing selection tests adapted).
- **Delivery-independence demo**: bump the runtime-cloudflare pack
  version in a city, observe the new provider behavior with the same
  `gc` binary.
- `gc runtime check` runs green in gascity-packs CI against the
  installed executable, with no Go imports from gascity.
- `go vet ./...`, fast unit baseline, and the sharded suites pass at
  each PR boundary.
- A written t3bridge extraction checklist: which ops it needs, ledger
  access via bd CLI / gc API, and whether T3 Code hosts the executable.

## Alternatives considered

- **Go-module extraction (previous revision of this doc):** nested
  `runtimeapi` contract module + provider modules in gascity-packs,
  pinned in go.mod like the gastown pack. Gives independent *source*
  maintenance and clean fork isolation, but every provider update still
  requires a pin bump and a gc rebuild/release — it fails the governing
  delivery-independence requirement. Rejected 2026-06-12. (If a future
  provider needs in-process performance, this remains the fallback
  shape; PR 1's registry serves both.)
- **`pkg/runtime` in the gascity module, providers depend on gascity:**
  module cycle the moment gc links a provider; rejected.
- **Go `plugin` package:** exact-toolchain-match requirement makes it
  unshippable; rejected.
- **Long-lived sidecar protocol first (go-plugin style JSON-RPC):**
  strictly more capable but strictly more machinery; v0 per-op exec
  already exists, passes conformance, and covers the PoC. Sidecar mode
  is the documented evolution, gated on a concrete stateful runtime
  needing it.

## Risks

- **Protocol stability.** Once packs ship against RPP v0 it is a
  compatibility surface. Mitigations: exit-2 forward compatibility is
  inherited from the exec contract; `protocol` handshake carries the
  version; the spec doc is the contract and changes go through design
  review.
- **Per-op fork/exec overhead.** Health patrol and status paths poll
  `IsRunning`/`Peek`; each call is a process spawn. The existing exec
  provider already lives this life; cloudflare adds HTTP latency that
  dwarfs it. Measure during the PoC; the sidecar mode is the escape
  hatch.
- **Distribution friction.** `go install`-based packs require a Go
  toolchain on the host. Acceptable for the PoC (dev-tool audience);
  the checksummed release-artifact fetch is the general answer and is
  scoped out deliberately.
- **Capability fidelity.** `runtime.Provider` has 7 optional extension
  interfaces; the proxy must map handshake capabilities to them
  accurately or gc will silently skip features. The conformance command
  must assert declared capabilities actually work.

## Follow-ups after the PoC

1. **t3bridge extraction** (fork payoff): RPP executable owned by the
   T3 Code repo (or a t3 pack), ledger access via bd CLI / gc API;
   `template_resolve_t3bridge.go` and the legacy `exec:` alias retire
   behind the pack boundary.
2. Sidecar (long-lived) protocol mode when a stateful runtime needs it;
   `[[service]] kind = "proxy_process"` supplies supervision.
3. Checksummed release-artifact distribution for compiled providers.
4. Track B: builtin agent-provider TOML via the bundled-import
   registry.
5. Revisit `k8s`/`hybrid` extraction against the proven protocol.
