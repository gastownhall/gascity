# V2 Loader & Pack Composition — Design

> **Status:** Design description of the v.next loader as proposed in
> [doc-pack-v2.md](doc-pack-v2.md) ([gastownhall/gascity#360](https://github.com/gastownhall/gascity/issues/360)).
> Companion to the current release-branch loader behavior and to the v.next
> design captured here.
> Read them side-by-side to see the diff.

## Conceptual overview

V2 reframes loading around five ideas, all of which are missing or weak in
V1:

1. **A city is a pack.** The root of composition is a `pack.toml` (the
   *definition*) plus a companion `city.toml` (the *deployment plan*) plus
   `.gc/` (per-machine *site binding*). Delete `city.toml` and what
   remains is a valid, importable pack. The loader's root case becomes
   "load a pack, then layer the deployment file on top."

2. **Imports replace includes.** A pack composes other packs through
   named bindings (`[imports.gastown]`), not textual concatenation. Each
   import has a durable name that survives composition: after loading,
   `gastown.mayor` is a real, addressable thing — not just an `Agent`
   that happens to be called `mayor`. Imports are versioned, aliasable,
   and transitive by default (with `transitive = false` opt-out).

3. **Convention defines structure.** A pack's filesystem layout *is* its
   declaration. If `agents/foo/` exists, an agent named `foo` exists. If
   `formulas/bar.formula.toml` exists, a formula named `bar` exists. The
   loader discovers content by walking standard directories instead of
   reading explicit `[[agent]]` and `[[formula]].path` declarations.

4. **Packs are self-contained.** A pack's transitive closure is its
   directory tree plus its declared imports. Any path that resolves
   outside the pack directory is a load-time error. This makes packs
   portable in a way V1 packs are not.

5. **Definition / deployment / site binding are physically separated.**
   `pack.toml` carries definition. `city.toml` carries deployment
   (rigs, substrates, capacity). `.gc/` carries site binding (paths,
   prefixes, suspended flags, machine-local credentials). The loader
   reads from all three but never confuses them; commands like
   `gc rig add` write to `.gc/`, not to checked-in TOML.

For the first skills/MCP slice, the loader only discovers the current
city pack's `skills/` and `mcp/` catalogs. Imported-pack catalogs are a
later wave.

The output is still a flattened `City` value plus `Provenance`, but the
internal model carries qualified names throughout instead of resolving
collisions by load order.

## Top-level entry point

The single public entrypoint is conceptually unchanged but takes a
broader input: the city directory rather than a single TOML file.

```go
// internal/config/config.go (proposed)
func LoadCity(
    fs fsys.FS,
    cityDir string,
    extraIncludes ...string,
) (*City, *Provenance, error)
```

`cityDir` is the directory containing both `pack.toml` and (optionally)
`city.toml` and `.gc/`. `extraIncludes` continues to mean CLI-supplied
fragment paths, kept for parity with `-f` and for `gc init` flows that
need to inject system fragments.

`Provenance` extends V1's audit trail to track *qualified names* and
*import bindings* in addition to source files:

```go
type Provenance struct {
    Root          string
    Sources       []string
    Agents        map[string]string  // qualified name → file
    Imports       map[string]ImportProvenance  // binding name → import details
    Rigs          map[string]string
    SiteBindings  map[string]string  // .gc/-sourced fields
    Workspace     map[string]string
    Warnings      []string
}

type ImportProvenance struct {
    BindingName  string  // e.g., "gastown" or alias "gs"
    PackName     string  // pack.name from the imported pack
    Source       string  // resolved source string
    Version      string  // resolved version (semver or "local")
    Commit       string  // resolved commit hash for remote imports
    Exported     bool    // re-exported by parent
    Path         string  // on-disk location after fetch
}
```

## Core data structures

### `City`

```go
type City struct {
    // Root pack — what this city IS.
    Pack         Pack

    // Deployment file — how this city RUNS.
    Deployment   Deployment

    // Site binding — machine-local attachments.
    SiteBinding  SiteBinding

    // Composed view (derived).
    Agents             []Agent
    NamedSessions      []NamedSession
    Providers          map[string]ProviderSpec
    FormulaLayers      FormulaLayers
    ScriptLayers       ScriptLayers
    OverlayLayers      OverlayLayers
    Patches            Patches             // applied during compose

    // Per-rig composed views.
    Rigs               []Rig

    // Resolved import graph (for inspection / gc commands).
    ImportGraph        ImportGraph

    // Derived identity.
    ResolvedWorkspaceName string  // from SiteBinding or Pack.Meta.Name fallback
}
```

`Pack`, `Deployment`, and `SiteBinding` are the three on-disk inputs.
Everything else is derived during composition.

### `Pack`

The contents of `pack.toml`:

```go
type Pack struct {
    Meta             PackMeta
    Imports          map[string]Import
    DefaultRig       DefaultRigPolicy   // [defaults.rig.imports]
    AgentDefaults    AgentDefaults      // [agent_defaults]
    Providers        map[string]ProviderSpec
    NamedSessions    []NamedSession
    Patches          Patches
}

type PackMeta struct {
    Name        string
    Version     string
    Schema      int
    RequiresGc  string
    Description string
}
```

Notably absent: `[[agent]]`, `[[formula]]`, `[[order]]`, `[[script]]`,
`overlay_dir`, `prompt_template`, `formulas_dir`, `scripts_dir`. All of
these are replaced by directory walks.

### `Import`

```go
type Import struct {
    Source   string  // ./packs/x, github.com/org/x, etc.
    Version  string  // semver constraint; empty for local paths
    Export   bool    // re-export to parents
    // Resolved at load time:
    Path         string  // on-disk location
    ResolvedVer  string  // commit hash or "local"
    Pack         *Pack   // loaded pack metadata
}
```

`Source` accepts the same three formats as V1 includes (local, remote
git, github tree URL), but the *meaning* is different: an import is a
named binding, not an inline insertion.

### `Deployment` (city.toml)

```go
type Deployment struct {
    Beads        BeadsConfig
    Session      SessionConfig
    Mail         MailConfig
    Events       EventsConfig
    Daemon       DaemonConfig
    Orders       OrdersConfig
    API          APIConfig

    Rigs         []DeploymentRig
}

type DeploymentRig struct {
    Name              string
    Imports           map[string]Import  // [rigs.imports.X]
    Patches           Patches
    MaxActiveSessions int
    DefaultSlingTarget string
    SessionSleep      DurationConfig
    // ...other deployment knobs
}
```

Notably absent from `city.toml`: identity (`workspace.name`), site
binding (`rig.path`, `rig.prefix`, `rig.suspended`), or any `[pack]`
content.

### `SiteBinding` (`.gc/`)

```go
type SiteBinding struct {
    WorkspaceName    string
    WorkspacePrefix  string

    RigBindings      map[string]RigBinding   // by rig name
    LocalConfig      map[string]string       // api.bind, dolt.host, etc.
}

type RigBinding struct {
    Name      string
    Path      string
    Prefix    string
    Suspended bool
}
```

`SiteBinding` is read from `.gc/` files but **never written by the
loader**. Mutations come from commands (`gc init`, `gc rig add`,
`gc rig suspend`, etc.). The loader treats `.gc/` as read-only.

### `Agent`

`Agent` keeps most of its V1 fields, but composition-relevant identity
changes:

| Field | V1 | V2 |
|---|---|---|
| `Name` | bare name | bare name (e.g., `mayor`) |
| `Dir` | rig prefix or empty | rig prefix or empty |
| `BindingName` | — | name of the `[imports.X]` block this agent came from (`""` for the city pack itself) |
| `PackName` | — | `pack.name` of the pack the agent came from |
| `QualifiedName()` | `Dir/Name` | `Dir/BindingName.Name` (with simplification when `BindingName == ""` or unambiguous) |

The `BindingName` is what makes `gastown.mayor` addressable as a real
identity throughout the runtime. It's set during import expansion and
travels with the agent forever.

### `Rig`

```go
type Rig struct {
    // From city.toml [[rigs]]
    Name              string
    Imports           map[string]Import
    Patches           Patches
    MaxActiveSessions int
    DefaultSlingTarget string

    // From .gc/ site binding
    Path       string
    Prefix     string
    Suspended  bool
    Bound      bool   // true if .gc/ has a binding for this rig

    // Derived
    Agents          []Agent
    FormulaLayers   FormulaLayers
    ImportGraph     ImportGraph
}
```

A rig is now a *two-phase object*: declared in `city.toml` (structural),
bound in `.gc/` (machine-local). A declared-but-unbound rig is a valid
state; the loader produces it but `gc start` warns and offers to bind.

### `ImportGraph`

A new top-level structure that V1 doesn't have. It records the resolved
import DAG so commands like `gc deps`, `gc why <agent>`, and `gc upgrade`
can answer "where did this come from?" without re-running composition:

```go
type ImportGraph struct {
    Root  *ImportNode
    All   map[string]*ImportNode  // by qualified binding path, e.g., "gastown" or "gastown.maintenance"
}

type ImportNode struct {
    Binding   string       // flat binding name (e.g., "gastown")
    Pack      *Pack
    Source    string
    Version   string
    Commit    string
    Exported  bool
    Children  []*ImportNode
}
// Note: re-exported names are FLATTENED. If gastown re-exports
// maintenance's "dog" agent, the city sees it as "gastown.dog",
// not "gastown.maintenance.dog". The ImportGraph preserves the
// full tree for tooling (gc why), but addressable names use the
// re-exporting pack's binding.
```

## Pack files

A pack is a directory with a `pack.toml` at its root and any of these
standard subdirectories. **If the directory exists, its contents are
loaded** — no TOML declaration required.

```
my-pack/
├── pack.toml              # metadata, imports, agent defaults, patches
├── agents/                # agent definitions (one dir per agent)
│   └── mayor/
│       ├── agent.toml
│       ├── prompt.md
│       ├── overlay/       # per-agent overlays
│       ├── skills/        # per-agent skills
│       └── mcp/           # per-agent MCP defs
├── formulas/              # *.toml formula files
├── orders/                # *.toml order files
├── commands/              # pack-provided CLI commands (per-entry dirs)
│   └── status/
│       ├── command.toml   # optional; only when defaults aren't enough
│       ├── run.sh         # default entrypoint
│       └── help.md        # default help file
├── doctor/                # diagnostic checks (parallel to commands)
│   └── git-clean/
│       ├── run.sh
│       └── help.md
├── patches/               # prompt replacements for imported agents
├── overlays/              # pack-wide overlay files
├── skills/                # current-city-pack skills
├── mcp/                   # current-city-pack MCP server definitions
├── template-fragments/    # prompt template fragments
└── assets/                # opaque pack-owned files (not discovered by convention)
```

The top level is controlled — standard names are recognized, unknowns are errors,
`assets/` is the one opaque bucket. There is no `scripts/` directory; scripts
live next to the manifest that uses them or under `assets/`. See
[doc-directory-conventions.md](doc-directory-conventions.md) and
[doc-commands.md](doc-commands.md).

`pack.toml` carries metadata, imports, and agent defaults — *not* a list
of agents:

```toml
[pack]
name        = "gastown"
version     = "1.2.0"
schema      = 2
requires_gc = ">=0.20"

[imports.maintenance]
source  = "../maintenance"
export  = false

[imports.util]
source  = "github.com/org/util"
version = "^1.4"

[defaults.rig.imports.gastown]
source = "./packs/gastown"

[agent_defaults]
provider = "claude"
scope    = "rig"

[providers.claude]
model = "claude-sonnet-4"
```

## Convention-based loading

The biggest workload shift from V1 → V2 is in *how* per-pack content is
discovered. V1 reads explicit declarations from `pack.toml`; V2 walks
standard subdirectories.

| Content type | V1 source | V2 source |
|---|---|---|
| Agents | `[[agent]]` tables | `agents/<name>/` directories |
| Agent prompts | `prompt_template = "prompts/x.md"` | `agents/<name>/prompt.md` |
| Per-agent overlays | `overlay_dir = "overlays/x"` | `agents/<name>/overlay/` |
| Pack-wide overlays | `overlay_dir = "overlays/default"` | `overlays/` directory |
| Formulas | `[[formula]].path` + dir scan | `formulas/*.toml` directly |
| Orders | inside formulas | `orders/*.toml` (top-level, convention-discovered) |
| Scripts | `scripts_dir = "scripts"` | **Gone.** Scripts live next to the manifest that uses them (`commands/<id>/run.sh`, `agents/<name>/`) or under `assets/` |
| Skills | n/a | `skills/` directory in the current city pack + `agents/<name>/skills/` (per-agent); imported-pack catalogs are later |
| MCP defs | n/a | `mcp/` directory in the current city pack + `agents/<name>/mcp/` (per-agent); imported-pack catalogs are later |
| Template fragments | inline strings | `template-fragments/` (pack-wide) + `agents/<name>/template-fragments/` (per-agent) |
| Commands | `[[commands]]` in pack.toml | `commands/<id>/` directories (convention-based; optional `command.toml` manifest) |
| Doctor checks | n/a | `doctor/<id>/` directories (convention-based; optional `doctor.toml` manifest) |
| Opaque assets | scattered | `assets/` directory (loader-opaque, reached only via explicit path references) |

The top level of a pack is **controlled surface area**. Standard directory
names are explicitly recognized; unknown top-level directories are errors.
`assets/` is the one opaque escape hatch. See
[doc-directory-conventions.md](doc-directory-conventions.md) for the full
layout specification.

The walk is shallow and predictable: each directory has a known schema
(file extension or per-entry `agent.toml`). Anything that doesn't match
the schema is a warning, not silently ignored.

`agent.toml` inside an agent directory carries the per-agent fields that
used to live in `[[agent]]` (provider, session lifecycle, work_query,
etc.) — minus the path fields, since those are now implicit.

## Pack reference formats

Imports support the same three reference formats as V1 includes, but
because they're versioned, the parser is stricter and the resolution
is materially different.

```toml
# 1. Local path (no version constraint)
[imports.maint]
source = "../maintenance"

# 2. Remote git, semver constraint
[imports.gastown]
source  = "github.com/gastownhall/gastown"
version = "^1.2"

# 3. Local pack inside the city's assets/ directory
[imports.helper]
source = "./assets/local/helper"
```

Local paths *cannot* take a version. Remote sources *must* take a
version (or pin to a commit). The loader rejects ambiguity.

The actual fetch / cache mechanism is owned by `gc import`
([doc-packman.md](doc-packman.md)), not the loader. The loader assumes
imports have already been resolved into local directories under the
hidden cache (`~/.gc/cache/repos/<sha256(url+commit)>/`) and reads the
lock file (`pack.lock`) to know which commit to use.

This is a significant separation-of-concerns change. In V1, the loader
itself clones git repos. In V2, that responsibility moves to
`gc import` and the loader becomes purely a reader.

## Lock file consumption

The loader reads, but does not write, the lock file produced by
`gc import install`. `pack.lock` is part of config/import management,
not a loader concern — the loader assumes composed config is correct
([#583](https://github.com/gastownhall/gascity/issues/583)). Each `[imports.X]` block in `pack.toml` (or
`[rigs.imports.X]` in `city.toml`) is paired with a `[packs.X]` block
in the lock file:

```toml
# pack.toml
[imports.gastown]
source  = "github.com/gastownhall/gastown"
version = "^1.2"
```

```toml
# pack.lock
[packs.gastown]
source  = "github.com/gastownhall/gastown"
commit  = "abc123..."
version = "1.4.2"
parent  = "(root)"
```

For each declared import, the loader looks up the matching `[packs.X]`
record, finds the cached directory under the corresponding sha256 key,
and proceeds. If no match exists, that's a load-time error telling the
user to run `gc import install`.

The `parent` field records who introduced this pack into the graph
(`(root)` for direct imports, `(implicit)` for system packs spliced via
`~/.gc/implicit-import.toml`, or another binding name for transitive
imports).

## Composition pipeline

The new pipeline runs in this order. Numbered for parallel comparison
to V1's 14 steps.

### 1. Locate the city

Resolve `cityDir`, find `pack.toml` (required), `city.toml` (required
for a city, absent for a non-city pack load), and `.gc/` (optional).

### 2. Parse the root pack

Decode `pack.toml` into `Pack` with `parsePack()`. Same TOML metadata
collection as V1 for "was this field set?" decisions.

### 3. Parse the deployment file

Decode `city.toml` into `Deployment`. The two files are parsed
independently — the loader does not silently merge them.

### 4. Read site binding

Walk `.gc/` and populate `SiteBinding`. Read-only.

### 5. Initialize provenance and import graph

Fresh `Provenance`. Empty `ImportGraph` with the root pack as `Root`.

### 6. Apply CLI fragments

`extraIncludes` are still respected for backward compatibility, but they
target `pack.toml`-equivalent content only. Each fragment is loaded and
folded into the in-memory `Pack` using the same per-section rules as V1
(concat for slices, deep merge for maps, last-writer-wins for scalars
with warnings).

System packs are no longer injected here. They flow through the
implicit-imports mechanism (see step 9 below).

### 7. Validate self-containment of the root pack

Walk the root pack's directory tree. Any path resolved from `pack.toml`
that escapes the pack directory is a hard error. This is the new
"transitive closure" check that V1 lacks.

### 8. Resolve direct imports

For each entry in `Pack.Imports`:

1. Look up the matching `[packs.X]` lock record. Error if missing.
2. Resolve the on-disk path (the sha256 cache directory).
3. Parse the imported pack's `pack.toml`.
4. Validate the imported pack is self-contained.
5. Validate the imported pack's `pack.name` matches the lock record (or
   warn if the binding name aliases it).
6. Create an `ImportNode` and attach it as a child of the root.

### 9. Splice implicit imports

Read `~/.gc/implicit-import.toml` (managed by `gc import`). For each
implicit binding not already imported by the root, splice it into the
root's import set with `parent = "(implicit)"` recorded in provenance.
This is how `import` and `registry` themselves end up loaded for every
city without the city declaring them.

### 10. Resolve transitive imports

Walk the import DAG depth-first. For each imported pack, recursively
resolve its own `[imports.X]` against the **root city's single lock
file** (not per-pack locks — the root lock contains the entire
transitive graph). Mark any import flagged `export = true` as visible
to the parent's parent.

The DAG must be a tree (cycles are an error). Each node carries:
- The binding name *as the root sees it* (qualified path,
  `gastown.maintenance`).
- The original binding name inside the importing pack (`maintenance`).
- The export flag.
- The resolved version, source, commit.

### 11. Compose city-pack agents

For each pack reachable from the root through non-rig imports (i.e.,
visible to the city scope, including transitive re-exports):

1. Walk the pack's `agents/` directory.
2. For each `agents/<name>/` subdirectory, parse `agent.toml` (or
   defaults if missing) and load `prompt.md`, `overlay/`, etc.
3. Stamp each agent with `BindingName` (the qualified path the root sees
   it under) and `PackName`.
4. Filter by `scope`: keep `scope="city"` and unscoped agents; drop
   `scope="rig"`.
5. Apply the pack's `[agent_defaults]` defaults to its own agents.
6. Add to `City.Agents`.

The city pack itself is processed last so its agents win against any
imports without needing fallback resolution.

### 12. Handle name collisions

V2's collision rules are stricter and simpler than V1's:

- **Within a single pack:** impossible.
- **City pack vs. import:** city pack always wins. A **warning is emitted
  by default**; the user can suppress it per-import with
  `[imports.X] shadow = "silent"` when the shadowing is intentional.
- **Two imports define the same bare name:** **not an error**. Both
  agents exist; both are addressable by qualified name (`gastown.mayor`,
  `swarm.mayor`). The bare name `mayor` becomes ambiguous and any
  reference to it elsewhere (formulas, sling targets) must qualify.
- **Bare name reference to an ambiguous name:** error at the *referring*
  site, not at composition time.

This is the core advantage of qualified names: collisions stop being
errors at the composition layer and become resolution problems at the
reference layer.

### 13. Apply patches

`pack.Patches` (root and all imported packs) and `deployment.Rigs[].Patches`
(rig-specific) apply against the composed agent set. Targeting is by
qualified name now: `[[patches]]` can target `gastown.mayor` directly.
Bare-name targeting still works when unambiguous.

Patches from imported packs are scoped to the agents *they brought in*.
A pack cannot patch agents it didn't define.

### 14. Compose rig agents

For each rig declared in `Deployment.Rigs`:

1. Read the rig's `Imports` (rig-scoped imports from `[rigs.imports.X]`).
2. Resolve each import the same way as step 8 (lock-file-based).
3. Walk each imported pack's `agents/` and load with `scope="rig"` filter.
4. Stamp agents with `Dir = rig.Name` *and* `BindingName`.
5. Apply rig-level patches.
6. Compute formula layers for this rig (see step 16).

### 15. Apply pack globals

Each pack can declare `[global]` content (currently `session_live`).
`global_fragments` is removed in V2 — replaced by `template-fragments/`
with explicit `{{ template }}` inclusion. Pack globals apply to:

- City-pack `[global]`: applies to all city-scope agents.
- Imported-pack `[global]`: applies only to agents *that came from
  that pack* (or its re-exports). This is a *fix* relative to V1, where
  pack globals applied indiscriminately to all agents.
- Rig-import `[global]`: scoped to that rig's agent set.

### 16. Compute formula and asset layers

Layered from lowest to highest priority:

1. Imported pack formulas (in import declaration order).
2. The city pack's own `formulas/`.
3. Rig-level imported pack formulas (in import declaration order).
4. (No "rig local" layer — rigs no longer have a `formulas_dir`. If
   you need rig-specific formulas, declare a rig-scoped local pack.)

The "importing pack always wins over its imports" rule is preserved.
Same layering scheme for overlays, skills, mcp, and template-fragments.
For the first slice, that layering applies only within the current city
pack; imported-pack skill/MCP catalogs are later.

**Note:** there is no `ScriptLayers` in V2. The `scripts/` directory is
gone; scripts live next to the manifests that use them (`commands/<id>/`,
`doctor/<id>/`, `agents/<name>/`) or under `assets/`.

### 17. Inject implicit agents (built-in providers)

Same as V1: create implicit agents for **configured providers only** (the
city's `[providers]` entries plus the builtin provider matching
`workspace.provider`, plus the control-dispatcher when enabled). Not
every built-in provider gets an implicit agent — only those the city has
explicitly configured or referenced. This logic is unchanged from V1.

### 18. Apply agent defaults

Same as V1 step 11: `[agent_defaults]` defaults from the city pack apply to all
agents that don't override. Imported pack `[agent_defaults]` defaults apply only
to that pack's own agents (already handled in step 11).

### 19. Bind site state

For each declared rig, look up its binding in `SiteBinding`. Populate
`Path`, `Prefix`, `Suspended`, set `Bound = true`. Unbound rigs get
`Bound = false` and a warning.

For workspace identity: `City.ResolvedWorkspaceName = SiteBinding.WorkspaceName`
(falling back to `Pack.Meta.Name` if no binding exists, with a warning).

### 20. Validate

Three passes, same shape as V1:

1. **Named sessions** — template references must point at agents that
   exist in the composed view (qualified or unambiguous).
2. **Durations** — every duration string parses.
3. **Semantics** — pool config, work_query / sling_query consistency,
   agent scope vs rig availability, *plus new V2 checks*:
   - All `pack.requires` are satisfied.
   - No path in any pack escapes its directory.
   - Every imported pack's `pack.name` matches the lock record (or the
     binding name, if aliased).
   - Every reference to an ambiguous bare name is qualified.
   - The import graph has no cycles.

### 21. Load namepools

Same as V1 step 14, unchanged.

### 22. Return

Return `(City, Provenance, nil)`. The `City` carries `ImportGraph`,
`Pack`, `Deployment`, `SiteBinding`, plus the composed agent set.

## Collision and precedence — the V1→V2 diff

| Concern | V1 | V2 |
|---|---|---|
| Two packs define `mayor` | Error or fallback resolution | Both exist as `gastown.mayor` and `swarm.mayor`; bare `mayor` is ambiguous |
| City and pack both define `mayor` | City wins via prepend ordering | City wins explicitly; optional warning |
| `fallback = true` on agents | Used for soft-overrideable defaults | **Removed.** Qualified names + explicit precedence make it unnecessary |
| Provider collisions | Per-field deep merge with warnings | Same |
| Workspace field collisions | Per-field merge with warnings | N/A — workspace identity moves to `.gc/` |
| Pack name collisions in `[packs.X]` | Last-writer-wins with warning | Lock file is canonical; multiple imports of the same pack at different versions are an error |
| Patches missing target | Error | Error (qualified-name aware) |
| Path escapes pack directory | Allowed | Hard error |
| Cyclic imports | N/A (includes are flat) | Hard error |

`fallback = true` is the most notable casualty. It exists in V1 as a way
to let a system pack provide a default agent that user packs can
silently override. In V2, the same effect is achieved by qualified names
and explicit shadowing: the system pack provides `system.mayor`, the
user pack provides `mine.mayor`, and the city pack chooses which to
reference. There's no need for silent overriding because there's no
ambiguity.

## Provider resolution

Provider resolution uses a **hybrid flat model**: one global `providers`
map, with imported packs contributing via per-field deep merge and the
city pack always winning.

1. **Provider namespace is flat.** There is one global `providers` map,
   not per-pack namespaces. An imported pack's `[providers.claude]`
   merges into the global map using the same per-field deep-merge
   semantics as V1 (scalar fields: override + warn; slice fields:
   replace; map fields: additive). The city pack's `[providers.claude]`
   always shadows any imported pack's definition.

2. **Agent `provider = "claude"` resolves to the merged result.** No
   qualified provider references needed. The resolution chain is:
   `agent.StartCommand` (escape hatch) → `agent.Provider` → merged
   global `providers[name]` → built-in preset → auto-detect via PATH.

3. **Built-in provider list is unchanged.** Same canonical names
   (claude, codex, gemini, cursor, copilot, amp, opencode, auggie,
   pi, omp).

## Where the loader is called from

Same call sites as V1 (`cmd_start.go`, `cmd_config.go`, `cmd_agent.go`,
`cmd_init.go`), but the function signature changes from
`LoadWithIncludes(fs, "city.toml", -f...)` to
`LoadCity(fs, cityDir, -f...)`. Callers update their argument once.

## Atomic writes and git safety

The loader no longer clones git. The atomic-write and git-env-blacklist
mechanisms move out of `internal/config/` and into `gc import` (which
owns the cache under `~/.gc/cache/repos/`). The loader becomes a pure
reader and gains no new I/O surface.

Lock-file *reading* is the only new I/O the loader does, and it's
read-only.

## Migration story

Cities running on V1 must be converted before V2 can load them. Hard
cutover: `gc doctor` detects V1 patterns and `gc doctor --fix` handles
the safe mechanical conversion. `gc import migrate` is no longer the
primary public path.

The migration is sequenced in two steps matching the implementation order:

### Step 1: Pack/city restructuring (ships first)

1. **`includes` → `[imports]`.** For each `workspace.includes` entry:
   - Local path: synthesize `[imports.<basename>]` with `source = path`.
   - Git-backed source: synthesize `[imports.<repo-name>]` with `source`
     and `version`. Semver tags become version constraints. Untagged git
     sources are pinned with an exact SHA (`version = "sha:<commit>"`).
2. **`workspace.name` → `.gc/`.** Run `gc init` against the existing
   directory; populate `.gc/` with the workspace name and prefix.
3. **`rig.path`, `rig.prefix`, `rig.suspended` → `.gc/`.** For each
   rig, write a binding file under `.gc/rigs/<name>.toml`.
4. **`workspace.default_rig_includes` → `[defaults.rig.imports]`.** Same
   mapping as `includes` → `[imports]`.
5. **`fallback = true` agents.** Drop the field; warn the user about
   any agents that previously relied on fallback shadowing and may need
   manual disambiguation.

`[[agent]]` tables in pack.toml continue to work during this step.

### Step 2: Agent-as-directory (ships in the same release)

6. **`[[agent]]` tables → `agents/<name>/` directories.** For each
   `[[agent]]` block, create `agents/<name>/agent.toml` with the
   non-path fields, move `prompt_template` content to `prompt.md`,
   move `overlay_dir` content to `overlay/`.

The migration is mechanical for the common case and produces a working
V2 city. Edge cases (name collisions previously masked by fallback,
legacy git sources with no semver tags, or other unsafe rewrites) emit
warnings the user must review or leave for manual follow-up.

## Summary: full pipeline in one list

1. Locate city (`pack.toml` + `city.toml` + `.gc/`).
2. Parse root `pack.toml` → `Pack`.
3. Parse `city.toml` → `Deployment`.
4. Read `.gc/` → `SiteBinding` (read-only).
5. Initialize `Provenance` and `ImportGraph`.
6. Apply CLI fragments to `Pack` (per-section merge).
7. Validate root pack self-containment (no path escapes).
8. Resolve direct imports against lock file → cache directories.
9. Splice implicit imports from `~/.gc/implicit-import.toml`.
10. Resolve transitive imports DFS, honoring `export = true`.
11. Compose city-scope agents from imported + city packs (qualified names).
12. Detect ambiguous bare names; record but do not error.
13. Apply patches (qualified-name aware) against composed set.
14. For each rig: resolve rig imports, compose rig agents, apply rig patches.
15. Apply pack globals (scoped to originating pack's agents).
16. Compute formula / overlay / skill / mcp / template-fragment layers (no script layers — scripts are entry-local).
17. Inject implicit agents for built-in providers.
18. Apply `[agent_defaults]` defaults.
19. Bind site state (rig paths, workspace name).
20. Validate (named sessions, durations, semantics + V2-specific checks).
21. Load namepools.
22. Return `(City, Provenance, nil)`.

`Provenance` and `ImportGraph` accumulate throughout, providing the
audit trail every command needs to answer "where did this come from?"
without re-running composition.
