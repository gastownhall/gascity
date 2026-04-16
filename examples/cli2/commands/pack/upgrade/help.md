# `gc cli2 pack upgrade`

Upgrade imported packs in scope and fetch the new resolved result.

## Usage

```sh
gc cli2 pack upgrade [<import-name>] [--pack <path>] [--rig <name-or-path>]
```

## Notes

- with no target, upgrades all imports in scope
- with a target, upgrades one imported pack
- fetches the new resolved result into cache
