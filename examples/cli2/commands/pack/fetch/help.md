# `gc cli2 pack fetch`

Fetch resolved pack content into cache for the selected scope.

## Usage

```sh
gc cli2 pack fetch [<import-name>] [--pack <path>] [--rig <name-or-path>]
```

## Notes

- with no target, fetches all imports in scope
- with a target, fetches one imported pack in scope
- does not edit imports
