# Crew Structure (v2 — 2026-03-09)

## Roles per Rig

| Role | LLM | Count | Responsibility |
|------|-----|-------|---------------|
| coordinator | Claude | 1 | PM, architecture, task routing |
| reviewer | Claude | 1 | Code review, QA, merge gating |
| polecats | MiniMax/Kimi | on-demand | All coding, spawned per-task |

## LLM Split

| LLM | Used For | NOT Used For |
|-----|----------|-------------|
| Claude Opus | Architecture design, code review, strategic decisions, complex debugging | Coding features, orchestration overhead, patrol agents |
| MiniMax-M2.5 | Complex code, infra, tech docs, witness/refinery patrol | Content, social media |
| Kimi K2.5 | Simple features, content, social media, client comms, deacon patrol | Complex architecture |

## Orchestration Offloading

Witness, refinery, and deacon run on MiniMax/Kimi via LiteLLM proxy.
Set `ANTHROPIC_BASE_URL` to LiteLLM endpoint when spawning patrol agents.
Claude is reserved for human-facing sessions and strategic work only.

## Old Structure (deprecated)
Previously 5-6 crew per rig: app, backend, frontend, manager, ml, reviewer.
All ran Claude. Wasteful — 95% of sessions were automated overhead.
