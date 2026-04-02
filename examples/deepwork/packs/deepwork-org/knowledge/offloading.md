# Orchestration Offloading Guide

## Problem
95% of Claude sessions are automated overhead (witness, refinery, patrol).
These are mechanical tasks that don't need Claude's reasoning.

## Solution
Route patrol agents through LiteLLM proxy → MiniMax/Kimi backend.

### Environment Variables
```bash
ANTHROPIC_BASE_URL=http://100.108.196.44:4000  # LiteLLM proxy
ANTHROPIC_API_KEY=<litellm-key-with-aliases>    # Key with model aliases
```

### LiteLLM Per-Key Aliases
```json
{
  "aliases": {
    "claude-sonnet-4-6": "minimax-m2.5",
    "claude-haiku-4-5-20251001": "minimax-m2.5",
    "claude-opus-4-6": "minimax-m2.5"
  }
}
```

### Configure Per-Role
In `.gt-town/town.toml` or per-rig `rig.toml`:
```toml
[role_env.witness]
ANTHROPIC_BASE_URL = "http://100.108.196.44:4000"
ANTHROPIC_API_KEY = "<litellm-patrol-key>"

[role_env.refinery]
ANTHROPIC_BASE_URL = "http://100.108.196.44:4000"
ANTHROPIC_API_KEY = "<litellm-patrol-key>"
```

### Cost Impact
Before: ~$200-500/month (Claude on patrol)
After: ~$0 (MiniMax self-hosted on 8xH100)
