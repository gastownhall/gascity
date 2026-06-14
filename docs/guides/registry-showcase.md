---
title: "Public Registry Packs"
description: Find and import first-party packs from the public Gas City registry.
---

# Public Registry Packs

Gas City publishes first-party reusable packs through the public
`gascity-packs` registry. A registry is a discovery catalog: checked-in
`pack.toml` files still record durable GitHub tree URLs plus an optional
version constraint or pin.

Add the public registry locally:

```bash
gc pack registry add main https://github.com/gastownhall/gascity-packs.git
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

Gas City's built-in packs are explicit imports, not implicit loader magic.
New cities created by `gc init` include pinned imports for the bundled packs
they need. `gc doctor --fix` can repair missing or stale bundled-pack pins.
See [System Packs](/reference/system-packs) for the built-in pack contract.

## Freshness

Registry records are cached locally. `gc pack registry search` and
`gc pack registry show` warn when a cache is older than the freshness window.
The default window is 24 hours.

Use `--refresh` when you want the command to fetch the latest catalog before
reading it:

```bash
gc pack registry search gascity --refresh
gc pack registry show main:gascity --refresh
```

Set `GC_REGISTRY_FRESHNESS` to a positive Go duration string when you want a
different warning window:

```bash
GC_REGISTRY_FRESHNESS=1h gc pack registry search gascity
```

Invalid, zero, or negative values warn and are ignored for freshness
calculation.

## Publishing

Use the registry submission command for pack publish requests:

```bash
gc pack registry publish .
```

The command submits the pack rooted at the given path to the configured
registry service. The hosted registry still reviews and lands catalog changes
before other users see them; after a publish is accepted, refresh local caches
before searching or showing the new entry.
