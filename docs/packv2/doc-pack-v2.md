# Pack/City Model v.next

**GitHub Issue:** [gastownhall/gascity#360](https://github.com/gastownhall/gascity/issues/360) (supersedes [#159](https://github.com/gastownhall/gascity/issues/159))

Title: `feat: Pack/City Model v.next — cities as packs, import model, managed state`

Companion to [doc-agent-v2.md](doc-agent-v2.md) ([gastownhall/gascity#356](https://github.com/gastownhall/gascity/issues/356)), which covers agent definition restructuring.

> **Keeping in sync:** This file is the source of truth. When updating, edit here, then update the issue body with `gh issue edit 360 --repo gastownhall/gascity --body-file <(sed -n '/^---BEGIN ISSUE---$/,/^---END ISSUE---$/{ /^---/d; p; }' issues/doc-pack-v2.md)`.

---BEGIN ISSUE---

## Problem

The current model tangles three concerns that should be separate: portable **definition** (agents, providers, formulas), team **deployment** decisions (rigs, substrates, capacity), and per-machine **site binding** (paths, prefixes, suspended flags). city.toml carries all three; packs carry the first but dissolve on composition; `.gc/` doesn't exist as a distinct layer. This creates a cascade of problems:

1. **Cities are pack-like but not structured as packs.** City-level content participates in layered resolution and overrides pack content, but it can't be composed, shared, or imported the way packs can. We want one unit of composition — a city definition should just be a pack.

2. **Include semantics are too weak.** `includes` dumps pack content into the city with no qualified identity. Collisions depend on load order. There's no aliasing, no version pinning, no explicit collision handling. We want named imports with durable identity so you can say "the mayor from gastown."

3. **Convention and declaration fight each other.** Formulas are discovered by directory; prompts need explicit TOML paths; scripts are discovered again. We want convention to define structure — if a directory exists, its contents are loaded.

4. **Packs are not self-contained.** Content can reference paths outside the pack boundary. No enforced transitive closure. We want packs to be fully portable — directory tree plus declared imports, nothing else.

5. **Managed state has no clear home.** `workspace.name`, `rig.path`, and operational toggles live in checked-in TOML alongside shareable definition. We want clean separation: `pack.toml` is the *definition* (what this city is), `city.toml` is the *deployment plan* (team-shared decisions about how to run it), `.gc/` is the *site binding* (machine-local state that attaches the deployment to a specific filesystem).

This proposal does not cover `.gc/` internals beyond what the pack changes require, any package-registry or implicit-import surface, or the mechanical migration UX for breaking existing cities. Old cities may hard-break until migrated; the public migration path is `gc doctor` followed by `gc doctor --fix`.
## Proposed change

### Cities

A city is a pack with a companion deployment file, `city.toml`. Delete `city.toml` and what remains is a valid, portable pack.

The structure of a city definition is identical to that of a pack (agents, formulas, prompts, scripts) including a `pack.toml` file that defines the structure of the city.

The deployment decisions (rigs, substrates, capacity) live in `city.toml`. Site binding (paths, prefixes, operational state) lives in `.gc/`. (The examples below use the agent-as-directory model from the companion proposal [#356](https://github.com/gastownhall/gascity/issues/356). Both proposals ship together in the same breaking wave. During implementation, the pack/city restructuring lands first with `[[agent]]` syntax preserved; agent-as-directory layers on top as the second step.)

```
my-city/
├── pack.toml              # what this city IS (portable definition)
├── agents/                # agent definitions (convention-discovered)
├── formulas/              # formula definitions (convention-discovered)
├── orders/                # order definitions (convention-discovered)
├── commands/              # pack-provided CLI commands
├── doctor/                # diagnostic check scripts
├── patches/               # prompt replacements for imported agents
├── overlay/               # pack-wide overlay files
├── skills/                # current-city-pack skills catalog (imported-pack catalogs later)
├── mcp/                   # current-city-pack MCP server definitions (imported-pack catalogs later)
├── template-fragments/    # prompt template fragments
├── assets/                # opaque pack-owned files (not convention-discovered)

├── city.toml              # how this city is DEPLOYED (team-shared)
└── .gc/                   # site binding (machine-local, gitignored)
```

The top level of a pack is **controlled surface area** — standard directory names are explicitly recognized and unknown top-level directories are errors. Arbitrary files live under `assets/`. See [doc-directory-conventions.md](doc-directory-conventions.md) for the full layout specification.

For the first skills/MCP slice, only the current city pack contributes
`skills/` and `mcp/` catalogs; imported-pack catalogs are a later wave.

Embedded packs (if needed) live under `assets/` and are referenced by explicit import path:

```toml
[imports.maintenance]
source = "./assets/maintenance"
```

The city's `pack.toml` contains everything that defines *what this city is*. Imports are covered in their own section below — for now, note that pack composition is declared here rather than in city.toml:

```toml
# pack.toml (the city pack)

[pack]
name = "my-city"
version = "0.1.0"

[imports.gastown]
source = "./assets/gastown"

[imports.maint]
source = "./assets/maintenance"

# Pack-wide agent defaults — individual agents defined in agents/ directories
# City-level [agent_defaults] in city.toml can override these.
[agent_defaults]
provider = "claude"

[[named_session]]
template = "mayor"
mode = "always"

# Provider settings — model, permissions, etc.
[providers.claude]
model = "claude-sonnet-4-20250514"
```

The city deployment file `city.toml` contains what the team agrees on for *how this city runs*:

```toml
# city.toml (team-shared deployment — no identity fields)

[beads]
provider = "dolt"

[[rigs]]
name = "api-server"
max_active_sessions = 4
default_sling_target = "api-server/polecat"
session_sleep = { idle = "10m" }

[[rigs]]
name = "frontend"
max_active_sessions = 2
```

Site binding (rig paths, suspended flags, prefixes) is managed by `gc` commands and stored in `.gc/`:

```
gc rig add ~/src/api-server --name api-server
gc rig add ~/src/frontend --name frontend
```

#### What makes a city pack different from a regular pack?

Very little:

1. **A city pack is the root of composition.** It need not be imported by anything else.
2. **A city pack has a companion `city.toml` describing a deployment.** Regular packs have only `pack.toml`.
3. **Only cities have rigs.** Rigs are declared in city.toml, not in packs.

Everything else — agents, named sessions, providers, formulas, prompts, scripts, overlays, imports, patches — works identically.

> **Design principle:** if you deleted `city.toml` from a city directory, what remains is a valid pack that could be imported by another city.

#### Names, prefixes, and generation

`gc init` and `gc rig add` generate names and prefixes by default. Users can override with `--name` and `--prefix` (typically to resolve conflicts). `gc init` now writes the chosen machine-local workspace name/prefix to `.gc/site.toml`; `pack.toml` keeps the portable definition identity.

`gc register` accepts `--name` to set the city's registration name explicitly. The chosen name is stored in the machine-local supervisor registry and is not written back to `city.toml`. When `--name` is omitted, `gc register` uses the current effective city identity (site-bound workspace name if present, otherwise legacy `workspace.name`, otherwise the directory basename) and stores that value in the registry. `gc register` does not rewrite `city.toml` or `pack.toml`. ([#602](https://github.com/gastownhall/gascity/issues/602))

Names and prefixes are both managed by `gc`. The authoritative copy lives in `.gc/`. Names are human-facing labels; prefixes are derived from names and baked into bead IDs. Neither should be casually changed after creation.

#### Renaming

Renaming is done through `gc rig rename` (or `gc workspace rename`), not by editing TOML files. The rename command updates the name in city.toml, the name-to-prefix mapping in `.gc/`, and optionally picks a new prefix (with `--prefix`). If prefix migration is needed (existing beads use the old prefix), the command handles it.

If `gc` detects a mismatch between a rig name in city.toml and its managed state, it blocks startup and tells the user to resolve it via the rename command.

#### How `pack.name` and workspace identity relate

`pack.name` is the identity of the definition — "this pack is called gastown." It lives in `pack.toml`, is portable, and travels with the pack when imported.

`workspace.name` and `workspace.prefix` are now legacy compatibility fields. Fresh `gc init` writes machine-local identity to `.gc/site.toml`, and `gc doctor --fix` migrates legacy values out of `city.toml`. `gc register` treats the supervisor registry as the machine-local source of truth for registration identity: an explicit `--name` alias can differ from site-bound or legacy workspace identity, and runtime supervisor-managed flows prefer that registered alias.

The long-term direction remains the same: keep portable identity in `pack.name`, deployment plan in `city.toml`, and machine-local naming/bindings in site binding under `.gc/`.

The full field-by-field migration is in the appendix.

### Import model

The current `includes` mechanism has three problems: packs lose their identity after composition (you can't say "the mayor from gastown"), collisions are resolved by load order with no explicit handling, and transitive dependencies are invisible (adding a sub-include to a pack silently changes what agents appear in every city that uses it). Imports fix all three by giving each composed pack a durable name, requiring explicit collision resolution, and being closed by default.

Packs compose other packs through **imports**, not includes. An import creates a named binding to another pack.

#### A concrete example

A pack called `gastown` defines agents and formulas:

```toml
# assets/gastown/pack.toml
[pack]
name = "gastown"
version = "1.2.0"

[agent_defaults]
provider = "claude"
scope = "rig"
```

```
assets/gastown/
├── pack.toml
├── agents/
│   ├── mayor/
│   │   ├── agent.toml     # scope = "city"
│   │   └── prompt.md
│   └── polecat/
│       └── prompt.md
├── formulas/
│   ├── mol-polecat-work.toml
│   └── mol-idea-to-plan.toml
└── assets/
    └── worktree-setup.sh
```

A city pack imports it:

```toml
# pack.toml (city pack)
[pack]
name = "my-city"

[imports.gastown]
source = "./assets/gastown"
```

After import, agents are available by bare name (`mayor`, `polecat`) when unambiguous, or by qualified name (`gastown.mayor`, `gastown.polecat`) when disambiguation is needed.

#### Aliasing

The binding name does not have to match the pack name:

```toml
[imports.gs]
source = "./assets/gastown"
```

Now `gs.mayor` and `gs.polecat` are available as qualified names.

#### Version constraints

Remote imports use semver constraints:

```toml
[imports.gastown]
source = "github.com/gastownhall/gastown"
version = "^1.2"
```

Local path imports have no version constraint.

Resolved versions for remote imports are recorded in the lock file (`packs.lock`; format owned by [doc-packman.md](doc-packman.md)). The loader reads the lock file to find which commit each import resolves to and which directory under `~/.gc/cache/repos/` holds it. The loader itself does not clone git or self-heal missing state — that responsibility belongs to `gc import install`. A missing lock entry or cache entry is a load-time error telling the user to run `gc import install`.

#### Transitive import and export

By default, imports are **transitive**. If `gastown` imports `maintenance` internally, anyone who imports `gastown` also gets `maintenance`'s contents automatically. This is the common case — if a pack requires a dependency, the consumer needs it too.

A pack can suppress transitive resolution for a specific import with `transitive = false`:

```toml
# assets/gastown/pack.toml
[imports.maintenance]
source = "../maintenance"
transitive = false
```

This is unusual — it means "I import this for my own use, but consumers of my pack should not see it." The typical use case is internal tooling or test-only dependencies.

A pack can explicitly re-export an imported pack to make its contents available under the re-exporting pack's namespace:

```toml
# assets/gastown/pack.toml
[imports.maintenance]
source = "../maintenance"
export = true
```

With `export = true`, maintenance's agents appear flattened into gastown's namespace: `gastown.dog`, not `gastown.maintenance.dog`. Re-export is opaque — the consumer doesn't need to know that `dog` came from `maintenance` internally. Provenance is still tracked in the import graph for tooling (`gc why dog`), but the addressable name is the re-exporting pack's binding, not the transitive path.

#### Lock file model

The root city's lock file (`packs.lock`) records every pack in the entire transitive import graph. Imported packs do **not** carry their own lock files. `gc import install` is the only command that bootstraps or repairs this file: when `packs.lock` is missing it resolves the declared graph and writes it, and when `packs.lock` is present it restores the cache from that committed state. Normal load/start/config flows remain pure readers. See [doc-packman.md](doc-packman.md) for the lock file format.

#### Lifecycle verbs

Four distinct operations, currently partially conflated:

| Operation | Verb | What it does |
|---|---|---|
| Define a city's contents | `gc init` (creates files), or hand-edit | Creates pack.toml, city.toml, directory structure |
| Validate installed imports | `gc import check` | Checks declared imports, `packs.lock`, and local cache state without fetching or mutating |
| Install a city's packs | `gc import install` | Bootstraps or repairs `packs.lock` and materializes all imports into the cache |
| Register a city with the controller | `gc register` | Binds the city to `.gc/`; tells the controller it exists |
| Start the city's runtime | `gc start` | Controller activates the registered city |

`gc start` implies `gc register` if not yet done (zero-config preserved). `gc register` is the explicit binding step for workflows that want to stage a city before activating it.

#### Rig-level imports

Rigs are a city concept. A pack does not know about rigs. Rig-level imports live in city.toml:

```toml
# city.toml
[[rigs]]
name = "api-server"

[rigs.imports.gastown]
source = "./assets/gastown"

[rigs.imports.custom]
source = "./assets/api-tools"
```

Rig-level imports produce rig-scoped agents: `api-server/gastown.polecat`. City-level imports produce city-scoped agents: `gastown.mayor`.

#### Default rig imports

The current `workspace.default_rig_includes` becomes `[defaults.rig.imports.<binding>]` entries for new rigs:

```toml
# pack.toml
[defaults.rig.imports.gastown]
source = "./assets/gastown"
```

When `gc rig add` creates a new rig and the user does not specify imports, these defaults are used.

### Convention-based structure

A pack's filesystem layout is its declaration. The top level is **controlled** — standard names are recognized, unknown top-level directories are errors, and arbitrary files live under `assets/`.

```
my-pack/
├── pack.toml              # metadata, imports, agent defaults, patches
├── agents/                # agent definitions (convention-discovered)
├── formulas/              # *.toml formula files (convention-discovered)
├── orders/                # *.toml order files (convention-discovered)
├── commands/              # pack-provided CLI commands
├── doctor/                # diagnostic check scripts
├── patches/               # prompt replacements for imported agents
├── overlay/               # pack-wide overlay files
├── skills/                # current-city-pack skills catalog (imported-pack catalogs later)
├── mcp/                   # current-city-pack MCP server definitions (imported-pack catalogs later)
├── template-fragments/    # prompt template fragments
└── assets/                # opaque pack-owned files (NOT convention-discovered)
```

**What convention replaces:**

| Current mechanism | Convention replacement |
|---|---|
| `[[agent]]` tables in pack.toml | `agents/<name>/` directory exists → agent exists |
| `prompt_template = "prompts/mayor.md"` | `agents/<name>/prompt.md` |
| `[[formula]].path` | File exists in `formulas/` → it's a formula |
| `overlay_dir = "overlay/default"` | `overlay/` + `agents/<name>/overlay/` |
| `scripts_dir = "scripts"` | Gone. Scripts live next to the manifest that uses them (`commands/<id>/run.sh`, `agents/<name>/`) or under `assets/` |
| `[formulas].dir` | Gone. `formulas/` is a fixed convention, not a configurable path |

The rule: **if a standard directory exists, its contents are loaded.** `assets/` is the one exception — it exists but is opaque to the loader, reachable only via explicit path references.

See [doc-directory-conventions.md](doc-directory-conventions.md) for the full directory layout specification, design principles, and pack-local path behavior rules.

#### Formula layering

When multiple packs are imported, formulas layer by priority (lowest to highest):

1. Imported pack formulas (in import declaration order)
2. City pack's own `formulas/`
3. Rig-level imported pack formulas (in import declaration order)

The importing pack always wins over its imports.

### Pack identity and qualified names

After composition, every agent, formula, and prompt retains its pack provenance.

#### Qualified name format

- `gastown.mayor` — the mayor agent from the gastown import
- `swarm.coder` — the coder agent from the swarm import
- `librarian` — a city pack's own agent (no qualifier needed)
- `api-server/gastown.polecat` — rig-scoped with pack provenance

`/<name>` targets the city-scoped version explicitly. `/mayor` means "the city-scoped mayor" from any context. This mirrors filesystem absolute-path semantics (leading slash = from the root).

#### When qualification is required

Bare names work when unambiguous. Qualification is required only when two imported packs export the same agent name. The city pack's own agents are never ambiguous — they always win.

Two imports defining the same bare name is **not** a composition-time error. Both agents exist; both are addressable by their qualified names. The error moves to the *referring* site: any formula, sling target, or named-session template that uses the ambiguous bare name must qualify it. This is the central advantage of named imports over V1 includes — collisions become resolution problems, not load-time failures.

#### Pack global scoping

Pack-wide content like `[global].session_live` applies only to agents that came from the same pack (or its re-exports). In V1, pack globals applied indiscriminately to all agents in the composed city; this is fixed in V2 so an imported pack can't silently inject session state into agents it doesn't own. (`global_fragments` is removed in V2 — replaced by `template-fragments/` with explicit `{{ template }}` inclusion.)

#### `fallback = true` removal

V1 has a `fallback = true` flag on agents that lets a system pack provide a default that user packs silently override. V2 removes the flag entirely. Qualified names plus explicit precedence (city pack always wins over imports) cover the same use cases without the silent-shadowing footgun.

### Transitive closure

A pack is self-contained. Its transitive closure is its directory tree plus its declared imports.

- All paths in pack.toml resolve relative to the pack directory. No `../` escaping.
- Imports are the only mechanism for referencing external content.
- `gc` validates pack self-containment: any resolved path that escapes the pack directory is an error.

### Site binding (`.gc/`)

Per-machine state lives in `.gc/` and is managed by `gc` commands:

| Category | Examples | Set by |
|---|---|---|
| **Identity bindings** | Workspace name, workspace prefix | `gc init`, `gc config set` |
| **Rig bindings** | Rig paths, rig prefixes | `gc rig add` |
| **Operational toggles** | Rig suspended flag | `gc rig suspend/resume` |
| **Machine-local config** | api.bind, session.socket, dolt.host | `gc config set` |
| **Runtime state** | Sessions, beads, caches, logs, sockets | `gc` runtime |

The rule: **if it's in a checked-in TOML file, it's definition or deployment. If it's in `.gc/`, it's site binding.** No gray area.

> **Current rollout:** workspace identity (`workspace.name`, `workspace.prefix`) and `rig.path` now live in `.gc/site.toml`. The loader overlays site binding onto `city.toml` at load time, legacy authored values still read during migration, and `gc doctor --fix` migrates the legacy fields into `.gc/site.toml`. `rig.prefix` and `rig.suspended` remain in `city.toml` for now.

See also [doc-rig-binding-phases.md](doc-rig-binding-phases.md) for the current
Phase A / Phase B split between path extraction and post-15.0 multi-city rig
sharing.

#### Rig lifecycle

A rig has a two-phase lifecycle:

1. **Declared** — `[[rigs]]` entry exists in city.toml (team-shared structure)
2. **Bound** — path binding exists in `.gc/` (machine-local attachment)

A declared-but-unbound rig is a valid state. `gc start` warns about unbound rigs and offers to bind them. This supports the workflow where one teammate adds a rig to city.toml, commits, and other teammates bind it to their local paths after pulling.

## Alternatives considered

- **Keep includes, add qualification.** Doesn't solve weak composition semantics. Includes is fundamentally textual insertion, not module composition.
- **Put all config in one file.** Loses the definition/deployment separation that makes packs portable.
- **Three files (pack.toml, city.toml, city.local.toml).** The third file is unnecessary — machine-local state belongs in `.gc/`, managed by commands, not hand-edited.
- **Keep `formulas_dir` on rigs.** Breaks the "packs are the one unit of composition" principle. A rig-specific local pack achieves the same thing consistently.

## Scope and impact

- **Breaking:** `includes` replaced by `[imports]`. `[[agent]]` tables move to `agents/` directories. `workspace.name` moves to `.gc/`. `fallback = true` removed (replaced by qualified names + explicit precedence). Pack globals are now scoped to the originating pack instead of applying city-wide.
- **New concepts:** Import model with aliasing, versioning, transitive-by-default imports, flattened re-export, single root lock file (`packs.lock`). Lock file consumption (loader is a reader; `gc import` owns bootstrap, repair, and cache materialization). Shadow warnings. Lifecycle verb separation (define / install / register / start).
- **Config split:** Current city.toml splits into pack.toml (definition) + city.toml (deployment) + `.gc/` (site binding).
- **Convention:** Filesystem layout replaces most TOML path declarations.
- **Migration:** Hard cutover. `gc doctor` detects V1 patterns and `gc doctor --fix` handles the safe mechanical conversion. `gc import migrate` is no longer the primary public path. After one release of deprecation warnings, the V2 loader will refuse V1 shapes.

## Resolved questions

Questions from the original proposal that have been settled:

- **Registration verbs:** Binding-flavored. `gc register` binds a city to the controller. `gc start` implies register if not done (zero-config preserved). See "Lifecycle verbs" above.
- **Re-export naming:** **Flattened.** `gastown.dog`, not `gastown.maintenance.dog`. See "Transitive import and export" above.
- **Shadow warnings:** **Warn by default**, with a per-import opt-out (`[imports.X] shadow = "silent"`) for intentional shadowing. Shadowing IS a valid way to "turn off" an agent from an imported pack, but accidental collisions should be visible.
- **Rig-specific formula overrides:** **Rig-local pack.** Consistent with the principle that packs are the one unit of composition. A city is just another rig with different resolution defaults — the city-rig is queried by all other rigs, but rig-rigs are not queried by the city. Rig-local packs achieve formula overrides consistently.
- **`packs/` directory:** **Removed entirely.** There is no `packs/` directory in V2. The top-level pack structure is controlled; embedded packs live under `assets/` and are referenced via explicit import paths. See [doc-directory-conventions.md](doc-directory-conventions.md).
- **Rig vs. city scope disambiguation:** **`/<name>` for city scope.** `/mayor` means "the city-scoped mayor." Mirrors filesystem absolute-path semantics.
- **SHA pinning:** Supported. `version = "sha:<full sha>"` in `[imports.X]`. Documented in [doc-packman.md](doc-packman.md).
- **Transitive imports default:** **Transitive by default.** `transitive = false` is the opt-out for the unusual case.
- **Alias propagation:** **Propagates everywhere.** The local handle IS the runtime identity — sessions, beads, log lines all use the alias. The upstream `[pack].name` is the fallback default for the local handle, but doesn't appear at runtime if the user has aliased.
- **Provider resolution across imports:** **Hybrid (flat namespace, packs contribute via deep merge, city wins).** One global `providers` map. Imported packs' `[providers.X]` blocks merge in. City-level `[providers.X]` always shadows. Bare `provider = "claude"` resolves to the merged result.
- **Settings JSON merge:** **Deep merge** for settings JSON specifically. All other config (TOML keys, lists) is last-writer-wins. The asymmetry is intentional and matches VS Code / most ecosystem conventions.

## Principles

- **A city is just another rig** with different resolution defaults. The city-rig is queried by all other rigs; rig-rigs are not queried by the city. Most "city does X, rig does Y" conditional logic collapses into this framing.
- **Packs are the one unit of composition.** Everything composes through packs — formulas, agents, providers, scripts. There is no second mechanism.
- **Convention defines structure.** If a standard directory exists, its contents are loaded. No TOML declaration needed.
- **Definition / deployment / site binding are physically separated.** pack.toml / city.toml / `.gc/` — no gray area.

## Runtime state storage

v1 stores runtime state in `.gc/` and `~/.gc/` files. Whether some of this should move into beads is an open question, dependent on whether beads DBs are local-only or team-shared. If team-shared, putting per-machine state like `~/.gc/cities.toml` in beads would incorrectly sync across developers. Decision: stay with files for v1; revisit when the local-vs-shared beads question is settled. This is a v.next refactor that doesn't change anything user-visible.

## Open questions

None for the command/doctor surface in this proposal. `commands/<name>/run.sh`
and `doctor/<name>/run.sh` are the settled convention paths; any remaining
manifest symmetry work is tracked in the command-specific docs and issue
backlog.

## Appendix: field placement reference

### Test for placement

- **Definition** (pack.toml) — if someone imported this pack, would this field come along and make sense?
- **Deployment** (city.toml) — would your teammates share this value, but a different deployment of the same pack would not?
- **Site binding** (`.gc/`) — is this per-machine, derived, operational, or does it have durable side effects?

### Identity

| Field | pack.toml | city.toml | `.gc/` | Rationale |
|---|---|---|---|---|
| `[pack].name` | **yes** | | | Pack identity is definition |
| `[pack].version` | **yes** | | | Pack version is definition |
| Workspace name | | | **yes** | Derived from `pack.name` at registration |

### Composition

| Field | pack.toml | city.toml | `.gc/` | Rationale |
|---|---|---|---|---|
| `[imports]` | **yes** | | | What packs compose this city |
| `[defaults.rig.imports.<binding>]` | **yes** | | | Default imports for new rigs |

### Agents and sessions

| Field | pack.toml | city.toml | `.gc/` | Rationale |
|---|---|---|---|---|
| Agent definitions | **yes** | | | Behavioral definition |
| `[[named_session]]` | **yes** | | | Behavioral definition |
| `[agent_defaults]` defaults | **yes** | **yes** | | Pack-wide in pack.toml; city-level overrides in city.toml |
| `[patches]` | **yes** | | | Definition-level modification |

### Providers

| Field | pack.toml | city.toml | `.gc/` | Rationale |
|---|---|---|---|---|
| `[providers]` model/defaults | **yes** | | | Behavioral definition |
| Provider credentials/endpoints | | | **yes** | Per-developer |

### Rigs

| Field | pack.toml | city.toml | `.gc/` | Rationale |
|---|---|---|---|---|
| `[[rigs]].name` | | **yes** | | Structural deployment config |
| `[[rigs]].path` | | | **yes** | Machine-local binding |
| `[[rigs]].prefix` | | | **yes** | Derived, baked into bead IDs |
| `[[rigs]].suspended` | | | **yes** | Operational toggle |
| `[[rigs]].imports` | | **yes** | | Team-shared rig composition |
| `[[rigs]].patches` | | **yes** | | Deployment-specific customization |
| `[[rigs]].max_active_sessions` | | **yes** | | Deployment capacity |

### Runtime substrates

| Field | pack.toml | city.toml | `.gc/` | Rationale |
|---|---|---|---|---|
| `[beads]`, `[session]`, `[events]` | | **yes** | | Substrate choice is deployment |
| `[session].socket` | | | **yes** | Machine-local tmux state |

### Infrastructure

| Field | pack.toml | city.toml | `.gc/` | Rationale |
|---|---|---|---|---|
| `[api].port` | | **yes** | | Team default |
| `[api].bind` | | | **yes** | Machine-local network |
| `[daemon]`, `[orders]`, `[convergence]` | | **yes** | | Deployment behavior |

---END ISSUE---
