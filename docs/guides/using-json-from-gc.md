---
title: "Using JSON from the Gas City CLI (`gc`)"
description: Use `gc --json` and JSON schemas from scripts, agents, tests, and other software.
---

Gas City's CLI is human-readable by default. When software calls `gc`, use
`--json` on commands that support it so callers do not have to parse tables,
status text, or progress messages.

The standardized JSON contract is being rolled out across the CLI. Commands
that support it should produce deterministic machine-readable output on stdout
while preserving the normal human output when `--json` is not present.

## Quick Start

Use `--json` on supported commands:

```sh
gc status --json
gc session list --json
gc rig list --json
```

Treat stdout as JSONL: one complete JSON value per line. Most bounded commands
emit exactly one record, so ordinary JSON parsers can read the whole stdout
body as one JSON value after trimming the trailing newline.

```sh
gc status --json | jq .
```

Shell scripts should continue to use the process exit code for control flow:

```sh
if out="$(gc status --json)"; then
  jq -r '.city_name' <<<"$out"
else
  code=$?
  jq -r '.error.message' <<<"$out" >&2
  exit "$code"
fi
```

## Stdout And Stderr

When `--json` is passed, stdout is reserved for machine-readable output.

Supported JSON commands should not write human progress lines, tables, banners,
debug text, or summaries to stdout. Important command results belong in JSON
fields, not copied prose.

Stderr remains available for operational diagnostics. A caller should not need
stderr to understand the successful result shape, but stderr may still contain
human-readable details that help debug failures.

## Failure Output

When a supported JSON command fails, stdout should contain one structured JSON
failure record and the process should return a nonzero exit code. The JSON
failure record includes the same code:

```json
{
  "schema_version": "1",
  "ok": false,
  "error": {
    "code": "command_failed",
    "message": "command failed; see stderr for diagnostics",
    "exit_code": 1
  }
}
```

Agents and scripts should:

- parse stdout as the result or failure payload.
- use the process exit code for shell success/failure logic.
- expect `error.exit_code` to match the process exit code.
- treat stderr as diagnostics, not as the result contract.

## JSONL Framing

The `gc --json` transport is JSONL. Each stdout line is one complete JSON
record.

For the common bounded-command case, the default contract is exactly one record.
That single record should usually be a top-level object with fields such as
`schema_version`, command data, warnings, and summary counts.

Streaming commands may emit multiple records when their schema says so. For
example, event streams naturally use one record per event.

## Discovering Schemas

Use `--json-schema` to discover the JSON contract for a command.

```sh
gc status --json-schema
```

This prints one JSONL manifest record:

```json
{
  "schema_version": "1",
  "command": ["status"],
  "transport": "jsonl",
  "json_supported": true,
  "schemas": {
    "result": {
      "$schema": "https://json-schema.org/draft/2020-12/schema",
      "type": "object"
    },
    "failure": {
      "$schema": "https://json-schema.org/draft/2020-12/schema",
      "type": "object"
    }
  }
}
```

Ask for a specific schema role when you only need that schema:

```sh
gc status --json-schema=result
gc status --json-schema=failure
```

If a known command does not declare JSON support, `--json-schema` returns a
manifest with `json_supported: false` and an empty `schemas` object:

```json
{
  "schema_version": "1",
  "command": ["version"],
  "transport": "jsonl",
  "json_supported": false,
  "schemas": {}
}
```

Role-specific requests for unavailable schemas fail with the standard failure
shape.

## Record Counts

JSON Schema describes one JSON value. For `gc --json`, each schema describes
one JSONL record. Gas City uses an optional extension keyword to describe the
record stream around that schema:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "x-gc-jsonl": {
    "minRecords": 0
  },
  "type": "object"
}
```

When `x-gc-jsonl` is absent, the command emits exactly one record. When it is
present:

- `minRecords` is the minimum number of records. If omitted, the minimum is `0`.
- `maxRecords` is the maximum number of records. If omitted, there is no maximum.
- `{}` means zero or more records.
- `{ "minRecords": 1 }` means one or more records.
- `{ "minRecords": 0, "maxRecords": 1 }` means zero or one record.
- `{ "minRecords": 1, "maxRecords": 1 }` means exactly one record, explicitly.

## Field Conventions

New or newly standardized JSON commands should use stable field names:

| Concept | Preferred field |
| --- | --- |
| Schema version | `schema_version` |
| Identifier | `id` |
| Display name | `name` |
| Fully scoped name | `qualified_name` or `scoped_name` |
| Filesystem path | `path` |
| Source of data | `source` |
| Durable reference | `ref` |
| Lifecycle value | `status` or `state` |
| Type discriminator | `type` |
| Dispatch target | `target` |
| Creation time | `created_at` |
| Update time | `updated_at` |
| Warnings | `warnings` |
| Summary counts | `summary` |

Timestamps should be RFC3339 strings.

Warnings that matter to software consumers should appear in structured JSON,
for example:

```json
{
  "warnings": [
    {
      "code": "partial_data",
      "message": "session provider was unavailable",
      "path": "sessions"
    }
  ]
}
```

Commands may also write human-readable diagnostics to stderr for compatibility
and troubleshooting.

## Pack-Defined Commands

Pack-defined commands can be scripts or external programs, so Gas City does not
automatically make arbitrary pack command output JSON-safe.

A pack command declares JSON support by placing a result schema next to the
command implementation:

```text
commands/
  review/
    pr/
      run.sh
      schemas/
        result.schema.json
```

Nested command directories imply nested command paths. In the example above,
the schema belongs to the pack command leaf represented by `commands/review/pr/`.

`schemas/failure.schema.json` is optional. Use it only when the command has
meaningful command-specific failure fields beyond the shared default failure
shape.

## Passthrough Commands

Some commands pass arguments through to another CLI. For example, `gc bd ...`
routes to the bead CLI in the correct city or rig context.

Passthrough commands are not native `gc` JSON contracts. If the downstream tool
supports JSON, it owns that output shape. Gas City should not represent
passthrough output with a fake "anything is valid" schema.

## Compatibility Notes

Existing JSON commands may be standardized over time. A PR that changes an
existing JSON output shape should call that out explicitly, including:

- the command and invocation.
- the old shape.
- the new shape.
- whether the change is additive or intentionally incompatible.
- the rationale for making the change in that PR.

Human-readable output remains the default and should stay compatible unless a
command's normal human behavior is intentionally changed.

## Related Reference

Use the generated [CLI Reference](/reference/cli) for exact command flags.
Use [Events](/reference/events) for the `gc events` JSONL event contract.
