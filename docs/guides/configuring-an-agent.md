---
title: Configuring an Agent
description: The five axes of an agent — harness, model, upstream, transport, runtime — and how to set each in city.toml / agent.toml.
---

The [Agents tutorial](/tutorials/02-agents) shows the fast path: drop an
`agent.toml` with a `provider` and a prompt, and sling work to it. This guide is
the how-to reference for everything you can tune underneath that — the
**five independent axes** of an agent — with a deep dive on the **upstream**
axis (who serves the model), which is the newest knob.

For the exact field list and types, see the generated
[Config Reference](/reference/config). This guide explains *how the pieces fit*;
the reference is the authoritative spec.

## The five axes

An agent is the composition of five orthogonal choices. You set each one
independently — change the model without touching the harness, switch who serves
the model without changing the box, and so on.

| Axis | Question | Where you set it | Example |
|------|----------|------------------|---------|
| **Harness** | which agent CLI? | agent `provider` | `provider = "claude"` |
| **Model** | which model label? | agent `option_defaults.model` | `option_defaults = { model = "sonnet" }` |
| **Upstream** | who serves the model? | agent `upstream` + `[upstreams.<name>]` | `upstream = "bedrock"` |
| **Transport** | how does gc drive it? | agent `session` | `session = "acp"` |
| **Runtime** | where does it run? | city `[session] provider` / `GC_SESSION` | `provider = "k8s"` |

> **A note on the word "provider."** It is overloaded by history. In an
> `[[agent]]` / `agent.toml` block, `provider` selects the **harness** (the agent
> CLI — `claude`, `codex`, …). In the city `[session]` block, `provider` selects
> the **runtime backend** (where sessions run — `tmux`, `k8s`, …). They are
> different axes; this guide always says "harness" or "runtime" to disambiguate.

The first three axes (harness, model, upstream) are per-agent. Transport is
per-agent. Runtime is city-/environment-wide (every session in a city runs on
the same backend).

## Axis 1 — Harness (`provider`)

The harness is the agent CLI gc launches and drives. Gas City ships built-in
presets for the popular ones — `claude`, `codex`, `gemini`, `grok`, `kimi`,
`cursor`, `copilot`, `amp`, `opencode`, and more — so the minimal agent is just:

```toml
# agents/reviewer/agent.toml
dir      = "my-project"
provider = "codex"
```

Each preset defines the command, default args, resume behavior, prompt
delivery, and the model + upstream contracts (below). Customize one with a
city-level `[providers.<name>]` block: a same-named block merges over the
built-in (you set only what you change), and a differently-named one can inherit
from any provider via `base`:

```toml
# city.toml — customize the built-in claude harness
[providers.claude]
args = ["--verbose"]            # everything else inherits from builtin:claude

# …or define a new harness that inherits from a built-in
[providers.claude-fast]
base            = "builtin:claude"
option_defaults = { model = "haiku" }
```

See [Understanding Packs](/guides/understanding-packs) for shipping a harness
preset as a reusable pack.

## Axis 2 — Model (`option_defaults.model`)

A harness exposes its configurable knobs through an **options schema**. The
`model` option is the common one; selecting a value injects the right CLI flags
for that harness — you pick an abstract label, the harness renders the flag.

```toml
# agents/reviewer/agent.toml
provider        = "codex"
option_defaults = { model = "sonnet", permission_mode = "plan" }
```

`option_defaults` is a map of option key → choice value. The provider's
`options_schema` declares the allowed choices and the `flag_args` each one emits
(see `ProviderOption` / `OptionChoice` in the [reference](/reference/config)).
This is why you write `model = "sonnet"` and not a raw `--model` flag: the schema
keeps the model selection portable and the flags server-side.

## Axis 3 — Upstream (who serves the model)

**This is the newest axis.** The *model* is *what* you ask for; the *upstream* is
*who serves and resolves it* — direct Anthropic, Bedrock, Vertex, a self-run
proxy, an OpenAI-compatible gateway. Switching upstream changes the base URL and
credentials the harness talks to, **without changing the model, the harness, or
the box** — and (post-un-weld) without re-provisioning: it relaunches the agent
in the warm box.

### Declare an upstream, select it

Upstreams are named presets at the city level; an agent selects one by name.

```toml
# city.toml
[upstreams.bedrock]
description = "Anthropic models via AWS Bedrock"
base_url    = "https://bedrock.example.com/anthropic"
api_key     = "$AWS_BEDROCK_KEY"      # a $VAR ref — never inline a secret

[agent_defaults]
upstream = "bedrock"                  # city-wide default for every agent
```

```toml
# agents/reviewer/agent.toml — this agent overrides the default
upstream = "anthropic-direct"
```

Resolution order for an agent's upstream: agent `upstream` → `agent_defaults.upstream`
→ unset (no upstream env injected; the harness uses whatever is ambient).

### Abstract vs. raw — and why abstract is portable

An upstream can be written two ways, which compose:

**Abstract (portable).** `base_url`, `api_key`, and `auth_token` are
**harness-agnostic**. The resolver renders them onto *that harness's* env-var
names, declared by the harness as its `upstream_env` binding. So one upstream
works on any harness:

```toml
[upstreams.bedrock]
base_url = "https://bedrock.example.com/anthropic"
api_key  = "$AWS_BEDROCK_KEY"
```

- on a `claude` agent → `ANTHROPIC_BASE_URL` + `ANTHROPIC_API_KEY`
- on a `codex` agent → `OPENAI_BASE_URL` + `OPENAI_API_KEY`

The built-in harnesses ship their bindings out of the box (claude → `ANTHROPIC_*`,
codex → `OPENAI_*`, gemini → `GOOGLE_GEMINI_BASE_URL`/`GEMINI_API_KEY`, and so
on). A custom harness declares its own:

```toml
[providers.myharness.upstream_env]
base_url = "MYHARNESS_BASE_URL"
api_key  = "MYHARNESS_API_KEY"
```

> An abstract field with **no** matching harness binding (and no override, below)
> is a **hard error**, never a silent no-op — you find out at resolution time,
> not when the agent quietly talks to the wrong endpoint.

**Raw (escape hatch).** When the abstract trio doesn't cover what a harness
needs, set raw env keys with `env`. They merge **after** the abstract render, so
they win:

```toml
[upstreams.bedrock]
base_url = "https://bedrock.example.com/anthropic"
api_key  = "$AWS_BEDROCK_KEY"

[upstreams.bedrock.env]
AWS_REGION                = "us-east-1"
CLAUDE_CODE_USE_BEDROCK   = "1"
```

### Gateway harnesses — the per-field override

Some harnesses are **gateways**: one CLI (e.g. `opencode`) fronts many upstreams
whose credential env var is *upstream-dependent* (`GROQ_API_KEY` for Groq,
`CEREBRAS_API_KEY` for Cerebras, …). Such a harness has no single binding to
declare. The upstream names its own target with the per-field `*_env` overrides:

```toml
[upstreams.groq]
api_key     = "$GROQ_KEY"
api_key_env = "GROQ_API_KEY"     # this upstream renders api_key to GROQ_API_KEY
```

Per-field precedence is: **upstream `*_env` override → harness binding → hard
error.** Native single-upstream harnesses stay fully abstract via their binding;
gateways supply the name themselves.

### Secrets and fingerprints

- **Secrets are never inlined.** Abstract and raw values may reference controller
  env vars with `$VAR` / `${VAR}`, expanded at resolution — so `api_key =
  "$ANTHROPIC_API_KEY"` keeps the secret out of `city.toml`.
- **Switching the upstream name** relaunches the agent in the warm box (it is a
  launch-half fingerprint change). **Rotating the key** within the same upstream
  moves no fingerprint — the resolved serving env is excluded from the hash, so a
  credential rotation never churns live sessions.

## Axis 4 — Transport (`session`)

The transport is *how* gc drives the harness. The default is **tmux** (gc sends
keystrokes and captures the pane). Set `session = "acp"` to drive the harness
over the [Agent Client Protocol](/reference/exec-session-provider) (JSON-RPC over
stdio) instead — the harness's resolved provider must declare `supports_acp = true`.

```toml
# agents/reviewer/agent.toml
provider = "claude"
session  = "acp"        # drive over ACP instead of tmux
```

## Axis 5 — Runtime (where it runs)

The runtime is *where* the session's box lives. It is selected city-wide via the
`[session]` block (or the `GC_SESSION` environment variable), not per-agent:

```toml
# city.toml
[session]
provider = "k8s"        # run every session in a Kubernetes pod
```

Built-in runtime backends: `tmux` (local, default), `subprocess` (local,
headless), `k8s` (pods), `ssh:user@host` (a remote box over SSH), and
`exec:<script>` (a pluggable [exec session pack](/reference/exec-session-provider)
— this is how the Daytona / E2B / Morph / Runloop / Blaxel sandbox runtimes
plug in). `GC_SESSION=ssh:user@host` or `GC_SESSION=exec:gc-runtime-daytona`
selects one without editing `city.toml`.

> An agent that never needs interactive attach can set `attach = false` to let gc
> pick a lighter runtime (subprocess) where the city allows it.

## Putting it together

A fully-specified agent that runs Claude on Bedrock, in a Kubernetes pod, driven
over tmux:

```toml
# city.toml
[session]
provider = "k8s"

[upstreams.bedrock]
base_url = "https://bedrock.example.com/anthropic"
api_key  = "$AWS_BEDROCK_KEY"

[upstreams.bedrock.env]
AWS_REGION = "us-east-1"
```

```toml
# agents/reviewer/agent.toml
dir             = "my-project"
provider        = "claude"                       # harness
option_defaults = { model = "opus", permission_mode = "plan" }  # model
upstream        = "bedrock"                       # who serves it
# session omitted → tmux transport; [session] above → k8s runtime
```

Inspect the result without launching anything:

```shell
$ gc prime my-project/reviewer      # the rendered prompt
$ gc doctor                          # validates your city / agent config
```

> A missing upstream binding (an abstract field with no harness `upstream_env`
> and no `*_env` override) is reported as a hard error when the agent's session is
> resolved — so a misconfiguration fails the launch loudly rather than silently
> talking to the wrong endpoint.

## Where to go next

- **Spec / reference:** the authoritative field list and types —
  [Config Reference](/reference/config). Wire contract for exec runtimes —
  [Exec Session Provider](/reference/exec-session-provider).
- **Packs:** ship harness presets and upstreams as reusable, shareable bundles —
  [Understanding Packs](/guides/understanding-packs),
  [Shareable Packs](/guides/shareable-packs).
- **Tutorial:** the guided walkthrough that introduces agents —
  [Tutorial 02 — Agents](/tutorials/02-agents).
