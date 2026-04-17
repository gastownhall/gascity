# Spec vs. Implementation Skew Analysis — Current Pack/City v2 Desired State

> Generated 2026-04-12 by comparing `docs/reference/config.md` (as-built
> from the release branch Go structs) against the reconciled pack v2 specs.
> Revised through field-by-field walkthrough to reflect the **current
> Pack/City v2 desired state** — not the ideal end-state, but what should
> ship in this release wave.

## Color key

| Color | Meaning |
|-------|---------|
| 🟢 | Implemented on release branch |
| 🔴 | Not implemented on release branch |
| 🟡 | NYI — in plan for the current rollout |
| 🔵 | NYI — later wave |

## Field placement authority

### city.toml only (not legal in pack.toml)

- `[[rigs]]` and all rig sub-fields
- `[[patches.rigs]]`
- `[beads]`, `[session]`, `[mail]`, `[events]`, `[dolt]`
- `[daemon]`, `[orders]`, `[api]`
- `[chat_sessions]`, `[session_sleep]`, `[convergence]`
- `[[service]]` (#657 tracks whether packs can define services in a later wave)
- `max_active_sessions` (city-wide, currently on `[workspace]`)

### pack.toml only (not legal in city.toml)

- `[pack]` (name, version, schema, requires_gc)
- `[imports]`
- `[defaults.rig.imports]`

### Legal in both (city wins on merge)

- `[agent_defaults]`
- `[providers]`
- `[[named_session]]`
- `[[patches.agent]]`
- `[[patches.providers]]`

---

## Warning levels

- **Loud warning** — emitted on every `gc start` / `gc config` for schema 2 cities. These are V1 surfaces that users should not be writing new content against.
- **Soft warning** — emitted once. Field is accepted but deprecated.
- **Hard error** — field value is rejected.
- **Accept silently** — no warning in this rollout. Tracked for post-release deprecation.

**Fast-follow (pre-April 21 launch):** implement deprecation warning infrastructure for all soft/loud warnings below.

---

## City (top-level struct)

| Status | Field | As-built | Current rollout disposition |
|--------|-------|----------|--------------------|
| 🟢 | `include` | []string, merges fragments | **Keep.** Fragment-only (`-f` path). If a fragment contains `[imports]`, `includes`, or references `pack.toml` → hard error. |
| 🟢 | `workspace` | Required block | **Keep as container.** Deprecated after this rollout (#600). Sub-fields walked individually below. |
| 🟡 | `packs` | map[string]PackSource | **Loud warning on schema 2.** V1 mechanism, use `[imports]` + `pack.lock`. |
| 🟡 | `agent` | []Agent, required | **Loud warning on schema 2.** Not required for schema 2 — agents discovered from `agents/<name>/`. |
| 🟢 | `imports` | map[string]Import | **Keep.** V2 mechanism, working. |
| 🟢 | `named_session` | []NamedSession | **Keep.** Legal in both pack.toml and city.toml, city wins. |
| 🟢 | `rigs` | []Rig | **Keep in city.toml.** |
| 🟢 | `patches` | Patches | **Keep.** `[[patches.agent]]` and `[[patches.providers]]` legal in both, city wins. `[[patches.rigs]]` city.toml only. |
| 🟢 | `agent_defaults` | AgentDefaults | **Keep.** Legal in both pack.toml and city.toml, city wins. Surface stays as-is (no expansion in this wave). |
| 🟢 | `providers` | map[string]ProviderSpec | **Keep.** Legal in both, city wins. |
| 🟡 | `formulas` | FormulasConfig | See `[formulas].dir` below. |
| 🟢 | `beads` | BeadsConfig | **Keep in city.toml.** |
| 🟢 | `session` | SessionConfig | **Keep in city.toml.** |
| 🟢 | `mail` | MailConfig | **Keep in city.toml.** |
| 🟢 | `events` | EventsConfig | **Keep in city.toml.** |
| 🟢 | `dolt` | DoltConfig | **Keep in city.toml.** |
| 🟢 | `daemon` | DaemonConfig | **Keep in city.toml.** |
| 🟢 | `orders` | OrdersConfig | **Keep in city.toml.** |
| 🟢 | `api` | APIConfig | **Keep in city.toml.** |
| 🟢 | `chat_sessions` | ChatSessionsConfig | **Keep in city.toml.** |
| 🟢 | `session_sleep` | SessionSleepConfig | **Keep in city.toml.** |
| 🟢 | `convergence` | ConvergenceConfig | **Keep in city.toml.** |
| 🟢 | `service` | []Service | **Keep in city.toml.** Pack-defined services deferred (#657). |

## Workspace sub-fields

| Status | Field | As-built | Current rollout disposition | Later destination |
|--------|-------|----------|--------------------|-----------------------|
| 🟡 | `name` | Required string | **Optional.** Transitional runtime identity field in this wave. Fresh `gc init` keeps it aligned with `pack.name`; `gc register` keeps it aligned with the registered city name and backfills it from `pack.name` when absent. Soft warning: full site-binding cutover remains later. | `.gc/` site binding (#600) |
| 🟡 | `prefix` | String | **Optional.** Same treatment as `name`. Soft warning. | `.gc/` site binding (#600) |
| 🟡 | `provider` | String | **Soft warning.** "Use `[agent_defaults] provider = ...` instead." | `[agent_defaults]` in pack.toml |
| 🟡 | `start_command` | String | **Soft warning.** "Use per-agent `start_command` in `agent.toml` instead." | Per-agent `agent.toml` |
| 🟡 | `suspended` | Boolean | **Soft warning.** "Use `gc suspend`/`gc resume` instead." | `.gc/` site binding |
| 🟢 | `max_active_sessions` | Integer | **Keep as-is.** Deployment capacity. | Top-level city.toml field when `[workspace]` is dismantled |
| 🟢 | `session_template` | String | **Keep as-is.** Deployment. | `[session]` when `[workspace]` is dismantled |
| 🟡 | `install_agent_hooks` | []string | **Soft warning.** "Use `[agent_defaults]` instead." | `[agent_defaults]` in pack.toml |
| 🟡 | `global_fragments` | []string | **Soft warning.** "Use `[agent_defaults] append_fragments` or explicit `{{ template }}` instead." | Removed (replaced by template-fragments) |
| 🟡 | `includes` | []string | **Loud warning on schema 2.** V1 composition, use `[imports]`. | Removed |
| 🟡 | `default_rig_includes` | []string | **Loud warning on schema 2.** Use `[defaults.rig.imports]` in pack.toml. | Removed |

## Agent fields

In this rollout, `[[agent]]` gets a loud warning on schema 2. Agent fields below describe what is legal in `agent.toml` inside `agents/<name>/`.

### Convention-replaced (no TOML field)

| Status | Field | As-built | Current rollout disposition |
|--------|-------|----------|--------------------|
| 🟢 | `name` | Required string | **Convention-replaced.** Directory name is identity. |
| 🟢 | `prompt_template` | Path string | **Convention-replaced.** `prompt.template.md` or `prompt.md` in agent dir. |
| 🟢 | `overlay_dir` | Path string | **Convention-replaced.** `agents/<name>/overlay/` + pack-wide `overlays/`. |
| 🟢 | `namepool` | Path string | **Convention-replaced.** `agents/<name>/namepool.txt`. |

### V1 remnants

| Status | Field | As-built | Current rollout disposition |
|--------|-------|----------|--------------------|
| 🟡 | `dir` | String | **Gone.** Rig scoping handled by import binding. |
| 🟡 | `inject_fragments` | []string | **Loud warning on schema 2.** Use `append_fragments` or explicit `{{ template }}`. |
| 🟡 | `fallback` | Boolean | **Loud warning on schema 2.** Use qualified names + explicit precedence. |

### Legal in agent.toml

All other agent fields are legal in `agent.toml`. `[agent_defaults]` surface stays as-is in this wave (no expansion).

| Status | Field | Notes |
|--------|-------|-------|
| 🟢 | `description` | |
| 🟢 | `scope` | `"city"` or `"rig"` |
| 🟢 | `suspended` | Stays in agent.toml in this wave; moves to `.gc/` post-release |
| 🟢 | `provider` | |
| 🟢 | `start_command` | |
| 🟢 | `args` | |
| 🟢 | `session` | `"acp"` transport override |
| 🟢 | `prompt_mode` | |
| 🟢 | `prompt_flag` | |
| 🟢 | `ready_delay_ms` | |
| 🟢 | `ready_prompt_prefix` | |
| 🟢 | `process_names` | |
| 🟢 | `emits_permission_warning` | |
| 🟢 | `env` | |
| 🟢 | `option_defaults` | |
| 🟢 | `resume_command` | |
| 🟢 | `wake_mode` | |
| 🟢 | `attach` | |
| 🟢 | `max_active_sessions` | |
| 🟢 | `min_active_sessions` | |
| 🟢 | `scale_check` | |
| 🟢 | `drain_timeout` | |
| 🟢 | `pre_start` | |
| 🟢 | `on_boot` | |
| 🟢 | `on_death` | |
| 🟢 | `session_setup` | |
| 🟢 | `session_setup_script` | Path resolves against pack root |
| 🟢 | `session_live` | |
| 🟢 | `install_agent_hooks` | Overrides agent_defaults |
| 🟢 | `hooks_installed` | |
| 🟢 | `idle_timeout` | |
| 🟢 | `sleep_after_idle` | |
| 🟢 | `work_dir` | |
| 🟢 | `default_sling_formula` | |
| 🟢 | `depends_on` | |
| 🟢 | `nudge` | |
| 🟢 | `work_query` | |
| 🟢 | `sling_query` | |

## AgentDefaults

| Status | Field | As-built | Current rollout disposition |
|--------|-------|----------|--------------------|
| 🟢 | `model` | Present | **Keep.** Not yet auto-applied at runtime. |
| 🟢 | `wake_mode` | Present | **Keep.** Not yet auto-applied at runtime. |
| 🟢 | `default_sling_formula` | Present | **Keep.** Applied at runtime. |
| 🟢 | `allow_overlay` | Present | **Keep.** Not yet auto-applied at runtime. |
| 🟢 | `allow_env_override` | Present | **Keep.** Not yet auto-applied at runtime. |
| 🟢 | `append_fragments` | Present | **Keep.** Migration bridge for global_fragments/inject_fragments. |

No expansion of `[agent_defaults]` surface in this wave.

## FormulasConfig

| Status | Field | As-built | Current rollout disposition |
|--------|-------|----------|--------------------|
| 🟡 | `dir` | Default `"formulas"` | **Soft warning if present and equals `"formulas"`.** Hard error if set to anything else. `formulas/` is a fixed convention. |

## Import

| Status | Field | As-built | Current rollout disposition |
|--------|-------|----------|--------------------|
| 🟢 | `source` | Present | **Keep.** |
| 🟢 | `version` | Present | **Keep.** |
| 🟢 | `export` | Present | **Keep.** |
| 🟢 | `transitive` | Present | **Keep.** |
| 🟢 | `shadow` | Present | **Keep.** |

All Import fields match spec. No changes needed.

## Rig

| Status | Field | As-built | Current rollout disposition | Later destination |
|--------|-------|----------|--------------------|----|
| 🟢 | `name` | Required | **Keep in city.toml.** | |
| 🟢 | `path` | Required | **Keep in city.toml.** | `.gc/site.toml` (#588) |
| 🟢 | `prefix` | String | **Keep in city.toml.** | `.gc/` (#588) |
| 🟢 | `suspended` | Boolean | **Keep in city.toml.** | `.gc/` (#588) |
| 🟡 | `includes` | []string | **Loud warning on schema 2.** Use `[rigs.imports]`. | Removed |
| 🟢 | `imports` | map[string]Import | **Keep in city.toml.** | |
| 🟢 | `max_active_sessions` | Integer | **Keep in city.toml.** | |
| 🟡 | `overrides` | []AgentOverride | **Soft warning.** "Use `patches` instead." Both accepted. | Removed |
| 🟢 | `patches` | []AgentOverride | **Keep in city.toml.** V2 name. | |
| 🟢 | `default_sling_target` | String | **Keep in city.toml.** | |
| 🟢 | `session_sleep` | SessionSleepConfig | **Keep in city.toml.** | |
| 🟡 | `formulas_dir` | String | **Loud warning on schema 2.** Use rig-scoped import instead. | Removed |
| 🟢 | `dolt_host` | String | **Keep in city.toml.** | |
| 🟢 | `dolt_port` | String | **Keep in city.toml.** | |

## AgentOverride / AgentPatch

| Status | Field | As-built | Current rollout disposition |
|--------|-------|----------|--------------------|
| 🟡 | `inject_fragments` | Present | **Loud warning.** V1 remnant. |
| 🟡 | `inject_fragments_append` | Present | **Loud warning.** V1 remnant. |
| 🟢 | `prompt_template` | Path string | **Keep in this wave.** Post-release: convention-based via `patches/`. |
| 🟢 | `overlay_dir` | Path string | **Keep in this wave.** Post-release: convention-based. |
| 🟢 | `dir` + `name` targeting (AgentPatch) | Present | **Keep in this wave.** Qualified name targeting already works. |
| 🟢 | All other override fields | Present | **Keep.** |

## PackSource

| Status | Field | As-built | Current rollout disposition |
|--------|-------|----------|--------------------|
| 🟡 | (entire struct) | Present | **Loud warning on schema 2.** V1 mechanism, use `[imports]` + `pack.lock`. |

---

## Spec features — implementation status

| Status | Concept | Spec location | Notes |
|--------|---------|--------------|-------|
| 🟢 | `[imports]` resolution | doc-pack-v2, doc-loader-v2 | `ExpandCityPacks` in pack.go |
| 🟢 | Convention agent discovery (`agents/<name>/`) | doc-agent-v2 | `DiscoverPackAgents` in agent_discovery.go |
| 🟢 | `pack.toml` separate parsing | doc-loader-v2 | compose.go reads pack.toml alongside city.toml |
| 🟢 | `[agent_defaults]` in both files | doc-pack-v2 | Works via composition pipeline |
| 🟢 | `prompt.template.md` convention | doc-agent-v2 | agent_discovery.go discovers it |
| 🟢 | `agents/<name>/overlay/` convention | doc-agent-v2 | agent_discovery.go discovers it |
| 🟢 | `agents/<name>/namepool.txt` convention | doc-agent-v2 | agent_discovery.go discovers it |
| 🟢 | `per-provider/` overlay filtering | doc-agent-v2 | `CopyDirForProvider` in overlay.go |
| 🟢 | `template-fragments/` discovery | doc-agent-v2 | Pack + per-agent level in prompt.go |
| 🟢 | `append_fragments` on AgentDefaults | doc-agent-v2 | Applied at runtime |
| 🟢 | Qualified name patch targeting | doc-agent-v2 | `qualifiedNameFromPatch` in patch.go |
| 🟢 | Import `shadow` field | doc-pack-v2 | Warning/silent logic in pack.go |
| 🟢 | `orders/` top-level discovery | doc-directory-conventions | `discoverFlatFiles` in orders/discovery.go |
| 🟢 | `commands/` convention discovery | doc-commands | `DiscoverPackCommands` in command_discovery.go |
| 🔴 | `[defaults.rig.imports]` loader support | doc-pack-v2 | Migrate tool writes it, loader ignores it |
| 🟢 | `gc register --name` flag | doc-pack-v2 | Implemented. The current rollout persists the chosen registration name into `workspace.name`; no-flag registration uses `workspace.name` first, then `pack.name` and backfills `workspace.name`. |
| 🔴 | `patches/` directory convention | doc-agent-v2 | Not implemented |
| 🔴 | `skills/` pack discovery | doc-agent-v2 | First slice is current-city-pack only with list-only visibility; imported-pack catalogs are later |
| 🔴 | `mcp/` TOML abstraction | doc-agent-v2 | First slice is current-city-pack only with list-only visibility; provider projection is later |
| 🔵 | `.gc/site.toml` for rig bindings | doc-pack-v2 | #588 (may slip to a later wave) |
| 🔵 | Pack/Deployment/SiteBinding struct separation | doc-loader-v2 | Loader composes into one City struct |
| 🔵 | Pack-defined `[[service]]` | — | #657 |
| 🔵 | Expansion of `[agent_defaults]` to all agent fields | — | later wave |

---

## Fast-follow deliverables (post-merge, pre-April 21 launch)

1. **Deprecation warning infrastructure** — implement loud and soft warnings for all V1 fields listed above.
2. **Loud warnings for schema 2 cities** using `[[agent]]`, `workspace.includes`, `workspace.default_rig_includes`, `[packs]`, `rigs.includes`, `rigs.formulas_dir`, `fallback`, `inject_fragments`.
3. **Soft warnings** for `workspace.name`, `workspace.prefix`, `workspace.provider`, `workspace.start_command`, `workspace.suspended`, `workspace.install_agent_hooks`, `workspace.global_fragments`, `rigs.overrides`, `[formulas].dir`.
4. **Hard error** for `[formulas].dir` set to anything other than `"formulas"`.
5. **Hard error** for `include` fragments that contain `[imports]`, `includes`, or reference `pack.toml`.
