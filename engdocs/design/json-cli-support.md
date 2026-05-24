# GC CLI JSON Support Design and Plan

Status: proposed

Tracking issue: [#2138](https://github.com/gastownhall/gascity/issues/2138)

Human owner: D. Box

Workstream: `gc --json` support

## Framing

Gas City's CLI is primarily human-facing today, and that should remain the default. At the same time, more software is starting to call `gc` directly: agents, scripts, tests, dashboards, workflow controllers, and future UI/API integrations. Those callers need deterministic machine-readable output instead of parsing tables, prose, status lines, or terminal-oriented formatting.

This design adds a product-wide `--json` contract for the `gc` CLI without redesigning the entire command surface. The goal is to make JSON support easy to add consistently, land it in small reviewable PRs, and keep existing human-readable output compatible by default.

The first implementation batch should focus on high-value read/inspect and dispatch surfaces. After that, the work should proceed by command family, with shared conventions and tests so new built-in commands can pick up JSON support naturally.

## Current State Assessment

JSON support exists in parts of the CLI, but it is not yet a consistent product contract. Some commands already have machine-oriented modes, some emit bounded or streaming JSON, and many important read/inspect surfaces still require consumers to parse human-readable output.

The current gaps fall into a few categories:

- Some commands have no `--json` flag even though they expose state that software needs.
- Some existing JSON surfaces need an envelope, schema version, record-count
  contract, or
  stdout-purity audit.
- All JSON-mode stdout should be treated as newline-delimited JSON at the
  transport level. Bounded commands emit one result record by default;
  streaming commands may emit many records when their schema says so.
- Mutation commands need structured summaries, but those should follow after the read/inspect conventions are stable.
- Pack-defined commands need an extension contract rather than assuming `gc` can retrofit arbitrary external command output.

The most important immediate rule is stdout purity: when `--json` is passed, stdout should contain only the intended JSON payload, including structured JSON error payloads for nonzero exits.

## Design Goals

- Preserve current human-readable output by default.
- Make `--json` deterministic enough for agents, scripts, tests, and UI/API integrations.
- Prefer one top-level JSON object record with `schema_version` for newly
  touched bounded commands.
- Make JSONL record count part of the command schema contract.
- Return structured JSON error payloads for `gc` command failures in JSON mode.
- Keep stderr available for operational diagnostics, but do not make it part of
  the first command-output schema model.
- Avoid broad CLI redesign or command-family rewrites.
- Land work in small PRs with focused tests.
- Make new built-in commands easy to implement consistently.
- Treat pack-defined commands as extension points with declared capabilities.

## Non-Goals

- Do not make JSON the default output.
- Do not redesign command naming or command behavior.
- Do not globally convert all errors into JSON in the first wave.
- Do not promise automatic JSON support for arbitrary pack-defined commands.
- Do not preserve incidental human stdout by stuffing it into JSON fields.
- Do not make `--json` a general subprocess stdout/stderr capture API. If a
  caller needs raw subprocess capture, that should be an explicit feature such
  as `--json --capture`, not the default `--json` command contract.

## JSON Output Contract

- JSON-mode stdout uses JSONL framing at the transport level: one complete JSON
  value per line.
- Bounded JSON commands should emit exactly one top-level object record, not a
  bare array. Use collection fields such as `orders`, `sessions`, or `events`,
  plus `summary`.
- Streaming JSON commands may emit zero or more object records.
- Each command schema should document JSONL record count when it differs from
  the default of exactly one result record.
- Include `schema_version` as a string, starting at `"1"`, for new or newly touched JSON surfaces.
- Any PR that changes an existing JSON output shape must call that out explicitly in the PR description, including the old shape, the new shape, the compatibility risk, and the rationale for changing it now.
- Warnings should eventually be represented as `warnings: [{code,message,field,path}]` while still emitting important diagnostics to stderr when compatible.
- Partial/stale/offline data should use explicit booleans and nullable detail fields, for example `available`, `stale`, `source`, `reason`.
- Timestamps should be RFC3339 strings.
- Use consistent field names: `id`, `name`, `qualified_name`, `scoped_name`, `path`, `source`, `ref`, `status`, `state`, `type`, `target`, `created_at`, `updated_at`.
- `--json` should ignore human formatting knobs such as `--quiet` unless a command documents a machine-readable terse mode separately.
- Streaming commands should use JSONL with an explicit multi-record contract;
  bounded snapshots should use the default exactly-one stdout record contract.
- Failed `--json` commands should emit a structured error object that includes the same exit code returned by the process.

## Stdout And Stderr Contract

`--json` is a machine-readable IO contract, not a promise that the process is silent.

When `--json` is passed, stdout must contain only the command's intentional
machine-readable payload. At the transport level, stdout is JSONL: every
record is one complete JSON value followed by a newline. The command schema
defines how many records may appear.

- Bounded commands emit exactly one JSON object record to stdout.
- Streaming commands that opt into JSON streaming emit JSONL to stdout, one
  complete JSON object per line.
- Commands that have no data to return should emit a valid JSON object with an
  empty collection, `available: false`, or an explicit `summary`, not prose.
- No progress lines, human summaries, debug banners, table headers, or copied
  helper output may be written to stdout in JSON mode.

On failure, `--json` commands should still emit a structured JSON error object
when the command can do so deliberately:

```json
{
  "schema_version": "1",
  "ok": false,
  "error": {
    "code": "config_load_failed",
    "message": "loading config: city.toml not found",
    "exit_code": 1
  }
}
```

The process exit code must match `error.exit_code`, so shell scripts can keep
using ordinary success/failure logic while agents can parse the failure detail.
Commands should not hide failures by returning `0` just because they emitted a
well-formed JSON error object.

Stderr remains available for operational diagnostics, but it is not part of
the first command-output schema model:

- Consumers must not need stderr to understand command success, failure, or
  result semantics.
- Debug or trace diagnostics may use stderr when the user has explicitly
  enabled a diagnostic mode.
- Warnings that matter to machine consumers should also appear in structured
  JSON fields, but compatibility-sensitive human warnings may still be emitted
  to stderr.
- A later structured-diagnostics design may define JSONL stderr conventions,
  but command-specific stderr schemas are out of scope for the first schema
  contract.

This gives agents and scripts a simple rule: read stdout as JSONL, enforce the
documented record count, parse the stdout record as the result or structured error
payload, trust the process exit code for shell control flow, and treat stderr
as operational diagnostics.

Some Cobra/global error paths are cross-cutting, so the rollout may start with
command-local structured errors for touched commands and then consolidate the
helper layer. The target contract is still explicit: if a command accepts
`--json`, nonzero outcomes should be machine-readable and should carry the
return code.

## JSONL And Record Count

The product-level `--json` transport should be JSONL, because a single JSON
object followed by a newline is also a valid one-record JSONL stream. That lets
bounded and streaming commands share one transport rule without making every
bounded command behave like a stream.

The important distinction is record count, not whether the bytes are called
JSON or JSONL.

JSON Schema describes one JSON value. For Gas City JSONL output, each schema
describes one JSONL record. Gas City adds one optional extension keyword for
the stream around that record. The common bounded-command case does not need
the extension:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "schema_version": { "type": "string" }
  },
  "required": ["schema_version"]
}
```

When `x-gc-jsonl` is absent, the default `gc --json` contract is exactly one
record. That keeps the common bounded-command case terse. The schema still
describes the JSON value in that one JSONL record.

Streaming, optional, or unusual record-count contracts opt in with
`x-gc-jsonl`:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "x-gc-jsonl": {
    "minRecords": 0
  },
  "type": "object",
  "properties": {
    "event": { "type": "string" }
  },
  "required": ["event"]
}
```

`x-gc-jsonl` uses array-style count semantics for JSONL records:

- `minRecords` is the minimum number of JSONL records. When omitted, the
  minimum is `0`.
- `maxRecords` is the maximum number of JSONL records. When omitted, there is
  no maximum.
- `x-gc-jsonl: {}` means zero or more records.
- `x-gc-jsonl: { "minRecords": 1 }` means one or more records.
- `x-gc-jsonl: { "minRecords": 0, "maxRecords": 1 }` means zero or one record.
- `x-gc-jsonl: { "minRecords": 1, "maxRecords": 1 }` means exactly one record,
  explicitly.

Bounded command results should still be single object records:

- A single object record cleanly represents envelope metadata, schema version,
  filters, warnings, one or more collections, and summary counts together.
- It is easier to validate against JSON Schema as one result document.
- It is easier for agents and UI/API integrations to pass around as one value.
- It avoids asking consumers to infer which record is metadata, which record is
  an item, and whether all expected records arrived.

Therefore the convention is: JSONL everywhere as the transport; one object
record by default for bounded snapshots and command results; multi-record JSONL
only for streaming data and future explicitly designed diagnostic channels.

## Schema Discovery And Exposure

Machine consumers need a way to discover the output contract without reading Go
source or reverse-engineering examples. Each JSON-capable command should expose
its result schema from the command line. Commands may also expose a
command-specific failure schema when the shared default failure schema is not
specific enough.

Use JSON Schema 2020-12 for each JSONL record. Gas City should not require a
system-level schema id. Schema authors may still use JSON Schema's optional
`$id` when it helps external tooling, but `gc` should not depend on it.

Schema roles:

- `result.schema.json`: stdout record schema for successful exit code `0`.
- `failure.schema.json`: optional stdout record schema for nonzero exits when
  the command has meaningful command-specific failure fields.

If `result.schema.json` is absent, the command does not declare JSON support and
`gc <command> --json` should fail clearly rather than falling back to human
output. If `failure.schema.json` is absent, nonzero JSON-mode stdout uses the
shared Gas City default failure schema.

Action-result envelopes are intentionally extensible at the top level. Schemas
for commands that return `schema_version`, `ok`, `command`, and `action` should
set top-level `additionalProperties: true` so additive result fields do not
break consumers. Nested domain objects may stay stricter when their own shape is
the contract being modeled.

`gc dolt-cleanup --json` is the explicit exception to the usual
`ok`-means-operation-succeeded convention. Its `ok: true` means the cleanup
report was produced successfully; cleanup-stage failures are represented inside
the typed `errors`, `dropped.failed`, `purge`, and `reaped.errors` fields and
may still accompany a nonzero exit.

Schema exposure should happen at three levels:

- `gc <command> --help` should say whether `--json` is supported and summarize
  the output role, for example: `--json emits one result record as JSONL`.
- `gc <command> --json-schema` should print one JSONL manifest record with
  the command path, support status, and embedded schemas for available roles.
- `gc <command> --json-schema=result` or `--json-schema=failure` should print
  the requested JSON Schema object directly when available.
- The repository should carry checked-in JSON Schema documents for built-in
  commands so tests, docs, and future UI integrations can use the same source
  of truth.

Manifest output should embed available schemas inline and omit unavailable
roles. For a JSON-capable command, `result` is command-specific and `failure`
is either command-specific or the shared default:

```json
{
  "schema_version": "1",
  "command": ["status"],
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

For a command that does not declare JSON support, `--json-schema` should still
return a manifest with no embedded schemas rather than failing:

```json
{
  "schema_version": "1",
  "command": ["some", "command"],
  "json_supported": false,
  "schemas": {}
}
```

Role-specific schema requests for unavailable schemas should fail clearly using
the shared failure shape.

The shared default failure schema is intentionally minimal and extensible:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["schema_version", "ok", "error"],
  "properties": {
    "schema_version": { "type": "string" },
    "ok": { "const": false },
    "error": {
      "type": "object",
      "required": ["code", "message", "exit_code"],
      "properties": {
        "code": { "type": "string" },
        "message": { "type": "string" },
        "exit_code": { "type": "integer" }
      },
      "additionalProperties": true
    }
  },
  "additionalProperties": true
}
```

The first implementation wave does not need full JSON Schema validation for
every command, but it should establish the schema vocabulary and make each new
JSON command document:

- result record schema.
- command-specific failure record schema only when it adds useful structure
  beyond the shared default failure schema.
- exit-code relationship to `error.exit_code`.

## Built-In Schema Definition Pattern

Built-in commands should define schemas close to their typed output structs, but
the contract should not be implicit in Go types alone. A small schema registry
should eventually connect command definitions, docs, and tests.

Target pattern:

- command registers `--json` support with an output contract.
- command has typed Go structs for its JSON records.
- command tests assert stdout record count and parse the record into the typed
  struct.
- optional schema tests validate emitted examples against checked-in JSON
  Schema once the schema registry exists.
- help/docs are generated from the same contract where practical.

This keeps implementation ergonomic while giving external consumers a stable
place to discover contracts.

## Handling Incidental Human Output

Some `gc` commands share helpers that currently write human output directly to a
provided stdout writer. In JSON mode, those helpers need an explicit sink:

- Prefer passing `io.Discard` to human-output paths while collecting typed
  result data separately.
- Emit the final JSONL record once, at the end, using the real stdout writer.
- Do not buffer arbitrary human stdout and add it to a JSON field such as
  `stdout` or `messages`. That preserves accidental implementation details and
  makes the JSON contract unstable.
- If the text is meaningful product data, model it as a first-class field with a
  command-specific name. For example, a future `gc session peek --json` can
  include `output` because the captured session text is the command's actual
  result.

This is intentionally different from capturing subprocess stdout or stderr. For
commands that execute external programs, native `--json` should describe the
`gc` action and its product-level result. It should not automatically wrap the
child process's raw stdout/stderr as command JSON. A command may expose raw
captured output only when that output is the command's meaningful result, and it
should use command-specific field names. A future explicit capture mode can be
designed separately if consumers need a transport for subprocess results.

This approach keeps existing human output compatible while making `--json`
strict enough for agents, scripts, tests, and future UI/API consumers.

## Existing JSON Compatibility

Some `gc` commands already expose JSON today. Normalizing those surfaces is
more risky than adding JSON to a previously human-only command, because
existing scripts may already depend on the current field names, envelopes,
arrays, or nullability.

For any PR that alters existing JSON output, reviewers should expect a dedicated
compatibility note with:

- the command and invocation being changed.
- whether the command had JSON support before the PR.
- the old output shape, at the level of envelope/array/object, key fields, and
  notable nullability.
- the new output shape.
- whether the change is additive, normalizing-but-breaking, or deliberately
  incompatible.
- why the change is worth making in that PR instead of preserving the old shape
  or introducing a compatibility window.
- any consumer-facing mitigation, such as keeping old fields temporarily,
  adding `schema_version`, or documenting the first standardization wave as the
  compatibility boundary.
- any changelog entry that should ship with the release.

The first standardization wave may still choose to normalize existing JSON
surfaces when the benefit is high, but that choice must be visible and
reviewable. Hidden JSON shape changes are not acceptable.

## Extensibility

New built-in CLI commands should pick up JSON support through a shared local pattern rather than inventing command-specific output plumbing.

Recommended built-in command pattern:

- Add a local `--json` flag for read/inspect commands by default.
- Route human stdout through a writer that becomes `io.Discard` in JSON mode.
- Write the final JSON payload through the real stdout writer exactly once.
- On nonzero outcomes in JSON mode, write a structured JSON error object with
  `error.exit_code` matching the process exit code.
- Use a top-level object with `schema_version`.
- Put meaningful machine data in typed fields, not in copied human prose.
- Add a stdout-purity test for every command touched: successful `--json`
  stdout must parse as JSONL, must satisfy the command's record-count contract,
  and must not include human banners, progress lines, summaries, or stderr
  diagnostics.
- Keep stderr available for operational diagnostics, but do not make it part of
  the first command-output schema contract.

In a later contract/helper PR, this should become a small shared helper layer so new commands can follow the same shape with very little ceremony:

- `jsonStdout(jsonMode, stdout)` or equivalent, returning `io.Discard` for human-output paths in JSON mode.
- `writeJSON(stdout, payload)` for final payload emission.
- `jsonModeWriters(jsonMode, stdout, stderr)` or equivalent, making the stdout/stderr split hard to misuse.
- `writeJSONError(stdout, code, message, exitCode, details)` or equivalent for
  consistent nonzero outputs.
- shared warning/error structs.
- test helpers that assert JSON-only stdout.

## Pack-Defined Commands

Pack-defined commands can be arbitrary scripts or external programs, so `gc` should not assume it can automatically make them JSON-safe. Instead, pack commands should declare JSON capability and then honor the same stdout contract.

Proposed pack command contract:

- Pack commands may support JSON output by convention.
- If a pack command has `schemas/result.schema.json`, `gc <pack-command>
  --json` should pass the JSON request through to the command.
- If a pack command does not have `schemas/result.schema.json`, `gc
  <pack-command> --json` should fail clearly rather than falling back to human
  output.
- A JSON-capable pack command owns stdout purity for its command-specific
  output and should provide role schemas by convention.
- The first contract should avoid surprising transformations of arbitrary pack
  command stdout.

Pack-defined command schemas should live next to the command implementation
rather than requiring a TOML file for the common case. Nested command
directories imply nested CLI commands.

Example:

```text
commands/
  review/
    pr/
      run.sh
      schemas/
        result.schema.json
```

The presence of `schemas/result.schema.json` means the command supports
`--json`. `failure.schema.json` is optional and should be used only when the
command has meaningful command-specific failure fields beyond the shared
default failure schema. `command.toml` remains available for exceptions, but it
should not be required just to hold schema paths or basic descriptor fields.

Built-in commands should use the same role-file convention in a checked-in
schema tree, for example:

```text
schemas/
  status/
    result.schema.json
  session/
    list/
      result.schema.json
```

Open design questions for pack-defined commands:

- Which examples and external packs already expose `--json`, and what migration
  notes do they need?
- What conformance tests should verify pack-command JSONL record count and
  stdout purity?
- How should external pack schema revisions be decoupled from built-in `gc`
  schema revisions?

The bias should be toward testing-layer validation for pack-defined commands
rather than runtime wrapping. Pack commands should own their result schema and
`schema_version`; `gc` should discover that contract, pass `--json` through only
when the contract exists, and avoid introducing a common wrapper around arbitrary
pack output in the first wave.

Known local examples already use JSON-capable scripts or commands, including
`examples/dolt/commands/sync/run.sh` and `examples/dolt` health formulas/tests.
Those should be part of the pack-command inventory before tightening enforcement
around pack-defined `--json`.

## First Batch

Initial software-consumer batch:

- `gc status --json`
- `gc session list --json`
- `gc rig list --json`
- `gc sling --json`

These are the minimum useful surfaces for software-initiated Gas City work: verify readiness, discover durable sessions, discover registered rigs/repos, and dispatch work with structured refs.

## Staged Implementation

Stage 1: initial software-consumer batch.

- Add or normalize JSON support for `gc status`, `gc session list`, `gc rig list`, and `gc sling`.
- Add focused tests for parseable JSON, stdout purity, and structured JSON error
  payloads for representative nonzero paths.
- Suppress incidental human stdout in JSON mode, usually with `io.Discard`,
  while keeping stderr available for diagnostics.
- Keep all current human output as the default.

Stage 2: high-value inspection surfaces.

- Add `--json` for `gc rig status`, `gc session peek`, `gc session logs`, `gc formula list`, `gc formula show`, `gc order list`, `gc order show`, `gc order history`, `gc config show`, and `gc config explain`.
- Normalize existing JSON for `gc events`, `gc trace status`, `gc trace show`, and `gc converge status/list`.

Stage 3: mutation summaries.

- Add JSON summaries for lifecycle and dispatch mutations after envelopes are stable.
- Prioritize commands that create refs or change durable state: session creation, order runs, formula cooks, convoy actions, rig mutations, and supervisor/runtime state changes.

Stage 4: pack and extension contract.

- Define pack command schema-file conventions for JSON capability.
- Define pack command result schemas, optional command-specific failure schemas,
  and discovery behavior.
- Decide whether `gc` validates pack-command JSON or only passes through declared support.
- Add tests for unsupported pack commands invoked with `--json`.

Stage 5: workflow run/status/result surfaces.

- Design future workflow command JSON from the start instead of retrofitting it later.
- Reuse the same envelope, warning, timestamp, and stdout-purity conventions.

## Candidate Follow-Up Already Prototyped

Candidate follow-up work already explored in the integration worktree:

- `gc order list --json`
- `gc order show <name> [--rig <rig>] --json`
- focused unit tests for both JSON surfaces

Proposed list shape:

```json
{
  "schema_version": "1",
  "orders": [],
  "summary": { "total": 0 }
}
```

Proposed detail shape:

```json
{
  "schema_version": "1",
  "order": {
    "name": "digest",
    "scoped_name": "digest",
    "type": "formula",
    "formula": "mol-digest",
    "gate": "cooldown",
    "enabled": true
  }
}
```

## Dispatchable Tasks

1. Normalize existing JSON envelopes for `gc status`, `gc rig list`, and `gc session list`.
   Acceptance: each emits `schema_version`, retains existing useful fields, and includes a PR compatibility note documenting any intentional shape change.

2. Add `--json` to `gc sling`.
   Acceptance: emits one object with target, bead id, formula/workflow/molecule refs when created, dispatch success, and routed/queued state.

3. Add `--json` to `gc rig status`.
   Acceptance: emits one object with rig metadata, agent rows, running/draining/suspended booleans, and summary counts.

4. Add `--json` to `gc formula list` and `gc formula show`.
   Acceptance: list emits deterministic sorted formulas; show emits compiled recipe fields, vars, steps, and dependency edges.

5. Add `--json` to `gc order list`, `gc order show`, and `gc order history`.
   Acceptance: list/detail/history emit object envelopes; history entries use RFC3339 timestamps and include filters plus summary counts.

6. Add `--json` to `gc config show` and `gc config explain`.
   Acceptance: resolved config/provenance data are machine-readable; warnings are included in JSON and still visible enough for humans.

7. Extract shared structured JSON helpers.
   Acceptance: helper layer covers success payload writes, nonzero JSON error payloads, exit-code mirroring, and structured diagnostics without each command hand-rolling the contract.

8. Add schema manifests and discovery for built-in JSON commands.
   Acceptance: first-batch commands provide result schemas, rely on the shared
   default failure schema unless they need command-specific failure fields,
   `--json-schema` exposes embedded schemas, and help text mentions JSONL
   output.

9. Define pack-command JSON capability metadata.
   Acceptance: pack-defined commands can declare JSON support through adjacent
   schema files, nested commands infer schema paths from directory structure,
   and unsupported `--json` behavior is clear without `gc` guessing.

10. Audit pack and registry commands after pack surface stabilizes.
   Acceptance: document which commands exist, which are planned, and which should support JSON in the first pack-focused wave.

11. Design workflow run/status/result JSON from the start.
   Acceptance: proposed command surfaces follow these conventions before implementation.

## Product Decisions Needed

- For existing `--json` commands, when is the first standardization wave allowed to make normalizing-but-breaking schema changes instead of preserving old shapes or adding a compatibility window?
- Which global Cobra/pre-run error paths need centralized handling before we can say every `--json` failure returns a structured JSON error object?
- Should `gc trace show --json` and `gc events --json` use object envelopes in bounded modes while preserving JSONL for stream/tail/follow modes?
- Should mutation commands join the second wave with JSON summaries, or stay human-only until read surfaces are complete?
- Should schema exposure start as command-local `--json-schema` only, or should
  a broader command catalog/search surface follow later?
- Should runtime validation of pack-command JSONL record count be opt-in, always-on for
  declared JSON commands, or test-only at first?

## Appendix A: Audit Summary

Priority key: P0 = initial software-consumer batch, P1 = high-value read/inspect next wave, P2 = mutation summaries or useful but less urgent, P3 = human-only/server/internal unless product asks otherwise.

JSON support states: First batch = proposed initial implementation set, Existing = pre-existing JSON exists but may need envelope/stdout audit, Gap = should add, Later = defer unless product asks.

### Initial Software-Consumer Batch

| Command | JSON state | Desired JSON support | Priority | Complexity |
| --- | --- | --- | --- | --- |
| `gc status` | First batch | City/workspace identity, running/suspended state, health/degraded signals, agents, rigs, summary | P0 | Medium |
| `gc session list` | First batch | Object envelope with filters, durable sessions, id/name/template/provider/state/title/rig/session refs, summary | P0 | Medium |
| `gc rig list` | First batch | Registered rigs/repos, prefix, suspended/running state, beads status, default sling target, summary | P0 | Low |
| `gc sling` | First batch | Structured dispatch result: success, target, bead id, formula, molecule/workflow refs, convoy id, routed/queued status | P0 | Medium |

### City, Config, And Supervisor

| Command | JSON state | Desired JSON support | Priority | Complexity |
| --- | --- | --- | --- | --- |
| `gc config show` | Gap | Resolved config object, validation result, provenance/warnings; TOML remains default | P1 | Medium |
| `gc config explain` | Gap | Filtered agents plus field provenance and source files | P1 | Medium |
| `gc doctor` | Gap | Check results, severities, remediation hints, fixed/skipped flags | P1 | Medium |
| `gc cities` | Gap | Registered cities, paths, supervisor state if known | P1 | Low |
| `gc register` / `gc unregister` | Later | Mutation summary with city path and registry action | P2 | Low |
| `gc start` / `gc stop` / `gc restart` / `gc suspend` / `gc resume` | Later | Mutation summary with city path, controller/supervisor action, affected agents | P2 | Medium |
| `gc supervisor status` | Gap | Supervisor running/pid/socket/registered cities | P1 | Low |
| `gc supervisor start` / `stop` / `reload` / `run` / `install` / `uninstall` | Later | Mutation/lifecycle summaries | P2 | Medium |
| `gc supervisor logs` | Later | Bounded logs as entries; follow mode as JSONL only if requested | P2 | Medium |
| `gc dashboard serve` | Later | Server command; JSON not useful except startup metadata | P3 | Low |
| `gc version` | Gap | Version/build metadata object | P2 | Low |

### Sessions, Runtime, Waits, And Nudges

| Command | JSON state | Desired JSON support | Priority | Complexity |
| --- | --- | --- | --- | --- |
| `gc session new` | Later | Created session refs; especially useful with `--no-attach` | P2 | Medium |
| `gc session submit` | Later | Delivery result, intent, queued/woke/interrupted state | P2 | Medium |
| `gc session attach` | Later | Interactive command; JSON probably only useful for `--no-attach`-style dry result | P3 | Medium |
| `gc session suspend` / `close` / `rename` / `reset` / `prune` / `kill` / `wake` / `nudge` | Later | Mutation summaries with session id/name/state changes | P2 | Medium |
| `gc session peek` | Gap | Session id, line count, output text, stale/available flags | P1 | Low |
| `gc session logs` | Gap | Bounded entries object; follow mode JSONL if requested | P1 | Medium |
| `gc session wait` | Gap | Wait creation/update summary | P2 | Medium |
| `gc wait list` / `inspect` / `ready` | Gap | Durable wait records and readiness state | P1 | Medium |
| `gc wait cancel` | Later | Cancellation summary | P2 | Low |
| `gc runtime drain-check` / `drain-ack` | Gap | Agent drain state for scripts/controllers | P1 | Low |
| `gc runtime drain` / `undrain` / `request-restart` | Later | Mutation summaries | P2 | Low |
| `gc nudge status` / `drain` / `poll` | Gap | Deferred nudge status and delivery decisions | P1 | Medium |

### Rigs, Agents, And Work Routing

| Command | JSON state | Desired JSON support | Priority | Complexity |
| --- | --- | --- | --- | --- |
| `gc rig status` | Gap | Rig metadata, agent rows, running/draining/suspended booleans, summary | P1 | Low |
| `gc rig add` / `remove` / `default` / `suspend` / `resume` / `restart` | Later | Mutation summary with rig name/path/prefix/default target | P2 | Medium |
| `gc agent add` / `suspend` / `resume` | Later | Mutation summary with agent identity and effective config path | P2 | Low |
| `gc hook` | Existing-ish | Hook/script contract already machine-oriented; audit stdout purity and normalized JSON where applicable | P1 | Medium |
| `gc bd` | Later | Wrapper around `bd`; prefer passthrough unless `gc` adds metadata | P3 | Low |
| pack-discovered commands, e.g. `gc dolt ...` | Later | Command-specific; do not promise global shape | P3 | Unknown |

### Orders, Formulas, Convergence, And Graphs

| Command | JSON state | Desired JSON support | Priority | Complexity |
| --- | --- | --- | --- | --- |
| `gc order list` | Gap | Orders and summary | P1 | Low |
| `gc order show` | Gap | Single order object | P1 | Low |
| `gc order history` | Gap | Filtered history entries, RFC3339 timestamps, summary | P1 | Low |
| `gc order check` | Gap | Due/not-due gate evaluations and reasons; exit-code semantics need care | P1 | Medium |
| `gc order run` | Later | Mutation summary with order, wisp/root refs, target | P2 | Medium |
| `gc formula list` | Gap | Deterministic formulas, search paths, source/shadow info if available | P1 | Medium |
| `gc formula show` | Gap | Compiled recipe, variables, steps, dependencies | P1 | Medium |
| `gc formula cook` | Later | Created root/id mapping/attach refs | P2 | Medium |
| `gc converge status` / `list` | Existing | Existing JSON for status/list; audit envelope/stdout purity | P1 | Medium |
| `gc converge create` / `approve` / `iterate` / `stop` / `retry` / `test-gate` | Later | Mutation/evaluation summaries | P2 | Medium |
| `gc graph` | Gap | Nodes/edges/subgraph metadata | P1 | Medium |

### Convoys, Messaging, Events, And Trace

| Command | JSON state | Desired JSON support | Priority | Complexity |
| --- | --- | --- | --- | --- |
| `gc convoy list` / `status` / `check` / `stranded` | Gap | Convoy records, member beads, target/branch/routing status | P1 | Medium |
| `gc convoy create` / `add` / `target` / `close` / `delete` / `land` / `autoclose` | Later | Mutation summaries with convoy/member refs | P2 | Medium |
| `gc workflow control` / `poke` | Later | Controller/workflow response summary | P2 | Medium |
| `gc mail check` / `inbox` / `read` / `peek` / `thread` / `count` | Gap | Mail/message/thread objects for agent workflows | P1 | Medium |
| `gc mail send` / `reply` / `archive` / `mark-read` / `mark-unread` / `delete` | Later | Message/action summaries | P2 | Medium |
| `gc events` | Existing | Bounded event snapshots; streaming remains JSONL | P1 | Medium |
| `gc event emit` | Later | Emitted event seq/type summary | P2 | Low |
| `gc trace status` | Existing | Already object-ish; audit envelope/stdout purity | P1 | Low |
| `gc trace show` / `cycle` / `reasons` | Existing | Debug records; decide array vs envelope for bounded output | P1 | Medium |
| `gc trace tail` | Existing-ish | Streaming JSONL only if requested | P2 | Medium |
| `gc trace start` / `stop` | Later | Trace arm mutation summary | P2 | Low |

### Packs, Imports, Skills, Services, And Build

| Command | JSON state | Desired JSON support | Priority | Complexity |
| --- | --- | --- | --- | --- |
| `gc import list` | Gap | Imports, source refs, installed/locked state | P1 | Medium |
| `gc import add` / `remove` / `install` / `upgrade` / `migrate` | Later | Mutation summaries and lockfile refs | P2 | Medium |
| `gc pack list` | Gap | Pack sources, cache status, refs, locked commits | P1 | Low |
| `gc pack fetch` | Later | Fetch/update summary and lock refs | P2 | Medium |
| `gc pack show` / `outdated` / `registry ...` | Absent | Track as pack product work, not retrofit | P3 | Unknown |
| `gc service list` / `doctor` | Gap | Service specs, process/proxy health, check results | P1 | Medium |
| `gc service restart` | Later | Restart summary | P2 | Low |
| `gc beads health` | Gap | Provider health, backend, store path, errors | P1 | Low |
| `gc mcp list` | Gap | Catalog visibility records | P2 | Low |
| `gc skill list` / `gc skills [topic]` | Gap | Visible skills/topics | P2 | Low |
| `gc build-image` | Later | Build image result/tag/cache info | P2 | Medium |
| `gc init` / `gc prime` / `gc handoff` | Later | Useful but command-specific; keep human default | P2 | Medium |

## Appendix B: Detailed CLI Inventory

The audit summary groups related commands by product area. This inventory lists the known built-in and discovered command surface more explicitly so the sweep can be tracked without hiding subcommands inside slash-separated rows.

### City And Lifecycle

| Command | Surface | JSON plan |
| --- | --- | --- |
| `gc status` | Read/inspect | P0 JSON object with city/workspace identity, health, agents, rigs, summary |
| `gc init` | Mutation/setup | P2 setup summary after read surfaces stabilize |
| `gc start` | Mutation/lifecycle | P2 lifecycle summary |
| `gc stop` | Mutation/lifecycle | P2 lifecycle summary |
| `gc restart` | Mutation/lifecycle | P2 lifecycle summary |
| `gc suspend` | Mutation/lifecycle | P2 lifecycle summary |
| `gc resume` | Mutation/lifecycle | P2 lifecycle summary |
| `gc register` | Mutation/registry | P2 registry mutation summary |
| `gc unregister` | Mutation/registry | P2 registry mutation summary |
| `gc cities` | Read/inspect | P1 registered-city inventory |
| `gc doctor` | Read/inspect | P1 check result object |
| `gc version` | Read/inspect | P2 version/build metadata |
| `gc dashboard serve` | Server | P3 startup metadata only if needed |
| `gc prime` | Workflow/setup | P2 command-specific summary |
| `gc handoff` | Workflow/human handoff | P2 command-specific summary if used by automation |

### Config And Supervisor

| Command | Surface | JSON plan |
| --- | --- | --- |
| `gc config show` | Read/inspect | P1 resolved config object |
| `gc config explain` | Read/inspect | P1 provenance/explanation object |
| `gc supervisor status` | Read/inspect | P1 supervisor state object |
| `gc supervisor logs` | Read/inspect/logs | P2 bounded entries; streaming JSONL later |
| `gc supervisor run` | Server/internal | P3 unless supervisor automation needs startup metadata |
| `gc supervisor install` | Mutation/lifecycle | P2 install summary |
| `gc supervisor uninstall` | Mutation/lifecycle | P2 uninstall summary |
| `gc supervisor start` | Mutation/lifecycle | P2 lifecycle summary |
| `gc supervisor stop` | Mutation/lifecycle | P2 lifecycle summary |
| `gc supervisor reload` | Mutation/lifecycle | P2 lifecycle summary |

### Rigs, Agents, And Hooks

| Command | Surface | JSON plan |
| --- | --- | --- |
| `gc rig list` | Read/inspect | P0 registered rigs/repos with state and summary |
| `gc rig status` | Read/inspect | P1 rig health and agent state |
| `gc rig add` | Mutation/registry | P2 mutation summary |
| `gc rig remove` | Mutation/registry | P2 mutation summary |
| `gc rig default` | Mutation/config | P2 mutation summary |
| `gc rig suspend` | Mutation/state | P2 mutation summary |
| `gc rig resume` | Mutation/state | P2 mutation summary |
| `gc rig restart` | Mutation/state | P2 mutation summary |
| `gc agent add` | Mutation/config | P2 mutation summary |
| `gc agent suspend` | Mutation/state | P2 mutation summary |
| `gc agent resume` | Mutation/state | P2 mutation summary |
| `gc hook` | Hook/script | P1 stdout-purity and hook contract audit |
| `gc bd` | Passthrough | P3 wrapper metadata only if `gc` adds value |

### Sessions, Waits, Runtime, And Nudges

| Command | Surface | JSON plan |
| --- | --- | --- |
| `gc session list` | Read/inspect | P0 durable session inventory |
| `gc session peek` | Read/inspect | P1 bounded output object |
| `gc session logs` | Read/inspect/logs | P1 bounded entries; streaming JSONL later |
| `gc session new` | Mutation/create | P2 created-session summary |
| `gc session submit` | Mutation/dispatch | P2 delivery summary |
| `gc session attach` | Interactive | P3 unless a noninteractive mode is added |
| `gc session suspend` | Mutation/state | P2 mutation summary |
| `gc session close` | Mutation/state | P2 mutation summary |
| `gc session rename` | Mutation/metadata | P2 mutation summary |
| `gc session reset` | Mutation/state | P2 mutation summary |
| `gc session prune` | Mutation/cleanup | P2 mutation summary |
| `gc session kill` | Mutation/state | P2 mutation summary |
| `gc session wake` | Mutation/state | P2 mutation summary |
| `gc session nudge` | Mutation/delivery | P2 delivery summary |
| `gc session wait` | Wait/control | P2 wait registration/update summary |
| `gc wait list` | Read/inspect | P1 wait inventory |
| `gc wait inspect` | Read/inspect | P1 wait detail object |
| `gc wait ready` | Read/inspect | P1 readiness result object |
| `gc wait cancel` | Mutation/state | P2 cancellation summary |
| `gc runtime drain-check` | Read/inspect | P1 drain state |
| `gc runtime drain-ack` | Read/inspect/mutation | P1/P2 ack result object |
| `gc runtime drain` | Mutation/state | P2 mutation summary |
| `gc runtime undrain` | Mutation/state | P2 mutation summary |
| `gc runtime request-restart` | Mutation/state | P2 mutation summary |
| `gc nudge status` | Read/inspect | P1 nudge state |
| `gc nudge drain` | Mutation/state | P2 mutation summary |
| `gc nudge poll` | Read/control | P1/P2 poll result object |

### Dispatch, Orders, Formulas, And Convergence

| Command | Surface | JSON plan |
| --- | --- | --- |
| `gc sling` | Mutation/dispatch | P0 dispatch result with created/routed refs |
| `gc order list` | Read/inspect | P1 order inventory |
| `gc order show` | Read/inspect | P1 order detail |
| `gc order history` | Read/inspect | P1 history entries |
| `gc order check` | Read/evaluate | P1 gate evaluation result |
| `gc order run` | Mutation/dispatch | P2 run summary |
| `gc formula list` | Read/inspect | P1 formula inventory |
| `gc formula show` | Read/inspect | P1 compiled formula detail |
| `gc formula cook` | Mutation/create | P2 created refs summary |
| `gc converge list` | Read/inspect | P1 normalize existing JSON |
| `gc converge status` | Read/inspect | P1 normalize existing JSON |
| `gc converge create` | Mutation/create | P2 mutation summary |
| `gc converge approve` | Mutation/state | P2 mutation summary |
| `gc converge iterate` | Mutation/state | P2 mutation summary |
| `gc converge stop` | Mutation/state | P2 mutation summary |
| `gc converge retry` | Mutation/state | P2 mutation summary |
| `gc converge test-gate` | Read/evaluate | P1/P2 gate evaluation result |
| `gc graph` | Read/inspect | P1 nodes/edges object |

### Convoys And Workflow Control

| Command | Surface | JSON plan |
| --- | --- | --- |
| `gc convoy list` | Read/inspect | P1 convoy inventory |
| `gc convoy status` | Read/inspect | P1 convoy detail |
| `gc convoy check` | Read/evaluate | P1 check result object |
| `gc convoy stranded` | Read/inspect | P1 stranded convoy/member inventory |
| `gc convoy create` | Mutation/create | P2 created-convoy summary |
| `gc convoy add` | Mutation/state | P2 member-add summary |
| `gc convoy target` | Mutation/config | P2 target update summary |
| `gc convoy close` | Mutation/state | P2 close summary |
| `gc convoy land` | Mutation/workflow | P2 land summary |
| `gc convoy autoclose` | Mutation/workflow | P2 autoclose summary |
| `gc workflow control` | Workflow/control | P2 control response summary |
| `gc workflow poke` | Workflow/control | P2 poke response summary |
| `gc workflow delete` | Workflow/control | P2 delete response summary if exposed |

### Mail, Events, And Trace

| Command | Surface | JSON plan |
| --- | --- | --- |
| `gc mail inbox` | Read/inspect | P1 inbox records |
| `gc mail read` | Read/inspect | P1 message detail |
| `gc mail peek` | Read/inspect | P1 message preview |
| `gc mail thread` | Read/inspect | P1 thread detail |
| `gc mail count` | Read/inspect | P1 count object |
| `gc mail check` | Read/control | P1 check result object |
| `gc mail send` | Mutation/send | P2 send summary |
| `gc mail reply` | Mutation/send | P2 reply summary |
| `gc mail archive` | Mutation/state | P2 archive summary |
| `gc mail mark-read` | Mutation/state | P2 mutation summary |
| `gc mail mark-unread` | Mutation/state | P2 mutation summary |
| `gc mail delete` | Mutation/state | P2 delete summary |
| `gc events` | Read/stream | P1 bounded snapshot; streaming JSONL |
| `gc event emit` | Mutation/event | P2 emitted-event summary |
| `gc trace status` | Read/inspect | P1 normalize existing JSON |
| `gc trace show` | Read/inspect | P1 bounded trace object |
| `gc trace cycle` | Read/inspect | P1 trace cycle object |
| `gc trace reasons` | Read/inspect | P1 reasons object |
| `gc trace tail` | Stream | P2 JSONL stream contract |
| `gc trace start` | Mutation/state | P2 mutation summary |
| `gc trace stop` | Mutation/state | P2 mutation summary |

### Packs, Imports, Services, Skills, And Build

| Command | Surface | JSON plan |
| --- | --- | --- |
| `gc import list` | Read/inspect | P1 import inventory |
| `gc import add` | Mutation/config | P2 mutation summary |
| `gc import remove` | Mutation/config | P2 mutation summary |
| `gc import install` | Mutation/install | P2 install summary |
| `gc import upgrade` | Mutation/install | P2 upgrade summary |
| `gc import migrate` | Mutation/migration | P2 migration summary |
| `gc pack list` | Read/inspect | P1 pack inventory |
| `gc pack show` | Planned/absent | P3 until pack product surface exists |
| `gc pack outdated` | Planned/absent | P3 until pack product surface exists |
| `gc pack registry list` | Planned/absent | P3 until registry product surface exists |
| `gc pack registry search` | Planned/absent | P3 until registry product surface exists |
| `gc pack registry show` | Planned/absent | P3 until registry product surface exists |
| `gc pack fetch` | Mutation/network | P2 fetch summary |
| `gc service list` | Read/inspect | P1 service inventory |
| `gc service doctor` | Read/inspect | P1 service check result |
| `gc service restart` | Mutation/lifecycle | P2 restart summary |
| `gc beads health` | Read/inspect | P1 provider/store health object |
| `gc mcp list` | Read/inspect | P2 catalog records |
| `gc skill list` | Read/inspect | P2 skill records |
| `gc skills [topic]` | Read/inspect | P2 skill/topic records |
| `gc build-image` | Mutation/build | P2 build summary |
| `gc <pack-defined command>` | Pack extension | P3 until command declares JSON capability |
