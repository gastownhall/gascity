# Track 3 Contract

This file pins the implementation contract for command and doctor discovery on branch `feat/commands-doctor`.

## Agreed Direction

- Commands are exposed through import bindings: `gc <binding> <command...>`.
- Doctor checks contribute to plain `gc doctor`.
- Commands are closed by default with the rest of the import model.
- This track keeps command exposure city-scoped; rig imports may contribute doctor checks, but not `gc <binding> ...` command namespaces.
- This track does not invent new extension roots, city-root command exposure, or transitive command re-export behavior.

## Command Shape

- Command discovery is convention-based under `commands/`.
- Nested directories imply nested command words.
- A command leaf is a directory containing `run.sh`.
- Command leaves are terminal: once a directory is treated as a command leaf, discovery does not recurse into its child directories. Child directories under a leaf are treated as local assets, not nested commands.
- `help.md` is optional help content for that leaf.
- Hidden and underscore-prefixed directories are skipped.
- `command.toml` is optional and is an override/metadata escape hatch, not the primary source of command placement.

Examples:

```text
commands/status/run.sh            -> status
commands/repo/sync/run.sh         -> repo sync
commands/repo/sync/help.md        -> help for repo sync
```

## Doctor Shape

- Doctor discovery is convention-based under `doctor/`.
- Each doctor leaf is a directory containing `run.sh`.
- `help.md` is optional.
- Hidden and underscore-prefixed directories are skipped.
- `doctor.toml` is optional metadata.

Examples:

```text
doctor/git-clean/run.sh
doctor/binaries/run.sh
```

## Loader + CLI Scope

- Discovery should follow the existing V2 agent-discovery pattern in `internal/config/pack.go`.
- Discovered commands and doctor checks should flow through pack loading rather than being re-scanned later from raw pack dirs.
- CLI command registration should use binding names, not pack names.
- Existing script execution behavior should be preserved as much as possible.

## Commit Plan

1. Discovery primitives and tests.
2. Loader integration for discovered commands and doctor checks.
3. CLI cutover for `gc <binding> <command...>` and `gc doctor`.
