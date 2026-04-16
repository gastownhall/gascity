# `gc cli2 pack add`

Add an import to the selected scope and fetch it by default.

## Usage

```sh
gc cli2 pack add <source-or-name> [--name <import-name>] [--pack <path>] [--rig <name-or-path>]
```

## Notes

- accepts qualified registry names, unqualified names when unambiguous, URLs, and local paths
- fetches resolved content into cache by default
- `--rig` is the only way to do rig-scoped imports
