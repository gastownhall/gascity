# `gc cli2 pack`

Local import and cache-oriented pack commands.

## Subcommands

- `add`
- `remove`
- `list`
- `show`
- `fetch`
- `outdated`
- `upgrade`

## Notes

- `add` manages imports and fetches by default
- `list --transitive` is the first-release transitive view
- `fetch` is the explicit warm-cache and reconcile command
- registry browsing lives under `gc cli2 registry`
