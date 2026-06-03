---
title: "Public Registry Packs"
description: Find and import the first-party packs published through the public Gas City registry.
---

# Public Registry Packs

Gas City publishes first-party reusable packs through the public
`gascity-packs` registry. The registry is a catalog for discovery; your
checked-in `pack.toml` still records durable GitHub tree URLs and optional
version constraints.

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
`gc pack registry show`. It writes a durable `source` URL and optional
`version`; it does not write the local registry handle into `pack.toml`.

## First-Party Packs

| Pack | Use it for | Registry source |
|---|---|---|
| `gascity` | Planning and implementation workflow support for Gas City work. | `https://github.com/gastownhall/gascity-packs/tree/main/gascity` |
| `gastown` | Default Gas Town coding workflow support. | `https://github.com/gastownhall/gascity-packs/tree/main/gastown` |
| `cass` | Coding Agent Session Search prompt fragments and skill overlays. | `https://github.com/gastownhall/gascity-packs/tree/main/cass` |
| `discord` | Discord services, commands, and prompt fragments. | `https://github.com/gastownhall/gascity-packs/tree/main/discord` |
| `github` | GitHub webhook intake services and commands. | `https://github.com/gastownhall/gascity-packs/tree/main/github` |
| `slack-full` | Slack services, commands, and adapter integration. | `https://github.com/gastownhall/gascity-packs/tree/main/slack-full` |

The built-in `core` and `maintenance` packs remain implicit in this wave. Do
not add an import just to receive standard built-in behavior from `gc`.

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

## Not A Marketplace Yet

This page advertises first-party public registry entries. It is not a
marketplace or external community curation workflow. Publishing a new registry
entry is still a registry-repo change: update the catalog, review the change,
merge it, then refresh local registry caches before searching or showing the
new entry.
