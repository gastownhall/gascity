# Gas City Configuration

Schema for city.toml — the top-level configuration file for a Gas City instance.

> **Auto-generated** — do not edit. Run `go run ./cmd/genschema` to regenerate.

## City

City is the top-level configuration for a Gas City instance.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `include` | []string |  |  | Include lists config fragment files to merge into this config. Processed by LoadWithIncludes; not recursive (fragments cannot include). |
| `workspace` | Workspace | **yes** |  | Workspace holds city-level metadata (name, default provider). |
| `providers` | map[string]ProviderSpec |  |  | Providers defines named provider presets for agent startup. |
| `packs` | map[string]PackSource |  |  | Packs defines named remote pack sources fetched via git. |
| `agents` | []Agent | **yes** |  | Agents lists all configured agents in this city. |
| `rigs` | []Rig |  |  | Rigs lists external projects registered in the city. |
| `patches` | Patches |  |  | Patches holds targeted modifications applied after fragment merge. |
| `beads` | BeadsConfig |  |  | Beads configures the bead store backend. |
| `session` | SessionConfig |  |  | Session configures the session provider backend. |
| `mail` | MailConfig |  |  | Mail configures the mail provider backend. |
| `events` | EventsConfig |  |  | Events configures the events provider backend. |
| `dolt` | DoltConfig |  |  | Dolt configures optional dolt server connection overrides. |
| `formulas` | FormulasConfig |  |  | Formulas configures formula directory settings. |
| `daemon` | DaemonConfig |  |  | Daemon configures controller daemon settings. |
| `automations` | AutomationsConfig |  |  | Automations configures automation settings (skip list). |
| `api` | APIConfig |  |  | API configures the optional HTTP API server. |

## ACPSessionConfig

ACPSessionConfig holds settings for the ACP session provider.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `handshake_timeout` | string |  | `30s` | HandshakeTimeout is how long to wait for the ACP handshake to complete. Duration string (e.g., "30s", "1m"). Defaults to "30s". |
| `nudge_busy_timeout` | string |  | `60s` | NudgeBusyTimeout is how long to wait for an agent to become idle before sending a new prompt. Duration string. Defaults to "60s". |
| `output_buffer_lines` | integer |  | `1000` | OutputBufferLines is the number of output lines to keep in the circular buffer for Peek. Defaults to 1000. |

## APIConfig

APIConfig configures the optional HTTP API server.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `port` | integer |  |  | Port is the TCP port to listen on. 0 = disabled (default). |
| `bind` | string |  |  | Bind is the address to bind the listener to. Defaults to "127.0.0.1". |

## Agent

Agent defines a configured agent in the city.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the unique identifier for this agent. |
| `dir` | string |  |  | Dir is the working directory for the agent session. |
| `scope` | string |  |  | Scope defines where this agent is instantiated: "city" (one per city) or "rig" (one per rig, the default). Only meaningful for pack-defined agents; inline agents in city.toml use Dir directly. When set, replaces the older city_agents list mechanism. Enum: `city`, `rig` |
| `suspended` | boolean |  |  | Suspended prevents the reconciler from spawning this agent. Toggle with gc agent suspend/resume. |
| `pre_start` | []string |  |  | PreStart is a list of shell commands run before session creation. Commands run on the target filesystem: locally for tmux, inside the pod/container for exec providers. Template variables same as session_setup. |
| `prompt_template` | string |  |  | PromptTemplate is the path to this agent's prompt template file. Relative paths resolve against the city directory. |
| `nudge` | string |  |  | Nudge is text typed into the agent's tmux session after startup. Used for CLI agents that don't accept command-line prompts. |
| `provider` | string |  |  | Provider names the provider preset to use for this agent. |
| `start_command` | string |  |  | StartCommand overrides the provider's command for this agent. |
| `args` | []string |  |  | Args overrides the provider's default arguments. |
| `prompt_mode` | string |  | `arg` | PromptMode controls how prompts are delivered: "arg", "flag", or "none". Enum: `arg`, `flag`, `none` |
| `prompt_flag` | string |  |  | PromptFlag is the CLI flag used to pass prompts when prompt_mode is "flag". |
| `ready_delay_ms` | integer |  |  | ReadyDelayMs is milliseconds to wait after launch before considering the agent ready. |
| `ready_prompt_prefix` | string |  |  | ReadyPromptPrefix is the string prefix that indicates the agent is ready for input. |
| `process_names` | []string |  |  | ProcessNames lists process names to look for when checking if the agent is running. |
| `emits_permission_warning` | boolean |  |  | EmitsPermissionWarning indicates whether the agent emits permission prompts that should be suppressed. |
| `env` | map[string]string |  |  | Env sets additional environment variables for the agent process. |
| `pool` | PoolConfig |  |  | Pool configures elastic pool behavior. When set, the agent becomes a pool. |
| `work_query` | string |  |  | WorkQuery is the shell command to find available work for this agent. Used by gc hook and available in prompt templates as {{.WorkQuery}}. Default for fixed agents: "bd ready --assignee=<qualified-name>". Default for pool agents: "bd ready --label=pool:<qualified-name> --limit=1". Override to integrate with external task systems. |
| `sling_query` | string |  |  | SlingQuery is the command template to route a bead to this agent/pool. Used by gc sling to make a bead visible to the target's work_query. The placeholder {} is replaced with the bead ID at runtime. Default for fixed agents: "bd update {} --assignee=<qualified-name>". Default for pool agents: "bd update {} --add-label=pool:<qualified-name>". Pool agents must set both sling_query and work_query, or neither. |
| `idle_timeout` | string |  |  | IdleTimeout is the maximum time an agent session can be inactive before the controller kills and restarts it. Duration string (e.g., "15m", "1h"). Empty (default) disables idle checking. |
| `install_agent_hooks` | []string |  |  | InstallAgentHooks overrides workspace-level install_agent_hooks for this agent. When set, replaces (not adds to) the workspace default. |
| `hooks_installed` | boolean |  |  | HooksInstalled overrides automatic hook detection. Set to true when hooks are manually installed (e.g., merged into the project's own hook config) and auto-installation via install_agent_hooks is not desired. When true, the agent is treated as hook-enabled for startup behavior: no prime instruction in beacon and no delayed nudge. Interacts with install_agent_hooks — set this instead when hooks are pre-installed. |
| `session_setup` | []string |  |  | SessionSetup is a list of shell commands run after session creation. Each command is a template string supporting placeholders: {{.Session}}, {{.Agent}}, {{.Rig}}, {{.CityRoot}}, {{.CityName}}, {{.WorkDir}}. Commands run in gc's process (not inside the agent session) via sh -c. |
| `session_setup_script` | string |  |  | SessionSetupScript is the path to a script run after session_setup commands. Relative paths resolve against the city directory. The script receives context via environment variables (GC_SESSION plus existing GC_* vars). |
| `session_live` | []string |  |  | SessionLive is a list of shell commands that are safe to re-apply without restarting the agent. Run at startup (after session_setup) and re-applied on config change without triggering a restart. Must be idempotent. Typical use: tmux theming, keybindings, status bars. Same template placeholders as session_setup. |
| `overlay_dir` | string |  |  | OverlayDir is a directory whose contents are recursively copied (additive) into the agent's working directory at startup. Existing files are not overwritten. Relative paths resolve against the declaring config file's directory (pack-safe). |
| `default_sling_formula` | string |  |  | DefaultSlingFormula is the formula name automatically applied via --on when beads are slung to this agent, unless --no-formula is set. Example: "mol-polecat-work" |
| `inject_fragments` | []string |  |  | InjectFragments lists named template fragments to append to this agent's rendered prompt. Fragments come from shared template directories across all loaded packs. Each name must match a {{ define "name" }} block. |
| `attach` | boolean |  |  | Attach controls whether the agent's session supports interactive attachment (e.g., tmux attach). When false, the agent can use a lighter runtime (subprocess instead of tmux). Defaults to true. |
| `fallback` | boolean |  |  | Fallback marks this agent as a fallback definition. During pack composition, a non-fallback agent with the same name wins silently. When two fallbacks collide, the first loaded (depth-first) wins. |
| `multi` | boolean |  |  | Multi marks this agent as a multi-instance template. Users manually start/stop named instances via "gc agent start/stop/destroy". Unlike pools (declarative auto-scaling), multi is imperative. Multi and pool are mutually exclusive. |

## AgentOverride

AgentOverride modifies a pack-stamped agent for a specific rig.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `agent` | string | **yes** |  | Agent is the name of the pack agent to override (required). |
| `dir` | string |  |  | Dir overrides the stamped dir (default: rig name). |
| `scope` | string |  |  | Scope overrides the agent's scope ("city" or "rig"). |
| `suspended` | boolean |  |  | Suspended sets the agent's suspended state. |
| `pool` | PoolOverride |  |  | Pool overrides pool configuration fields. |
| `env` | map[string]string |  |  | Env adds or overrides environment variables. |
| `env_remove` | []string |  |  | EnvRemove lists env var keys to remove. |
| `pre_start` | []string |  |  | PreStart overrides the agent's pre_start commands. |
| `prompt_template` | string |  |  | PromptTemplate overrides the prompt template path. Relative paths resolve against the city directory. |
| `provider` | string |  |  | Provider overrides the provider name. |
| `start_command` | string |  |  | StartCommand overrides the start command. |
| `nudge` | string |  |  | Nudge overrides the nudge text. |
| `idle_timeout` | string |  |  | IdleTimeout overrides the idle timeout duration string (e.g., "30s", "5m", "1h"). |
| `install_agent_hooks` | []string |  |  | InstallAgentHooks overrides the agent's install_agent_hooks list. |
| `hooks_installed` | boolean |  |  | HooksInstalled overrides automatic hook detection. |
| `session_setup` | []string |  |  | SessionSetup overrides the agent's session_setup commands. |
| `session_setup_script` | string |  |  | SessionSetupScript overrides the agent's session_setup_script path. Relative paths resolve against the city directory. |
| `session_live` | []string |  |  | SessionLive overrides the agent's session_live commands. |
| `overlay_dir` | string |  |  | OverlayDir overrides the agent's overlay_dir path. Copies contents additively into the agent's working directory at startup. Relative paths resolve against the city directory. |
| `default_sling_formula` | string |  |  | DefaultSlingFormula overrides the default sling formula. |
| `inject_fragments` | []string |  |  | InjectFragments overrides the agent's inject_fragments list. |
| `pre_start_append` | []string |  |  | PreStartAppend appends commands to the agent's pre_start list (instead of replacing). Applied after PreStart if both are set. |
| `session_setup_append` | []string |  |  | SessionSetupAppend appends commands to the agent's session_setup list. |
| `session_live_append` | []string |  |  | SessionLiveAppend appends commands to the agent's session_live list. |
| `install_agent_hooks_append` | []string |  |  | InstallAgentHooksAppend appends to the agent's install_agent_hooks list. |
| `attach` | boolean |  |  | Attach overrides the agent's attach setting. |
| `multi` | boolean |  |  | Multi overrides the agent's multi-instance template flag. |
| `inject_fragments_append` | []string |  |  | InjectFragmentsAppend appends to the agent's inject_fragments list. |

## AgentPatch

AgentPatch modifies an existing agent identified by (Dir, Name).

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `dir` | string | **yes** |  | Dir is the targeting key (required with Name). Identifies the agent's working directory scope. Empty for city-scoped agents. |
| `name` | string | **yes** |  | Name is the targeting key (required). Must match an existing agent's name. |
| `scope` | string |  |  | Scope overrides the agent's scope ("city" or "rig"). |
| `suspended` | boolean |  |  | Suspended overrides the agent's suspended state. |
| `pool` | PoolOverride |  |  | Pool overrides pool configuration fields. |
| `env` | map[string]string |  |  | Env adds or overrides environment variables. |
| `env_remove` | []string |  |  | EnvRemove lists env var keys to remove after merging. |
| `pre_start` | []string |  |  | PreStart overrides the agent's pre_start commands. |
| `prompt_template` | string |  |  | PromptTemplate overrides the prompt template path. Relative paths resolve against the city directory. |
| `provider` | string |  |  | Provider overrides the provider name. |
| `start_command` | string |  |  | StartCommand overrides the start command. |
| `nudge` | string |  |  | Nudge overrides the nudge text. |
| `idle_timeout` | string |  |  | IdleTimeout overrides the idle timeout. Duration string (e.g., "30s", "5m", "1h"). |
| `install_agent_hooks` | []string |  |  | InstallAgentHooks overrides the agent's install_agent_hooks list. |
| `hooks_installed` | boolean |  |  | HooksInstalled overrides automatic hook detection. |
| `session_setup` | []string |  |  | SessionSetup overrides the agent's session_setup commands. |
| `session_setup_script` | string |  |  | SessionSetupScript overrides the agent's session_setup_script path. Relative paths resolve against the city directory. |
| `session_live` | []string |  |  | SessionLive overrides the agent's session_live commands. |
| `overlay_dir` | string |  |  | OverlayDir overrides the agent's overlay_dir path. Copies contents additively into the agent's working directory at startup. Relative paths resolve against the city directory. |
| `default_sling_formula` | string |  |  | DefaultSlingFormula overrides the default sling formula. |
| `inject_fragments` | []string |  |  | InjectFragments overrides the agent's inject_fragments list. |
| `attach` | boolean |  |  | Attach overrides the agent's attach setting. |
| `multi` | boolean |  |  | Multi overrides the agent's multi-instance template flag. |
| `pre_start_append` | []string |  |  | PreStartAppend appends commands to the agent's pre_start list (instead of replacing). Applied after PreStart if both are set. |
| `session_setup_append` | []string |  |  | SessionSetupAppend appends commands to the agent's session_setup list. |
| `session_live_append` | []string |  |  | SessionLiveAppend appends commands to the agent's session_live list. |
| `install_agent_hooks_append` | []string |  |  | InstallAgentHooksAppend appends to the agent's install_agent_hooks list. |
| `inject_fragments_append` | []string |  |  | InjectFragmentsAppend appends to the agent's inject_fragments list. |

## AutomationOverride

AutomationOverride modifies a scanned automation's scheduling fields.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the automation name to target (required). |
| `rig` | string |  |  | Rig scopes the override to a specific rig's automation. Empty matches city-level automations. |
| `enabled` | boolean |  |  | Enabled overrides whether the automation is active. |
| `gate` | string |  |  | Gate overrides the gate type. |
| `interval` | string |  |  | Interval overrides the cooldown interval. Go duration string. |
| `schedule` | string |  |  | Schedule overrides the cron expression. |
| `check` | string |  |  | Check overrides the condition gate check command. |
| `on` | string |  |  | On overrides the event gate event type. |
| `pool` | string |  |  | Pool overrides the target agent/pool. |
| `timeout` | string |  |  | Timeout overrides the per-automation timeout. Go duration string. |

## AutomationsConfig

AutomationsConfig holds automation settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `skip` | []string |  |  | Skip lists automation names to exclude from scanning. |
| `max_timeout` | string |  |  | MaxTimeout is an operator hard cap on per-automation timeouts. No automation gets more than this duration. Go duration string (e.g., "60s"). Empty means uncapped (no override). |
| `overrides` | []AutomationOverride |  |  | Overrides apply per-automation field overrides after scanning. Each override targets an automation by name and optionally by rig. |

## BeadsConfig

BeadsConfig holds bead store settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `provider` | string |  | `bd` | Provider selects the bead store backend: "bd" (default), "file", or "exec:<script>" for a user-supplied script. |

## DaemonConfig

DaemonConfig holds controller daemon settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `patrol_interval` | string |  | `30s` | PatrolInterval is the health patrol interval. Duration string (e.g., "30s", "5m", "1h"). Defaults to "30s". |
| `max_restarts` | integer |  | `5` | MaxRestarts is the maximum number of agent restarts within RestartWindow before the agent is quarantined. 0 means unlimited (no crash loop detection). Defaults to 5. |
| `restart_window` | string |  | `1h` | RestartWindow is the sliding time window for counting restarts. Duration string (e.g., "30s", "5m", "1h"). Defaults to "1h". |
| `shutdown_timeout` | string |  | `5s` | ShutdownTimeout is the time to wait after sending Ctrl-C before force-killing agents during shutdown. Duration string (e.g., "5s", "30s"). Set to "0s" for immediate kill. Defaults to "5s". |
| `wisp_gc_interval` | string |  |  | WispGCInterval is how often wisp GC runs. Duration string (e.g., "5m", "1h"). Wisp GC is disabled unless both WispGCInterval and WispTTL are set. |
| `wisp_ttl` | string |  |  | WispTTL is how long a closed molecule survives before being purged. Duration string (e.g., "24h", "7d"). Wisp GC is disabled unless both WispGCInterval and WispTTL are set. |
| `drift_drain_timeout` | string |  | `2m` | DriftDrainTimeout is the maximum time to wait for an agent to acknowledge a drain signal during a config-drift restart. If the agent doesn't ack within this window, the controller force-kills and restarts it. Duration string (e.g., "2m", "5m"). Defaults to "2m". |
| `observe_paths` | []string |  |  | ObservePaths lists extra directories to search for Claude JSONL session files (e.g., aimux session paths). The default search path (~/.claude/projects/) is always included. |

## DoltConfig

DoltConfig holds optional dolt server overrides.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `port` | integer |  | `3307` | Port is the dolt server port. Defaults to 3307. |
| `host` | string |  | `localhost` | Host is the dolt server hostname. Defaults to localhost. |

## EventsConfig

EventsConfig holds events provider settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `provider` | string |  |  | Provider selects the events backend: "fake", "fail", "exec:<script>", or "" (default: file-backed JSONL). |

## FormulasConfig

FormulasConfig holds formula directory settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `dir` | string |  | `.gc/formulas` | Dir is the path to the formulas directory. Defaults to ".gc/formulas". |

## K8sConfig

K8sConfig holds native K8s session provider settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `namespace` | string |  | `gc` | Namespace is the K8s namespace for agent pods. Default: "gc". |
| `image` | string |  |  | Image is the container image for agents. |
| `context` | string |  |  | Context is the kubectl/kubeconfig context. Default: current. |
| `cpu_request` | string |  | `500m` | CPURequest is the pod CPU request. Default: "500m". |
| `mem_request` | string |  | `1Gi` | MemRequest is the pod memory request. Default: "1Gi". |
| `cpu_limit` | string |  | `2` | CPULimit is the pod CPU limit. Default: "2". |
| `mem_limit` | string |  | `4Gi` | MemLimit is the pod memory limit. Default: "4Gi". |
| `prebaked` | boolean |  |  | Prebaked skips init container staging and EmptyDir volumes when true. Use with images built by `gc build-image` that have city content baked in. |

## MailConfig

MailConfig holds mail provider settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `provider` | string |  |  | Provider selects the mail backend: "fake", "fail", "exec:<script>", or "" (default: beadmail). |

## PackSource

PackSource defines a remote pack repository.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `source` | string | **yes** |  | Source is the git repository URL. |
| `ref` | string |  |  | Ref is the git ref to checkout (branch, tag, or commit). Defaults to HEAD. |
| `path` | string |  |  | Path is a subdirectory within the repo containing the pack files. |

## Patches

Patches holds all patch blocks from composition.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `agents` | []AgentPatch |  |  | Agents targets agents by (dir, name). |
| `rigs` | []RigPatch |  |  | Rigs targets rigs by name. |
| `providers` | []ProviderPatch |  |  | Providers targets providers by name. |

## PoolConfig

PoolConfig defines elastic pool parameters for an agent.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `min` | integer |  | `0` | Min is the minimum number of pool instances. Defaults to 0. |
| `max` | integer |  | `0` | Max is the maximum number of pool instances. 0 means the pool is disabled (no instances will be created). -1 means unlimited (the check command's output determines scale with no upper cap). Defaults to 0. |
| `check` | string |  | `echo 1` | Check is a shell command whose output determines desired pool size. Defaults to "echo 1". |
| `drain_timeout` | string |  | `5m` | DrainTimeout is the maximum time to wait for a pool instance to finish its current work before force-killing it. Duration string (e.g., "5m", "30m", "1h"). Defaults to "5m". |
| `on_death` | string |  |  | OnDeath is a shell command run when a pool instance dies. Default: unclaims in_progress beads assigned to the dead instance. |
| `on_boot` | string |  |  | OnBoot is a shell command run once at controller startup for each pool. Default: unclaims all in_progress beads labeled for this pool. |

## PoolOverride

PoolOverride modifies pool configuration fields.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `min` | integer |  |  | Min overrides pool minimum instances. |
| `max` | integer |  |  | Max overrides pool maximum instances. 0 means the pool is disabled. |
| `check` | string |  |  | Check overrides the pool check command. |
| `drain_timeout` | string |  |  | DrainTimeout overrides the drain timeout. Duration string (e.g., "5m", "30m", "1h"). |
| `on_death` | string |  |  | OnDeath overrides the on_death command. |
| `on_boot` | string |  |  | OnBoot overrides the on_boot command. |

## ProviderPatch

ProviderPatch modifies an existing provider identified by Name.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the targeting key (required). Must match an existing provider's name. |
| `command` | string |  |  | Command overrides the provider command. |
| `args` | []string |  |  | Args overrides the provider args. |
| `prompt_mode` | string |  |  | PromptMode overrides prompt delivery mode. Enum: `arg`, `flag`, `none` |
| `prompt_flag` | string |  |  | PromptFlag overrides the prompt flag. |
| `ready_delay_ms` | integer |  |  | ReadyDelayMs overrides the ready delay in milliseconds. |
| `env` | map[string]string |  |  | Env adds or overrides environment variables. |
| `env_remove` | []string |  |  | EnvRemove lists env var keys to remove. |
| `_replace` | boolean |  |  | Replace replaces the entire provider block instead of deep-merging. |

## ProviderSpec

ProviderSpec defines a named provider's startup parameters.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `display_name` | string |  |  | DisplayName is the human-readable name shown in UI and logs. |
| `command` | string |  |  | Command is the executable to run for this provider. |
| `args` | []string |  |  | Args are default command-line arguments passed to the provider. |
| `prompt_mode` | string |  | `arg` | PromptMode controls how prompts are delivered: "arg", "flag", or "none". Enum: `arg`, `flag`, `none` |
| `prompt_flag` | string |  |  | PromptFlag is the CLI flag used when prompt_mode is "flag" (e.g. "--prompt"). |
| `ready_delay_ms` | integer |  |  | ReadyDelayMs is milliseconds to wait after launch before the provider is considered ready. |
| `ready_prompt_prefix` | string |  |  | ReadyPromptPrefix is the string prefix that indicates the provider is ready for input. |
| `process_names` | []string |  |  | ProcessNames lists process names to look for when checking if the provider is running. |
| `emits_permission_warning` | boolean |  |  | EmitsPermissionWarning indicates whether the provider emits permission prompts. |
| `env` | map[string]string |  |  | Env sets additional environment variables for the provider process. |
| `path_check` | string |  |  | PathCheck overrides the binary name used for PATH detection. When set, lookupProvider and detectProviderName use this instead of Command for exec.LookPath checks. Useful when Command is a shell wrapper (e.g. sh -c '...') but we need to verify the real binary is installed. |

## Rig

Rig defines an external project registered in the city.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the unique identifier for this rig. |
| `path` | string | **yes** |  | Path is the absolute filesystem path to the rig's repository. |
| `prefix` | string |  |  | Prefix overrides the auto-derived bead ID prefix for this rig. |
| `suspended` | boolean |  |  | Suspended prevents the reconciler from spawning agents in this rig. Toggle with gc rig suspend/resume. |
| `formulas_dir` | string |  |  | FormulasDir is a rig-local formula directory (Layer 4). Overrides pack formulas for this rig by filename. Relative paths resolve against the city directory. |
| `includes` | []string |  |  | Includes lists pack directories or URLs for this rig. Replaces the older pack/packs fields. Each entry is a local path, a git source//sub#ref URL, or a GitHub tree URL. |
| `overrides` | []AgentOverride |  |  | Overrides are per-agent patches applied after pack expansion. |
| `default_sling_target` | string |  |  | DefaultSlingTarget is the agent qualified name used when gc sling is invoked with only a bead ID (no explicit target). Resolved via resolveAgentIdentity. Example: "rig/polecat" |

## RigPatch

RigPatch modifies an existing rig identified by Name.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the targeting key (required). Must match an existing rig's name. |
| `path` | string |  |  | Path overrides the rig's filesystem path. |
| `prefix` | string |  |  | Prefix overrides the bead ID prefix. |
| `suspended` | boolean |  |  | Suspended overrides the rig's suspended state. |

## SessionConfig

SessionConfig holds session provider settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `provider` | string |  |  | Provider selects the session backend: "fake", "fail", "subprocess", "acp", "exec:<script>", "k8s", or "" (default: tmux). |
| `k8s` | K8sConfig |  |  | K8s holds Kubernetes-specific settings for the native K8s provider. |
| `acp` | ACPSessionConfig |  |  | ACP holds settings for the ACP (Agent Client Protocol) session provider. |
| `setup_timeout` | string |  | `10s` | SetupTimeout is the per-command/script timeout for session setup and pre_start commands. Duration string (e.g., "10s", "30s"). Defaults to "10s". |
| `nudge_ready_timeout` | string |  | `10s` | NudgeReadyTimeout is how long to wait for the agent to be ready before sending nudge text. Duration string. Defaults to "10s". |
| `nudge_retry_interval` | string |  | `500ms` | NudgeRetryInterval is the retry interval between nudge readiness polls. Duration string. Defaults to "500ms". |
| `nudge_lock_timeout` | string |  | `30s` | NudgeLockTimeout is how long to wait to acquire the per-session nudge lock. Duration string. Defaults to "30s". |
| `debounce_ms` | integer |  | `500` | DebounceMs is the default debounce interval in milliseconds for send-keys. Defaults to 500. |
| `display_ms` | integer |  | `5000` | DisplayMs is the default display duration in milliseconds for status messages. Defaults to 5000. |
| `startup_timeout` | string |  | `60s` | StartupTimeout is how long to wait for each agent's Start() call before treating it as failed. Duration string (e.g., "60s", "2m"). Defaults to "60s". |
| `socket` | string |  |  | Socket specifies the tmux socket name for per-city isolation. When set, all tmux commands use "tmux -L <socket>" to connect to a dedicated server. When empty, defaults to the city name (workspace.name) — giving every city its own tmux server automatically. Set explicitly to override. |
| `remote_match` | string |  |  | RemoteMatch is a substring pattern for the hybrid provider to route sessions to the remote (K8s) backend. Sessions whose names contain this pattern go to K8s; all others stay local (tmux). Overridden by the GC_HYBRID_REMOTE_MATCH env var if set. |

## Workspace

Workspace holds city-level metadata and optional defaults that apply to all agents unless overridden per-agent.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the human-readable name for this city. |
| `provider` | string |  |  | Provider is the default provider name used by agents that don't specify one. |
| `start_command` | string |  |  | StartCommand overrides the provider's command for all agents. |
| `suspended` | boolean |  |  | Suspended controls whether the city is suspended. When true, all agents are effectively suspended: the reconciler won't spawn them, and gc hook/prime return empty. Inherits downward — individual agent/rig suspended fields are checked independently. |
| `session_template` | string |  |  | SessionTemplate is a template string supporting placeholders: {{.City}}, {{.Agent}} (sanitized), {{.Dir}}, {{.Name}}. Controls tmux session naming. Default (empty): "{{.Agent}}" — just the sanitized agent name. Per-city tmux socket isolation makes a city prefix unnecessary. |
| `install_agent_hooks` | []string |  |  | InstallAgentHooks lists provider names whose hooks should be installed into agent working directories. Agent-level overrides workspace-level (replace, not additive). Supported: "claude", "gemini", "opencode", "copilot". |
| `global_fragments` | []string |  |  | GlobalFragments lists named template fragments injected into every agent's rendered prompt. Applied before per-agent InjectFragments. Each name must match a {{ define "name" }} block from a pack's prompts/shared/ directory. |
| `includes` | []string |  |  | Includes lists pack directories or URLs to compose into this workspace. Replaces the older pack/packs fields. Each entry is a local path, a git source//sub#ref URL, or a GitHub tree URL. |

