# Gas City Configuration

Schema for city.toml — the PackV2 deployment file for a Gas City instance. Pack definitions live in pack.toml and conventional pack directories such as agents/, formulas/, orders/, and commands/. Use [imports.*] for PackV2 composition; legacy includes, [packs.*], and [[agent]] fields remain visible for migration compatibility.

> **Auto-generated** — do not edit. Run `go run ./cmd/genschema` to regenerate.

## City

City is the top-level configuration for a Gas City instance.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `include` | []string |  |  | Include lists config fragment files to merge into this config. Processed by LoadWithIncludes; not recursive (fragments cannot include). |
| `workspace` | Workspace | **yes** |  | Workspace holds city-level metadata (name, default provider). |
| `providers` | map[string]ProviderSpec |  |  | Providers defines named provider presets for agent startup. |
| `packs` | map[string]PackSource |  |  | Packs defines named remote pack sources fetched via git (V1 mechanism). |
| `imports` | map[string]Import |  |  | Imports defines named pack imports (V2 mechanism). Each key is a binding name; the value specifies the source and optional version, export, and transitive controls. Processed during ExpandCityPacks. |
| `agent` | []Agent | **yes** |  | Agents lists all configured agents in this city. |
| `named_session` | []NamedSession |  |  | NamedSessions lists canonical alias-backed sessions built from reusable agent templates. |
| `rigs` | []Rig |  |  | Rigs lists external projects registered in the city. |
| `patches` | Patches |  |  | Patches holds targeted modifications applied after fragment merge. |
| `beads` | BeadsConfig |  |  | Beads configures the bead store backend. |
| `session` | SessionConfig |  |  | Session configures the session provider backend. |
| `mail` | MailConfig |  |  | Mail configures the mail provider backend. |
| `events` | EventsConfig |  |  | Events configures the events provider backend. |
| `dolt` | DoltConfig |  |  | Dolt configures optional dolt server connection overrides. |
| `formulas` | FormulasConfig |  |  | Formulas configures formula directory settings. |
| `daemon` | DaemonConfig |  |  | Daemon configures controller daemon settings. |
| `orders` | OrdersConfig |  |  | Orders configures order settings (skip list). |
| `api` | APIConfig |  |  | API configures the optional HTTP API server. |
| `chat_sessions` | ChatSessionsConfig |  |  | ChatSessions configures chat session behavior (auto-suspend). |
| `session_sleep` | SessionSleepConfig |  |  | SessionSleep configures idle sleep policy defaults for managed sessions. |
| `convergence` | ConvergenceConfig |  |  | Convergence configures convergence loop limits. |
| `doctor` | DoctorConfig |  |  | Doctor configures gc doctor thresholds and policy toggles (worktree size warnings, nested-worktree auto-prune). |
| `service` | []Service |  |  | Services declares workspace-owned HTTP services mounted on the controller edge under /svc/&#123;name&#125;. |
| `agent_defaults` | AgentDefaults |  |  | AgentDefaults provides city-level defaults for agents that don't override them (canonical TOML key: agent_defaults). The runtime currently applies default_sling_formula and append_fragments; the attachment-list fields remain tombstones, and the other fields are parsed/composed but not yet inherited automatically. |
| `pricing` | []ModelPricing |  |  | Pricing holds per-model cost rate overrides keyed by (provider, model). City-level entries override pack-level entries which override the defaults shipped with the pricing package. See internal/pricing for the estimation seam introduced by issue #1255 (1d). |

## ACPSessionConfig

ACPSessionConfig holds settings for the ACP session provider.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `handshake_timeout` | string |  | `30s` | HandshakeTimeout is how long to wait for the ACP handshake to complete. Duration string (e.g., "30s", "1m"). Defaults to "30s". |
| `nudge_busy_timeout` | string |  | `60s` | NudgeBusyTimeout is how long to wait for an agent to become idle before sending a new prompt. Duration string. Defaults to "60s". |
| `output_buffer_lines` | integer |  | `1000` | OutputBufferLines is the number of output lines to keep in the circular buffer for Peek. Defaults to 1000. |

## APIConfig

APIConfig configures the HTTP API server.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `port` | integer |  |  | Port is the TCP port to listen on. Defaults to 9443; 0 = disabled. |
| `bind` | string |  |  | Bind is the address to bind the listener to. Defaults to "127.0.0.1". |
| `allow_mutations` | boolean |  |  | AllowMutations overrides the default read-only behavior when bind is non-localhost. Set to true in containerized environments where the API must bind to 0.0.0.0 for health probes but mutations are still safe. |

## Agent

Agent defines a configured agent in the city.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the unique identifier for this agent. |
| `description` | string |  |  | Description is a human-readable description shown in MC's session creation UI. |
| `dir` | string |  |  | Dir is the identity prefix for rig-scoped agents and the default working directory when WorkDir is not set. |
| `work_dir` | string |  |  | WorkDir overrides the session working directory without changing the agent's qualified identity. Relative paths resolve against city root and may use the same template placeholders as session_setup. |
| `scope` | string |  |  | Scope defines where this agent is instantiated: "city" (one per city) or "rig" (one per rig, the default). Only meaningful for pack-defined agents; inline agents in city.toml use Dir directly. Enum: `city`, `rig` |
| `suspended` | boolean |  |  | Suspended prevents the reconciler from spawning this agent. Toggle with gc agent suspend/resume. |
| `pre_start` | []string |  |  | PreStart is a list of shell commands run before session creation. Commands run on the target filesystem: locally for tmux, inside the pod/container for exec providers. Template variables same as session_setup. |
| `prompt_template` | string |  |  | PromptTemplate is the path to this agent's prompt template file. Relative paths resolve against the city directory. |
| `nudge` | string |  |  | Nudge is text typed into the agent's tmux session after startup. Used for CLI agents that don't accept command-line prompts. |
| `session` | string |  |  | Session overrides the session transport for this agent. "" (default) uses the city-level session provider (typically tmux). "acp" uses the Agent Client Protocol (JSON-RPC over stdio). The agent's resolved provider must have supports_acp = true. Enum: `acp` |
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
| `option_defaults` | map[string]string |  |  | OptionDefaults overrides the provider's effective schema defaults for this agent. Keys are option keys, values are choice values. Applied on top of the provider's OptionDefaults (agent keys win). Example: option_defaults = &#123; permission_mode = "plan", model = "sonnet" &#125; |
| `max_active_sessions` | integer |  |  | MaxActiveSessions is the agent-level cap on concurrent sessions. Nil means inherit from rig, then workspace, then unlimited. Replaces pool.max. |
| `min_active_sessions` | integer |  |  | MinActiveSessions is the minimum number of sessions to keep alive. Agent-level only. Counts against rig/workspace caps. Replaces pool.min. |
| `scale_check` | string |  |  | ScaleCheck is a shell command template whose output reports new unassigned session demand. In bead-backed reconciliation this is additive: assigned work is resumed separately, and ScaleCheck reports only how many new generic sessions to start, still bounded by all cap levels. Legacy no-store evaluation continues to treat the output as the desired session count. If it contains Go template placeholders, gc expands them using the same PathContext fields as work_dir and session_setup (Agent, AgentBase, Rig, RigRoot, CityRoot, CityName) before running the command. |
| `drain_timeout` | string |  | `5m` | DrainTimeout is the maximum time to wait for a session to finish its current work before force-killing it during scale-down. Duration string (e.g., "5m", "30m", "1h"). Defaults to "5m". |
| `on_boot` | string |  |  | OnBoot is a shell command template run once at controller startup for this agent. If it contains Go template placeholders, gc expands them using the same PathContext fields as work_dir and session_setup (Agent, AgentBase, Rig, RigRoot, CityRoot, CityName) before running the command. |
| `on_death` | string |  |  | OnDeath is a shell command template run when a session dies unexpectedly. If it contains Go template placeholders, gc expands them using the same PathContext fields as work_dir and session_setup (Agent, AgentBase, Rig, RigRoot, CityRoot, CityName) before running the command. |
| `namepool` | string |  |  | Namepool is the path to a plain text file with one name per line. When set, sessions use names from the file as display aliases. |
| `work_query` | string |  |  | WorkQuery is the shell command template to find available work for this agent. If it contains Go template placeholders, gc expands them using the same PathContext fields as work_dir and session_setup (Agent, AgentBase, Rig, RigRoot, CityRoot, CityName) before probe, hook, and prompt-context execution. Used by gc hook and available in prompt templates as &#123;&#123;.WorkQuery&#125;&#125;. If unset, Gas City uses a three-tier default query:   1. in_progress work assigned to this session/alias (crash recovery)   2. ready work assigned to this session/alias (pre-assigned work)   3. ready unassigned work with gc.routed_to=&lt;qualified-name&gt; When the controller probes for demand without session context, only the routed_to tier applies. Override to integrate with external task systems. |
| `sling_query` | string |  |  | SlingQuery is the command template to route a bead to this session config. If it contains Go template placeholders, gc expands them using the same PathContext fields as work_dir and session_setup (Agent, AgentBase, Rig, RigRoot, CityRoot, CityName) before replacing &#123;&#125; with the bead ID. Used by gc sling to make a bead visible to the target's work_query. The placeholder &#123;&#125; is replaced with the bead ID at runtime. Default for all agents: "bd update &#123;&#125; --set-metadata gc.routed_to=&lt;qualified-name&gt;". Routing is metadata-based; sling stamps the target template and the reconciler/scale_check paths decide when sessions are created. Custom sling_query and work_query can be overridden independently. |
| `idle_timeout` | string |  |  | IdleTimeout is the maximum time an agent session can be inactive before the controller kills and restarts it. Duration string (e.g., "15m", "1h"). Empty (default) disables idle checking. |
| `max_session_age` | string |  |  | MaxSessionAge is the maximum wall-clock lifetime of a single runtime session before the controller preemptively restarts it. Duration string (e.g., "5h"). Empty (default) disables preemptive restarts. The restart is idle-gated: sessions with a pending interaction or an in-progress assigned work bead are left alone until they settle.  Motivation: provider SDKs that cache credentials at session start (e.g., Claude Code via Bedrock) can wedge when the underlying token expires if the SDK doesn't re-chain providers. Cycling long-running sessions before the token-expiry window prevents that failure mode without requiring upstream provider fixes. |
| `max_session_age_jitter` | string |  |  | MaxSessionAgeJitter bounds random jitter added to MaxSessionAge on a per-session basis so a fleet of identically-configured agents doesn't synchronize restarts. Duration string (e.g., "15m"). Empty or 0 disables jitter (every session restarts at exactly MaxSessionAge). Ignored when MaxSessionAge is unset. |
| `sleep_after_idle` | string |  |  | SleepAfterIdle overrides idle sleep policy for this agent. Accepts a duration string (e.g., "30s") or "off". |
| `install_agent_hooks` | []string |  |  | InstallAgentHooks overrides workspace-level install_agent_hooks for this agent. When set, replaces (not adds to) the workspace default. |
| `skills` | []string |  |  | Skills is a tombstone field retained for v0.15.1 backwards compatibility. Accepted during parse for migration visibility, but attachment-list fields are accepted but ignored by the active materializer. |
| `mcp` | []string |  |  | MCP is a tombstone field retained for v0.15.1 backwards compatibility. Accepted during parse for migration visibility, but attachment-list fields are accepted but ignored by the active materializer. |
| `hooks_installed` | boolean |  |  | HooksInstalled overrides automatic hook detection. Set to true when hooks are manually installed (e.g., merged into the project's own hook config) and auto-installation via install_agent_hooks is not desired. When true, the agent is treated as hook-enabled for startup behavior: no prime instruction in beacon and no delayed nudge. Interacts with install_agent_hooks — set this instead when hooks are pre-installed. |
| `session_setup` | []string |  |  | SessionSetup is a list of shell commands run after session creation. Each command is a template string supporting placeholders: &#123;&#123;.Session&#125;&#125;, &#123;&#123;.Agent&#125;&#125;, &#123;&#123;.AgentBase&#125;&#125;, &#123;&#123;.Rig&#125;&#125;, &#123;&#123;.RigRoot&#125;&#125;, &#123;&#123;.CityRoot&#125;&#125;, &#123;&#123;.CityName&#125;&#125;, &#123;&#123;.WorkDir&#125;&#125;. Commands run in gc's process (not inside the agent session) via sh -c. |
| `session_setup_script` | string |  |  | SessionSetupScript is the path to a script run after session_setup commands. Relative paths resolve against the declaring config file's directory (pack-safe). Paths prefixed with "//" resolve against the city root. The script receives context via environment variables (GC_SESSION plus existing GC_* vars). |
| `session_live` | []string |  |  | SessionLive is a list of shell commands that are safe to re-apply without restarting the agent. Run at startup (after session_setup) and re-applied on config change without triggering a restart. Must be idempotent. Typical use: tmux theming, keybindings, status bars. Same template placeholders as session_setup. |
| `overlay_dir` | string |  |  | OverlayDir is a directory whose contents are recursively copied (additive) into the agent's working directory at startup. Existing files are not overwritten. Relative paths resolve against the declaring config file's directory (pack-safe). |
| `default_sling_formula` | string |  |  | DefaultSlingFormula is the formula name automatically applied via --on when beads are slung to this agent, unless --no-formula is set. Example: "mol-polecat-work" |
| `inject_fragments` | []string |  |  | InjectFragments lists named template fragments to append to this agent's rendered prompt. Fragments come from shared template directories across all loaded packs. Each name must match a &#123;&#123; define "name" &#125;&#125; block. |
| `append_fragments` | []string |  |  | AppendFragments is the V2 per-agent alias for prompt fragment injection. It layers after InjectFragments and before inherited/default fragments. |
| `inject_assigned_skills` | boolean |  |  | InjectAssignedSkills controls whether gc appends an "assigned skills" appendix to the agent's rendered prompt. The appendix lists every skill visible to this agent, partitioned into (assigned-to-you, shared-with-every-agent), so agents sharing a scope-root sink can tell which skills are their specialization vs which are the city-wide set.  Pointer tri-state:   nil   -&gt; inherit: inject when the agent has a vendor sink   *true -&gt; explicitly inject (equivalent to the default)   *false -&gt; disable; the template is responsible for rendering             any skill guidance itself |
| `attach` | boolean |  |  | Attach controls whether the agent's session supports interactive attachment (e.g., tmux attach). When false, the agent can use a lighter runtime (subprocess instead of tmux). Defaults to true. |
| `fallback` | boolean |  |  | Fallback marks this agent as a fallback definition. During pack composition, a non-fallback agent with the same name wins silently. When two fallbacks collide, the first loaded (depth-first) wins. |
| `depends_on` | []string |  |  | DependsOn lists agent names that must be awake before this agent wakes. Used for dependency-ordered startup and shutdown. Validated for cycles at config load time. |
| `resume_command` | string |  |  | ResumeCommand is the full shell command to run when resuming this agent. Supports &#123;&#123;.SessionKey&#125;&#125; template variable. When set, takes precedence over the provider's ResumeFlag/ResumeStyle. Example:   "claude --resume &#123;&#123;.SessionKey&#125;&#125; --dangerously-skip-permissions" |
| `wake_mode` | string |  |  | WakeMode controls context freshness across sleep/wake cycles. "resume" (default): reuse provider session key for conversation continuity. "fresh": start a new provider session on every wake (polecat pattern). Enum: `resume`, `fresh` |

## AgentDefaults

AgentDefaults provides city-level agent defaults declared via [agent_defaults] in city.toml.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `model` | string |  |  | Model is the parsed/composed default model name for agents (e.g., "claude-sonnet-4-6"), but it is not yet auto-applied at runtime. Agents with their own model override would take precedence. |
| `wake_mode` | string |  |  | WakeMode is the parsed/composed default wake mode ("resume" or "fresh"), but it is not yet auto-applied at runtime. Enum: `resume`, `fresh` |
| `default_sling_formula` | string |  |  | DefaultSlingFormula is the city-level default formula used for agents that inherit [agent_defaults]. Explicit agents only receive this value when agent_defaults.default_sling_formula is set; implicit multi-session configs are seeded with "mol-do-work" elsewhere when no explicit default is set. |
| `allow_overlay` | []string |  |  | AllowOverlay is parsed and composed as a city-level allowlist for session overlays, but it is not yet inherited onto agents automatically at runtime. |
| `allow_env_override` | []string |  |  | AllowEnvOverride is parsed and composed as a city-level allowlist for session env overrides, but it is not yet inherited onto agents automatically at runtime. Names must match ^[A-Z][A-Z0-9_]&#123;0,127&#125;$. |
| `append_fragments` | []string |  |  | AppendFragments lists named template fragments to auto-append to .template.md prompts after rendering. Legacy .md.tmpl prompts are still supported during the transition; plain .md remains inert. V2 migration convenience — replaces global_fragments/inject_fragments for city-wide defaults. |
| `skills` | []string |  |  | Skills is a tombstone field retained for v0.15.1 backwards compatibility. Parsed and composed for migration visibility, but attachment-list fields are accepted but ignored by the active materializer. |
| `mcp` | []string |  |  | MCP is a tombstone field retained for v0.15.1 backwards compatibility. Parsed and composed for migration visibility, but attachment-list fields are accepted but ignored by the active materializer. |

## AgentOverride

AgentOverride modifies a pack-stamped agent for a specific rig.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `agent` | string | **yes** |  | Agent is the name of the pack agent to override (required). |
| `dir` | string |  |  | Dir overrides the stamped dir (default: rig name). |
| `work_dir` | string |  |  | WorkDir overrides the agent's working directory without changing its qualified identity or rig association. |
| `scope` | string |  |  | Scope overrides the agent's scope ("city" or "rig"). |
| `suspended` | boolean |  |  | Suspended sets the agent's suspended state. |
| `pool` | PoolOverride |  |  | Pool overrides legacy [pool] fields that map to session scaling. |
| `env` | map[string]string |  |  | Env adds or overrides environment variables. |
| `env_remove` | []string |  |  | EnvRemove lists env var keys to remove. |
| `pre_start` | []string |  |  | PreStart overrides the agent's pre_start commands. |
| `prompt_template` | string |  |  | PromptTemplate overrides the prompt template path. Relative paths resolve against the city directory. |
| `session` | string |  |  | Session overrides the session transport ("acp"). |
| `provider` | string |  |  | Provider overrides the provider name. |
| `start_command` | string |  |  | StartCommand overrides the start command. |
| `nudge` | string |  |  | Nudge overrides the nudge text. |
| `idle_timeout` | string |  |  | IdleTimeout overrides the idle timeout duration string (e.g., "30s", "5m", "1h"). |
| `max_session_age` | string |  |  | MaxSessionAge overrides the max session age. Duration string (e.g., "5h"). Empty disables preemptive restart. |
| `max_session_age_jitter` | string |  |  | MaxSessionAgeJitter overrides the jitter added on top of MaxSessionAge. Duration string (e.g., "15m"). Empty disables jitter. |
| `sleep_after_idle` | string |  |  | SleepAfterIdle overrides idle sleep policy for this agent. Accepts a duration string (e.g., "30s") or "off". |
| `install_agent_hooks` | []string |  |  | InstallAgentHooks overrides the agent's install_agent_hooks list. |
| `skills` | []string |  |  | Skills is a tombstone field retained for v0.15.1 backwards compatibility. Parsed for migration visibility, but attachment-list fields are accepted but ignored by the active materializer. |
| `mcp` | []string |  |  | MCP is a tombstone field retained for v0.15.1 backwards compatibility. Parsed for migration visibility, but attachment-list fields are accepted but ignored by the active materializer. |
| `hooks_installed` | boolean |  |  | HooksInstalled overrides automatic hook detection. |
| `inject_assigned_skills` | boolean |  |  | InjectAssignedSkills overrides Agent.InjectAssignedSkills (see that field for semantics). |
| `session_setup` | []string |  |  | SessionSetup overrides the agent's session_setup commands. |
| `session_setup_script` | string |  |  | SessionSetupScript overrides the agent's session_setup_script path. Relative paths resolve against the declaring config file's directory (pack-safe). Paths prefixed with "//" resolve against the city root. |
| `session_live` | []string |  |  | SessionLive overrides the agent's session_live commands. |
| `overlay_dir` | string |  |  | OverlayDir overrides the agent's overlay_dir path. Copies contents additively into the agent's working directory at startup. Relative paths resolve against the city directory. |
| `default_sling_formula` | string |  |  | DefaultSlingFormula overrides the default sling formula. |
| `inject_fragments` | []string |  |  | InjectFragments overrides the agent's inject_fragments list. Leave this field unset to keep inherited fragments; JSON callers may send null for the same no-op. Set an empty list to clear fragments; set a populated list to replace fragments. |
| `append_fragments` | []string |  |  | AppendFragments appends named template fragments to this agent's rendered prompt. It is the V2 spelling for per-agent fragment selection. |
| `pre_start_append` | []string |  |  | PreStartAppend appends commands to the agent's pre_start list (instead of replacing). Applied after PreStart if both are set. |
| `session_setup_append` | []string |  |  | SessionSetupAppend appends commands to the agent's session_setup list. |
| `session_live_append` | []string |  |  | SessionLiveAppend appends commands to the agent's session_live list. |
| `install_agent_hooks_append` | []string |  |  | InstallAgentHooksAppend appends to the agent's install_agent_hooks list. |
| `skills_append` | []string |  |  | SkillsAppend is a tombstone field retained for v0.15.1 backwards compatibility. Parsed for migration visibility, but attachment-list fields are accepted but ignored by the active materializer. |
| `mcp_append` | []string |  |  | MCPAppend is a tombstone field retained for v0.15.1 backwards compatibility. Parsed for migration visibility, but attachment-list fields are accepted but ignored by the active materializer. |
| `attach` | boolean |  |  | Attach overrides the agent's attach setting. |
| `depends_on` | []string |  |  | DependsOn overrides the agent's dependency list. |
| `resume_command` | string |  |  | ResumeCommand overrides the agent's resume_command template. |
| `wake_mode` | string |  |  | WakeMode overrides the agent's wake mode ("resume" or "fresh"). Enum: `resume`, `fresh` |
| `inject_fragments_append` | []string |  |  | InjectFragmentsAppend appends to the agent's inject_fragments list. |
| `max_active_sessions` | integer |  |  | MaxActiveSessions overrides the agent-level cap on concurrent sessions. |
| `min_active_sessions` | integer |  |  | MinActiveSessions overrides the minimum number of sessions to keep alive. |
| `scale_check` | string |  |  | ScaleCheck overrides the shell command whose output reports new unassigned session demand for bead-backed reconciliation. |
| `option_defaults` | map[string]string |  |  | OptionDefaults adds or overrides provider option defaults for this agent. Keys are option keys, values are choice values. Merges additively (override keys win over existing agent keys). Example: option_defaults = &#123; model = "sonnet" &#125; |

## AgentPatch

AgentPatch modifies an existing agent identified by (Dir, Name).

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `dir` | string | **yes** |  | Dir is the targeting key (required with Name). Identifies the agent's working directory scope. Empty for city-scoped agents. |
| `name` | string | **yes** |  | Name is the targeting key (required). Must match an existing agent's name. |
| `work_dir` | string |  |  | WorkDir overrides the agent's session working directory. |
| `scope` | string |  |  | Scope overrides the agent's scope ("city" or "rig"). |
| `suspended` | boolean |  |  | Suspended overrides the agent's suspended state. |
| `pool` | PoolOverride |  |  | Pool overrides legacy [pool] fields that map to session scaling. |
| `env` | map[string]string |  |  | Env adds or overrides environment variables. |
| `env_remove` | []string |  |  | EnvRemove lists env var keys to remove after merging. |
| `pre_start` | []string |  |  | PreStart overrides the agent's pre_start commands. |
| `prompt_template` | string |  |  | PromptTemplate overrides the prompt template path. Relative paths resolve against the city directory. |
| `session` | string |  |  | Session overrides the session transport ("acp" or "tmux"). |
| `provider` | string |  |  | Provider overrides the provider name. |
| `start_command` | string |  |  | StartCommand overrides the start command. |
| `nudge` | string |  |  | Nudge overrides the nudge text. |
| `idle_timeout` | string |  |  | IdleTimeout overrides the idle timeout. Duration string (e.g., "30s", "5m", "1h"). |
| `max_session_age` | string |  |  | MaxSessionAge overrides the max session age. Duration string (e.g., "5h"). |
| `max_session_age_jitter` | string |  |  | MaxSessionAgeJitter overrides the max session age jitter. Duration string (e.g., "15m"). |
| `sleep_after_idle` | string |  |  | SleepAfterIdle overrides idle sleep policy for this agent. Accepts a duration string or "off". |
| `install_agent_hooks` | []string |  |  | InstallAgentHooks overrides the agent's install_agent_hooks list. |
| `skills` | []string |  |  | Skills is a tombstone field retained for v0.15.1 backwards compatibility.  Deprecated: removed in v0.16. Tombstone — accepted but ignored. See engdocs/proposals/skill-materialization.md |
| `mcp` | []string |  |  | MCP is a tombstone field retained for v0.15.1 backwards compatibility.  Deprecated: removed in v0.16. Tombstone — accepted but ignored. See engdocs/proposals/skill-materialization.md |
| `skills_append` | []string |  |  | SkillsAppend is a tombstone field retained for v0.15.1 backwards compatibility.  Deprecated: removed in v0.16. Tombstone — accepted but ignored. See engdocs/proposals/skill-materialization.md |
| `mcp_append` | []string |  |  | MCPAppend is a tombstone field retained for v0.15.1 backwards compatibility.  Deprecated: removed in v0.16. Tombstone — accepted but ignored. See engdocs/proposals/skill-materialization.md |
| `hooks_installed` | boolean |  |  | HooksInstalled overrides automatic hook detection. |
| `inject_assigned_skills` | boolean |  |  | InjectAssignedSkills overrides per-agent appendix injection (see Agent.InjectAssignedSkills). |
| `session_setup` | []string |  |  | SessionSetup overrides the agent's session_setup commands. |
| `session_setup_script` | string |  |  | SessionSetupScript overrides the agent's session_setup_script path. Relative paths resolve against the declaring config file's directory (pack-safe). Paths prefixed with "//" resolve against the city root. |
| `session_live` | []string |  |  | SessionLive overrides the agent's session_live commands. |
| `overlay_dir` | string |  |  | OverlayDir overrides the agent's overlay_dir path. Copies contents additively into the agent's working directory at startup. Relative paths resolve against the city directory. |
| `default_sling_formula` | string |  |  | DefaultSlingFormula overrides the default sling formula. |
| `inject_fragments` | []string |  |  | InjectFragments overrides the agent's inject_fragments list. Leave this field unset to keep inherited fragments; JSON callers may send null for the same no-op. Set an empty list to clear fragments; set a populated list to replace fragments. |
| `append_fragments` | []string |  |  | AppendFragments overrides the agent's append_fragments list. |
| `attach` | boolean |  |  | Attach overrides the agent's attach setting. |
| `depends_on` | []string |  |  | DependsOn overrides the agent's dependency list. |
| `resume_command` | string |  |  | ResumeCommand overrides the agent's resume_command template. |
| `wake_mode` | string |  |  | WakeMode overrides the agent's wake mode ("resume" or "fresh"). Enum: `resume`, `fresh` |
| `pre_start_append` | []string |  |  | PreStartAppend appends commands to the agent's pre_start list (instead of replacing). Applied after PreStart if both are set. |
| `session_setup_append` | []string |  |  | SessionSetupAppend appends commands to the agent's session_setup list. |
| `session_live_append` | []string |  |  | SessionLiveAppend appends commands to the agent's session_live list. |
| `install_agent_hooks_append` | []string |  |  | InstallAgentHooksAppend appends to the agent's install_agent_hooks list. |
| `inject_fragments_append` | []string |  |  | InjectFragmentsAppend appends to the agent's inject_fragments list. |
| `max_active_sessions` | integer |  |  | MaxActiveSessions overrides the agent-level cap on concurrent sessions. |
| `min_active_sessions` | integer |  |  | MinActiveSessions overrides the minimum number of sessions to keep alive. |
| `scale_check` | string |  |  | ScaleCheck overrides the command template whose output reports new unassigned session demand for bead-backed reconciliation. Supports the same Go template placeholders as Agent.scale_check. |
| `option_defaults` | map[string]string |  |  | OptionDefaults adds or overrides provider option defaults for this agent. Keys are option keys, values are choice values. Merges additively (patch keys win over existing agent keys). Example: option_defaults = &#123; model = "sonnet" &#125; |

## BeadsConfig

BeadsConfig holds bead store settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `provider` | string |  | `bd` | Provider selects the bead store backend: "bd" (default), "file", or "exec:&lt;script&gt;" for a user-supplied script. |

## ChatSessionsConfig

ChatSessionsConfig configures chat session behavior.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `idle_timeout` | string |  |  | IdleTimeout is the duration after which a detached chat session is auto-suspended. Duration string (e.g., "30m", "1h"). 0 = disabled. |

## ConvergenceConfig

ConvergenceConfig holds convergence loop limits.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `max_per_agent` | integer |  | `2` | MaxPerAgent is the maximum number of active convergence loops per agent. 0 means use default (2). |
| `max_total` | integer |  | `10` | MaxTotal is the maximum total number of active convergence loops. 0 means use default (10). |

## DaemonConfig

DaemonConfig holds controller daemon settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `formula_v2` | boolean |  |  | FormulaV2 enables formula v2 graph workflow infrastructure: the control-dispatcher implicit agent, graph.v2 formula compilation, and batch graph-apply bead creation. Requires bd with --graph support. Default: false (opt-in while the feature stabilizes). |
| `graph_workflows` | boolean |  |  | GraphWorkflows is the deprecated predecessor of FormulaV2. Retained for backwards compatibility: if graph_workflows is true in TOML and formula_v2 is not set, FormulaV2 is promoted automatically during parsing. |
| `patrol_interval` | string |  | `30s` | PatrolInterval is the health patrol interval. Duration string (e.g., "30s", "5m", "1h"). Defaults to "30s". |
| `max_restarts` | integer |  | `5` | MaxRestarts is the maximum number of agent restarts within RestartWindow before the agent is quarantined. 0 means unlimited (no crash loop detection). Defaults to 5. |
| `restart_window` | string |  | `1h` | RestartWindow is the sliding time window for counting restarts. Duration string (e.g., "30s", "5m", "1h"). Defaults to "1h". |
| `session_circuit_breaker` | boolean |  |  | SessionCircuitBreaker enables the named-session respawn circuit breaker. When enabled, the controller suppresses no-progress named-session respawns after the configured restart threshold is exceeded. |
| `session_circuit_breaker_max_restarts` | integer |  | `5` | SessionCircuitBreakerMaxRestarts overrides MaxRestarts for the named-session respawn circuit breaker. Nil reuses MaxRestartsOrDefault. 0 disables the circuit breaker even when SessionCircuitBreaker is true. |
| `session_circuit_breaker_window` | string |  | `1h` | SessionCircuitBreakerWindow overrides RestartWindow for the named-session respawn circuit breaker. Empty reuses RestartWindowDuration. |
| `session_circuit_breaker_reset_after` | string |  |  | SessionCircuitBreakerResetAfter is the cooldown before an open named-session breaker resets automatically. Empty defaults to 2 * SessionCircuitBreakerWindowDuration. |
| `shutdown_timeout` | string |  | `5s` | ShutdownTimeout is the time to wait after sending Ctrl-C before force-killing agents during shutdown. Duration string (e.g., "5s", "30s"). Set to "0s" for immediate kill. Defaults to "5s". |
| `wisp_gc_interval` | string |  |  | WispGCInterval is how often wisp GC runs. Duration string (e.g., "5m", "1h"). Wisp GC is disabled unless both WispGCInterval and WispTTL are set. |
| `wisp_ttl` | string |  |  | WispTTL is how long a closed molecule survives before being purged. Duration string (e.g., "24h", "7d"). Wisp GC is disabled unless both WispGCInterval and WispTTL are set. |
| `drift_drain_timeout` | string |  | `2m` | DriftDrainTimeout is the maximum time to wait for an agent to acknowledge a drain signal during a config-drift restart. If the agent doesn't ack within this window, the controller force-kills and restarts it. Duration string (e.g., "2m", "5m"). Defaults to "2m". |
| `observe_paths` | []string |  |  | ObservePaths lists extra directories to search for Claude JSONL session files (e.g., aimux session paths). The default search path (~/.claude/projects/) is always included. |
| `probe_concurrency` | integer |  | `8` | ProbeConcurrency bounds the number of concurrent bd subprocess probes issued by the pool scale_check and work_query paths. bd serializes on a shared dolt sql-server, so unbounded parallelism causes contention. Nil (unset) defaults to 8. Set higher for workspaces with a fast dedicated dolt server, or lower to reduce contention on slow storage. |
| `max_wakes_per_tick` | integer |  | `5` | MaxWakesPerTick caps how many sessions the reconciler may start in a single tick. Nil (unset) defaults to 5. Values &lt;= 0 are treated as the default — set a positive integer to override. |
| `nudge_dispatcher` | string |  | `legacy` | NudgeDispatcher selects how queued nudges get delivered to running sessions. "legacy" (default) auto-spawns a per-session `gc nudge poll` process that polls the file-backed queue every 2s. "supervisor" runs the delivery loop inside the city runtime instead, with a unix-socket wake fast path triggered by enqueue, eliminating the per-session bd shellout storm. Enum: `legacy`, `supervisor` |

## DoctorConfig

DoctorConfig holds settings for the gc doctor surface.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `worktree_rig_warn_size` | string |  | `10GB` | WorktreeRigWarnSize is the per-rig warning threshold for the total disk footprint under .gc/worktrees/&lt;rig&gt;/. Reported by the worktree-disk-size check. Go-style human size string ("10GB", "500MB"). Empty or unparseable falls back to the default (10 GB). |
| `worktree_rig_error_size` | string |  | `50GB` | WorktreeRigErrorSize is the per-rig error threshold. When any rig exceeds this, the worktree-disk-size check reports an error rather than a warning. Empty or unparseable falls back to the default (50 GB). |
| `nested_worktree_prune` | boolean |  | `false` | NestedWorktreePrune escalates the nested-worktree-prune check from warning to error severity when safely-prunable nested worktrees are present, so CI / scripted doctor runs fail until the operator runs `gc doctor --fix`. Actual removal still requires --fix; this flag does not auto-prune. Safety is enforced by mechanical checks (no uncommitted changes, no unpushed commits, no stashes) — never by role identity. |

## DoltConfig

DoltConfig holds optional dolt server overrides.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `port` | integer |  | `0` | Port is the dolt server port. 0 means use ephemeral port allocation (hashed from city path). Set explicitly to override. |
| `host` | string |  | `localhost` | Host is the dolt server hostname. Defaults to localhost. |
| `archive_level` | integer |  | `0` | ArchiveLevel controls Dolt's auto_gc archive aggressiveness. 0 disables archive compaction (lower CPU on startup). 1 enables archive compaction (higher CPU on startup). nil (omitted) defaults to 0. |

## EventsConfig

EventsConfig holds events provider settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `provider` | string |  |  | Provider selects the events backend: "fake", "fail", "exec:&lt;script&gt;", or "" (default: file-backed JSONL). |

## FormulasConfig

FormulasConfig holds formula directory settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `dir` | string |  | `formulas` | Dir is the path to the formulas directory. Defaults to "formulas". |

## Import

Import defines a named import of another pack.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `source` | string | **yes** |  | Source is the pack location: a local relative path (e.g., "./assets/imports/gastown") or a remote URL (e.g., "github.com/gastownhall/gastown"). Local paths have no version. |
| `version` | string |  |  | Version is a semver constraint for remote imports (e.g., "^1.2"). Empty for local paths. "sha:&lt;hex&gt;" for commit pinning. |
| `export` | boolean |  |  | Export re-exports this import's contents into the parent pack's namespace. Consumers of the parent get this import's agents flattened under the parent's binding name. |
| `transitive` | boolean |  |  | Transitive controls whether this import's own imports are visible to the consumer. Defaults to true (transitive). Set to false to suppress transitive resolution for this specific import. |
| `shadow` | string |  |  | Shadow controls shadow warnings when the importer defines an agent with the same name as one from this import. "warn" (default) emits a warning; "silent" suppresses it. Enum: `warn`, `silent` |

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
| `provider` | string |  |  | Provider selects the mail backend: "fake", "fail", "exec:&lt;script&gt;", or "" (default: beadmail). |

## ModelPricing

ModelPricing is a complete pricing entry for a (Provider, Model) pair.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `provider` | string | **yes** |  | Provider is the LLM provider label (e.g. "claude", "codex", "gemini"). |
| `model` | string | **yes** |  | Model is the provider-specific model identifier (e.g. "claude-opus-4-7"). |
| `tier` | Tier | **yes** |  | Tier holds the per-token-type rates. |
| `last_verified` | string | **yes** |  | LastVerified is the date these rates were confirmed (YYYY-MM-DD). |

## NamedSession

NamedSession defines a canonical persistent session backed by an agent template.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string |  |  | Name is the configured public session identity. When omitted, Template remains the compatibility identity. |
| `template` | string | **yes** |  | Template is the referenced agent template name. Root declarations may target imported PackV2 agents via "binding.agent". |
| `scope` | string |  |  | Scope defines where this named session is instantiated in pack expansion: "city" (one per city) or "rig" (one per rig). Enum: `city`, `rig` |
| `dir` | string |  |  | Dir is the identity prefix for rig-scoped named sessions after pack expansion. Empty means city-scoped. |
| `mode` | string |  |  | Mode controls controller behavior for this named session. "on_demand" (default): reserve identity and materialize when work or an explicit reference requires it. "always": keep the canonical session controller-managed. Enum: `on_demand`, `always` |

## OptionChoice

OptionChoice is one allowed value for a "select" option.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `value` | string | **yes** |  |  |
| `label` | string | **yes** |  |  |
| `flag_args` | []string | **yes** |  | FlagArgs are the CLI arguments injected when this choice is selected. json:"-" is intentional: FlagArgs must never appear in the public API DTO (security boundary — prevents clients from seeing internal CLI flags). |
| `flag_aliases` | []array |  |  | FlagAliases are equivalent CLI argument sequences stripped from legacy provider args. Like FlagArgs, they stay server-side only. |

## OrderOverride

OrderOverride modifies a scanned order's scheduling fields.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the order name to target (required). |
| `rig` | string |  |  | Rig scopes the override to a specific rig's order. Empty matches city-level orders. |
| `enabled` | boolean |  |  | Enabled overrides whether the order is active. |
| `trigger` | string |  |  | Trigger overrides the trigger type. |
| `gate` | string |  |  | Gate is a deprecated alias for Trigger accepted during the gate-&gt;trigger migration. Parsed inputs are normalized to Trigger. |
| `interval` | string |  |  | Interval overrides the cooldown interval. Go duration string. |
| `schedule` | string |  |  | Schedule overrides the cron expression. |
| `check` | string |  |  | Check overrides the condition trigger check command. |
| `on` | string |  |  | On overrides the event trigger event type. |
| `pool` | string |  |  | Pool overrides the target session config. |
| `timeout` | string |  |  | Timeout overrides the per-order timeout. Go duration string. |

## OrdersConfig

OrdersConfig holds order settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `skip` | []string |  |  | Skip lists order names to exclude from scanning. |
| `max_timeout` | string |  |  | MaxTimeout is an operator hard cap on per-order timeouts. No order gets more than this duration. Go duration string (e.g., "60s"). Empty means uncapped (no override). |
| `overrides` | []OrderOverride |  |  | Overrides apply per-order field overrides after scanning. Each override targets an order by name and optionally by rig. |

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
| `agent` | []AgentPatch |  |  | Agents targets agents by (dir, name). |
| `rigs` | []RigPatch |  |  | Rigs targets rigs by name. |
| `providers` | []ProviderPatch |  |  | Providers targets providers by name. |

## PoolOverride

PoolOverride modifies legacy [pool] fields that map to session scaling.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `min` | integer |  |  | Min overrides the minimum number of sessions. |
| `max` | integer |  |  | Max overrides the maximum number of sessions. 0 means no sessions can claim routed work. |
| `check` | string |  |  | Check overrides the session scale check command template. Supports the same Go template placeholders as Agent.scale_check. |
| `drain_timeout` | string |  |  | DrainTimeout overrides the drain timeout. Duration string (e.g., "5m", "30m", "1h"). |
| `on_death` | string |  |  | OnDeath overrides the on_death command template. Supports the same Go template placeholders as Agent.on_death. |
| `on_boot` | string |  |  | OnBoot overrides the on_boot command template. Supports the same Go template placeholders as Agent.on_boot. |

## ProviderOption

ProviderOption declares a single configurable option for a provider.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `key` | string | **yes** |  |  |
| `label` | string | **yes** |  |  |
| `type` | string | **yes** |  | "select" only (v1) |
| `default` | string | **yes** |  |  |
| `choices` | []OptionChoice | **yes** |  |  |
| `omit` | boolean |  |  | Omit is the removal sentinel for options_schema_merge = "by_key". When set on a child layer's entry, the matching Key inherited from a parent layer is pruned from the resolved schema. |

## ProviderPatch

ProviderPatch modifies an existing provider identified by Name.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the targeting key (required). Must match an existing provider's name. |
| `base` | string |  |  | Base overrides the provider's inheritance parent (presence-aware). Pointer to a pointer so the patch can distinguish "no change" (double-nil) from "clear to inherit default" (single-nil value in outer pointer) from "set to explicit empty opt-out" (value "" in inner pointer) from "set to &lt;name&gt;". Callers use:   nil          = patch does not touch Base   &(*string)(nil) = patch clears Base to absent   &(&"")       = patch sets Base = "" (explicit opt-out)   &(&"builtin:codex") = patch sets Base to that value |
| `command` | string |  |  | Command overrides the provider command. |
| `acp_command` | string |  |  | ACPCommand overrides the provider command for ACP transport sessions. |
| `args` | []string |  |  | Args overrides the provider args. |
| `acp_args` | []string |  |  | ACPArgs overrides the provider args for ACP transport sessions. |
| `args_append` | []string |  |  | ArgsAppend overrides the provider args_append list. |
| `options_schema_merge` | string |  |  | OptionsSchemaMerge overrides the options_schema merge mode. |
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
| `base` | string |  |  | Base names the parent provider this spec inherits from. Supported forms:   "&lt;name&gt;"          - custom first (self-excluded), then built-in   "builtin:&lt;name&gt;"  - force built-in lookup   "provider:&lt;name&gt;" - force custom lookup   ""                - explicit standalone opt-out   nil               - field absent; no explicit declaration |
| `args_append` | []string |  |  | ArgsAppend accumulates extra args after each layer's Args replacement. |
| `options_schema_merge` | string |  |  | OptionsSchemaMerge controls OptionsSchema merge mode across the chain: "replace" (default) or "by_key". Enum: `replace`, `by_key` |
| `display_name` | string |  |  | DisplayName is the human-readable name shown in UI and logs. |
| `command` | string |  |  | Command is the executable to run for this provider. |
| `args` | []string |  |  | Args are default command-line arguments passed to the provider. The built-in Kiro provider defaults to ["chat", "--no-interactive", "--agent", "gascity", "--trust-all-tools"]; remove or replace "--trust-all-tools" by defining [providers.kiro].args explicitly in city.toml. |
| `prompt_mode` | string |  | `arg` | PromptMode controls how prompts are delivered: "arg", "flag", or "none". Enum: `arg`, `flag`, `none` |
| `prompt_flag` | string |  |  | PromptFlag is the CLI flag used when prompt_mode is "flag" (e.g. "--prompt"). |
| `ready_delay_ms` | integer |  |  | ReadyDelayMs is milliseconds to wait after launch before the provider is considered ready. |
| `ready_prompt_prefix` | string |  |  | ReadyPromptPrefix is the string prefix that indicates the provider is ready for input. |
| `process_names` | []string |  |  | ProcessNames lists process names to look for when checking if the provider is running. |
| `emits_permission_warning` | boolean |  |  | EmitsPermissionWarning is tri-state: nil = inherit, &true = enable, &false = explicit disable. |
| `env` | map[string]string |  |  | Env sets additional environment variables for the provider process. |
| `path_check` | string |  |  | PathCheck overrides the binary name used for PATH detection. When set, lookupProvider and detectProviderName use this instead of Command for exec.LookPath checks. Useful when Command is a shell wrapper (e.g. sh -c '...') but we need to verify the real binary is installed. |
| `supports_acp` | boolean |  |  | SupportsACP indicates the binary speaks the Agent Client Protocol (JSON-RPC 2.0 over stdio). When an agent sets session = "acp", its resolved provider must have SupportsACP = true. |
| `supports_hooks` | boolean |  |  | SupportsHooks indicates the provider has an executable hook mechanism (settings.json, plugins, etc.) for lifecycle events. |
| `instructions_file` | string |  |  | InstructionsFile is the filename the provider reads for project instructions (e.g., "CLAUDE.md", "AGENTS.md"). Empty defaults to "AGENTS.md". |
| `resume_flag` | string |  |  | ResumeFlag is the CLI flag for resuming a session by ID. Empty means the provider does not support resume. Examples: "--resume" (claude), "resume" (codex) |
| `resume_style` | string |  |  | ResumeStyle controls how ResumeFlag is applied:   "flag"       → command --resume &lt;key&gt;              (default)   "subcommand" → command resume &lt;key&gt; |
| `resume_command` | string |  |  | ResumeCommand is the full shell command to run when resuming a session. Supports only the &#123;&#123;.SessionKey&#125;&#125; template variable. When set, takes precedence over ResumeFlag/ResumeStyle. When schema-managed defaults are inserted, the resolver tokenizes and re-emits the command; for subcommand-style resume it inserts after the ResumeFlag token that precedes &#123;&#123;.SessionKey&#125;&#125;. Example:   "claude --resume &#123;&#123;.SessionKey&#125;&#125; --dangerously-skip-permissions" Schema-managed defaults missing from a subcommand-style resume command are inserted before &#123;&#123;.SessionKey&#125;&#125; during provider resolution. |
| `session_id_flag` | string |  |  | SessionIDFlag is the CLI flag for creating a session with a specific ID. Enables the Generate & Pass strategy for session key management. Example: "--session-id" (claude) |
| `permission_modes` | map[string]string |  |  | PermissionModes maps permission mode names to CLI flags. Example: &#123;"unrestricted": "--dangerously-skip-permissions", "plan": "--permission-mode plan"&#125; This is a config-only lookup table consumed by external clients (e.g., real-world app) to populate permission mode dropdowns. Launch-time flag substitution is planned for a follow-up PR — currently no runtime code reads this field. |
| `option_defaults` | map[string]string |  |  | OptionDefaults overrides the Default value in OptionsSchema entries without redefining the schema itself. Keys are option keys (e.g., "permission_mode"), values are choice values (e.g., "unrestricted"). city.toml users set this to customize provider behavior without touching Args or OptionsSchema. |
| `options_schema` | []ProviderOption |  |  | OptionsSchema declares the configurable options this provider supports. Each option maps to CLI args via its Choices[].FlagArgs field. Serialized via a dedicated DTO (not directly to JSON) so FlagArgs stays server-side. |
| `print_args` | []string |  |  | PrintArgs are CLI arguments that enable one-shot non-interactive mode. The provider prints its response to stdout and exits. When empty, the provider does not support one-shot invocation. Examples: ["-p"] (claude, gemini), ["exec"] (codex) |
| `title_model` | string |  |  | TitleModel is the OptionsSchema model key used for title generation. Resolved via the "model" option in OptionsSchema to get FlagArgs. Defaults to the cheapest/fastest model for each provider. Examples: "haiku" (claude), "o4-mini" (codex), "gemini-2.5-flash" (gemini) |
| `acp_command` | string |  |  | ACPCommand overrides Command when the session transport is ACP. When empty, Command is used for both tmux and ACP transports. |
| `acp_args` | []string |  |  | ACPArgs overrides Args when the session transport is ACP. When nil, Args is used for both tmux and ACP transports. |

## Rig

Rig defines an external project registered in the city.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the unique identifier for this rig. |
| `path` | string |  |  | Path is the absolute filesystem path to the rig's repository. |
| `prefix` | string |  |  | Prefix overrides the auto-derived bead ID prefix for this rig. |
| `default_branch` | string |  |  | DefaultBranch is the rig repository's mainline branch (e.g. "main", "master", "develop"). When set, routing formulas use this as the default merge target instead of probing origin/HEAD at sling time. Captured by `gc rig add` from the rig's git config; set manually for rigs whose mainline isn't reachable via origin/HEAD. |
| `suspended` | boolean |  |  | Suspended prevents the reconciler from spawning agents in this rig. Toggle with gc rig suspend/resume. |
| `formulas_dir` | string |  |  | FormulasDir is a rig-local formula directory (Layer 4). Overrides pack formulas for this rig by filename. Relative paths resolve against the city directory. |
| `includes` | []string |  |  | Includes lists pack directories or URLs for this rig (V1 mechanism). Each entry is a local path, a git source//sub#ref URL, or a GitHub tree URL. |
| `imports` | map[string]Import |  |  | Imports defines named pack imports for this rig (V2 mechanism). Each key is a binding name; agents from these imports get qualified names like "rigName/bindingName.agentName". |
| `max_active_sessions` | integer |  |  | MaxActiveSessions is the rig-level cap on total concurrent sessions across all agents in this rig. Nil means inherit from workspace (or unlimited). |
| `overrides` | []AgentOverride |  |  | Overrides are per-agent patches applied after pack expansion. V2 renames this to "patches" for consistency with [[patches.agent]]. Both TOML keys are accepted during migration. |
| `patches` | []AgentOverride |  |  | Patches is the V2 name for rig-level agent overrides. Takes precedence over Overrides if both are set. |
| `default_sling_target` | string |  |  | DefaultSlingTarget is the agent qualified name used when gc sling is invoked with only a bead ID (no explicit target). Resolved via resolveAgentIdentity. Example: "rig/polecat" |
| `session_sleep` | SessionSleepConfig |  |  | SessionSleep overrides workspace-level idle sleep defaults for agents in this rig. |
| `dolt_host` | string |  |  | DoltHost overrides the city-level Dolt host for this rig's beads. Use when the rig's database lives on a different Dolt server (e.g., shared from another city). |
| `dolt_port` | string |  |  | DoltPort overrides the city-level Dolt port for this rig's beads. When set, controller commands (scale_check, work_query) prefix their shell invocations with BEADS_DOLT_SERVER_PORT=&lt;port&gt; so bd connects to the correct server instead of the city-level default. |
| `formula_vars` | map[string]string |  |  | FormulaVars provides rig-scoped defaults for formula vars. Keys match var names declared in formula `[vars.&lt;name&gt;]` blocks. Values are used when a formula runs in this rig and the caller did not pass an explicit --var override. Takes precedence over formula-level defaults but loses to --var flags. |

## RigPatch

RigPatch modifies an existing rig identified by Name.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the targeting key (required). Must match an existing rig's name. |
| `path` | string |  |  | Path overrides the rig's filesystem path. |
| `prefix` | string |  |  | Prefix overrides the bead ID prefix. |
| `default_branch` | string |  |  | DefaultBranch overrides the rig's recorded mainline branch. |
| `suspended` | boolean |  |  | Suspended overrides the rig's suspended state. |
| `formula_vars` | map[string]string |  |  | FormulaVars adds or overrides rig-scoped formula var defaults. Additive merge: patch keys win over existing rig keys, unspecified keys are preserved. |

## Service

Service declares a workspace-owned HTTP service mounted under /svc/{name}.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string | **yes** |  | Name is the unique service identifier within a workspace. |
| `kind` | string |  |  | Kind selects how the service is implemented. Enum: `workflow`, `proxy_process` |
| `publish_mode` | string |  |  | PublishMode declares how the service is intended to be published. v0 supports private services and direct reuse of the API listener. Enum: `private`, `direct` |
| `state_root` | string |  |  | StateRoot overrides the managed service state root. Defaults to .gc/services/&#123;name&#125;. The path must stay within .gc/services/. |
| `publication` | ServicePublicationConfig |  |  | Publication declares generic publication intent. The platform decides whether and how that intent becomes a public route. |
| `workflow` | ServiceWorkflowConfig |  |  | Workflow configures controller-owned workflow services. |
| `process` | ServiceProcessConfig |  |  | Process configures controller-supervised proxy services. |

## ServiceProcessConfig

ServiceProcessConfig configures a controller-supervised local process that is reverse-proxied under /svc/{name}.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `command` | []string |  |  | Command is the argv used to start the local service process. |
| `health_path` | string |  |  | HealthPath, when set, is probed on the local listener before the service is marked ready. |

## ServicePublicationConfig

ServicePublicationConfig declares platform-neutral publication intent.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `visibility` | string |  |  | Visibility selects whether the service is private to the workspace, available publicly, or gated by tenant auth at the platform edge. Enum: `private`, `public`, `tenant` |
| `hostname` | string |  |  | Hostname overrides the default hostname label derived from service.name. |
| `allow_websockets` | boolean |  |  | AllowWebSockets permits websocket upgrades on the published route. |

## ServiceWorkflowConfig

ServiceWorkflowConfig configures controller-owned workflow services.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `contract` | string |  |  | Contract selects the built-in workflow handler. |

## SessionConfig

SessionConfig holds session provider settings.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `provider` | string |  |  | Provider selects the session backend: "fake", "fail", "subprocess", "acp", "exec:&lt;script&gt;", "k8s", or "" (default: tmux). |
| `k8s` | K8sConfig |  |  | K8s holds Kubernetes-specific settings for the native K8s provider. |
| `acp` | ACPSessionConfig |  |  | ACP holds settings for the ACP (Agent Client Protocol) session provider. |
| `setup_timeout` | string |  | `10s` | SetupTimeout is the per-command/script timeout for session setup and pre_start commands. Duration string (e.g., "10s", "30s"). Defaults to "10s". |
| `nudge_ready_timeout` | string |  | `10s` | NudgeReadyTimeout is how long to wait for the agent to be ready before sending nudge text. Duration string. Defaults to "10s". |
| `nudge_retry_interval` | string |  | `500ms` | NudgeRetryInterval is the retry interval between nudge readiness polls. Duration string. Defaults to "500ms". |
| `nudge_lock_timeout` | string |  | `30s` | NudgeLockTimeout is how long to wait to acquire the per-session nudge lock. Duration string. Defaults to "30s". |
| `debounce_ms` | integer |  | `500` | DebounceMs is the default debounce interval in milliseconds for send-keys. Defaults to 500. |
| `display_ms` | integer |  | `5000` | DisplayMs is the default display duration in milliseconds for status messages. Defaults to 5000. |
| `startup_timeout` | string |  | `60s` | StartupTimeout is how long to wait for each agent's Start() call before treating it as failed. Duration string (e.g., "60s", "2m"). Defaults to "60s". |
| `socket` | string |  |  | Socket specifies the tmux socket name for per-city isolation. When set, all tmux commands use "tmux -L &lt;socket&gt;" to connect to a dedicated server. When empty, defaults to the city name (workspace.name) — giving every city its own tmux server automatically. Set explicitly to override. |
| `remote_match` | string |  |  | RemoteMatch is a substring pattern for the hybrid provider to route sessions to the remote (K8s) backend. Sessions whose names contain this pattern go to K8s; all others stay local (tmux). Overridden by the GC_HYBRID_REMOTE_MATCH env var if set. |

## SessionSleepConfig

SessionSleepConfig configures default idle sleep policies by session class.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `interactive_resume` | string |  |  | InteractiveResume applies to attachable sessions using wake_mode=resume. Accepts a duration string or "off". |
| `interactive_fresh` | string |  |  | InteractiveFresh applies to attachable sessions using wake_mode=fresh. Accepts a duration string or "off". |
| `noninteractive` | string |  |  | NonInteractive applies to sessions with attach=false. Accepts a duration string or "off". |

## Tier

Tier defines per-token-type rates in USD per 1 million tokens.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `prompt_usd_per_1m` | number | **yes** |  |  |
| `completion_usd_per_1m` | number | **yes** |  |  |
| `cache_read_usd_per_1m` | number | **yes** |  |  |
| `cache_creation_usd_per_1m` | number | **yes** |  |  |

## Workspace

Workspace holds city-level metadata and optional defaults that apply to all agents unless overridden per-agent.

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `name` | string |  |  | Name is the legacy checked-in city name. Runtime identity now resolves from site binding (.gc/site.toml workspace_name), declared config, and basename precedence instead; gc init writes the machine-local name to site.toml and omits it from city.toml. |
| `prefix` | string |  |  | Prefix overrides the auto-derived HQ bead ID prefix. When empty, the prefix is derived from the city Name via DeriveBeadsPrefix. |
| `provider` | string |  |  | Provider is the default provider name used by agents that don't specify one. |
| `start_command` | string |  |  | StartCommand overrides the provider's command for all agents. |
| `suspended` | boolean |  |  | Suspended controls whether the city is suspended. When true, all agents are effectively suspended: the reconciler won't spawn them, and gc hook/prime return empty. Inherits downward — individual agent/rig suspended fields are checked independently. |
| `max_active_sessions` | integer |  |  | MaxActiveSessions is the workspace-level cap on total concurrent sessions. Nil means unlimited. Agents and rigs inherit this if they don't set their own. |
| `session_template` | string |  |  | SessionTemplate is a template string supporting placeholders: &#123;&#123;.City&#125;&#125;, &#123;&#123;.Agent&#125;&#125; (sanitized), &#123;&#123;.Dir&#125;&#125;, &#123;&#123;.Name&#125;&#125;. Controls tmux session naming. Default (empty): "&#123;&#123;.Agent&#125;&#125;" — just the sanitized agent name. Per-city tmux socket isolation makes a city prefix unnecessary. |
| `install_agent_hooks` | []string |  |  | InstallAgentHooks lists provider names whose hooks should be installed into agent working directories. Agent-level overrides workspace-level (replace, not additive). Supported: "claude", "codex", "gemini", "kiro", "opencode", "copilot", "cursor", "pi", "omp". |
| `global_fragments` | []string |  |  | GlobalFragments lists named template fragments injected into every agent's rendered prompt. Applied before per-agent InjectFragments. Each name must match a &#123;&#123; define "name" &#125;&#125; block from a pack's prompts/shared/ directory. |
| `includes` | []string |  |  | Includes lists pack directories or URLs to compose into this workspace. Replaces the older pack/packs fields. Each entry is a local path, a git source//sub#ref URL, or a GitHub tree URL. |
| `default_rig_includes` | []string |  |  | DefaultRigIncludes lists pack directories applied to new rigs when "gc rig add" is called without --include. Allows cities to define a default pack for all rigs. |

