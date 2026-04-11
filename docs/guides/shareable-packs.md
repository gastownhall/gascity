---
title: "Shareable Packs"
---

A practical guide for creating and consuming shareable packs as
pluggable features in Gas City.

## What is a shareable pack?

A pack is a directory containing a `pack.toml` file and any
supporting assets (prompt templates, scripts, formulas). It defines a
reusable set of agents that can be composed into any city.

```
my-pack/
├── pack.toml        # agent definitions + metadata
├── prompts/
│   └── worker.md        # prompt templates (auto-resolved)
├── scripts/
│   └── setup.sh         # session setup scripts
└── formulas/            # optional formula directory
    └── code-review.formula.toml
```

Packs are self-contained: they carry everything their agents need.
Paths in `pack.toml` (prompt_template, session_setup_script,
overlay_dir) resolve relative to the pack directory, so the
pack works regardless of where it's referenced from.

Packs can be:
- **Local directories** — referenced by relative or absolute path
- **Remote git repos** — fetched and cached via `[[packs]]` source

## Creating a shareable pack

### pack.toml format

```toml
[pack]
name = "code-review"
version = "1.0.0"
schema = 1

[[agent]]
name = "reviewer"
prompt_template = "prompts/reviewer.md"
provider = "claude"

[agent.pool]
min = 0
max = 3

[[agent]]
name = "summarizer"
prompt_template = "prompts/summarizer.md"
provider = "claude"
```

Required metadata fields:
- **name** — identifier for the pack
- **schema** — format version (currently `1`)

Optional metadata:
- **version** — semver string for tracking
- **requires_gc** — minimum gc version
- **city_agents** — agent names that should be city-scoped (see below)

### Prompt templates and scripts

Reference prompts and scripts using paths relative to the pack
directory:

```toml
[[agent]]
name = "reviewer"
prompt_template = "prompts/reviewer.md"
session_setup_script = "scripts/setup.sh"
overlay_dir = "overlays/reviewer"
```

During expansion, Gas City rewrites these paths to absolute paths so
they work regardless of which city references the pack.

### Including formulas

Add a `[formulas]` section to include a formula directory:

```toml
[formulas]
dir = "formulas"
```

Formula directories participate in the layered formula resolution
system. Pack formulas are lower priority than city-local or
rig-local formulas, so consumers can override specific formulas.

### Including providers

Packs can define provider presets that their agents depend on:

```toml
[providers.claude]
start_command = "claude --dangerously-skip-permissions"
```

Provider definitions merge additively — existing city providers are
not overwritten. This means the consumer's provider config takes
precedence.

### Dual-scope packs (city_agents)

Some packs define agents that should run at city scope (not per-rig)
alongside agents that run per-rig. Use `city_agents` to declare which
agents are city-scoped:

```toml
[pack]
name = "gastown"
schema = 1
city_agents = ["mayor", "deacon"]

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[agent]]
name = "deacon"
prompt_template = "prompts/deacon.md"

[[agent]]
name = "polecat"
prompt_template = "prompts/polecat.md"

[agent.pool]
min = 0
max = 5
```

When this pack is referenced from both `workspace.includes` and a
rig's `includes`:
- City expansion keeps only `mayor` and `deacon` (dir="")
- Rig expansion keeps only `polecat` (dir=rig name)

## Consuming a shareable pack

### Local reference

Reference a pack directory by path in your `city.toml`:

```toml
# City-level (agents get dir="")
[workspace]
includes = ["packs/base"]

# Or multiple city packs
[workspace]
includes = ["packs/base", "packs/monitoring"]

# Rig-level (agents get dir=rig name)
[[rigs]]
name = "my-project"
path = "/home/user/my-project"
includes = ["packs/gastown"]

# Or multiple rig packs
[[rigs]]
name = "my-project"
path = "/home/user/my-project"
includes = ["packs/base", "packs/review"]
```

Relative paths resolve against the city directory (where `city.toml`
lives).

### Remote reference

Define named pack sources and reference them by name:

```toml
[packs.gastown]
source = "https://github.com/example/gastown-pack.git"
ref = "v1.0.0"
path = "pack"  # subdirectory within the repo

[[rigs]]
name = "my-project"
path = "/home/user/my-project"
includes = ["gastown"]
```

Remote packs are fetched once and cached in `.gc/pack-cache/`.
The cache key includes the source URL, ref, and path.

### Customizing pack agents

Use per-rig overrides to customize agents from a pack without
modifying the pack itself:

```toml
[[rigs]]
name = "my-project"
path = "/home/user/my-project"
includes = ["packs/gastown"]

[[rigs.overrides]]
agent = "polecat"
provider = "gemini"
idle_timeout = "30m"

[rigs.overrides.env]
CUSTOM_VAR = "value"

[rigs.overrides.pool]
max = 10
```

Override fields (all optional):
- **provider** — change the agent's provider
- **suspended** — suspend/unsuspend the agent
- **idle_timeout** — change idle timeout
- **prompt_template** — replace the prompt template
- **start_command** — change the start command
- **nudge** — change the nudge text
- **env** / **env_remove** — add/remove environment variables
- **pool** — override pool settings (min, max, check, drain_timeout)
- **pre_start** — replace pre-start commands
- **session_setup** / **session_setup_script** — replace session setup
- **overlay_dir** — replace overlay directory
- **install_agent_hooks** — replace agent hook installation list

For city-level customization, use patches:

```toml
[[patches.agent]]
name = "mayor"
provider = "gemini"
```

## Handling name collisions

When two packs define an agent with the same name and both apply to
the same scope (same rig, or both city-level), Gas City reports an error
with provenance:

```
rig "myrig": packs define duplicate agent "worker":
  - packs/base
  - packs/extras
rename one agent in its pack.toml, or use separate rigs
```

### Resolution strategies

1. **Rename in pack.toml** — if you control the pack, change one
   agent's name to be unique.

2. **Use separate rigs** — apply each pack to a different rig. Since
   agent uniqueness is scoped to `(dir, name)`, the same agent name in
   different rigs is valid.

3. **Split the pack** — extract the conflicting agent into its own
   pack so you can choose which version to include.

## Example: composing three packs

```toml
[workspace]
name = "full-stack-city"
provider = "claude"
includes = ["packs/orchestration"]

# Remote pack source
[packs.code-review]
source = "https://github.com/example/review-pack.git"
ref = "main"

# Provider presets
[providers.claude]
start_command = "claude --dangerously-skip-permissions"

[providers.gemini]
start_command = "gemini-cli"

# Rig with two composed packs
[[rigs]]
name = "backend"
path = "/home/user/backend"
includes = ["packs/base-agents", "code-review"]

# Override the reviewer to use gemini
[[rigs.overrides]]
agent = "reviewer"
provider = "gemini"

# Second rig with just the base pack
[[rigs]]
name = "frontend"
path = "/home/user/frontend"
includes = ["packs/base-agents"]
```

This city composes:
- **orchestration** pack at city scope (dir="")
- **base-agents** pack on both rigs
- **code-review** pack only on the backend rig
- Per-rig overrides to customize the reviewer agent
