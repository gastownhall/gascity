---
title: "Using JSON from the Gas City CLI (`gc`)"
description: Use `gc --json` from scripts, agents, tests, and other software.
---

Gas City's CLI is human-readable by default. When software calls `gc`, use
`--json` on commands that support it so callers do not have to parse tables,
status text, or progress messages.

The standardized JSON contract is being rolled out across the CLI. This guide
separates what works today from conventions that new and newly standardized
commands should follow.

## Quick Start

Use `--json` on supported commands:

```sh
gc status --json
gc session list --json
gc rig list --json
```

Most bounded commands emit one JSON value. Ordinary JSON parsers can read the
whole stdout body after trimming the trailing newline.

```sh
gc status --json | jq .
```

Shell scripts should continue to use the process exit code for control flow:

```sh
if out="$(gc status --json)"; then
  jq -r '.city_name' <<<"$out"
else
  code=$?
  printf 'gc status failed with exit code %s\n' "$code" >&2
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

## Failure Output Today

Current `gc --json` commands use the process exit code for shell
success/failure logic. On failure, commands may write human-readable diagnostics
to stderr and may write no JSON to stdout.

Agents and scripts should:

- use the process exit code for shell success/failure logic.
- parse stdout as JSON only after a successful exit.
- capture stderr separately when they need diagnostic text.
- not assume every command emits a structured JSON failure payload yet.

## JSONL Framing

For the common bounded-command case, stdout is one complete JSON value. That
value may be pretty-printed across multiple physical lines, so do not treat each
line as a standalone JSON record for bounded commands.

Streaming commands may emit multiple records when their schema says so. For
example, event streams naturally use one JSON value per event.

## Planned Schema Discovery

The `--json-schema` flag is planned but is not implemented by the current `gc`
binary. Until it ships, use command help, generated reference docs, and command
source/tests to confirm exact JSON shapes.

The planned discovery contract is:

```sh
gc status --json-schema
```

It will print one manifest record:

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

Role-specific schema requests are also planned:

```sh
gc status --json-schema=result
gc status --json-schema=failure
```

When implemented, if a known command does not declare JSON support,
`--json-schema` should return a
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

Role-specific requests for unavailable schemas should fail with the standardized
failure shape once that failure shape exists.

## Planned Failure Shape

New or newly standardized JSON commands should eventually return one structured
JSON failure record on stdout when `--json` is passed and the command fails:

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

That failure shape is not universal today. Compatibility notes for any PR that
adopts it should call out which command changed and how existing callers should
migrate.

## Planned Record Counts

JSON Schema describes one JSON value. The planned schema-discovery contract may
use an optional extension keyword to describe the record stream around that
schema:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "x-gc-jsonl": {
    "minRecords": 0
  },
  "type": "object"
}
```

This `x-gc-jsonl` vocabulary is planned; no current CLI tooling enforces it.
When it is implemented, absence should mean the command emits exactly one
record. When present:

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

Current commands do not all match these conventions. For example,
`gc session list --json` currently emits Go struct field names such as `ID`,
`State`, `CreatedAt`, and `SessionName`. Treat each existing command's actual
output as authoritative until that command is explicitly standardized.

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

Planned schema discovery for pack-defined commands may use schemas next to the
command implementation:

```text
commands/
  review/
    pr/
      run.sh
      schemas/
        result.schema.json
```

Nested command directories would imply nested command paths. In the example
above, the schema belongs to the pack command leaf represented by
`commands/review/pr/`.

`schemas/failure.schema.json` is optional. Use it only when the command has
meaningful command-specific failure fields beyond the shared default failure
shape.

This convention is not loaded by current `gc` runtime code. Treat it as a design
direction until pack command schema discovery is implemented.

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
