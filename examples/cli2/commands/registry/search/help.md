# `gc cli2 registry search`

Search pack entries across all registries by default.

## Usage

```sh
gc cli2 registry search [query] [--registry <name>]
```

## Notes

- plain text query, not regex
- with no query, returns all pack entries
- default columns: `Registry`, `Name`, `Latest`, `Description`
