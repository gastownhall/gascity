---
title: "Prompt Templates"
---

> Last revised for merge-wave decisions: 2026-04-10

## Summary

Prompt Templates is a Layer 0-1 primitive that defines agent behavior
through Go `text/template` in Markdown. Prompt files now opt in to
template processing explicitly via the `.template.md` suffix; plain
`prompt.md` files remain plain content. All role-specific behavior is
user-supplied pack content — the SDK contains zero hardcoded role
names. Templates are rendered at agent startup with a `PromptContext`
that provides city, agent, rig, and git metadata, making every agent
prompt dynamically customized to its deployment context.

## Key Concepts

- **Prompt Template**: A Markdown file with Go template directives
  (`.template.md` extension). Each agent's `prompt_template` config
  field points to one. Templates define the agent's behavioral
  specification: what it does, how it finds work, how it communicates.

- **PromptContext**: The data available to templates during rendering.
  Includes CityRoot, AgentName (qualified: `rig/agent-1`),
  TemplateName (config name: `agent` for pool template), RigName,
  WorkDir, IssuePrefix, Branch, DefaultBranch, WorkQuery, SlingQuery,
  and custom Env vars from agent config.

- **Shared Templates**: Reusable template partials in a `shared/`
  directory next to the prompt templates. Automatically loaded and
  available via `{{template "name" .}}`. Used for cross-agent
  conventions like command glossaries and architecture context.

- **Appended Fragments**: Named template fragments that are rendered and
  appended after the main prompt body. Configured through
  `append_fragments` on either `[agent_defaults]` (city- and pack-wide)
  or per-agent on an `[[agent]]` block / `agents/<name>/agent.toml`.
  Per-agent `append_fragments` layers in front of imported-pack and
  city-level `[agent_defaults].append_fragments`. `inject_fragments` on
  an agent is the legacy per-agent spelling; it still appends, but new
  configs should prefer `append_fragments`. Explicit
  `{{template "name" .}}` calls still control in-body placement;
  appended fragment settings do not.

- **Template Functions**: Three built-in functions: `cmd` (binary
  name), `session` (compute session name for an agent), `basename`
  (extract base name from qualified name).

## Architecture

```
  Agent Config                 Template File
  ┌──────────────┐            ┌──────────────────┐
  │prompt_template│───────────▶│ prompts/agent     │
  │  = "prompts/ │            │  .template.md     │
  │   agent      │            └────────┬──────────┘
  │   .template.md"│                   │
  └──────────────┘            ┌────────▼──────────┐
                              │ shared/ partials   │
                              │  (auto-loaded)     │
  PromptContext               └────────┬──────────┘
  ┌──────────────┐                     │
  │ CityRoot     │            ┌────────▼──────────┐
  │ AgentName    │───────────▶│  renderPrompt()   │
  │ RigName      │            │  (text/template)  │
  │ WorkDir      │            └────────┬──────────┘
  │ WorkQuery    │                     │
  │ SlingQuery   │                     ▼
  │ Env          │              Rendered Markdown
  └──────────────┘              (agent's prompt)
```

### Data Flow

1. Controller resolves agent config including `prompt_template` path
2. `renderPrompt()` reads the template file from the city directory
3. Shared templates from the sibling `shared/` directory are loaded
   first, making them available via `{{template "name" .}}`
4. The main template is parsed last (its body becomes the root)
5. `buildTemplateData()` merges `Env` (lower priority) with SDK
   fields (higher priority) into a single `map[string]string`
6. Template executes against the merged data map
7. Any configured `append_fragments` are rendered and appended after
   the main prompt body
8. On parse/execute error, logs warning to stderr and returns raw text
   (graceful fallback — never blocks agent startup)

### Key Types

- **`PromptContext`** — template data struct. Defined in
  `cmd/gc/prompt.go`.
- **`renderPrompt()`** — reads, parses, and renders a template.
  Returns empty string if template path is empty or file doesn't
  exist. Defined in `cmd/gc/prompt.go`.
- **`buildTemplateData()`** — merges Env with SDK fields. SDK fields
  override Env keys. Defined in `cmd/gc/prompt.go`.
- **`promptFuncMap()`** — template function registration. Defined in
  `cmd/gc/prompt.go`.

## Invariants

1. **No hardcoded role names.** Templates define roles. The SDK never
   references specific role names like "mayor" or "deacon". If a Go
   file contains a role name, it's a bug.
2. **SDK fields override Env.** If an agent's `Env` map contains a key
   that collides with an SDK field (e.g., `CityRoot`), the SDK value
   wins.
3. **Graceful fallback on error.** Parse or execute errors produce the
   raw template text, not an empty string. Agents always get a prompt.
4. **Missing template returns empty.** If `prompt_template` is empty or
   the file doesn't exist, `renderPrompt()` returns `""` without error.
5. **Shared templates load from sibling directory.** Canonical
   `.template.md` files and legacy `.md.tmpl` files in the `shared/`
   subdirectory next to the template are loaded. Canonical files win on
   definition collisions. No recursive traversal.
6. **`append_fragments` is append-only.** It does not control in-body
   placement. If a fragment is explicitly referenced in the template and
   also listed in `append_fragments`, it appears twice.

## Interactions

| Depends on | How |
|---|---|
| `internal/fsys` | Reads template files from disk |
| `internal/config` | Agent.PromptTemplate path, Agent.Env vars |
| `internal/git` | DefaultBranch for PromptContext |
| `internal/agent` | SessionNameFor() via `session` template function |

| Depended on by | How |
|---|---|
| `cmd/gc/cmd_prime.go` | `gc prime` outputs rendered prompt |
| `cmd/gc/providers.go` | Rendered prompt passed to agent on start |
| Agent hooks | Hook calls `gc prime` to get the prompt |

## Code Map

- `cmd/gc/prompt.go` — PromptContext, renderPrompt, buildTemplateData,
  promptFuncMap (141 LOC)
- `cmd/gc/cmd_prime.go` — `gc prime` command (outputs rendered prompt)

Template files are user-supplied pack content, not SDK code. See example templates
in `examples/gastown/packs/gastown/prompts/`.

## Configuration

```toml
[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
[agent.env]
CUSTOM_VAR = "value"    # available as {{.CUSTOM_VAR}} in template
```

Preferred defaults naming:

```toml
[agent_defaults]
append_fragments = ["safety"]
```

### Template Variables

| Variable | Source | Example |
|---|---|---|
| `CityRoot` | City directory path | `/home/user/my-city` |
| `AgentName` | Qualified agent name | `frontend/worker-1` |
| `TemplateName` | Config template name | `worker` |
| `RigName` | Rig name (empty for city agents) | `frontend` |
| `WorkDir` | Agent working directory | `/projects/frontend` |
| `IssuePrefix` | Rig bead ID prefix | `FE` |
| `Branch` | Current git branch | `feature-x` |
| `DefaultBranch` | Default branch | `main` |
| `WorkQuery` | Work discovery command | `bd ready --assignee=...` |
| `SlingQuery` | Work routing command | `gc sling ...` |

### Template Functions

| Function | Usage | Returns |
|---|---|---|
| `cmd` | `{{cmd}}` | Binary name (`gc`) |
| `session` | `{{session .AgentName}}` | Session name for agent |
| `basename` | `{{basename .AgentName}}` | Base name from qualified name |

### Fragment Composition

There are two distinct ways fragment content can appear in a rendered
prompt:

| Mechanism | Where declared | Effect |
|---|---|---|
| `{{ template "name" . }}` | inside `prompt.template.md` | Places fragment content exactly where referenced |
| `append_fragments = ["name"]` | `[agent_defaults]` | Appends fragment content after the rendered prompt body |
| `append_fragments = ["name"]` | per-agent (`[[agent]]` or `agents/<name>/agent.toml`) | Appends fragment content after the rendered prompt body; layers in front of `[agent_defaults]` |
| `inject_fragments = ["name"]` | per-agent settings (legacy) | Appends fragment content after the rendered prompt body; retained for migration, new configs should use `append_fragments` |

## Testing

- `cmd/gc/prompt_test.go` — unit tests for renderPrompt, template
  function behavior, Env override semantics
- `examples/gastown/gastown_test.go` — TestPromptFilesExist,
  TestAllPromptTemplatesExist (validates all referenced templates exist)

## Known Limitations

- **No template inheritance.** Templates compose via `shared/` partials
  and `{{template}}`, but there's no `extends` mechanism. Each agent
  prompt is self-contained.
- **Flat data model.** `buildTemplateData()` merges everything into
  `map[string]string`. No nested data, no typed values, no arrays.
- **No runtime re-rendering.** Prompts are rendered once at agent
  startup. Config changes require agent restart to take effect.

## See Also

- [Agent Protocol](agent-protocol.md) — how rendered prompts are
  delivered to agents via runtime.Provider
- [Config System](config.md) — how Agent.PromptTemplate and Agent.Env
  are resolved through override layers
- [Glossary](glossary.md) — authoritative definitions of prompt
  template, nudge, and related terms
