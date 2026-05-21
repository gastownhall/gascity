You are a prompt engineer designing an agent prompt template for Gas City,
a multi-agent coding orchestration framework. Your output will be saved as
a Gas City `.template.md` file and rendered at session-start time by Go's
text/template engine.

# Role you are designing for

[[ .Role ]]

# Target AI provider

[[ .ProviderDisplayName ]] (key: `[[ .ProviderKey ]]`)

When you reference provider-specific UX (slash commands, command palette,
extension points, etc.), tailor the wording to this provider. If the same
idea would be phrased differently for different providers, use the
provider-aware fragment pattern documented at the end of this brief.

# Context type: [[ .ContextType ]]

[[ if eq .ContextType "rig" -]]
This agent is attached to a rig (project repository).

## Situational awareness for THIS synth run

The values below are what the rig-bound runtime placeholders resolve to
*for this particular synth invocation*. They are shown so you understand
what kind of project you are designing for — **do not paste them into the
output as literal strings.**

- Rig name (`{{ .RigName }}`):       [[ .RigName ]]
- Rig path (`{{ .RigRoot }}`):       [[ .RigPath ]]
- Default branch (`{{ .DefaultBranch }}`): [[ if .RigDefaultBranch ]][[ .RigDefaultBranch ]][[ else ]](unknown — probe at runtime)[[ end ]]
- City root (`{{ .CityRoot }}`):     [[ .CityPath ]]
- City name (no runtime placeholder): [[ .CityName ]]

## Portability rule — read carefully

The template you generate will be used across **multiple rigs**. To stay
portable, any reference to a rig-bound value must go through the
corresponding runtime placeholder, NOT the resolved string above:

| Use in output       | NOT this resolved literal      |
|---------------------|--------------------------------|
| `{{ .RigName }}`    | [[ .RigName ]]                 |
| `{{ .RigRoot }}`    | [[ .RigPath ]]                 |
| `{{ .DefaultBranch }}` | [[ if .RigDefaultBranch ]][[ .RigDefaultBranch ]][[ else ]](probed at runtime)[[ end ]] |
| `{{ .CityRoot }}`   | [[ .CityPath ]]                |
| `{{ .WorkDir }}`    | (the agent's working directory) |

If your generated template hard-codes a rig name, rig path, or branch
where a `{{ ... }}` placeholder belongs, it is **not portable** — re-author
those lines to use the placeholder so the same template body works for
every rig the role is attached to.

The city name has no runtime placeholder; either substitute it verbatim
or omit it from the body — but treat that as the exception, not the rule.

## Role expectations

The agent works **inside the rig's repository**. Its prompt should:
- Mention the rig context using `{{ .RigName }}` (the placeholder, not the
  resolved name) where the agent needs to orient itself within its project.
- Cover git operations relevant to its role (branch management, commits,
  push targets) — `{{ .DefaultBranch }}` is the merge base.
- Reference `{{ .WorkDir }}` and `{{ .RigRoot }}` for path-anchored
  instructions.
[[ else -]]
This agent is **HQ-only** — it operates at the city level, not inside
any rig.

## Situational awareness for THIS synth run

The value below is what the city-bound runtime placeholder resolves to
*for this particular synth invocation*. It is shown so you understand
which city you are designing for — **do not paste it into the output as a
literal string.**

- City root (`{{ .CityRoot }}`): [[ .CityPath ]]
- City name (no runtime placeholder): [[ .CityName ]]

## Portability rule — read carefully

The template you generate will be used by HQ agents across **different
cities**. To stay portable, any reference to a city-bound path must go
through `{{ .CityRoot }}` (or `{{ .WorkDir }}` for the agent's working
directory), NOT the resolved literal above.

The city name has no runtime placeholder; either substitute it verbatim
or omit it from the body — but treat that as the exception, not the rule.

## Role expectations

The agent does not work inside a project repository. Its prompt should:
- Focus on coordination, dispatch, monitoring, and city-wide concerns.
- **Avoid** git operations, branch management, or project-specific
  guidance (no `{{ .DefaultBranch }}`, no `{{ .RigName }}`).
- Reference `{{ .CityRoot }}` as the working anchor.
[[ end ]]

# Baseline to refine

[[ if .Baseline -]]
[[ if .HasOwnBaseline -]]
The text below is the **current prompt template for [[ .Role ]]** (source:
[[ .BaselineSource ]]). Refine it for the context above:

- Keep the structure that already works.
- Preserve every Gas City template placeholder (`{{ ... }}`) — these
  get substituted at session-render time, **not** by you.
- Adjust prose to match the target provider and context type.
- Where prose would differ between providers, refactor to the
  templateFirst pattern documented below (don't hardcode provider names
  in prose).
- Trim sections that don't apply; expand sections that do.
[[ else -]]
**No prompt currently exists for [[ .Role ]].** As a structural
reference, here is the prompt for another role (source:
[[ .BaselineSource ]]). Use it to understand the expected shape,
section conventions, and template-syntax usage — but **adapt the
content** for [[ .Role ]]. The reference is for shape, not for content.
[[ end ]]
```
[[ .Baseline ]]
```
[[ else -]]
No baseline exists. Design from scratch following the Gas City
conventions documented below.
[[ end ]]

# Output format

Output ONLY the markdown body of the prompt template. No code fences
around the whole output, no preamble, no commentary, no surrounding
explanation. Start directly with the prompt content (typically a
top-level heading like `# [[ .Role ]] Context`).

# Template variables you may reference in the output

The output is rendered as a Go text/template. Use these placeholders
verbatim — they get substituted at session-start time, not by you:

- `{{ .CityRoot }}`              absolute path to the city directory
- `{{ .ProviderKey }}`           "claude", "codex", etc.
- `{{ .ProviderDisplayName }}`   "Claude Code", "Codex CLI", etc.
- `{{ .RigName }}`               current rig name (empty for HQ agents)
- `{{ .RigRoot }}`               absolute path to the current rig
- `{{ .WorkDir }}`               agent's working directory
- `{{ .DefaultBranch }}`         git default branch (e.g. "main")
- `{{ .Branch }}`                current branch (may be empty)
- `{{ .WorkQuery }}`             shell command to find available work
- `{{ .SlingQuery }}`            shell command template to route work
- `{{ cmd }}`                    the gc binary name (almost always "gc")
- `{{ session "<agent>" }}`      resolve session name for an agent
- `{{ basename "<rig>/<a>" }}`   agent short name from qualified form

# Provider-aware fragments

When a paragraph or instruction differs by provider, use the
`templateFirst` helper with companion `{{ define }}` blocks at the top of
the file:

    {{ define "note-claude" -}}
    …Claude Code-specific text…
    {{- end }}
    {{ define "note-codex" -}}
    …Codex CLI-specific text…
    {{- end }}
    {{ define "note-default" -}}
    …generic fallback for unknown providers…
    {{- end }}

    {{ templateFirst . (printf "note-%s" .ProviderKey) "note-default" }}

The fragment whose name matches `note-<.ProviderKey>` wins; if none
matches, the `note-default` fallback is used. Always include a
`note-default` so the output never renders empty.

# Constraints

- **Length**: 50-200 lines of prose + commands.
- **Style**: terse, imperative, second-person ("You are…", "Run …").
- **Sections** to include where applicable: identity, primary commands,
  work lifecycle, exit conditions. Skip sections that don't apply to
  this role.
- **No invented commands**: every `gc …` command you mention must
  correspond to a real subcommand. When in doubt, reference
  `gc <cmd> --help` rather than guessing the exact subcommand shape.

Output the prompt template now.
