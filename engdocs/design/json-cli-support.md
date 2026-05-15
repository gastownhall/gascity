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
- Some existing JSON surfaces need an envelope, schema version, or stdout-purity audit.
- Streaming commands need an explicit JSONL convention distinct from bounded JSON snapshots.
- Mutation commands need structured summaries, but those should follow after the read/inspect conventions are stable.
- Pack-defined commands need an extension contract rather than assuming `gc` can retrofit arbitrary external command output.

The most important immediate rule is stdout purity: when `--json` is passed, stdout should contain only the intended JSON payload. Diagnostics may continue to use stderr unless and until a separate structured error contract is introduced.

## Design Goals

- Preserve current human-readable output by default.
- Make `--json` deterministic enough for agents, scripts, tests, and UI/API integrations.
- Prefer one top-level JSON object with `schema_version` for newly touched bounded commands.
- Keep stderr available for diagnostics and unexpected errors.
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

## JSON Output Contract

- JSON commands should prefer one top-level object, not a bare array. Use collection fields such as `orders`, `sessions`, or `events`, plus `summary`.
- Include `schema_version` as a string, starting at `"1"`, for new or newly touched JSON surfaces.
- Keep human output as the default. `--json` changes stdout only; diagnostics and unexpected errors still go to stderr with nonzero exit.
- In `--json` mode, stdout must contain only the intentional JSON payload.
- Human stdout produced by shared helpers should be suppressed or routed to `io.Discard` in JSON mode, then the command should emit exactly one JSON document at the end.
- Do not buffer incidental human stdout and include it in a JSON field. If output is meaningful to machines, model it as first-class JSON fields. Raw text belongs in JSON only when it is the command's product contract, for example a future `gc session peek --json` `output` field.
- For first wave, do not introduce structured JSON errors globally. Add that as a separate root-level design because Cobra error paths are cross-cutting.
- Warnings should eventually be represented as `warnings: [{code,message,field,path}]` while still emitting important diagnostics to stderr when compatible.
- Partial/stale/offline data should use explicit booleans and nullable detail fields, for example `available`, `stale`, `source`, `reason`.
- Timestamps should be RFC3339 strings.
- Use consistent field names: `id`, `name`, `qualified_name`, `scoped_name`, `path`, `source`, `ref`, `status`, `state`, `type`, `target`, `created_at`, `updated_at`.
- `--json` should ignore human formatting knobs such as `--quiet` unless a command documents a machine-readable terse mode separately.
- Streaming commands should use JSONL for streams and object JSON for bounded snapshots.

## Extensibility

New built-in CLI commands should pick up JSON support through a shared local pattern rather than inventing command-specific output plumbing.

Recommended built-in command pattern:

- Add a local `--json` flag for read/inspect commands by default.
- Route human stdout through a writer that becomes `io.Discard` in JSON mode.
- Write the final JSON payload through the real stdout writer exactly once.
- Use a top-level object with `schema_version`.
- Put meaningful machine data in typed fields, not in copied human prose.
- Add a stdout-purity test for every command touched: successful `--json` output must parse as JSON and must not include human banners, progress lines, or summaries.
- Keep stderr available for diagnostics until structured JSON errors are standardized.

In a later contract/helper PR, this should become a small shared helper layer so new commands can follow the same shape with very little ceremony:

- `jsonStdout(jsonMode, stdout)` or equivalent, returning `io.Discard` for human-output paths in JSON mode.
- `writeJSON(stdout, payload)` for final payload emission.
- shared warning/error structs.
- test helpers that assert JSON-only stdout.

## Pack-Defined Commands

Pack-defined commands can be arbitrary scripts or external programs, so `gc` should not assume it can automatically make them JSON-safe. Instead, pack commands should declare JSON capability and then honor the same stdout contract.

Proposed pack command contract:

- Pack commands may declare whether they support JSON output.
- If a pack command declares JSON support, `gc <pack-command> --json` should pass the JSON request through to the command.
- If a pack command does not declare JSON support, `gc <pack-command> --json` should fail clearly rather than falling back to human output.
- A JSON-capable pack command owns stdout purity for its command-specific output.
- `gc` may add wrapper metadata later, but the first contract should avoid surprising transformations of arbitrary pack command stdout.

Possible future metadata shape:

```toml
[command.capabilities]
json = true
```

or:

```toml
[command.output]
json = true
json_schema = "pack.command.v1"
```

Open design questions for pack-defined commands:

- Should `gc` reserve `--json` globally for pack commands, or only pass it through when declared?
- Should `gc` validate pack-command JSON stdout, or trust the command and leave validation to tests?
- Should pack commands emit their own schema version, or should `gc` eventually wrap them in a common envelope?

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
- Add focused tests for parseable JSON and stdout purity.
- Keep all current human output as the default.

Stage 2: high-value inspection surfaces.

- Add `--json` for `gc rig status`, `gc session peek`, `gc session logs`, `gc formula list`, `gc formula show`, `gc order list`, `gc order show`, `gc order history`, `gc config show`, and `gc config explain`.
- Normalize existing JSON for `gc events`, `gc trace status`, `gc trace show`, and `gc converge status/list`.

Stage 3: mutation summaries.

- Add JSON summaries for lifecycle and dispatch mutations after envelopes are stable.
- Prioritize commands that create refs or change durable state: session creation, order runs, formula cooks, convoy actions, rig mutations, and supervisor/runtime state changes.

Stage 4: pack and extension contract.

- Define pack command metadata for JSON capability.
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
   Acceptance: each emits `schema_version`, retains existing useful fields, and has backward-compatible tests documenting any intentional shape change.

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

7. Define error/warning policy.
   Acceptance: one documented CLI-wide policy for stderr, exit codes, JSON error objects, and partial/stale data.

8. Audit pack and registry commands after pack surface stabilizes.
   Acceptance: document which commands exist, which are planned, and which should support JSON in the first pack-focused wave.

9. Design workflow run/status/result JSON from the start.
   Acceptance: proposed command surfaces follow these conventions before implementation.

## Product Decisions Needed

- Are schema changes allowed for existing `--json` commands that currently emit arrays, or should they stay stable until a v2 flag or compatibility window?
- Should structured JSON errors be CLI-wide for `--json`, or remain command-local for now?
- Should `gc trace show --json` and `gc events --json` remain arrays for easy piping, or move to object envelopes in bounded modes?
- Should mutation commands join the second wave with JSON summaries, or stay human-only until read surfaces are complete?
- For pack-defined commands, should `gc` reserve `--json` globally or only pass it through when a command declares JSON support?

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
