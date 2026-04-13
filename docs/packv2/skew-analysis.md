# Spec vs. Implementation Skew Analysis — 0.13.6 Desired State

> Generated 2026-04-12 by comparing `docs/reference/config.md` (as-built
> from the release branch Go structs) against the reconciled pack v2 specs.
> Revised through field-by-field walkthrough to reflect the **0.13.6
> desired state** — not the ideal end-state, but what should ship in this
> release.

## Field placement authority

### city.toml only (not legal in pack.toml)

- `[[rigs]]` and all rig sub-fields
- `[[patches.rigs]]`
- `[beads]`, `[session]`, `[mail]`, `[events]`, `[dolt]`
- `[daemon]`, `[orders]`, `[api]`
- `[chat_sessions]`, `[session_sleep]`, `[convergence]`
- `[[service]]` (#657 tracks whether packs can define services post-0.13.6)
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
- **Accept silently** — no warning for 0.13.6. Tracked for post-release deprecation.

**Fast-follow (pre-April 21 launch):** implement deprecation warning infrastructure for all soft/loud warnings below.

---

## City (top-level struct)

| Field | As-built | 0.13.6 disposition |
|-------|----------|--------------------|
| `include` | []string, merges fragments | **Keep.** Fragment-only (`-f` path). If a fragment contains `[imports]`, `includes`, or references `pack.toml` → hard error. |
| `workspace` | Required block | **Keep as container.** Deprecated post-0.13.6 (#600). Sub-fields walked individually below. |
| `packs` | map[string]PackSource | **Loud warning on schema 2.** V1 mechanism, use `[imports]` + `pack.lock`. |
| `agent` | []Agent, required | **Loud warning on schema 2.** Not required for schema 2 — agents discovered from `agents/<name>/`. |
| `imports` | map[string]Import | **Keep.** V2 mechanism, working. |
| `named_session` | []NamedSession | **Keep.** Legal in both pack.toml and city.toml, city wins. |
| `rigs` | []Rig | **Keep in city.toml.** |
| `patches` | Patches | **Keep.** `[[patches.agent]]` and `[[patches.providers]]` legal in both, city wins. `[[patches.rigs]]` city.toml only. |
| `agent_defaults` | AgentDefaults | **Keep.** Legal in both pack.toml and city.toml, city wins. Surface stays as-is (no expansion in 0.13.6). |
| `providers` | map[string]ProviderSpec | **Keep.** Legal in both, city wins. |
| `formulas` | FormulasConfig | See `[formulas].dir` below. |
| `beads` | BeadsConfig | **Keep in city.toml.** |
| `session` | SessionConfig | **Keep in city.toml.** |
| `mail` | MailConfig | **Keep in city.toml.** |
| `events` | EventsConfig | **Keep in city.toml.** |
| `dolt` | DoltConfig | **Keep in city.toml.** |
| `daemon` | DaemonConfig | **Keep in city.toml.** |
| `orders` | OrdersConfig | **Keep in city.toml.** |
| `api` | APIConfig | **Keep in city.toml.** |
| `chat_sessions` | ChatSessionsConfig | **Keep in city.toml.** |
| `session_sleep` | SessionSleepConfig | **Keep in city.toml.** |
| `convergence` | ConvergenceConfig | **Keep in city.toml.** |
| `service` | []Service | **Keep in city.toml.** Pack-defined services deferred (#657). |

## Workspace sub-fields

| Field | As-built | 0.13.6 disposition | Post-0.13.6 destination |
|-------|----------|--------------------|-----------------------|
| `name` | Required string | **Optional.** Falls back to `pack.name` → directory basename. Soft warning: "use `gc register --name` instead." | `.gc/` site binding (#600) |
| `prefix` | String | **Optional.** Same treatment as `name`. Soft warning. | `.gc/` site binding (#600) |
| `provider` | String | **Soft warning.** "Use `[agent_defaults] provider = ...` instead." | `[agent_defaults]` in pack.toml |
| `start_command` | String | **Soft warning.** "Use per-agent `start_command` in `agent.toml` instead." | Per-agent `agent.toml` |
| `suspended` | Boolean | **Soft warning.** "Use `gc suspend`/`gc resume` instead." | `.gc/` site binding |
| `max_active_sessions` | Integer | **Keep as-is.** Deployment capacity. | Top-level city.toml field when `[workspace]` is dismantled |
| `session_template` | String | **Keep as-is.** Deployment. | `[session]` when `[workspace]` is dismantled |
| `install_agent_hooks` | []string | **Soft warning.** "Use `[agent_defaults]` instead." | `[agent_defaults]` in pack.toml |
| `global_fragments` | []string | **Soft warning.** "Use `[agent_defaults] append_fragments` or explicit `{{ template }}` instead." | Removed (replaced by template-fragments) |
| `includes` | []string | **Loud warning on schema 2.** V1 composition, use `[imports]`. | Removed |
| `default_rig_includes` | []string | **Loud warning on schema 2.** Use `[defaults.rig.imports]` in pack.toml. | Removed |

## Agent fields

In 0.13.6, `[[agent]]` gets a loud warning on schema 2. Agent fields below describe what is legal in `agent.toml` inside `agents/<name>/`.

### Convention-replaced (no TOML field)

| Field | As-built | 0.13.6 disposition |
|-------|----------|--------------------|
| `name` | Required string | **Convention-replaced.** Directory name is identity. |
| `prompt_template` | Path string | **Convention-replaced.** `prompt.template.md` or `prompt.md` in agent dir. |
| `overlay_dir` | Path string | **Convention-replaced.** `agents/<name>/overlay/` + pack-wide `overlays/`. |
| `namepool` | Path string | **Convention-replaced.** `agents/<name>/namepool.txt`. |

### V1 remnants

| Field | As-built | 0.13.6 disposition |
|-------|----------|--------------------|
| `dir` | String | **Gone.** Rig scoping handled by import binding. |
| `inject_fragments` | []string | **Loud warning on schema 2.** Use `append_fragments` or explicit `{{ template }}`. |
| `fallback` | Boolean | **Loud warning on schema 2.** Use qualified names + explicit precedence. |

### Legal in agent.toml

All other agent fields are legal in `agent.toml`. `[agent_defaults]` surface stays as-is for 0.13.6 (no expansion).

| Field | Notes |
|-------|-------|
| `description` | |
| `scope` | `"city"` or `"rig"` |
| `suspended` | Stays in agent.toml for 0.13.6; moves to `.gc/` post-release |
| `provider` | |
| `start_command` | |
| `args` | |
| `session` | `"acp"` transport override |
| `prompt_mode` | |
| `prompt_flag` | |
| `ready_delay_ms` | |
| `ready_prompt_prefix` | |
| `process_names` | |
| `emits_permission_warning` | |
| `env` | |
| `option_defaults` | |
| `resume_command` | |
| `wake_mode` | |
| `attach` | |
| `max_active_sessions` | |
| `min_active_sessions` | |
| `scale_check` | |
| `drain_timeout` | |
| `pre_start` | |
| `on_boot` | |
| `on_death` | |
| `session_setup` | |
| `session_setup_script` | Path resolves against pack root |
| `session_live` | |
| `install_agent_hooks` | Overrides agent_defaults |
| `hooks_installed` | |
| `idle_timeout` | |
| `sleep_after_idle` | |
| `work_dir` | |
| `default_sling_formula` | |
| `depends_on` | |
| `nudge` | |
| `work_query` | |
| `sling_query` | |

## AgentDefaults

| Field | As-built | 0.13.6 disposition |
|-------|----------|--------------------|
| `model` | Present | **Keep.** Not yet auto-applied at runtime. |
| `wake_mode` | Present | **Keep.** Not yet auto-applied at runtime. |
| `default_sling_formula` | Present | **Keep.** Applied at runtime. |
| `allow_overlay` | Present | **Keep.** Not yet auto-applied at runtime. |
| `allow_env_override` | Present | **Keep.** Not yet auto-applied at runtime. |
| `append_fragments` | Present | **Keep.** Migration bridge for global_fragments/inject_fragments. |

No expansion of `[agent_defaults]` surface in 0.13.6.

## FormulasConfig

| Field | As-built | 0.13.6 disposition |
|-------|----------|--------------------|
| `dir` | Default `"formulas"` | **Soft warning if present and equals `"formulas"`.** Hard error if set to anything else. `formulas/` is a fixed convention. |

## Import

| Field | As-built | 0.13.6 disposition |
|-------|----------|--------------------|
| `source` | Present ✓ | **Keep.** |
| `version` | Present ✓ | **Keep.** |
| `export` | Present ✓ | **Keep.** |
| `transitive` | Present ✓ | **Keep.** |
| `shadow` | Present ✓ | **Keep.** |

All Import fields match spec. No changes needed.

## Rig

| Field | As-built | 0.13.6 disposition | Post-0.13.6 |
|-------|----------|--------------------|----|
| `name` | Required | **Keep in city.toml.** | |
| `path` | Required | **Keep in city.toml.** | `.gc/site.toml` (#588) |
| `prefix` | String | **Keep in city.toml.** | `.gc/` (#588) |
| `suspended` | Boolean | **Keep in city.toml.** | `.gc/` (#588) |
| `includes` | []string | **Loud warning on schema 2.** Use `[rigs.imports]`. | Removed |
| `imports` | map[string]Import | **Keep in city.toml.** | |
| `max_active_sessions` | Integer | **Keep in city.toml.** | |
| `overrides` | []AgentOverride | **Soft warning.** "Use `patches` instead." Both accepted. | Removed |
| `patches` | []AgentOverride | **Keep in city.toml.** V2 name. | |
| `default_sling_target` | String | **Keep in city.toml.** | |
| `session_sleep` | SessionSleepConfig | **Keep in city.toml.** | |
| `formulas_dir` | String | **Loud warning on schema 2.** Use rig-scoped import instead. | Removed |
| `dolt_host` | String | **Keep in city.toml.** | |
| `dolt_port` | String | **Keep in city.toml.** | |

## AgentOverride / AgentPatch

| Field | As-built | 0.13.6 disposition |
|-------|----------|--------------------|
| `inject_fragments` | Present | **Loud warning.** V1 remnant. |
| `inject_fragments_append` | Present | **Loud warning.** V1 remnant. |
| `prompt_template` | Path string | **Keep for 0.13.6.** Post-release: convention-based via `patches/`. |
| `overlay_dir` | Path string | **Keep for 0.13.6.** Post-release: convention-based. |
| `dir` + `name` targeting (AgentPatch) | Present | **Keep for 0.13.6.** Post-release: target by qualified name. |
| All other override fields | Present | **Keep.** |

## PackSource

| Field | As-built | 0.13.6 disposition |
|-------|----------|--------------------|
| (entire struct) | Present | **Loud warning on schema 2.** V1 mechanism, use `[imports]` + `pack.lock`. |

---

## Not yet in code (spec features deferred from 0.13.6)

| Concept | Spec location | Status |
|---------|--------------|--------|
| `pack.toml` / `city.toml` / `.gc/` as separate parsed structs | doc-loader-v2 | Loader composes into one City struct. Structural separation is post-0.13.6. |
| `.gc/site.toml` for rig bindings | doc-pack-v2 | #588 (may slip post-0.13.6) |
| `orders/` top-level convention discovery | doc-directory-conventions | Orders still under `formulas/orders/` in bundled packs (#611) |
| `commands/` convention discovery for root city pack | doc-commands | #604 |
| `patches/` directory for prompt replacements | doc-agent-v2 | Not implemented |
| `skills/` directory discovery | doc-agent-v2 | Not implemented |
| `mcp/` TOML abstraction | doc-agent-v2 | Not implemented |
| `template-fragments/` full discovery | doc-agent-v2 | Partially implemented |
| `per-provider/` overlay filtering | doc-agent-v2 | Implemented on pack-v2 branch |
| `gc register --name` flag | doc-pack-v2 | #602 |
| Pack-defined `[[service]]` | doc-pack-v2 | #657 |
| Expansion of `[agent_defaults]` to all agent fields | — | 0.13.7 |

---

## Fast-follow deliverables (post-merge, pre-April 21 launch)

1. **Deprecation warning infrastructure** — implement loud and soft warnings for all V1 fields listed above.
2. **Loud warnings for schema 2 cities** using `[[agent]]`, `workspace.includes`, `workspace.default_rig_includes`, `[packs]`, `rigs.includes`, `rigs.formulas_dir`, `fallback`, `inject_fragments`.
3. **Soft warnings** for `workspace.name`, `workspace.prefix`, `workspace.provider`, `workspace.start_command`, `workspace.suspended`, `workspace.install_agent_hooks`, `workspace.global_fragments`, `rigs.overrides`, `[formulas].dir`.
4. **Hard error** for `[formulas].dir` set to anything other than `"formulas"`.
5. **Hard error** for `include` fragments that contain `[imports]`, `includes`, or reference `pack.toml`.
