---
title: Gas City 1.0 Pack System (PackV2)
description: Authoritative specification for Gas City pack format and loading semantics.
---

# Gas City 1.0 Pack System (PackV2)

| Field | Value |
|---|---|
| Status | Authoritative specification |
| Last verified | 2026-05-30 |
| Pack schema | 2 |
| Primary implementation | `internal/config/pack.go`, `internal/config/config.go`, `internal/config/compose.go` |
| User-facing guide | `docs/guides/shareable-packs.md` |

This document specifies the Gas City 1.0 pack system as a data model, file
format, and loading process. **PackV2** is a shorthand name for the Gas City Pack
Specification (2.0), but this document uses "pack" for the authoring surface.

The key words "must", "must not", "required", "shall", "shall not", "should",
"should not", and "may" are to be interpreted as normative requirements unless
the paragraph is explicitly marked as non-normative.

## 0. Data And Information Model

The pack system separates the *format* of a pack from the *loading* of a pack.
The format is the directory and TOML data a pack author writes. Loading is the
process that resolves dependencies, stamps definitions into a city or rig
context, applies patches and defaults, and produces one effective `City`
configuration.

### 0.1. Pack

A pack is a directory containing a `pack.toml` file and zero or more
definition, asset, and support files. The `pack.toml` file is the pack's
metadata and manifest. Other files are either definition files discovered by
well-known rules or private files referenced by TOML fields.

A pack may be used in three contexts:

1. As the city pack. The city pack is the root pack: the city directory
   containing `city.toml` and the root `pack.toml`.
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

The `schema` identifies the pack file-format schema. The schema specified by
this document is `2`. A loader must reject a pack whose schema is omitted, zero,
or greater than the loader's supported schema.

The `version` is pack metadata. Version selection is controlled by import
constraints and lockfile resolution, not by a loader comparing `[pack].version`
directly.

The `requires_gc` field is optional metadata for the minimum compatible `gc`
version. It is parsed as pack metadata.

### 0.3. Pack Contents

A pack may contain the following abstract content:

| Content | Preferred representation | Status |
|---|---|---|
| Pack identity | `[pack]` in `pack.toml` | required |
| Imports | `[imports.<binding>]` in `pack.toml` | preferred |
| Agents | `agents/<name>/` with optional `agent.toml` | preferred |
| Named sessions | `[[named_session]]` in `pack.toml` | current |
| Services | `[[service]]` in `pack.toml` | current |
| Providers | `[providers.<name>]` in `pack.toml` | current |
| Formulas | `formulas/` | preferred |
| Orders | `orders/<name>.toml` | preferred |
| Skills | `skills/` | preferred |
| MCP configuration | `mcp/` | preferred |
| Pack commands | `commands/<path>/run.sh` with optional `command.toml` | preferred |
| Doctor checks | `doctor/<name>/run.sh` with optional `doctor.toml` | preferred |
| Agent patches | `[[patches.agent]]` in `pack.toml` | current |
| Pack globals | `[global]` in `pack.toml` | current |
| Pricing | `[[pricing]]` in `pack.toml` | current |
| Pack overlay files | `overlay/` | current |
| Private support files | `assets/` | preferred |

The `pack.toml` file is the manifest. Well-known directories are the primary
format for pack contents that have moved out of inline TOML.

### 0.4. Public And Private Content

Pack definitions are visible to consumers according to loader scope rules. Files
under `assets/` are private support files unless a public definition references
them.

The current loader does not give import bindings a runtime namespace for agents.
If a city-level imported pack defines an agent named `reviewer`, the effective
city-level agent name is `reviewer`, not `<binding>/reviewer`.

## 1. File System Structure

A pack directory has this abstract shape:

```text
pack-root/
  pack.toml
  agents/
  assets/
  commands/
  doctor/
  formulas/
  orders/
  skills/
  mcp/
  overlay/
```

Only `pack.toml` is required.

### 1.1. Directory Rules

The pack root must be a directory. The pack root must contain a file named
`pack.toml`.

The following top-level paths are reserved by the pack format:

| Path | Kind | Meaning |
|---|---|---|
| `pack.toml` | file | Required pack manifest and metadata. |
| `agents/` | directory | Well-known directory for agent definitions. |
| `assets/` | directory | Preferred location for private implementation files. |
| `commands/` | directory | Well-known directory for pack commands. |
| `doctor/` | directory | Well-known directory for pack doctor checks. |
| `formulas/` | directory | Well-known formula directory. |
| `orders/` | directory | Well-known order definition directory. |
| `skills/` | directory | Well-known skill catalog directory. |
| `mcp/` | directory | Well-known MCP configuration directory. |
| `overlay/` | directory | Pack-level overlay directory collected automatically from imported packs. |

Pack authors must not invent additional top-level machine-readable directories.
Private support files should go under `assets/`, or under the specific
well-known directory that owns them. This reserves the top-level namespace for
future pack-format expansion.

The following file-system constructs must not be used as public pack format:

| Construct | Rule | Preferred replacement |
|---|---|---|
| Cache directories | A checked-in `pack.toml` must not point at Gas City's local cache as a durable dependency. | Use durable `[imports.<binding>].source` plus optional `version`. |
| Registry handles | A checked-in `pack.toml` must not persist command-time handles such as `main:gascity`. | Use the registry record's durable `source` and optional `version`. |
| Consumer rig names inside reusable packs | A reusable pack must not assume the names of rigs that will import it. | Use `scope = "rig"` and let the loader stamp the consuming rig name. |
| Top-level `scripts/` | New packs must not place user scripts in top-level `scripts/`. | Put command/doctor entrypoints beside their command/check, or put private scripts under `assets/`. |

### 1.2. The `pack.toml` File

The `pack.toml` file is UTF-8 TOML. It must contain a `[pack]` table.

Conceptually, the file may contain these tables:

| Table | Meaning | Status |
|---|---|---|
| `[pack]` | Pack metadata and identity. | required |
| `[imports.<binding>]` | Pack imports. | preferred |
| `[[named_session]]` | Pack-provided named sessions. | current |
| `[[service]]` | Pack-provided services. | current |
| `[providers.<name>]` | Pack-provided provider presets. | current |
| `[[patches.agent]]` | Pack-level agent patches. | current |
| `[global]` | Pack-wide live session commands. | current |
| `[[pricing]]` | Pack-provided pricing estimates. | current |

Unknown fields are not part of this specification. The current loader rejects
unknown `pack.toml` fields and unknown `[imports.<binding>]` fields; authors
must not rely on unknown keys being preserved or interpreted.

> **Compatibility:** The current loader still accepts legacy inline
> `pack.toml` `[[agent]]`, `[[doctor]]`, and `[[commands]]` entries for
> existing packs. New packs should use `agents/`, `doctor/`, and `commands/`.

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
| `description` | string | no | Human-readable pack summary. |
| `requires` | array of tables | no | Agent requirements validated after expansion. |

> **Compatibility:** The current loader still accepts `[pack].includes` for
> legacy pack composition. New packs must use `[imports.<binding>]`.

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
source = "https://github.com/gastownhall/gascity-packs/tree/main/gascity"
version = "sha:d3617d1319a1206ac85f69ba024ec395c49c6f4b"
```

The binding name is local to the importing file. Current loader behavior uses
binding names for deterministic ordering of imports. It does not add binding
names to runtime agent identities.

A `source` string is a pack resolver coordinate. For GitHub-hosted packs, a
browser-dereferenceable `tree/<ref>/<path>` URL is the preferred authored form.
For other Git-backed sources, the Git remote and any pack-root subdirectory
selector are part of the same source string.

| Field | Type | Required | Rule |
|---|---|---|---|
| `source` | string | yes | Durable resolver coordinate for the pack root. Must not be empty. |
| `version` | string | no | Compatibility constraint for versioned sources. |

Public import TOML must not use fields named `path`, `ref`, `commit`, or
`hash`. Registry handles such as `main:gascity` are command-time lookup handles
and must not be persisted as `source`.

City-pack imports use the same table shape at the top level of the root
`pack.toml`:

```toml
[imports.review]
source = "../packs/review"
```

> **Compatibility:** The current loader also accepts top-level
> `[imports.<binding>]` tables in `city.toml`. When a city has both `city.toml`
> and `pack.toml`, new imports should be written in `pack.toml`; `city.toml` is
> the deployment layer.

Default rig imports are a city deployment concern. The canonical surface is
`city.toml` `[defaults.rig.imports.<binding>]`. `pack.toml` must not define
`[defaults.rig.imports]`.

Rig imports are written under the `[[rigs]]` table they apply to:

```toml
[[rigs]]
name = "checkout-service"
path = "../checkout-service"

[rigs.imports.review]
source = "../packs/review"
```

> **Compatibility:** The removed `rigs.includes` field is not a pack import
> surface for schema-2 authored config. A loader must hard-fail if a schema-2
> city or fragment uses `rigs.includes`.

### 1.2.4. Agent Directories

Pack agents are authored as immediate child directories under `agents/`.
Each `agents/<name>/` directory defines one local agent template. The directory
name is the agent name.

```text
agents/
└── reviewer/
    ├── agent.toml
    └── prompt.template.md
```

`agent.toml` is optional. A minimal agent directory may contain only a prompt
file and inherit every other setting from the surrounding city or pack
composition.

The loader ignores entries under `agents/` that are not directories. Directory
names beginning with `.` or `_` are ignored and do not define agents.

The agent directory name must be a valid session identifier: it starts with an
ASCII letter or digit and continues with ASCII letters, digits, hyphens, or
underscores. Slashes, dots, and spaces are not valid agent name characters.

If `agent.toml` contains a `name` field, the loader ignores it for identity and
uses the directory name. Authors must not rely on `name` inside `agent.toml` to
rename an agent.

Prompt file discovery inside an agent directory uses this order:

1. `prompt.template.md`
2. `prompt.md.tmpl`
3. `prompt.md`

New agents should use `prompt.template.md`. The `prompt.md.tmpl` name remains
recognized for transitional compatibility.

The `scope` field controls where a pack-defined agent is instantiated:

| `scope` | Loader meaning |
|---|---|
| omitted | Agent is eligible in both city-level and rig-level pack loading. |
| `city` | Agent is kept only during city-level pack loading. |
| `rig` | Agent is kept only during rig-level pack loading. |

The following table is a curated pack authoring reference for
`agent.toml`. It is not an exhaustive decoder inventory: the generated schema
at `docs/schema/pack-schema.json` is the generated runtime schema inventory.
The normative authoring rules are specified here.

| Field | Type | Rule |
|---|---|---|
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
| `append_fragments` | array of string | Prompt template fragments appended after the rendered prompt body. |
| `inject_assigned_skills` | bool | Enables assigned-skill prompt injection. |
| `attach` | bool | Whether interactive attachment is supported. |
| `fallback` | bool | Marks this as a fallback definition for collision resolution. |
| `depends_on` | array of string | Agent startup dependencies. Bare rig-pack dependencies are qualified during rig loading. |
| `resume_command` | string | Provider resume command template. |
| `wake_mode` | string | `resume` or `fresh`. |

> **Compatibility:** The current runtime still parses agent-level `skills` and
> `mcp` arrays as compatibility tombstones, but active materialization ignores
> them. New packs should use the pack-level `skills/` and `mcp/` directories.

> **Compatibility:** Legacy inline `pack.toml` `[[agent]]` tables remain loader
> compatibility for existing packs. New packs must use
> `agents/<name>/agent.toml`; existing inline definitions should migrate by
> moving each table's fields into the matching agent directory and moving prompt
> content beside the agent as `prompt.template.md` when the prompt is templated.
> During an incremental same-pack migration, an inline `[[agent]]` with the same
> name takes precedence and the matching directory agent is ignored. A v1 inline
> agent and a v2 directory agent with the same name across composition layers is
> a migration error rather than a precedence rule.

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
root. The only formula directory the pack loader collects is `formulas/`.

Formula directories are collected as loader layers. Lower-priority pack
directories are collected before higher-priority pack directories.

> **Compatibility:** Older material may mention `[formulas].dir`. That field is
> invalid in `pack.toml`; new packs should put formulas under `formulas/`.

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

### 1.2.10. Doctor Directory

Pack doctor checks are authored under `doctor/`. Each immediate child directory
under `doctor/` defines one check when it contains a runnable check script.

```text
doctor/
└── tooling/
    ├── run.sh
    ├── fix.sh
    ├── help.md
    └── doctor.toml
```

The directory name is the default check name. `run.sh` is the default check
script. `fix.sh`, when present, opts the check into `gc doctor --fix`.
`help.md` is optional help text.

An optional `doctor.toml` may override the default script names or provide
metadata:

| Field | Type | Required | Rule |
|---|---|---|---|
| `description` | string | no | Human-readable description. |
| `run` | string | no | Check script path relative to the check directory. Defaults to `run.sh`. |
| `fix` | string | no | Fix script path relative to the check directory. Defaults to sibling `fix.sh` when present. |
| `warmup` | bool | no | When true, includes this check in the `gc start` warm-up scan. The check still runs on demand through `gc doctor`. |

> **Compatibility:** Legacy `pack.toml` `[[doctor]]` entries remain loader
> compatibility for existing packs. New packs should use `doctor/<name>/`.

### 1.2.11. Command Directory

Pack commands are authored under `commands/`. Each directory containing `run.sh`
defines one command leaf. Nested directories imply nested command words.

```text
commands/
└── repo/
    └── sync/
        ├── run.sh
        ├── help.md
        └── command.toml
```

The directory path is the default command word list. `run.sh` is the default
entrypoint. `help.md` is optional help text.

An optional `command.toml` may override the default command words or script:

| Field | Type | Required | Rule |
|---|---|---|---|
| `command` | array of string | no | Command words. Defaults to the directory path under `commands/`. |
| `description` | string | no | Short help text. |
| `run` | string | no | Entrypoint path relative to the command directory. Defaults to `run.sh`. |

> **Compatibility:** Legacy `pack.toml` `[[commands]]` entries remain loader
> compatibility for existing packs. New packs should use `commands/<path>/`.

### 1.2.12. `[global]`

The `[global]` table declares pack-wide live session commands.

```toml
[global]
session_live = ["{{.ConfigDir}}/assets/scripts/theme.sh {{.Session}}"]
```

| Field | Type | Required | Rule |
|---|---|---|---|
| `session_live` | array of string | no | Commands appended to matching agents after pack expansion. |

When `[global].session_live` is loaded, `{{.ConfigDir}}` is resolved to the
concrete pack directory. Other template variables remain for per-agent
expansion.

### 1.2.13. `[[pricing]]`

Each `[[pricing]]` table declares an estimated model-pricing entry for one
provider/model pair.

```toml
[[pricing]]
provider = "codex"
model = "gpt-5"
last_verified = "2026-05-30"

[pricing.tier]
prompt_usd_per_1m = 1.25
completion_usd_per_1m = 10.0
cache_read_usd_per_1m = 0.125
cache_creation_usd_per_1m = 1.25
```

| Field | Type | Required | Rule |
|---|---|---|---|
| `provider` | string | yes | Provider label. Must not be empty. |
| `model` | string | yes | Provider-specific model identifier. Must not be empty. |
| `tier` | table | yes | Per-token-type estimated rates. |
| `tier.prompt_usd_per_1m` | number | no | Prompt-token estimated USD per one million tokens. |
| `tier.completion_usd_per_1m` | number | no | Completion-token estimated USD per one million tokens. |
| `tier.cache_read_usd_per_1m` | number | no | Cache-read estimated USD per one million tokens. |
| `tier.cache_creation_usd_per_1m` | number | no | Cache-creation estimated USD per one million tokens. |
| `last_verified` | string | no | Date the entry was checked, in `YYYY-MM-DD` form. |

Pack pricing entries are lower priority than city-level `[[pricing]]` entries
and higher priority than the built-in default pricing table. Pricing entries
are estimates for decision support, not invoice reconciliation.

### 1.2.14. Authoring Summary

New packs should use these authoring constructs:

| Construct | Use for |
|---|---|
| `[pack]` | Pack identity and metadata. |
| `[imports.<binding>]` | Durable pack dependencies. |
| `agents/<name>/` | Agent definitions. |
| `commands/<path>/` | Pack commands. |
| `doctor/<name>/` | Pack doctor checks. |
| `formulas/` | Pack formulas. |
| `orders/` | Pack orders. |
| `skills/` | Pack skills. |
| `mcp/` | Pack MCP configuration. |
| `assets/` | Private support files. |
| `overlay/` | Pack-level overlay files. |
| `[[named_session]]` | Pack named sessions. |
| `[[service]]` | Pack services. |
| `[providers.<name>]` | Pack provider presets. |
| `[[patches.agent]]` | Pack-level agent patches. |
| `[global]` | Pack-wide live session commands. |
| `[[pricing]]` | Pack pricing estimates. |

> **Compatibility And Unsupported Constructs:** Existing packs may still contain
> old authoring forms, and the loader rejects several city-only or invalid
> tables from `pack.toml`.
>
> | Construct | Disposition | Replacement |
> |---|---|---|
> | `[[agent]]` | Legacy loader compatibility. | `agents/<name>/agent.toml` and colocated prompt files. |
> | `[[doctor]]` | Legacy loader compatibility. | `doctor/<name>/run.sh` with optional `doctor.toml`. |
> | `[[commands]]` | Legacy loader compatibility. | `commands/<path>/run.sh` with optional `command.toml`. |
> | `[pack].includes` | Legacy loader compatibility. | `[imports.<binding>]`. |
> | `[agent_defaults]` | Invalid in `pack.toml`; city-level only. | `city.toml` `[agent_defaults]`. |
> | `[agents]` | Invalid in `pack.toml`; city-level compatibility alias only. | `city.toml` `[agent_defaults]`. |
> | `[defaults.rig.imports]` | Invalid in `pack.toml`; city-level only. | `city.toml` `[defaults.rig.imports.<binding>]`. |
> | `[formulas].dir` | Invalid in `pack.toml`. | `formulas/`. |
> | `[[patches.rigs]]` | Invalid in `pack.toml`; city-level only. | `city.toml` `[[patches.rigs]]`. |
> | `[[patches.providers]]` | Invalid in `pack.toml`; city-level only. | `city.toml` `[[patches.providers]]`. |
> | `path`, `ref`, `commit`, or `hash` inside `[imports.<binding>]` | Invalid durable import TOML. | `[imports.<binding>].source` plus optional `version`. |
> | `transitive`, `shadow`, or inline import `export` controls | Current-runtime compatibility fields, not preferred authoring. | Omit them in newly-authored durable imports. |

### 1.3. Per-Directory Breakdown

### 1.3.1. `assets/`

`assets/` is the preferred home for private pack implementation files.
The loader does not scan `assets/` directly. Files under `assets/` become relevant
only when a pack definition references them, for example from
`agents/reviewer/agent.toml`:

```toml
session_setup_script = "assets/scripts/setup-reviewer.sh"
overlay_dir = "assets/overlays/reviewer"
```

Authors should put new private scripts, prompt fragments, overlays, and other
implementation files under `assets/` unless an established loader convention
requires a well-known directory.

### 1.3.2. `formulas/`

`formulas/` is the only pack formula directory. If it exists, the loader
collects it as a formula layer.

### 1.3.3. `overlay/`

`overlay/` is collected automatically from loaded pack directories. City-level
pack overlays are available to city agents and form the base overlay layer for
rig agents. Rig-level pack overlays are collected per rig.

Agent `overlay_dir` is different: it is an explicit path on an agent definition
or patch and resolves relative to the declaring pack.

Use `overlay/` only for pack-level overlay files that should be collected as a
pack overlay layer. Use `assets/` for private overlay trees referenced by an
agent's `overlay_dir`.

### 1.3.4. Command And Doctor Directories

`commands/` and `doctor/` are scanned by convention. Command entrypoint scripts
belong inside the command directory. Doctor check scripts belong inside the
doctor check directory.

Private helper scripts that are not themselves command or doctor entrypoints
should live under `assets/`.

### 1.3.5. Conventional Directories

`orders/`, `skills/`, and `mcp/` are scanned by their owning subsystems.
`assets/` is not scanned directly. Files under `assets/` become effective only
when a pack definition references them.

## 2. Loader

Loading a pack is the process of turning one or more pack directories into a
flat effective city configuration.

The loader has these major phases:

```text
LoadWithIncludes(city.toml):
    parse root city.toml
    merge city TOML fragments
    reject unsupported city-config surfaces
    resolve compatibility named pack sources
    expand city-level packs
    apply city-level patches
    expand rig-level packs
    apply pack globals
    validate requirements
    compute formula and overlay layers
    inject implicit agents
    apply city agent defaults
    validate and normalize final config
```

### 2.1. Pack Resolution

A pack reference is resolved to a pack root directory before `pack.toml` is
read.

For pack imports, the reference is the `source` field of a `PackImport`.
Import bindings are sorted lexicographically before their sources are loaded.
This gives deterministic load order for TOML maps.

City-pack imports are read from top-level `[imports.<binding>]` tables in the
root `pack.toml`. Rig imports are read from `[rigs.imports.<binding>]` under
the corresponding `[[rigs]]` entry. Pack-to-pack imports are read from
top-level `[imports.<binding>]` in `pack.toml`.

> **Compatibility:** The loader also accepts top-level `[imports.<binding>]`
> tables in `city.toml`; when a root `pack.toml` exists, that path warns authors
> to move imports to `pack.toml`.

> **Compatibility:** Legacy `includes` lists in `pack.toml` may also feed the
> pack loader. New packs should use `[imports.<binding>]`. Schema-2 root
> `city.toml` files and schema-2 config fragments reject legacy PackV1 surfaces
> such as `workspace.includes`, `workspace.default_rig_includes`, `[packs.*]`,
> `rigs.includes`, and inline agent definitions.

If a pack import has an empty binding name or empty `source`, loading must
fail.

### 2.2. Versioning

`PackImport.version` is a compatibility constraint for versioned sources.
The exact resolved version belongs to the pack lockfile, not to `pack.toml`.

The loader specified here consumes resolved sources. Registry lookup, remote
version selection, cache population, and lockfile update occur before or around
loading. They must produce a concrete pack root directory whose `pack.toml` can
be loaded by this specification.

The `gc pack` command surface is registry discovery and local registry
configuration: `gc pack registry add`, `list`, `remove`, `refresh`, `search`,
and `show`.

Registry handles are not durable dependency coordinates. The shipped registry
commands may accept a handle such as `main:gascity` while searching or
inspecting registry records; persisted pack TOML must store the resolved
durable `source` and optional `version` constraint instead.

Installing, checking, or repairing the authored import graph is owned by
`gc import install` and `gc import check`.

### 2.3. Recursive Pack Loading

The recursive pack loader operates on a pack root directory and a loading
context:

```text
loadPack(packRoot, cityRoot, rigName, seen):
    read packRoot/pack.toml
    validate [pack]
    validate imports
    recursively load pack imports
    discover this pack's agents/ definitions
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

1. Recursive pack imports load.
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

`[agent_defaults]` is a city-level `city.toml` table. It is not a pack
`pack.toml` table.

The current default application step actively applies
`agent_defaults.default_sling_formula` to agents whose `default_sling_formula`
is still unset. It skips the control-dispatcher infrastructure agent.

The current runtime also applies shared attachment defaults for
`append_fragments`. Attachment-list fields such as `skills` and `mcp` are
parsed as compatibility tombstones but ignored by active materialization.
Other fields in the `AgentDefaults` structure may be parsed and composed by the
city config loader, but they are not specified here as pack defaults.

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

Convention-discovered prompt, overlay, namepool, skill, and MCP paths inside
`agents/<name>/` are resolved by discovery to concrete pack-local paths. Explicit
relative `prompt_template`, `session_setup_script`, and `overlay_dir` values in
`agent.toml` remain pack-relative through the agent's source directory.

The same fields in pack-level patches resolve relative to the patching pack
directory.

`[global].session_live` commands replace `{{.ConfigDir}}` with the concrete
pack directory when the pack is loaded. Other template variables remain
unresolved until runtime.

Pack command and doctor script paths are declared relative to the pack
directory.

### 2.9. Formula And Overlay Layers

City formula layers are ordered from lower priority to higher priority:

1. Formula directories from city-level packs.
2. The city-local formula directory.

Rig formula layers are ordered from lower priority to higher priority:

1. City formula layers.
2. Formula directories from packs imported by that rig.
3. The rig-local formula directory, when configured.

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
6. A `pack.toml` key is unknown or unsupported by the pack authoring surface.
7. A `[imports.<binding>]` key is unknown or unsupported by the pack import shape.
8. A pack dependency cycle is detected.
9. A pack-level or city-level patch targets a missing agent.
10. A rig-level pack declares a service.
11. A pack service sets `publish_mode = "direct"`.
12. A non-fallback agent collision remains after fallback resolution.
13. A declared pack requirement is not satisfied.
14. A schema-2 `city.toml` or included fragment uses a removed PackV1 surface such as `rigs.includes`, `[packs.*]`, `workspace.includes`, `workspace.default_rig_includes`, or inline agent definitions.

The loader may skip missing remote pack subpaths in compatibility cases where a
remote source was fetched but the referenced pack directory no longer exists.
That compatibility behavior must not be used to justify new invalid pack
configuration.

## 3. Non-Normative Notes

This specification separates  file and directory structure from
loading and linking semantics: section 1 specifies the file and directory formats, and section 2 specifies the
loader.
