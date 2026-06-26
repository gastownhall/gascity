---
title: "Find and Import Public Packs"
description: Find and import first-party packs from the public Gas City registry.
---

Gas City publishes first-party reusable packs through the public
`gascity-packs` registry — a discovery catalog for finding packs to import (the
model lives in
[Understanding Packs](/guides/understanding-packs#registries-handles-and-sources)).
The public `main` registry is configured by default, so there is nothing to
add. Refresh its catalog before browsing:

```bash
gc pack registry refresh main
```

Search and inspect entries:

```bash
gc pack registry search gascity
gc pack registry show main:gascity
```

When you decide to use a pack, prefer the import command printed by
`gc pack registry show`. It writes the durable `source` URL and selected
`version`; it does not write the local registry handle into `pack.toml`.

## First-Party Packs

| Pack | Use it for | Registry source |
|---|---|---|
| `gascity` | Gas City planning and implementation workflow support. | `https://github.com/gastownhall/gascity-packs/tree/main/gascity` |
| `gastown` | Default Gas Town coding workflow support. | `https://github.com/gastownhall/gascity-packs/tree/main/gastown` |
| `cass` | Coding Agent Session Search prompt fragments and skill overlays. | `https://github.com/gastownhall/gascity-packs/tree/main/cass` |
| `discord` | Discord services, commands, and prompt fragments. | `https://github.com/gastownhall/gascity-packs/tree/main/discord` |
| `github` | GitHub webhook intake services and commands. | `https://github.com/gastownhall/gascity-packs/tree/main/github` |
| `slack-full` | Slack services, commands, and adapter integration. | `https://github.com/gastownhall/gascity-packs/tree/main/slack-full` |
| `slack-channel` | Shared Slack channel routing and session identity. | `https://github.com/gastownhall/gascity-packs/tree/main/slack-channel` |
| `slack-mini` | Minimal Slack mention bridge and outbound messaging. | `https://github.com/gastownhall/gascity-packs/tree/main/slack-mini` |

## Built-In Packs

Built-in packs (`core`, `bd`) are explicit pinned imports `gc init` writes, not
registry entries — see [System Packs](/reference/system-packs) for that
contract.

## Freshness

Registry records are cached locally, and `gc pack registry search`/`show` warn
when the cache is older than the freshness window (default 24 hours). Pass
`--refresh` to fetch the latest catalog first, or set `GC_REGISTRY_FRESHNESS` to
a Go duration to change the window:

```bash
gc pack registry search gascity --refresh
GC_REGISTRY_FRESHNESS=1h gc pack registry search gascity
```

## Publishing

```bash
gc pack registry publish .
```

`gc pack registry publish <path>` submits a pack to the configured registry
service. The hosted registry reviews and lands the change before others see it;
refresh local caches afterward.
