---
title: Gas City Pack Specification (2.0)
description: Authoritative specification for Gas City pack format and loading semantics.
---

# Gas City Pack Specification (2.0)

| Field | Value |
|---|---|
| Status | Authoritative specification |
| Last verified | 2026-05-22 |
| Pack schema | 2 |
| Primary implementation | `internal/config/pack.go`, `internal/config/config.go`, `internal/config/compose.go` |
| User-facing guide | `docs/guides/reusing-and-customizing-packs.md` |

This document specifies the Gas City pack format as a data model, file format,
and loading process. **PackV2** is the shorthand name for the Gas City Pack
Specification (2.0). Design notes may explain why a future direction is
attractive, but this document is the authoritative source for PackV2.

The key words "must", "must not", "required", "shall", "shall not", "should",
"should not", and "may" are to be interpreted as normative requirements unless
the paragraph is explicitly marked as non-normative.

## 0. Data And Information Model

PackV2 separates the *format* of a pack from the *loading* of a pack. The format
is the directory and TOML data a pack author writes. Loading is the process that
resolves dependencies, stamps definitions into a city or rig context, applies
patches and defaults, and produces one effective `City` configuration.

### 0.1. Pack

A pack is a directory containing a `pack.toml` file and zero or more
definition, asset, and support files. The `pack.toml` file is the pack's
metadata and manifest. Other files are either definition files discovered by
well-known rules or private files referenced by TOML fields.

A pack may be used in three contexts:

1. As the root pack of a city. The root pack is the city directory containing
   `city.toml` and the root `pack.toml`.
2. As a city-level imported pack. Its city-scoped and unscoped definitions are
   loaded into the city-level surface.
3. As a rig-level imported pack. Its rig-scoped and unscoped definitions are
   loaded into a specific rig surface and stamped with that rig's name.

### 0.2. Pack Identity

A pack identity consists of:

```text
PackIdentity {
    name: string
    schema: integer
    version: optional string
    requires_gc: optional string
}
```

The `name` identifies the pack as a product and as a provenance label. It must
be present in `[pack]`.

The `schema` identifies the PackV2 file-format schema. The schema specified by
this document is `2`. A loader must reject a pack whose
schema is omitted, zero, or greater than the loader's supported schema.

The `version` is pack metadata. Version selection is controlled by import
constraints and lockfile resolution, not by a loader comparing `[pack].version`
directly.

The `requires_gc` field is optional metadata for the minimum compatible `gc`
version. It is parsed as pack metadata.

### 0.3. Pack Contents

A pack may contain the following abstract content:

```text
PackContents {
    imports: ordered set of PackImport
    agents: list of AgentDefinition
    named_sessions: list of NamedSessionDefinition
    services: list of ServiceDefinition
    providers: map<string, ProviderDefinition>
    formulas: optional FormulaDirectoryDeclaration
    patches: optional PatchSet
    doctor_checks: list of DoctorCheck
    commands: list of PackCommand
    globals: optional GlobalSessionLiveDefinition
    assets: file tree
}
```

The concrete `pack.toml` fields that encode these contents are specified in
section 1.2.

### 0.4. Public And Private Content

PackV2 currently loads pack definitions by scope. It does not implement the
proposed `[export]` surface yet.

Definitions in `pack.toml` are therefore visible to consumers according to
loader scope rules, not according to an explicit export manifest. Files under
`assets/` are private support files unless a public definition references them.
Other non-reserved files are also private support files unless referenced by a
definition.

The current loader does not give import bindings a runtime namespace for agents.
If a city-level imported pack defines an agent named `reviewer`, the effective
city-level agent name is `reviewer`, not `<binding>/reviewer`.

## 1. File System Structure

A PackV2 directory has this abstract shape:

```text
pack-root/
  pack.toml
  assets/
  formulas/
  overlay/
  scripts/
  ...
```

Only `pack.toml` is required.

### 1.1. Directory Rules

The pack root must be a directory. The pack root must contain a file named
`pack.toml`.

The following top-level paths are reserved by PackV2:

| Path | Kind | Meaning |
|---|---|---|
| `pack.toml` | file | Required pack manifest and metadata. |
| `assets/` | directory | Preferred location for private implementation files. |
| `formulas/` | directory | Well-known formula directory. |
| `overlay/` | directory | Pack-level overlay directory collected automatically from imported packs. |
| `scripts/` | directory | Pack-level scripts directory collected automatically from imported packs. |

The following top-level paths are conventional and allowed:

| Path | Kind | Meaning |
|---|---|---|
| `commands/` | directory | Conventional location for scripts referenced by `[[commands]]`. |
| `doctor/` | directory | Conventional location for scripts referenced by `[[doctor]]`. |
| `namepools/` | directory | Conventional location for files referenced by agent `namepool`. |
| `prompts/` | directory | Conventional location for prompt templates referenced by agents. |
| `skills/` | directory | Conventional location for skill files shipped with a pack. |
| `orders/` | directory | Conventional location for order definitions. |
| `mcps/` | directory | Conventional location for MCP-related configuration. |

Pack authors may include additional private files and directories. New
machine-readable top-level directories should be added to this specification
before they are treated as part of the PackV2 format.

The following file-system constructs must not be used as public PackV2 format:

| Construct | Rule |
|---|---|
| Cache directories | A checked-in `pack.toml` must not point at Gas City's local cache as a durable dependency. |
| Registry handles | A checked-in `pack.toml` must not persist command-time handles such as `main:gascity`. |
| Consumer rig names inside reusable packs | A reusable pack must not assume the names of rigs that will import it. |

### 1.2. The `pack.toml` File

The `pack.toml` file is UTF-8 TOML. It must contain a `[pack]` table.

Conceptually, the file has this structure:

```text
PackToml {
    pack: PackMeta
    imports: map<string, PackImport>
    agent: list<Agent>
    named_session: list<NamedSession>
    service: list<Service>
    providers: map<string, ProviderSpec>
    formulas: FormulasConfig
    patches: Patches
    doctor: list<PackDoctorEntry>
    commands: list<PackCommandEntry>
    global: PackGlobal
}
```

Unknown fields are not part of this specification. Current loader behavior may
warn about unknown keys rather than failing immediately; authors must not rely
on unknown keys being preserved or interpreted.

### 1.2.1. `[pack]`

The `[pack]` table defines pack metadata.

```toml
[pack]
name = "gascity"
schema = 2
version = "1.4.0"
requires_gc = ">=0.13.0"
```

| Field | Type | Required | Rule |
|---|---|---|---|
| `name` | string | yes | Pack identifier and provenance label. Must not be empty. |
| `schema` | integer | yes | Pack format version. Must be `2` for this specification. |
| `version` | string | no | Pack version metadata. |
| `requires_gc` | string | no | Minimum compatible `gc` version metadata. |
| `includes` | array of string | legacy | Legacy pack composition list. New packs should use `[imports.<binding>]`. |
| `requires` | array of tables | no | Agent requirements validated after expansion. |

The `includes` field remains a compatibility surface in `pack.toml`. It should
not be used in newly-authored packs.

### 1.2.2. `[pack.requires]`

Each `[[pack.requires]]` entry declares an agent that must exist after loading.

```toml
[[pack.requires]]
scope = "city"
agent = "reviewer"
```

| Field | Type | Required | Rule |
|---|---|---|---|
| `scope` | string | yes | Must be `city` or `rig`. |
| `agent` | string | yes | Required agent local name. Must not be empty. |

City-scoped requirements are validated against the expanded city agent list.
Rig-scoped requirements are validated while loading the pack for a rig.

### 1.2.3. `[imports.<binding>]`

Pack imports are named dependencies.

```toml
[imports.gascity]
source = "https://packages.example/main/gascity"
version = "^1"
```

The binding name is local to the importing file. Current loader behavior uses
binding names for deterministic ordering of imports. It does not add binding
names to runtime agent identities.

| Field | Type | Required | Rule |
|---|---|---|---|
| `source` | string | yes | Durable source for the pack root. Must not be empty. |
| `version` | string | no | Compatibility constraint for versioned sources. |

Public import TOML must not use fields named `path`, `ref`, `commit`, or
`hash`. Registry handles such as `main:gascity` are command-time lookup handles
and must not be persisted as `source`.

Imports in `city.toml` use the same table shape for the root pack:

```toml
[imports.gascity]
source = "../packs/gascity"
```

Rig imports are written under the `[[rigs]]` table they apply to:

```toml
[[rigs]]
name = "checkout-service"
path = "../checkout-service"

[rigs.imports.gascity]
source = "../packs/gascity"
```

The removed `rigs.includes` field is not a PackV2 import surface. A loader must
hard-fail if a rig uses `includes`.

### 1.2.4. `[[agent]]`

Each `[[agent]]` table defines an agent template.

```toml
[[agent]]
name = "reviewer"
scope = "city"
prompt_template = "assets/prompts/reviewer.md"
provider = "codex"
```

The `name` field is required. Agent names must be valid session identifiers:
they start with an ASCII letter or digit and continue with ASCII letters,
digits, hyphens, or underscores. Slashes, dots, and spaces are not valid agent
name characters.

The `scope` field controls where a pack-defined agent is instantiated:

| `scope` | Loader meaning |
|---|---|
| omitted | Agent is eligible in both city-level and rig-level pack loading. |
| `city` | Agent is kept only during city-level pack loading. |
| `rig` | Agent is kept only during rig-level pack loading. |

The full set of currently parsed agent fields is shared with `city.toml`.
Pack authors should treat these fields as public PackV2 agent fields:

| Field | Type | Rule |
|---|---|---|
| `name` | string | Required local agent name. |
| `description` | string | Human-readable description. |
| `dir` | string | Identity prefix. Reusable packs should usually omit this. |
| `work_dir` | string | Session working directory without changing identity. |
| `scope` | string | `city`, `rig`, or omitted. |
| `suspended` | bool | Prevents controller startup for the agent. |
| `pre_start` | array of string | Commands before session creation. |
| `prompt_template` | string | Prompt template path. Relative paths resolve against the pack directory. |
| `nudge` | string | Startup nudge text. |
| `session` | string | Session transport override. Currently `acp` is the specified non-default value. |
| `provider` | string | Provider preset name. |
| `start_command` | string | Provider command override. |
| `args` | array of string | Provider arguments override. |
| `prompt_mode` | string | `arg`, `flag`, or `none`. |
| `prompt_flag` | string | Prompt flag when `prompt_mode = "flag"`. |
| `ready_delay_ms` | integer | Startup readiness delay in milliseconds. |
| `ready_prompt_prefix` | string | Provider readiness prompt prefix. |
| `process_names` | array of string | Process names used for liveness checks. |
| `emits_permission_warning` | bool | Whether the provider emits permission warnings. |
| `env` | table | Extra environment variables. |
| `option_defaults` | table | Provider option default overrides for this agent. |
| `max_active_sessions` | integer | Maximum active sessions for this agent. |
| `min_active_sessions` | integer | Minimum active sessions for this agent. |
| `scale_check` | string | Command returning desired session count. |
| `drain_timeout` | string | Go duration string for scale-down drain. |
| `on_boot` | string | Command run at controller startup. |
| `on_death` | string | Command run when a session dies unexpectedly. |
| `namepool` | string | Path to newline-separated display aliases. |
| `work_query` | string | Work discovery command. |
| `sling_query` | string | Work routing command template. |
| `idle_timeout` | string | Go duration string. Empty disables idle checking. |
| `sleep_after_idle` | string | Go duration string or `off`. |
| `install_agent_hooks` | array of string | Agent hook installation override. |
| `hooks_installed` | bool | Declares hooks already installed. |
| `session_setup` | array of string | Commands after session creation. |
| `session_setup_script` | string | Script path after `session_setup`. Relative paths resolve against the pack directory. |
| `session_live` | array of string | Idempotent live commands. |
| `overlay_dir` | string | Additive overlay directory. Relative paths resolve against the pack directory. |
| `default_sling_formula` | string | Formula automatically applied by sling unless disabled. |
| `inject_fragments` | array of string | Prompt template fragments to inject. |
| `attach` | bool | Whether interactive attachment is supported. |
| `fallback` | bool | Marks this as a fallback definition for collision resolution. |
| `depends_on` | array of string | Agent startup dependencies. Bare rig-pack dependencies are qualified during rig loading. |
| `resume_command` | string | Provider resume command template. |
| `wake_mode` | string | `resume` or `fresh`. |

Fields not listed above are not PackV2 agent fields.

### 1.2.5. `[[named_session]]`

Each `[[named_session]]` table declares a canonical session backed by an agent
template.

```toml
[[named_session]]
template = "reviewer"
scope = "city"
mode = "always"
```

| Field | Type | Required | Rule |
|---|---|---|---|
| `template` | string | yes | Agent template name. |
| `scope` | string | no | `city`, `rig`, or omitted. Uses the same filtering rule as agents. |
| `dir` | string | no | Identity prefix after expansion. Reusable packs should usually omit this. |
| `mode` | string | no | `on_demand` or `always`. |

### 1.2.6. `[[service]]`

Each `[[service]]` table declares a workspace-owned HTTP service.

Services may appear in city-level packs. Rig-level pack loading must fail if a
rig-imported pack declares any service.

Packs must not set `publish_mode = "direct"`.

### 1.2.7. `[providers.<name>]`

The `[providers]` table defines provider presets. Pack providers are merged
additively into the city. Included providers load first. Parent pack providers
win over included pack providers with the same name inside a pack load. When
multiple city or rig packs contribute providers, the first provider already in
the effective city wins.

Provider fields are the same provider fields accepted in `city.toml`.

### 1.2.8. Formula Directory

A pack's formula directory is the well-known `formulas/` directory at the pack
root. PackV2 does not define `[formulas].dir` in `pack.toml`.

Formula directories are collected as loader layers. Lower-priority pack
directories are collected before higher-priority pack directories.

### 1.2.9. `[[patches.agent]]`

Agent patches modify an existing agent by identity.

```toml
[[patches.agent]]
name = "reviewer"
provider = "codex"
session_setup_append = ["tmux set status-left '[review]'"]
```

| Field | Type | Required | Rule |
|---|---|---|---|
| `name` | string | yes | Target agent local name. |
| `dir` | string | context-dependent | Target identity prefix. Empty means city-level in `city.toml`; in `pack.toml`, empty matches by name before consumer rig stamping. |

Patch operation fields mirror agent fields. A scalar pointer field replaces the
target value. A list replacement field replaces the target list. A field whose
name ends in `_append` appends to the corresponding target list.

The specified append fields are:

| Field | Appends to |
|---|---|
| `pre_start_append` | `pre_start` |
| `session_setup_append` | `session_setup` |
| `session_live_append` | `session_live` |
| `install_agent_hooks_append` | `install_agent_hooks` |
| `inject_fragments_append` | `inject_fragments` |

Pack-level patch paths in `prompt_template`, `session_setup_script`, and
`overlay_dir` resolve relative to the patching pack directory.

### 1.2.10. `[[doctor]]`

Each `[[doctor]]` table declares a diagnostic check.

```toml
[[doctor]]
name = "check-tools"
script = "doctor/check-tools.sh"
description = "Verify required tools are installed"
```

| Field | Type | Required | Rule |
|---|---|---|---|
| `name` | string | yes | Short diagnostic identifier. |
| `script` | string | yes | Script path relative to pack directory. |
| `description` | string | no | Human-readable description. |

### 1.2.11. `[[commands]]`

Each `[[commands]]` table declares a pack CLI command.

```toml
[[commands]]
name = "status"
description = "Show pack status"
long_description = "commands/status-help.txt"
script = "commands/status.sh"
```

| Field | Type | Required | Rule |
|---|---|---|---|
| `name` | string | yes | Command name. |
| `description` | string | yes | Short help text. |
| `long_description` | string | no | Path to long help text, relative to pack directory. |
| `script` | string | yes | Script path, relative to pack directory. |

### 1.2.12. `[global]`

The `[global]` table declares pack-wide live session commands.

```toml
[global]
session_live = ["{{.ConfigDir}}/scripts/theme.sh {{.Session}}"]
```

| Field | Type | Required | Rule |
|---|---|---|---|
| `session_live` | array of string | no | Commands appended to matching agents after pack expansion. |

When `[global].session_live` is loaded, `{{.ConfigDir}}` is resolved to the
concrete pack directory. Other template variables remain for per-agent
expansion.

### 1.2.13. Fields Not In `pack.toml`

The following fields are not PackV2 `pack.toml` fields:

| Field | Reason |
|---|---|
| `[agent_defaults]` | City-level only. Appears in `city.toml`, not `pack.toml`. |
| `[agents]` | City-level compatibility alias only. It is not valid in `pack.toml`. |
| `[defaults.rig.imports]` | City-level only. Appears in `city.toml`, not `pack.toml`. |
| `[formulas].dir` | Formula directories use the well-known `formulas/` path. |
| `[[patches.rigs]]` | City-level only. Pack patches may target agents only. |
| `[[patches.providers]]` | City-level only. Pack patches may target agents only. |
| `[export]` | Specified in design notes but not implemented by the current loader. |
| `path` inside `[imports.<binding>]` | Not part of durable import TOML. |
| `ref` inside `[imports.<binding>]` | Not part of durable import TOML. |
| `commit` inside `[imports.<binding>]` | Not part of durable import TOML. |
| `hash` inside `[imports.<binding>]` | Not part of durable import TOML. |
| `transitive` or inline import `export` controls | Legacy/proposed surfaces, not authoritative PackV2 format. |

### 1.3. Per-Directory Breakdown

### 1.3.1. `assets/`

`assets/` is the preferred home for private pack implementation files.
PackV2 does not scan `assets/` directly. Files under `assets/` become relevant
only when a pack definition references them, for example:

```toml
[[agent]]
name = "reviewer"
prompt_template = "assets/prompts/reviewer.md"
session_setup_script = "assets/scripts/setup-reviewer.sh"
overlay_dir = "assets/overlays/reviewer"
```

Authors should put new private prompt templates, scripts, overlays, and other
implementation files under `assets/` unless an established loader convention
requires a top-level directory.

### 1.3.2. `formulas/`

`formulas/` is the only PackV2 formula directory. If it exists, the loader
collects it as a formula layer. `[formulas].dir` is invalid.

### 1.3.3. `overlay/`

`overlay/` is collected automatically from loaded pack directories. City-level
pack overlays are available to city agents and form the base overlay layer for
rig agents. Rig-level pack overlays are collected per rig.

Agent `overlay_dir` is different: it is an explicit path on an agent definition
or patch and resolves relative to the declaring pack.

### 1.3.4. `scripts/`

`scripts/` is collected automatically from loaded pack directories as a script
search layer. City-level pack scripts form the base layer. Rig-level pack
scripts are collected per rig.

Scripts referenced directly by `start_command`, `session_setup`,
`session_setup_script`, `[[commands]]`, or `[[doctor]]` are resolved according
to the field-specific rules in this document.

### 1.3.5. Conventional Directories

`commands/`, `doctor/`, `namepools/`, `prompts/`, `skills/`, `orders/`, and
`mcps/` are conventional directories. The loader does not give all of them the
same automatic treatment. A file in one of these directories is part of a pack's
effective behavior only if the relevant subsystem scans it or a TOML definition
references it.

## 2. Loader

Loading a pack is the process of turning one or more pack directories into a
flat effective city configuration.

The loader has these major phases:

```text
LoadWithIncludes(city.toml):
    parse root city.toml
    merge city TOML fragments
    resolve legacy named pack sources
    reject removed rig includes
    expand city-level packs
    apply city-level patches
    expand rig-level packs
    apply pack globals
    validate requirements
    compute formula and script layers
    inject implicit agents
    apply city agent defaults
    validate and normalize final config
```

### 2.1. Pack Resolution

A pack reference is resolved to a pack root directory before `pack.toml` is
read.

For PackV2 imports, the reference is the `source` field of a `PackImport`.
Import bindings are sorted lexicographically before their sources are loaded.
This gives deterministic load order for TOML maps.

The root city imports are read from top-level `[imports.<binding>]` in
`city.toml`. Rig imports are read from `[rigs.imports.<binding>]` under the
corresponding `[[rigs]]` entry. Pack-to-pack imports are read from top-level
`[imports.<binding>]` in `pack.toml`.

Legacy `includes` lists in `pack.toml` and legacy workspace includes may also
feed the pack loader. They remain compatibility mechanisms. They are not the
preferred PackV2 authoring surface.

If a pack import has an empty binding name or empty `source`, loading must
fail.

### 2.2. Versioning

`PackImport.version` is a compatibility constraint for versioned sources.
The exact resolved version belongs to the pack lockfile, not to
`pack.toml`.

The loader specified here consumes resolved sources. Registry lookup, remote
version selection, cache population, and lockfile update are pack management
operations that occur before or around loading. They must produce a concrete
pack root directory whose `pack.toml` can be loaded by this specification.

Registry handles are not durable dependency coordinates. A command may accept a
handle such as `main:gascity`; persisted PackV2 TOML must store the resolved
durable `source` and optional `version` constraint instead.

### 2.3. Recursive Pack Loading

The recursive pack loader operates on a pack root directory and a loading
context:

```text
loadPack(packRoot, cityRoot, rigName, seen):
    read packRoot/pack.toml
    validate [pack]
    validate imports
    recursively load pack includes/imports
    copy this pack's own definitions
    stamp agents and named sessions with rigName when applicable
    resolve pack-relative paths
    merge included definitions first, then this pack's definitions
    apply this pack's patches
    qualify rig depends_on entries
    merge providers
    return definitions and ordered pack directories
```

The `seen` set is a recursion-stack set. A pack directory already present in
the active recursion stack must cause loading to fail with a cycle error. A
diamond-shaped dependency graph is valid; the loader may reuse a cached result
when the same pack directory is reached through more than one acyclic path.

Included or imported pack definitions are lower-priority base definitions. The
parent pack's own definitions are appended after included definitions and are
therefore the later layer for fallback resolution and provider merging.

### 2.4. Scope Filtering And Stamping

When loading a pack for the city-level surface:

1. The loader keeps agents and named sessions whose `scope` is omitted or
   `city`.
2. The loader drops agents and named sessions whose `scope` is `rig`.
3. The effective identity prefix for kept agents is empty unless the definition
   explicitly supplied `dir`.

When loading a pack for a rig-level surface:

1. The loader keeps agents and named sessions whose `scope` is omitted or
   `rig`.
2. The loader drops agents and named sessions whose `scope` is `city`.
3. For kept agents and named sessions whose `dir` is empty, the loader sets
   `dir` to the consuming rig's `name`.

The runtime qualified name of an agent is:

```text
qualifiedName(agent):
    if agent.dir == "":
        return agent.name
    return agent.dir + "/" + agent.name
```

The `dir` field is an identity prefix, not a filesystem path.

### 2.5. Naming And Collisions

Agent names are local names. Import bindings do not qualify runtime agent
names.

The loader resolves fallback collisions before reporting duplicate pack agents:

1. If a fallback and a non-fallback from different source directories share a
   local name, the non-fallback wins and the fallback is removed.
2. If two or more fallbacks from different source directories share a local
   name, the first loaded fallback wins and later fallbacks are removed.
3. If two or more non-fallback agents from different source directories share a
   local name on the same surface, loading fails.

The "same surface" means the city-level surface or one rig-level surface. Two
different rigs may each have an agent with the same local name because their
qualified names differ by rig `dir`.

Identifier qualification across import bindings, runtime names, registry
selectors, and AgentScript is not yet settled. Until that work lands, this
specification deliberately avoids defining a binding-qualified runtime agent
syntax.

### 2.6. Patches

Patches are targeted modifications to existing definitions. A patch must not
create a new agent.

Pack-level patches run inside recursive pack loading after imported/base agents
and the current pack's own agents have been merged. Pack-level patches may only
use `[[patches.agent]]`. In pack-level patches, `dir = ""` matches by local
`name` only, because a reusable pack normally does not know which rig will
consume it.

City-level patches run after city-level pack expansion and before rig-level
pack expansion. A city-level patch targets agents that already exist in the
effective city config at that point.

Rig overrides run after all packs for that rig have expanded and after their
agents have been stamped with the rig name.

The patch/default order is:

1. Recursive pack imports/includes load.
2. Pack-level patches apply inside each pack load.
3. City-level packs expand.
4. City-level patches apply.
5. Rig-level packs expand.
6. Rig overrides apply.
7. Pack globals apply.
8. Implicit agents are injected.
9. City `[agent_defaults]` applies.

If a patch target does not exist when the patch runs, loading fails.

### 2.7. Defaults

`[agent_defaults]` is a city-level `city.toml` table. It is not a PackV2
`pack.toml` table.

The current default application step actively applies
`agent_defaults.default_sling_formula` to agents whose `default_sling_formula`
is still unset. It skips the control-dispatcher infrastructure agent.

Other fields in the `AgentDefaults` structure may be parsed and composed by the
city config loader, but they are not specified here as PackV2 pack defaults.

Defaults run after pack expansion, patches, rig overrides, pack globals, and
implicit agent injection. Defaults fill blank fields only; they do not override
fields explicitly set by a pack, patch, rig override, or implicit agent seed.

### 2.8. Path Resolution

The following fields on pack agents resolve relative to the declaring pack
directory:

| Field |
|---|
| `prompt_template` |
| `session_setup_script` |
| `overlay_dir` |

The same fields in pack-level patches resolve relative to the patching pack
directory.

`[global].session_live` commands replace `{{.ConfigDir}}` with the concrete
pack directory when the pack is loaded. Other template variables remain
unresolved until runtime.

Pack command and doctor script paths are declared relative to the pack
directory.

### 2.9. Formula, Overlay, And Script Layers

City formula layers are ordered from lower priority to higher priority:

1. Formula directories from city-level packs.
2. The city-local formula directory.

Rig formula layers are ordered from lower priority to higher priority:

1. City formula layers.
2. Formula directories from packs imported by that rig.
3. The rig-local formula directory, when configured.

City script layers contain `scripts/` directories from city-level packs.
Rig script layers contain city script layers followed by `scripts/` directories
from packs imported by that rig.

Overlay directories follow the same city-base then rig-specific collection
model. Agent-specific `overlay_dir` is applied separately by the runtime.

### 2.10. Pack Globals

City-level pack globals apply to all agents. Rig-level pack globals apply only
to agents in the corresponding rig.

Pack globals append live session commands. They do not replace agent
`session_live` entries.

### 2.11. Requirements

Pack requirements are checked after pack expansion.

A city requirement must be satisfied by an agent with matching local name on the
city-level surface. A rig requirement must be satisfied by an agent with
matching local name during rig pack loading.

If a requirement is not satisfied, loading fails.

### 2.12. Error Handling

The loader must fail when:

1. `pack.toml` cannot be read.
2. `pack.toml` cannot be parsed as TOML.
3. `[pack].name` is empty.
4. `[pack].schema` is missing, zero, or greater than the supported schema.
5. A pack import has an empty binding or empty source.
6. A pack dependency cycle is detected.
7. A pack-level or city-level patch targets a missing agent.
8. A rig-level pack declares a service.
9. A pack service sets `publish_mode = "direct"`.
10. A non-fallback agent collision remains after fallback resolution.
11. A declared pack requirement is not satisfied.
12. `city.toml` uses removed `rigs.includes`.

The loader may skip missing remote pack subpaths in compatibility cases where a
remote source was fetched but the referenced pack directory no longer exists.
That compatibility behavior must not be used to justify new invalid PackV2
configuration.

## 3. Non-Normative Notes

The proposed `[export]` surface is intentionally absent from this current-state
specification. It belongs in design notes until an implementation lands.

The `pack.toml` `includes` field and workspace-level pack includes exist for
compatibility and examples that predate PackV2 imports. New durable pack
dependencies should use `[imports.<binding>]`.

The Java Virtual Machine Specification separates class-file structure from
loading and linking semantics. PackV2 follows the same documentation pattern:
section 1 specifies the file format, and section 2 specifies the loader.
