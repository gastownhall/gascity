---
title: "Public Registry Packs"
description: Find and import Gas City-maintained packs published through the public registry.
---

# Public Registry Packs

Gas City maintains the public `gascity-packs` registry for reusable packs it
publishes and reviews in the `gascity-packs` repo. The registry is a catalog
for discovery; your checked-in `pack.toml` still records durable GitHub tree
URLs and optional version constraints.

## Use The Gas City Registry

1. Add the public registry locally:

```bash
gc pack registry add main https://github.com/gastownhall/gascity-packs.git
gc pack registry refresh main
```

2. Search and inspect entries:

```bash
gc pack registry search gascity
gc pack registry show main:gascity
```

3. Use the import command printed by `show`:

```bash
gc import add https://github.com/gastownhall/gascity-packs/tree/main/gascity --name gascity --version '>=0.1.0'
```

4. Install and validate the authored import:

```bash
gc import install
gc import check
gc config show --validate
```

When you decide to use a pack, prefer the exact command printed by
`gc pack registry show`. It writes a durable `source` URL and optional
`version`; it does not write the local registry handle into `pack.toml`.

## Published Packs

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

## Create Your Own Registry

A registry is a Git repository with a `registry.toml` catalog. To try your own
pack registry, publish a repo with that catalog, then add it locally under the
name you want to use:

```bash
gc pack registry add team https://github.com/example/team-packs.git
gc pack registry refresh team
gc pack registry show team:example-pack
```

Use the public `gascity-packs` registry as the reference shape for catalog
entries, release pins, and pack source URLs.

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

## Registry Scope

Registry support is catalog plumbing: add a registry repo you trust, refresh
its catalog, inspect an entry, and import the GitHub tree URL it advertises.
A marketplace would add a separate curation layer, such as submission policies,
moderation, ownership signals, ranking, and discovery across many publishers.

This page only advertises Gas City-maintained entries and the mechanics for
adding registry repos. Publishing a new Gas City-maintained entry is still a
`gascity-packs` repo change: update the catalog, review the change, merge it,
then refresh local registry caches before searching or showing the new entry.
