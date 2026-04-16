# `gc cli2 pack list`

List imports in the selected scope.

## Usage

```sh
gc cli2 pack list [--transitive] [--pack <path>] [--rig <name-or-path>]
```

## Notes

- default output is the direct import set
- `--transitive` includes the full resolved transitive set
- `--rig` is the only way to do rig-scoped imports
