# `gc cli2 pack remove`

Remove an import from the selected scope.

## Usage

```sh
gc cli2 pack remove <import-name> [--pack <path>] [--rig <name-or-path>]
```

## Notes

- manages imports only
- does not imply eager cache deletion
- `--rig` is the only way to do rig-scoped imports
