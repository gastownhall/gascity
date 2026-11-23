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
  WorkDir, IssuePrefix, Branch, DefaultBranch, WorkQuery,
  AssignedInProgressQuery, AssignedReadyQuery, RoutedPoolQuery,
  SlingQuery, and custom Env vars from agent config.

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
  │ Assigned...  │                     │
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
  Returns an empty string, silently, if the template path is empty,
  and an empty string after a stderr warning if a declared template
  cannot be read. Defined in `cmd/gc/prompt.go`.
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
4. **Missing template returns empty.** If `prompt_template` is empty,
   `renderPrompt()` returns `""` silently.
   If a declared template cannot be read, it returns `""` after a
   `gc: prompt_template …` warning to stderr: session resolution ships
   the beacon alone, and a non-strict `gc prime` serves the built-in
   default. Parse or execute errors still return the raw text
   (invariant 3).
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
| `cmd/gc/doctor_prompt_delivery_budget.go` | `gc doctor` renders each agent's prompt to check its delivery budget and reports the render's stderr warnings |
| Agent hooks | Hook calls `gc prime` to get the prompt |

## Code Map

- `cmd/gc/prompt.go` — PromptContext, renderPrompt, buildTemplateData,
  promptFuncMap (141 LOC)
- `cmd/gc/cmd_prime.go` — `gc prime` command (outputs rendered prompt)

Template files are user-supplied pack content, not SDK code. See the example
templates in the gastown pack's `prompts/` directory in the
[gascity-packs repository](https://github.com/gastownhall/gascity-packs).

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
| `AssignedInProgressQuery` | Assigned in-progress recovery command | `bd list --include-ephemeral --status in_progress ...` |
| `AssignedReadyQuery` | Assigned ready-work command | `bd ready --include-ephemeral --assignee=...` |
| `RoutedPoolQuery` | Unassigned routed pool-work command | `bd ready --metadata-field gc.routed_to=... --unassigned` |
| `SlingQuery` | Work routing command | `gc sling ...` |

### Claim Identity Convention

The `AssignedInProgressQuery` (Tier 1) and `RoutedPoolQuery` (Tier 3) rows
above are rendered once, at session start, from Go `text/template`
variables. But the *literal* prompt text written into `prompts/*.md.tmpl`
files also references two OS environment variables directly — `$GC_TEMPLATE`
and `$GC_ALIAS` — which the agent's own shell commands expand at run time,
not at template-render time. These are a separate injection mechanism from
the table above, and the two env vars are not interchangeable:

- `$GC_TEMPLATE` is the qualified template identity, shared by *every* live
  session of that template (the named-session holder and every pool slot
  alike).
- `$GC_ALIAS` is the current session's own concrete identity — the bare
  qualified name for a named holder or canonical singleton slot, or the
  suffixed `-N` identity (e.g. `rig/builder-1`) for a pool slot.

Convention for which one to use in a prompt template's startup section:

| Line | Token | Why |
|---|---|---|
| Tier 1 (crash-recovery query) | `${GC_ALIAS:-$GC_TEMPLATE}` | Ownership read — must resolve to *this session's own* prior claim, not a sibling's. |
| Claim (`bd update <id> --claim --assignee=...`) | `${GC_ALIAS:-$GC_TEMPLATE}` | Ownership write — must stamp *this session's* concrete identity. |
| Tier 2 (pre-assigned ready-work query) | bare `$GC_TEMPLATE` | Shared role discovery — pre-assigned work is deliberately assigned to the bare template. |
| Tier 3 (routed-pool queue query) | bare `$GC_TEMPLATE` | Shared queue key — `gc.routed_to` is a role-level routing target, not a per-session claim. |

Getting this wrong is not cosmetic: a template that is both a
`[[named_session]]` and a multi-slot pool has more than one live identity
sharing a single template string. If Tier 1 or the claim line uses the bare
template, two sessions can independently "recover" or adopt the same
in-progress bead and both complete it — duplicate commits, duplicate PRs,
duplicate closes. See `engdocs/design/session-model-unification.md` (Runtime
Environment) for where `GC_ALIAS`/`GC_TEMPLATE` are defined, and the
`ga-i1d0tr` architecture decision for the full incident writeup. The
`${GC_ALIAS:-$GC_TEMPLATE}` fallback form degrades safely to today's
behavior for sessions where `GC_ALIAS` is unset (pure singleton/named
templates, where the two values are identical anyway).

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

- [Session](session.md) — how rendered prompts are
  delivered to agents via runtime.Provider
- [Config System](config.md) — how Agent.PromptTemplate and Agent.Env
  are resolved through override layers
- [Glossary](glossary.md) — authoritative definitions of prompt
  template, nudge, and related terms
