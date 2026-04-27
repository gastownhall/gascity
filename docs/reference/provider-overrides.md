---
title: Provider Overrides
description: Cookbook patterns for provider presets, model flags, wrappers, and per-agent defaults.
---

Provider overrides let a city keep Gas City's built-in provider behavior while
changing the local launch details. Use them when you need a specific model,
wrapper script, settings file, or per-agent default without forking the built-in
provider.

## Start From A Built-In Provider

Set `base = "builtin:<provider>"` to inherit the provider's transport,
readiness, hooks, and process detection defaults. Then override only the fields
that differ.

```toml
[providers.opencode-deepseek-v4]
base = "builtin:opencode"
display_name = "OpenCode DeepSeek V4"
options_schema_merge = "by_key"
```

## DeepSeek V4 Through OpenCode

OpenCode uses model IDs in `provider/model` form. After connecting DeepSeek in
OpenCode, use `opencode models deepseek --refresh` to confirm the exact IDs your
local OpenCode install exposes.

DeepSeek's current V4 API model IDs are `deepseek-v4-pro` and
`deepseek-v4-flash`. In OpenCode, those are selected as
`deepseek/deepseek-v4-pro` and `deepseek/deepseek-v4-flash`.

```toml
[workspace]
provider = "opencode-deepseek-v4"

[providers.opencode-deepseek-v4]
base = "builtin:opencode"
display_name = "OpenCode DeepSeek V4"
options_schema_merge = "by_key"
title_model = "deepseek/deepseek-v4-flash"

[providers.opencode-deepseek-v4.option_defaults]
model = "deepseek/deepseek-v4-pro"

[[providers.opencode-deepseek-v4.options_schema]]
key = "model"
label = "Model"
type = "select"
default = "deepseek/deepseek-v4-pro"

  [[providers.opencode-deepseek-v4.options_schema.choices]]
  value = "deepseek/deepseek-v4-pro"
  label = "DeepSeek V4 Pro"
  flag_args = ["--model", "deepseek/deepseek-v4-pro"]

  [[providers.opencode-deepseek-v4.options_schema.choices]]
  value = "deepseek/deepseek-v4-flash"
  label = "DeepSeek V4 Flash"
  flag_args = ["--model", "deepseek/deepseek-v4-flash"]
```

Use the faster model for agents that handle routine or high-volume work:

```toml
[[agent]]
name = "triage"
provider = "opencode-deepseek-v4"
option_defaults = { model = "deepseek/deepseek-v4-flash" }
```

If the city explicitly installs OpenCode hooks, list the built-in hook family:

```toml
[workspace]
install_agent_hooks = ["opencode"]
```

Provider aliases inherit hook support, but `install_agent_hooks` is validated
against hook family names.

## Wrap A Provider Command

Use a wrapper when the executable needs local setup, logging, or environment
normalization before launching the real provider. Keep `path_check` pointed at
the real binary so `gc doctor` can still report whether the dependency exists.

```toml
[providers.opencode-logged]
base = "builtin:opencode"
command = "scripts/opencode-logged"
path_check = "opencode"
```

For wrapped providers that support resume through a subcommand or flag, declare
`resume_command` with `{{.SessionKey}}` so resumed pool sessions launch through
the same wrapper.

```toml
[providers.claude-logged]
base = "builtin:claude"
command = "scripts/claude-logged"
path_check = "claude"
resume_command = "scripts/claude-logged --resume {{.SessionKey}}"
```

## Prefer Options For User-Selectable Flags

Use `options_schema` when a flag should be visible as a session or per-agent
choice. Gas City removes stale schema-managed flags before applying the selected
defaults, so changing an agent's `option_defaults` updates the final launch
command cleanly.

Use `args_append` only for static arguments that should always be present,
should not be per-agent selectable, and belong to the provider's normal launch
arguments. For ACP providers with separate `acp_args`, prefer `options_schema`
for model or permission choices so the selected flags are applied to the
transport-specific command.

```toml
[providers.gemini-with-static-flag]
base = "builtin:gemini"
args_append = ["--some-static-flag"]
```

## Related References

- [Config Reference](/reference/config)
- [OpenCode provider setup](https://opencode.ai/docs/providers)
- [OpenCode model IDs](https://opencode.ai/docs/models)
- [DeepSeek V4 API announcement](https://api-docs.deepseek.com/news/news260424)
