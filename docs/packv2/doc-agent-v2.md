# Agent Definition v.next

**GitHub Issue:** [gastownhall/gascity#356](https://github.com/gastownhall/gascity/issues/356)

Title: `feat: Agent Definition v.next — agents as directories`

This is a companion to [doc-pack-v2.md](doc-pack-v2.md), which covers the pack/city model redesign.

> **Keeping in sync:** This file is the source of truth. When updating, edit here, then update the issue body with `gh issue edit 356 --repo gastownhall/gascity --body-file <(sed -n '/^---BEGIN ISSUE---$/,/^---END ISSUE---$/{ /^---/d; p; }' issues/doc-agent-v2.md)`.

---BEGIN ISSUE---

## Problem

Agent definitions are split across `[[agent]]` TOML tables and filesystem assets (prompts, overlays, scripts) scattered in separate directory trees. This creates six problems:
v
1. **Scattered identity.** There's no single place to understand what an agent is. Adding an agent means editing city.toml *and* creating files in multiple directories (`prompts/`, `overlays/`, `scripts/`).

2. **Invisible prompt injection.** Every `.md` file is secretly a Go template. Fragments get injected via `global_fragments` and `inject_fragments` without appearing in the prompt file itself. You can't read a prompt and know what the agent actually sees.

3. **Provider files leak across providers.** Overlay files (`.claude/settings.json`, `CLAUDE.md`) get copied into every agent's working directory regardless of which provider the agent uses. A Codex agent gets Claude's settings.

4. **No home for skills or MCP servers.** The [Agent Skills](https://agentskills.io) standard is adopted by 30+ tools (Claude Code, Codex, Gemini, Cursor, Copilot, etc.), but Gas City has no convention for shipping skills with a pack. MCP server config is provider-specific JSON baked into overlay files with no abstraction. For the first slice, both surfaces are current-city-pack only: imported-pack catalogs are later.

5. **Definition vs. modification conflated.** There's no separation between "I'm defining my own agent" and "I'm tweaking an imported agent." Both use `[[agent]]` tables, and collision resolution depends on load order and `fallback` flags.

6. **Ad hoc asset wiring.** Overlays, prompts, and scripts each have their own mechanism (`overlay_dir`, `prompt_template`, `scripts_dir`). There's no consistent pattern.

## Proposed change: agents as directories

Agents are defined by convention: a directory in `agents/` with at least a `prompt.md` file.  All additional assets live in the agent's directory, as does any configuration in an optional `agent.toml` file.

**Minimal agent** — just a prompt, inherits all defaults:

```
agents/polecat/
└── prompt.md
```

**Agent with config overrides:**

```
agents/mayor/
├── agent.toml         # optional — overrides defaults
└── prompt.md          # required — the system prompt
```

**Fully configured agent** with per-agent assets:

```
agents/mayor/
├── agent.toml         # optional — overrides defaults
├── prompt.md          # required — the system prompt
├── namepool.txt       # optional — display names for pool sessions
├── overlay/           # optional — agent-specific overlay files
│   ├── AGENTS.md      # provider-agnostic instructions (copied for all providers)
│   └── per-provider/
│       └── claude/
├── skills/            # optional — agent-specific skills
├── mcp/               # optional — agent-specific MCP servers
└── template-fragments/ # optional — agent-specific prompt fragments
```

**Full city** with city-wide assets and multiple agents:

```
my-city/
├── city.toml
├── agents/
│   ├── polecat/
│   │   └── prompt.md
│   └── mayor/
│       ├── agent.toml
│       └── prompt.md
├── overlays/                  # city-wide overlays (all agents)
│   ├── per-provider/
│   │   ├── claude/
│   │   │   ├── .claude/
│   │   │   │   └── settings.json
│   │   │   └── CLAUDE.md
│   │   └── codex/
│   │       └── AGENTS.md
│   └── .editorconfig          # provider-agnostic (all agents)
├── skills/                    # city-wide skills (all agents)
├── mcp/                       # city-wide MCP servers (all agents)
├── template-fragments/        # city-wide prompt template fragments
├── formulas/
├── orders/
├── commands/
├── doctor/
├── patches/
└── assets/
```

### city.toml: agent defaults

`[[agent]]` tables are replaced by `[agent_defaults]` for shared defaults. This block can appear in both `pack.toml` (pack-wide portable defaults) and `city.toml` (city-level deployment overrides), with city layering on top of pack:

```toml
# pack.toml — pack-wide defaults
[agent_defaults]
default_sling_formula = "mol-do-work"
```

```toml
# city.toml — city-level overrides (optional)
[agent_defaults]
append_fragments = ["operational-awareness"]
```

In this release wave the actively-applied defaults are still narrow:
`default_sling_formula` plus `[agent_defaults].append_fragments` during
prompt rendering. Other `AgentDefaults` fields are parsed and composed,
but are not yet auto-inherited at runtime. Per-agent fields such as
`provider` and `scope` still live in `agents/<name>/agent.toml`.

Individual agents override in their own `agent.toml`:

```toml
# agents/mayor/agent.toml — only what differs from defaults
scope = "city"
max_active_sessions = 1
```

A minimal agent (directory with just `prompt.md`) inherits all defaults and needs no `agent.toml`.

### Pool agents

Pool behavior is config, not structure. A pool agent is an agent that spawns multiple concurrent sessions from the same definition — useful when work arrives faster than a single session can handle. The controller scales sessions up and down based on demand, within the configured bounds:

```toml
# agents/polecat/agent.toml
min_active_sessions = 1
max_active_sessions = 3
```

If the agent's directory contains a `namepool.txt` file (one name per line), each session gets a name from it as a display alias — no TOML field needed, same convention-over-configuration approach as `prompt.md`. All instances share the same prompt, skills, MCP servers, and overlays — they differ only in their session identity and working directory.

### Provider-aware overlays

Overlays are files materialized into the agent's working directory before it starts. Provider-specific files live in `per-provider/` subdirectories so agents only get files for their provider.

Layering order (later wins on file collision):

1. City-wide `overlays/` — universal files (everything outside `per-provider/`)
2. City-wide `overlays/per-provider/<provider>/` — provider-matched
3. Agent-specific `agents/<name>/overlay/` — universal files
4. Agent-specific `agents/<name>/overlay/per-provider/<provider>/` — provider-matched

The `<provider>` name matches the Gas City provider name (`claude`, `codex`, `cursor`, etc.). Switching an agent's provider changes which overlay files apply — no manual cleanup.

This means a city can ship distinct `CLAUDE.md` and `AGENTS.md` files for different providers, and each agent only sees the one for its provider.

### Skills

Skills use the [Agent Skills](https://agentskills.io) open standard, adopted by 30+ providers including Claude Code, Codex, Gemini, Cursor, GitHub Copilot, JetBrains Junie, Goose, Roo Code, and many more.

A skill is a directory containing a `SKILL.md` file (YAML frontmatter + markdown instructions) with optional `scripts/`, `references/`, and `assets/` subdirectories:

```
skills/code-review/
├── SKILL.md               # required: metadata + instructions
├── scripts/               # optional: executable code
├── references/            # optional: documentation
└── assets/                # optional: templates, resources
```

```yaml
# SKILL.md frontmatter
---
name: code-review
description: Reviews code changes for bugs, security issues, and style. Use when reviewing PRs or changed files.
---
```

Skills are **portable across providers**. The same SKILL.md works with Claude Code, Codex, Gemini, and any other compliant agent. In a later slice, Gas City materializes skills into the provider-expected location in the agent's working directory at startup (e.g., `.claude/skills/` for Claude Code, `.agents/skills/` for Codex).

Skills can be city-wide or per-agent in the current city pack:

```
my-city/
├── skills/                    # city-wide — available to all agents
│   ├── code-review/
│   │   └── SKILL.md
│   └── test-runner/
│       ├── SKILL.md
│       └── scripts/
│           └── run-tests.sh
├── agents/
│   └── polecat/
│       └── skills/            # agent-specific — only this agent
│           └── polecat-workflow/
│               └── SKILL.md
```

An agent gets city-wide skills + its own skills. Agent-specific wins on name collision.

> **First slice:** skills discovery/materialization is current-city-pack only. Imported-pack skill catalogs are later.

The first skills CLI slice is list-only:

```sh
gc skill list
gc skill list --agent polecat
gc skill list --session <id>
```

#### Skill promotion

> **Later slice:** the first skills surface is list-only. Promote/retain flows are design-noted here, but the first implementation slice does not need them yet.

When an agent creates a skill during a session (in the rig's working directory), it stays local to that rig. To bring it into the city definition:

```
gc skill promote code-review --to city        # copies to city's skills/
gc skill promote code-review --to agent mayor  # copies to agents/mayor/skills/
```

Promoting is an explicit human decision — skills don't automatically flow from rigs back to the city.

### MCP servers

MCP (Model Context Protocol) servers provide tools, resources, and prompts to agents over a runtime protocol. Unlike skills (which have a portable file standard), MCP server configuration is provider-specific — each provider embeds it in its own settings file. Gas City abstracts this with a provider-agnostic TOML format.

> **First slice:** MCP discovery and listing are current-city-pack only. Imported-pack MCP catalogs, provider projection, and ownership/reconciliation are later slices.

The first MCP CLI slice is list-only:

```sh
gc mcp list
gc mcp list --agent polecat
gc mcp list --session <id>
```

#### Definition format

An MCP server is a named TOML file in `mcp/`:

```toml
# mcp/beads-health.toml
name = "beads-health"
description = "Query bead status and health metrics"
command = "scripts/mcp-beads-health.sh"
args = ["--city-root", "."]

[env]
BEADS_DB = ".beads"
```

For template expansion (dynamic paths, credentials), use `.template.toml`:

```toml
# mcp/beads-health.template.toml
name = "beads-health"
description = "Query bead status and health metrics"
command = "assets/mcp-beads-health.sh"
args = ["--city-root", "{{.CityRoot}}"]

[env]
BEADS_DB = "{{.RigRoot}}/.beads"
```

Same `.template.` rule as prompts — plain `.toml` is static, `.template.toml` goes through Go template expansion with `PromptContext` variables.

Remote MCP servers use `url` instead of `command`:

```toml
# mcp/sentry.template.toml — .template.toml triggers Go template expansion
name = "sentry"
description = "Sentry error tracking integration"
url = "https://mcp.sentry.io/sse"

[headers]
Authorization = "Bearer {{.SENTRY_TOKEN}}"
```

#### Field spec

| Field | Required | Description |
|---|---|---|
| `name` | Yes | Server name (must match filename without extension) |
| `description` | Yes | What this server provides |
| `command` | Yes* | Command to launch local server (stdio transport) |
| `args` | No | Arguments to the command |
| `url` | Yes* | URL for remote server (HTTP transport) |
| `headers` | No | HTTP headers for remote server |
| `[env]` | No | Environment variables passed to local server |

*One of `command` or `url` is required.

#### What Gas City does at agent startup (later slice)

1. Collects all MCP server definitions for this agent (city-wide + agent-specific)
2. Template-expands any `.template.toml` files
3. Resolves `command` paths to absolute paths (scripts are NOT copied to the rig)
4. Injects into the provider's config format:
   - Claude Code: merges into `.claude/settings.json` `mcpServers`
   - Cursor: merges into `.cursor/mcp.json` `mcpServers`
   - VS Code/Copilot: merges into VS Code settings
   - Others: provider-specific mapping as supported

Each MCP server is a separate file, so multiple packs' MCP servers merge cleanly — no last-writer-wins on a single settings file.

> **Later slice:** provider projection into provider settings is intentionally separate from the first slice; keep the neutral TOML model and list visibility as the first implementation boundary.

### Prompts and templates

**`.template.` infix required for template processing ([#582](https://github.com/gastownhall/gascity/issues/582)).** `prompt.md` is plain markdown — no template engine runs. `prompt.template.md` goes through Go `text/template`. No more "everything is secretly a template."

This applies to all file types, not just prompts. If a file needs template expansion, it has `.template.` in its name (e.g., `prompt.template.md`, `beads-health.template.toml`). If it doesn't, it doesn't.

### Template fragments

Fragments are reusable chunks of prompt content. They are named Go templates defined in `.template.md` files:

```markdown
{{ define "command-glossary" }}
Use `/gc-work`, `/gc-dispatch`, `/gc-agents`, `/gc-rigs`, `/gc-mail`,
or `/gc-city` to load command reference for any topic.
{{ end }}
```

Fragments live in `template-fragments/` at city or pack level:

```
my-city/
├── template-fragments/
│   ├── command-glossary.template.md
│   ├── operational-awareness.template.md
│   └── tdd-discipline.template.md
├── agents/
│   ├── mayor/
│   │   └── prompt.template.md
│   └── polecat/
│       └── prompt.md
```

An agent whose prompt is `.template.md` can pull in fragments explicitly:

```markdown
# Mayor

You are the mayor of this city.

{{ template "operational-awareness" . }}

---

{{ template "command-glossary" . }}
```

An agent whose prompt is plain `.md` cannot use fragments — no template engine runs.

**What this replaces:**

| Current mechanism | New model |
|---|---|
| `global_fragments` in workspace config | Gone — each prompt explicitly includes what it needs |
| `inject_fragments` on agent config | Gone — same reason |
| `inject_fragments_append` on patches | Gone — same reason |
| `prompts/shared/*.template.md` | `template-fragments/*.template.md` at city level |
| All `.md` files run through Go templates | Only `.template.md` files run through Go templates |

The three-layer injection pipeline (inline templates → global_fragments → inject_fragments) collapses to one: **explicit `{{ template "name" . }}` in the `.template.md` file.** The prompt file is the single source of truth for what the agent sees.

#### Auto-append (opt-in)

For migration and convenience, city-wide or pack-wide defaults can
auto-append fragments via `[agent_defaults].append_fragments`:

```toml
# pack.toml or city.toml
[agent_defaults]
append_fragments = ["operational-awareness", "command-glossary"]
```

Agent-local `append_fragments` remains a follow-up tracked in
[#671](https://github.com/gastownhall/gascity/issues/671); it is not part
of the supported migration contract in this wave.

`append_fragments` only works on `.template.md` prompts. Plain `.md` prompts are inert — nothing is injected, no template engine runs.

### Implicit agents

Gas City provides a built-in agent for each configured provider (claude, codex, gemini, etc.) so that `gc sling claude "do something"` works immediately after `gc init` with no agent configuration.

Implicit agents follow the same directory convention. They are materialized from the `gc` binary into `.gc/system/agents/`:

```
.gc/system/agents/
├── claude/
│   └── prompt.md
├── codex/
│   └── prompt.md
└── gemini/
    └── prompt.md
```

**Shadowing:** A user-defined agent with the same name wins over the system implicit. Priority chain (lowest to highest):

1. **System implicit** (`.gc/system/agents/`) — bare minimum, always exists
2. **Pack-defined** (`agents/claude/` in a pack) — overrides system
3. **City-defined** (`agents/claude/` in the city) — overrides packs

### Agent patches

Patches modify imported agents without defining new ones. They are distinct from agent definitions — `agents/<name>/` always creates YOUR agent; patches modify SOMEONE ELSE's agent.

**Config-only patch** — override agent.toml fields by qualified name:

```toml
# city.toml
[[patches.agent]]
name = "gastown.mayor"
model = "claude-opus-4-20250514"
max_active_sessions = 2

[patches.agent.env]
REVIEW_MODE = "strict"
```

**Prompt replacement** — redirect to a file in your city's `patches/` directory:

```toml
[[patches.agent]]
name = "gastown.mayor"
prompt = "gastown-mayor-prompt.md"     # relative to patches/
```

```
my-city/
├── city.toml
├── agents/                    # YOUR agents only
└── patches/                   # all patch-related files
    └── gastown-mayor-prompt.md
```

Key design decisions:
- `agents/<name>/` = new agent. `[[patches.agent]]` = modify imported agent. Never conflated.
- Patches target by qualified name (`gastown.mayor`). Bare names work when unambiguous.
- File-level: prompt replacement only for now. Skills, MCP, overlays deferred.

### Rig patches

Rig patches are agent patches scoped to one rig. They live in city.toml alongside the rig declaration:

```toml
# city.toml
[[rigs]]
name = "api-server"

# polecat in api-server gets 2 sessions; other rigs unaffected
[[rigs.patches]]
agent = "gastown.polecat"
max_active_sessions = 2
```

Same fields as agent patches, same qualified naming, same semantics. The only difference is scope:

| Mechanism | Where | Scope |
|---|---|---|
| Agent patches | `[[patches.agent]]` in city.toml | All rigs |
| Rig patches | `[[rigs.patches]]` in city.toml | One rig only |

**Application order** (later wins):

1. Agent definition (from `agents/` directory)
2. Pack-level agent patches (from pack's `[[patches.agent]]`)
3. City-level agent patches (from city.toml `[[patches.agent]]`)
4. Rig patches (from city.toml `[[rigs.patches]]`)

A rig patch can undo a city-level patch for that one rig.

## Alternatives considered

- **Keep `[[agent]]` tables, add asset conventions alongside.** Doesn't solve scattered identity — two parallel declaration mechanisms is worse than one.
- **Provider-specific overlay via separate `overlay_dir` fields per provider.** Doesn't compose when multiple packs contribute overlays.
- **Ship MCP config as raw provider JSON in overlays.** Current approach. Doesn't compose across packs (last-writer-wins on settings.json), duplicates across providers.
- **Build a custom skills system.** Agent Skills is already adopted by 30+ tools. Building our own creates a walled garden.

## Scope and impact

- **Breaking:** `[[agent]]` tables move to `agents/` directories. Migration tooling needed.
- **Config:** city.toml gains canonical `[agent_defaults]` defaults, loses `[[agent]]` tables. `agent.toml` is new per-agent. `[agents]` remains a compatibility alias only.
- **Prompts:** `.template.md` infix becomes required for template processing. Existing `.md` prompts using `{{` need renaming to `.template.md`.
- **New features:** Skills, MCP TOML abstraction, `per-provider/` overlays, `template-fragments/` convention, `patches/` directory.
- **Naming:** Current `[[rigs.overrides]]` renamed to `[[rigs.patches]]` for consistency with `[[patches.agent]]`.
- **Docs:** Tutorials and reference docs need updates.

## Open questions

- **Skill lifecycle:** Should agent-created skills auto-promote, stay local to the rig, or require explicit `gc skill promote`? Current design says explicit.
- **Provider-named agents:** Must `agents/claude/` use `provider = "claude"`, or is naming just convention?
- **Suppressing implicit agents:** How does a city say "I configure claude as a provider but don't want an implicit `claude` agent"?
- **Patch directory structure:** Flat `patches/` or namespaced by target pack?
- **Patches vs. overrides naming:** This proposal unifies on "patches" everywhere. Alternative: unify on "overrides" everywhere. The key property is that the mechanism is the same regardless of scope.

---END ISSUE---
