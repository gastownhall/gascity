---
title: Harness Recipes
description: Copy-paste upstream + model setup for each built-in harness — the fast path to swapping your CLI onto Gas City.
---

Already using a coding-agent CLI? This is the per-harness quick reference for
pointing it at Gas City. Each section gives the `provider` name, the env vars the
harness reads (its **serving-env contract**), and the two permutations you'll
actually use: **direct** (your existing login / API key) and a **custom
endpoint** (a proxy, Bedrock/Vertex, or an OpenAI-compatible gateway), plus how
to pick the **model**.

These are recipes. For *how* the pieces work — the abstract-vs-raw upstream
model, harness-portable rendering, secrets, the fingerprint behavior, and every
other agent knob — see **[Configuring an Agent](/guides/configuring-an-agent)**.
The authoritative field list is the [Config Reference](/reference/config).

Two things hold for every recipe:

- **Direct usually needs no upstream block at all.** If the harness already finds
  its credentials in the environment (your normal login or `*_API_KEY`), just set
  `provider` and go — Gas City passes the ambient environment through.
- **Secrets are `$VAR` refs**, expanded from the controller's environment at
  launch — never inlined into `city.toml`.

## At a glance

| Harness | `provider` | Reads (base URL · key/token) | `model` option |
|---------|-----------|------------------------------|----------------|
| Claude Code | `claude` | `ANTHROPIC_BASE_URL` · `ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` | yes |
| Codex CLI | `codex` | `OPENAI_BASE_URL` · `OPENAI_API_KEY` | yes |
| Gemini CLI | `gemini` | `GOOGLE_GEMINI_BASE_URL` · `GEMINI_API_KEY` | yes |
| Grok | `grok` | — · `XAI_API_KEY` | yes |
| Kimi Code | `kimi` | `KIMI_BASE_URL` · `KIMI_API_KEY` | yes |
| Kiro | `kiro` | — · `KIRO_API_KEY` | — |
| Cursor Agent | `cursor` | — · `CURSOR_API_KEY` | — |
| GitHub Copilot | `copilot` | `COPILOT_PROVIDER_BASE_URL` · `COPILOT_PROVIDER_API_KEY` / `COPILOT_GITHUB_TOKEN` | — |
| Sourcegraph AMP | `amp` | `AMP_URL` · `AMP_API_KEY` | — |
| OpenCode | `opencode` | gateway (per-upstream) | yes |
| Groq (OpenCode) | `groq` | gateway → `GROQ_API_KEY` | yes |
| Cerebras (OpenCode) | `cerebras` | gateway → `CEREBRAS_API_KEY` | yes |
| Pi | `pi` | login | yes |
| Auggie | `auggie` | login | — |
| Oh My Pi | `omp` | login | — |
| Antigravity | `antigravity` | login | — |

"—" under base URL means the harness has no standard base-URL env var: for a
custom endpoint, supply it through the upstream's raw [`env`](/guides/configuring-an-agent#axis-3-upstream-who-serves-the-model)
block. Model lists below are the built-in choices; override or extend a harness's
`options_schema` to add your own.

---

## Native harnesses

These ship a serving-env binding, so an **abstract** upstream (`base_url` /
`api_key` / `auth_token`) renders onto the harness's env vars automatically — the
same upstream works across harnesses.

### Claude Code — `provider = "claude"`

Reads `ANTHROPIC_BASE_URL` and `ANTHROPIC_API_KEY` (or `ANTHROPIC_AUTH_TOKEN` for
bearer-token gateways).

```toml
# Direct: your existing Claude login or ambient ANTHROPIC_API_KEY
# agents/dev/agent.toml
provider        = "claude"
option_defaults = { model = "sonnet" }     # opus · sonnet · haiku · opus-4-7 · fable-5
```

```toml
# Custom endpoint (Bedrock, a proxy, an Anthropic-compatible gateway)
# city.toml
[upstreams.bedrock]
base_url = "https://bedrock.example.com/anthropic"
api_key  = "$AWS_BEDROCK_KEY"
# a token gateway instead of a key:  auth_token = "$MY_BEARER"

# agents/dev/agent.toml
provider = "claude"
upstream = "bedrock"
```

### Codex CLI — `provider = "codex"`

Reads `OPENAI_BASE_URL` and `OPENAI_API_KEY`.

```toml
# Direct
provider        = "codex"
option_defaults = { model = "gpt-5.5" }    # gpt-5.6-sol · gpt-5.6-terra · gpt-5.6-luna · gpt-5.5 · gpt-5.3-codex · o3 · o4-mini
```

```toml
# Custom endpoint (an OpenAI-compatible proxy / Azure OpenAI / vLLM)
[upstreams.openai-proxy]
base_url = "https://oai.example.com/v1"
api_key  = "$OPENAI_PROXY_KEY"

# agent.toml
provider = "codex"
upstream = "openai-proxy"
```

### Gemini CLI — `provider = "gemini"`

Reads `GOOGLE_GEMINI_BASE_URL` and `GEMINI_API_KEY`.

```toml
provider        = "gemini"
option_defaults = { model = "gemini-2.5-pro" }   # gemini-2.5-pro · gemini-2.5-flash

# custom endpoint
[upstreams.gemini-proxy]
base_url = "https://gemini.example.com"
api_key  = "$GEMINI_PROXY_KEY"
```

### Kimi Code — `provider = "kimi"`

Reads `KIMI_BASE_URL` and `KIMI_API_KEY`.

```toml
provider        = "kimi"
option_defaults = { model = "kimi-k2.6" }   # kimi-k2.6 · kimi-k2-thinking-turbo

[upstreams.kimi]
base_url = "https://api.moonshot.example/v1"
api_key  = "$KIMI_KEY"
```

### Grok — `provider = "grok"`

Reads `XAI_API_KEY` (key only — no standard base-URL var).

```toml
provider        = "grok"
option_defaults = { model = "grok-build" }  # grok-build · grok-composer-2.5 · grok-composer-2.5-fast

# direct: just set XAI_API_KEY in the controller environment
[upstreams.xai]
api_key = "$XAI_API_KEY"
# need a custom base URL? add it raw — there is no abstract base_url binding:
#   [upstreams.xai.env]
#   XAI_BASE_URL = "https://xai.example.com/v1"
```

### Cursor Agent — `provider = "cursor"`

Reads `CURSOR_API_KEY` (`cursor-agent`; no built-in `model` option — Cursor picks
the model).

```toml
provider = "cursor"
[upstreams.cursor]
api_key = "$CURSOR_API_KEY"
```

### Kiro — `provider = "kiro"`

Reads `KIRO_API_KEY` (`kiro-cli`; no `model` option).

```toml
provider = "kiro"
[upstreams.kiro]
api_key = "$KIRO_API_KEY"
```

### GitHub Copilot — `provider = "copilot"`

Default path uses your GitHub account bearer (`COPILOT_GITHUB_TOKEN`). For a
custom model provider, set `COPILOT_PROVIDER_BASE_URL` + `COPILOT_PROVIDER_API_KEY`.

```toml
# Direct: GitHub-hosted models via your account token
[upstreams.copilot-github]
auth_token = "$COPILOT_GITHUB_TOKEN"

# Custom provider behind Copilot
[upstreams.copilot-custom]
base_url = "https://models.example.com/v1"
api_key  = "$COPILOT_PROVIDER_KEY"
# some setups also need COPILOT_PROVIDER_TYPE / COPILOT_MODEL — add them raw:
[upstreams.copilot-custom.env]
COPILOT_PROVIDER_TYPE = "openai"

# agent.toml
provider = "copilot"
upstream = "copilot-custom"
```

### Sourcegraph AMP — `provider = "amp"`

Reads `AMP_URL` and `AMP_API_KEY`.

```toml
provider = "amp"
[upstreams.amp]
base_url = "https://amp.example.com"
api_key  = "$AMP_API_KEY"
```

---

## OpenCode and gateways

[OpenCode](https://opencode.ai) is a multi-provider harness — one CLI fronting
many model providers, where the credential env var is *provider-specific*. It has
no single serving-env binding, so the **upstream names its own target** with the
per-field `*_env` override (see the
[gateway section](/guides/configuring-an-agent#gateway-harnesses-the-per-field-override)
of the main guide). Gas City ships ready-made `groq` and `cerebras` presets on
top of it.

### Groq — `provider = "groq"`

```toml
# Simplest: GROQ_API_KEY ambient in the controller environment
provider        = "groq"
option_defaults = { model = "groq/llama-3.3-70b-versatile" }
# also: groq/openai/gpt-oss-120b · groq/qwen/qwen3-32b · groq/llama-3.1-8b-instant

# Managed: have Gas City inject the key, naming Groq's env var explicitly
[upstreams.groq]
api_key     = "$GROQ_KEY"
api_key_env = "GROQ_API_KEY"

# agent.toml → provider = "groq", upstream = "groq"
```

### Cerebras — `provider = "cerebras"`

```toml
provider        = "cerebras"
option_defaults = { model = "cerebras/zai-glm-4.7" }
# also: cerebras/gpt-oss-120b · cerebras/qwen-3-235b-a22b-instruct-2507

[upstreams.cerebras]
api_key     = "$CEREBRAS_KEY"
api_key_env = "CEREBRAS_API_KEY"
```

### OpenCode (any provider) — `provider = "opencode"`

```toml
provider        = "opencode"
option_defaults = { model = "opencode/big-pickle" }
# also: opencode/deepseek-v4-flash-free · opencode/nemotron-3-super-free

# Point at any provider OpenCode supports by naming that provider's key var:
[upstreams.my-provider]
api_key     = "$MY_PROVIDER_KEY"
api_key_env = "OPENROUTER_API_KEY"   # whatever env var the chosen provider reads
```

---

## Login-based harnesses

These authenticate through their own login/device flow and read no Gas City
upstream env. Set `provider` and run their normal `login` once on the box; no
`[upstreams]` block is needed.

```toml
provider = "pi"          # Pi Coding Agent     — has a model option (ollama-cloud-gpt-oss-20b)
# provider = "auggie"    # Augment Auggie CLI
# provider = "omp"       # Oh My Pi
# provider = "antigravity"   # Antigravity (agy)
```

If one of these later needs a custom endpoint, give it a raw upstream `env` block
with whatever vars its CLI documents — the same escape hatch shown for Grok
above.

---

## Next steps

- **Other agent knobs** (prompt, model permission modes, transport, runtime,
  pools, lifecycle): [Configuring an Agent](/guides/configuring-an-agent).
- **Ship a harness as a reusable preset** — a provider and its `upstream_env`
  binding (a pack may also declare named `[upstreams]` endpoint presets as a
  base the importing city inherits; a city's or fragment's own `[upstreams]`
  override the pack-provided ones):
  [Understanding Packs](/guides/understanding-packs) ·
  [Shareable Packs](/guides/shareable-packs).
- **Exact fields and types:** [Config Reference](/reference/config).
