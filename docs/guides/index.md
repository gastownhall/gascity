---
title: Overview
description: Practical guides for common Gas City workflows.
---

Guides are for going deeper on a subsystem or accomplishing a specific
workflow — the "understanding-*" guides explain how to think about a piece,
the rest are task-oriented.

## Understand

- [Coming from Coding Agents](/guides/capabilities-for-coding-agent-users) — maps context, state, skills, history, messaging, roles, and identity from single-agent coding tools onto Gas City's shared infrastructure.
- [Understanding Packs](/guides/understanding-packs) — what a pack is, where its definitions live, and how imports become city behavior.
- [Understanding Formulas](/guides/understanding-formulas) — how to think about formulas, choose a contract, and apply the major patterns.

## How-to

- [Configuring an Agent](/guides/configuring-an-agent) — the five axes (harness, model, upstream, transport, runtime) and how to set each in city.toml / agent.toml.
- [Create and Share Packs](/guides/shareable-packs) — create, import, and customize packs across the `pack.toml` / `city.toml` / `.gc/` boundary.
- [Find and Import Public Packs](/guides/registry-showcase) — find and import first-party packs from the public Gas City registry.
- [Configure the Gastown Pack](/guides/gastown-config-recipes) — task-oriented config overrides for the Gastown pack: register rigs, scale pools, swap providers, patch agents, and tweak prompts.
- [Use JSON from the gc CLI](/guides/using-json-from-gc) — drive `gc --json` and `gc --json-schema` from scripts, agents, tests, and other software.
- [Set Up a Multi-Agent Engineering Environment](/guides/multi-agent-engineering-environment) — give a by-hand multi-human, multi-agent workflow a better home by writing the method down once.

See also the [Troubleshooting runbooks](/troubleshooting/dolt-bloat-recovery)
for operational recovery procedures, and the [Reference](/reference/index)
section for exact command and config details.
