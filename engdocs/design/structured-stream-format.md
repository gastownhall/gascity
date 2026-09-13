---
title: "Structured Session Stream and Transcript Format (`format=structured`)"
---

| Field | Value |
|---|---|
| Status | Accepted / Phase 1 partially implemented |
| Date | 2026-06-20 |
| Author(s) | Claude, Codex |
| Issue | N/A |
| Supersedes | N/A |

## Summary

The supervisor session stream and transcript endpoints accept two requested
formats today: `conversation` (the default) and `raw`. The transcript/peek path
can also return `format: "text"` when no transcript exists yet and pane output
is the only observable source; that is a fallback response shape, not a third
requested query format. `conversation` is **lossy by design** — it flattens
every provider's content blocks into a single `outputTurn.Text` string,
replacing a tool call with `[name]`, truncating a tool result to 500
characters, and redacting thinking to the literal `[thinking]`
(`internal/api/handler_agent_output_turns.go:34-49,81-94`). `raw` is the
opposite: provider-native JSON forwarded as `SessionRawMessageFrame` values.
The raw contract is full-fidelity pass-through for valid provider frames; the
current implementation has one transport-validity caveat, escaping malformed
literal control characters inside JSON strings before emission so SSE never
sends invalid JSON. The enclosing stream/transcript envelope carries only a
`provider` identifier — so **every consumer must re-implement per-provider
frame parsing** to recover structure
(`internal/api/session_frame_types.go:26-150`;
`internal/api/supervisor_city_routes.go:13-21`).

The result is that rich structured consumers cannot be built on the supervisor
API without each client owning a fragile, untested, client-side normalization
layer for 17 built-in profiles spanning fewer provider transcript families.
This is precisely what MC does today — its server reads `format=raw` and
reconstructs full fidelity (tool inputs, structured tool results, thinking,
usage, subagent nesting) before its clients consume the transcript. That work
is duplicated in every consumer that wants rich presentation or programmatic
access, and it lives one layer removed from the provider knowledge that already
exists *inside* Gas City.

This design adds a third requested format, **`format=structured`**, that emits
Gas City's already-parsed content blocks as a typed, versioned schema — text,
thinking (with signature when available and allowed), tool calls (with full
input), tool results (with structured Bash/Grep/Read shapes where GC can derive
them), interactions, usage, model, and stop reason. Inline-subagent
relationships remain part of the north star, but are deliberately excluded
from `session.structured.v1`; `ga-mb46n3` owns a later version. The
per-provider knowledge stays in **one tested place near the source**
(`internal/sessionlog` / `internal/worker`), and **any** consumer — a chat UI,
the dashboard, an external client — gets a rich, provider-agnostic event model
without duplicating provider parsers.

The north-star goal, and the primary success criterion for this design, is a
**rich, complete, unified, provider-neutral, structured, typed stream of
data** that exposes 100% of the semantically available data from every
supported provider without encoding UI decisions. A client that requests
`format=structured` must be able to consume the data programmatically, render a
rich GUI comparable to MC, build a CLI or analytics view, or
persist its own normalized model without falling back to provider-specific
parsing.

Completeness does **not** mean shipping provider-native JSON objects,
provider-native field names, or display HTML on the structured wire. The
provider-native evidence remains available through `format=raw`; the
structured contract is the typed normalization target. If MC or a
provider-specific adapter can recover a useful fact from raw provider data,
Gas City should expose the fact as provider-neutral typed data, not as HTML,
anonymous JSON, or provider-shaped escape hatches. When a provider exposes a
fact that the current schema cannot represent, Gas City must add or extend
typed provider-neutral fields/result families and tests rather than dropping
the fact, hiding it in an untyped map, or pushing native provider shape to the
client.

The headline finding from the cross-codebase and provider-format audit behind
this spec: Gas City now parses provider frames into typed blocks for all 17
built-in provider profiles when a supported native transcript or configured
capture source is available. The dominant gap is not a lack of a structured
wire shape — it is that Gas City's API flattens the structure it already has,
and several providers need either modernized readers, managed capture, or
fixture hardening because their public persisted transcript is not a rich
complete trace. One cross-cutting carrier/schema change therefore unlocks most
fidelity immediately; targeted provider adapters close the remainder.
This is **transcript-schema parity**, not end-to-end provider parity: provider
resume, hooks, MCP projection, skills, and runtime behavior remain governed by
their own provider-specific designs.

## Background: what already exists

### Provider frames are already parsed per family

`ReadProviderFile` dispatches by `ProviderFamily` to a dedicated reader per
vendor (`internal/sessionlog/reader.go`): `ReadAuggieFile`, `ReadAmpFile`,
`ReadCodexFile`, `ReadCopilotFile`, `ReadGeminiFile`, `ReadGrokFile`,
`ReadKimiFile`, `ReadKiroFile`, `ReadMimoCodeFile`, `ReadOpenCodeFile`,
`ReadPiFile`, `ReadAntigravityFile`, and the default Claude JSONL DAG reader
(`ReadFile`). Each one translates that vendor's native transcript into the
common `sessionlog.Entry` / `ContentBlock` shape
(`internal/sessionlog/entry.go:60-77`) **and** preserves the original
provider-frame bytes in `Entry.Raw` for raw pass-through. Valid raw frames are
byte-preserved by the API; malformed string control characters are escaped as a
transport-validity repair before emission.

This is the per-provider translation layer the structured format needs. It
is not greenfield.

### The fidelity is parsed, then discarded or only partially carried

`sessionlog.ContentBlock` already retains full tool input
(`Input json.RawMessage`, `entry.go:73`), full tool-result content
(`Content json.RawMessage`, `entry.go:75`), `IsError` (`entry.go:76`), text,
and structured interaction fields (`entry.go:64-71`). The normalized
`worker.HistoryBlock` mirrors the core block shape and has a first-class
`HistoryInteraction` pointer plus initial `StructuredInput` /
`StructuredResult` carriers for provider-neutral tool data
(`internal/worker/types.go`). The structure now survives through the worker
history path used by `format=structured`, but legacy `conversation` output
still deliberately throws it away into a flat string
(`internal/api/handler_agent_output_turns.go:26-54,73-101`). The remaining
structured-format work is to expand those carriers and push every
provider-specific extraction rule into provider/sessionlog normalization rather
than reintroducing inference at the API edge.

What the typed model still needs to complete:

- **complete thinking provider coverage** — `ContentBlock` and `HistoryBlock`
  now carry canonical thinking text plus a provider-neutral `signature` marker,
  and `format=structured` gates both thinking text and its signature behind
  `include_thinking=true`.
  Remaining work is to normalize every provider's thinking/reasoning dialect
  into that carrier, including encrypted or signature-only reasoning evidence
  where available.
- **complete usage / model / stop_reason coverage** — the normalized worker
  history entry now carries `model`, `stop_reason`, and typed token usage when
  those facts are present on the provider message or preserved raw frame. Claude,
  Pi-family, Gemini-style top-level model fields, and similar direct evidence can
  flow to `format=structured`. Codex per-invocation `token_count` events are
  parsed through `ExtractCodexTailUsage` and correlated to the nearest visible
  assistant message so `model`, token usage, and reported context window flow
  through the same provider-neutral structured usage fields.
- **complete structured tool-result families** — an initial worker carrier
  exists for
  command/stdin/read/grep/glob/edit/search/fetch/todo/plan/question/task/text data,
  including typed patch hunks, todo before/after lists, plan text/steps,
  question answer maps, and task/subprocess status/output, but not every
  provider-specific result family from the MC audit has a first-class
  provider-neutral DTO yet.
- **inline subagent nesting is outside v1** — subagent mappings and transcript
  reads exist as separate worker/API surfaces (`AgentMappings`,
  `AgentTranscript`), but `session.structured.v1` deliberately has no reserved
  lineage fields. A later version may add lineage after real provider evidence
  establishes stable parent/child identifiers and transcript joins
  (`ga-mb46n3`). These are provider inline subagents, not Gas City formula/drain
  fanout; drain correlation stays in beads, convoys, formulas, and events.

### Provider coverage contract

`format=structured` is a provider-neutral projection over all 17 built-in
provider profiles. The source adapter may read a native transcript, an official
export, a live NDJSON stream, an ACP event log, or a Gas City managed capture
file; the structured wire shape is the same either way. If a provider's native
local transcript omits tool output or file changes, Gas City must capture those
facts through that provider's hook/stream/ACP surface before claiming rich
structured coverage. It must not forward native provider JSON as a shortcut.

Builtin provider profiles live in `internal/worker/builtin/profiles.go`.
The current adapter/capture target is:

| Profile | Structured source | Expected v1 fidelity |
|---|---|---|
| claude | Claude JSONL under `~/.claude/projects/...` | rich messages, tool calls/results, thinking placeholder/text when allowed, raw only for native frames |
| codex | Codex rollout JSONL / `--json` event stream | rich tool calls/results; command and patch events normalize to Bash/Edit where derivable |
| gemini | Gemini session records | rich tool calls/results, thoughts, token usage; reader must support current JSONL session format |
| kimi | Kimi Code `agents/main/wire.jsonl` (and legacy context logs while supported) | rich main-agent and subagent messages, tool calls/results |
| opencode | OpenCode export/mirror JSON | rich tool calls/results and interactions from message parts |
| mimocode | MiMo Code export/session database mirror using the OpenCode-compatible shape | rich tool calls/results through the MiMo/OpenCode adapter |
| groq | OpenCode-backed Gas City profile | same as OpenCode; no Groq-specific transcript dialect |
| cerebras | OpenCode-backed Gas City profile | same as OpenCode; no Cerebras-specific transcript dialect |
| pi | Pi JSONL under `~/.pi/agent/sessions` | rich messages, tool calls/results, Bash/Python execution where present |
| omp | Oh My Pi JSONL under `~/.omp/agent/sessions` | same Pi-family adapter, including Bash/Python execution records |
| antigravity | Antigravity transcript JSONL / brain artifacts | rich messages, tool calls/results, interactions, artifacts where exposed |
| copilot | Copilot CLI session-state event log | rich for messages, `tool.execution_*`, command output, errors, and result-side edit diffs when present |
| kiro | Kiro ACP JSONL event log under `~/.kiro/sessions/cli` | rich through ACP `session/update` events (`ToolCall`, `ToolCallUpdate`, `TurnEnd`); DB-only chat export needs sampling before use |
| cursor | Cursor stream JSON plus Gas City hook capture | stream/hook captures normalize messages, tool calls/results, command output, read/write evidence, and result events; native local JSONL omits tool outputs and cannot provide complete rich results by itself |
| amp | `amp --execute --stream-json` live capture | rich for execute/headless flows; retrospective local thread discovery is not a public stable source |
| grok | captured Grok ACP JSONL (`session/update`) | rich for ACP capture; `--output-format streaming-json` and persisted `~/.grok/sessions` need fixture/schema validation before use |
| auggie | captured Auggie ACP JSONL (`session/update`) | rich for configured ACP capture; saved-session `chatHistory` / node graph, SDK capture, and hook capture need fixture/schema validation before use |

Practical consequence: the carrier + stop-flatten change immediately lights up
the provider-normalized transcript families for all 17 built-in profiles where
GC has either a native transcript reader or configured capture. Remaining gaps
require provider-reader hardening, managed capture, and real-provider fixtures,
not client-side provider parsers.

## The streaming ceiling (read before scoping "streaming")

Both Gas City and MC operate on **whole committed
frames**, not token/sub-frame deltas:

- Claude Code writes whole JSONL message lines (a `tool_use` block is
  committed with its `input` already complete; the `tool_result` arrives as
  one later whole line). There are no `content_block_delta` /
  `input_json_delta` frames in the transcript.
- Codex writes a whole `function_call_output` per result; its
  `exec_command_begin/end` progress frames are dropped today in
  `skipCodexEventMsgType` (`internal/sessionlog/codex_reader.go:315-327`).
- MC's streaming-delta assemblers exist but are **bypassed** in
  production — they consume the same whole frames.

Gas City's session stream already delivers these frames **live as they
commit**, keyed by a cursor. So "streaming tool results" realistically
means: *the tool call appears live, then its complete result appears live* —
not bytes accumulating character-by-character. True sub-frame streaming
would require tapping the provider's live API wire, which is outside the
transcript-reader model.

This spec therefore defines streaming in two phases (per direction set in
review):

- **Phase 1 — frame-granular streaming.** Structured blocks delivered live
  as each transcript frame commits. This matches the best fidelity MC ships
  today and is the v1 target.
- **Phase 2 — sub-frame streaming.** Token/delta-level streaming by tapping
  providers' live API output. A materially larger effort, scoped as future
  work, not blocking Phase 1.

One asymmetry to note: for **Gemini**, the GC reader reads the *whole file*
via `os.ReadFile` (`internal/sessionlog/gemini_reader.go:19`). Existing stream
machinery can re-read a valid whole-file snapshot on change and emit new entries
by cursor, but a tolerant/incremental Gemini parser is still needed for robust
frame-granular behavior while the provider file is being written. That is a
Phase 1 hardening task for Gemini specifically.

## Goals

- A third session-stream/transcript format, `format=structured`, emitting a
  typed, versioned, provider-agnostic message + block schema.
- A complete typed normalization target for all 17 built-in provider profiles:
  every provider fact that is present, derivable, or capturable must land in a
  provider-neutral typed field/result family rather than being flattened,
  ignored, hidden in untyped JSON, or exposed as provider-native shape.
- Phase 1 rich support prioritizes the provider families currently under active
  structured-session validation: Claude, Codex, Gemini, OpenCode-compatible
  providers, Pi-family providers, Cursor stream/hook capture, Amp stream-json
  capture, Grok ACP capture, and Auggie ACP capture. Every built-in profile uses
  the same typed envelope and gracefully downgrades to provider-neutral fallback
  data where provider-native or managed-capture evidence is incomplete.
- Highest available fidelity for tool inputs, **structured tool results**, file
  edits/diffs, thinking + signature, usage, model, stop-reason, artifacts,
  interactions, and any future provider data family that clients may need for
  display or programmatic use. Inline-subagent relationships remain a
  north-star goal, but v1 deliberately reserves no lineage fields;
  `ga-mb46n3` owns a later version.
- 100% of provider data that is present, derivable, or capturable in a
  provider run is represented as typed provider-neutral data in
  `format=structured`; provider-native bytes remain a separate `format=raw`
  escape hatch for audit/debug use only.
- Zero client-side per-provider enrichment required to build MC-parity
  presentation, build an alternate GUI/CLI, or consume the transcript
  programmatically.
- Provider-specific parsing/capture remains inside Gas City adapter code. A
  client asking for `format=structured` must never need to know a provider's
  native transcript dialect.
- No UI-specific payloads in the structured contract: no HTML, pre-rendered
  markdown, syntax-highlighted fragments, dashboard-only display blobs, or
  other presentation encodings.
- Existing additional families and aliases (`kimi`, `mimocode`, `groq`,
  `cerebras`, `pi`, `omp`, `antigravity`, `copilot`, `kiro`, `amp`) gain useful
  structured output without inventing new wire contracts.
- A golden-fixture regression guard so server-side translation is safe
  across provider/version drift.

## Non-goals

- Removing or changing `conversation` / `raw` (both remain; back-compat).
- Sub-frame/token streaming (Phase 2, documented but not built here).
- Passing provider-native JSON, provider-native metadata maps, or
  provider-specific field names through `format=structured`. That remains the
  purpose of `format=raw`.
- Presentation concerns — markdown→HTML, syntax highlighting, diff
  rendering, visual tool_use/result pairing. These stay in the consumer;
  `format=structured` ships *data*, not HTML.

## Tool result normalization contract

MC proves the rich-data surface is possible today by reading Gas
City's `format=raw`, parsing provider frames, and then adding display-ready
HTML and other augment fields before the browser renders the transcript. Gas
City should port the **data extraction algorithms**, not the presentation
output. Provider-specific raw frames are normalized once, inside Gas City's
provider/sessionlog layer, into typed provider-neutral tool inputs and tool
results. API handlers then project those typed values to OpenAPI/Huma wire
types. Clients may render HTML, colorized diffs, tables, or programmatic views
from the data, but `format=structured` itself must not ship HTML,
provider-native objects, anonymous JSON maps, or provider-specific field names.

### Ownership boundary

The normalization owner is the provider adapter path:

- `internal/sessionlog/*_reader.go` maps provider transcript frames into common
  content blocks and typed structured tool data.
- `internal/worker` carries the normalized data as part of history snapshots.
- `internal/api` only projects normalized history into Huma-registered wire
  structs.
- `cmd/gc/dashboard` renders the typed data but does not parse provider-native
  transcript formats.

The current implementation is partially aligned: the API projection consumes
worker-carried typed structured input/result data, and the structured wire
tests forbid native provider keys. `internal/api/session_structured_types.go`
no longer infers tool semantics from provider-native JSON; if a worker history
block lacks `StructuredInput` or `StructuredResult`, the structured API leaves
that typed field empty instead of re-parsing native data. The remaining
alignment work is to keep expanding provider/sessionlog/worker normalization
until every supported provider's rich data is represented before it reaches
the API edge. The end state is that provider-native field names appear only in
provider adapters and `format=raw` fixtures, never in structured wire DTOs,
API inference, or dashboard parsing code.

### Non-negotiable wire rules

- `format=structured` returns typed provider-neutral data, not display HTML.
- Provider-specific fields such as `toolUseResult`, `resultDisplay`,
  `_structuredPatch`, `_diffHtml`, `_highlightedContentHtml`, `call_id`,
  `tool_use_id`, or raw SDK result objects must not cross the structured API
  boundary.
- Rich edit results expose typed patch hunks, file paths, result-side old/new
  text, original-file text, and provider-reported edit flags such as
  replace-all and user-modified state when present. A flat patch string may
  remain as a convenience field while clients migrate, but it is not the only
  rich edit representation.
- Result-side edit evidence comes from provider result data: structured result
  fields, result displays, patch-apply events, or textual result content that
  is itself a patch. GC must not fabricate an edit result from a tool input and
  claim the provider reported it.
- Long-tail providers must degrade gracefully to existing typed neutral fields.
  Unknown result data may use `kind: "text"`. Search inputs may use the typed
  `query`, `pattern`, `file_path`, and `command` fields plus scalar `path`
  entries in `arguments`; unknown argument names, nested values, and arbitrary
  provider JSON remain available only through `format=raw`.
- Serialization/deserialization stays at the edges. Internal code should carry
  typed Go structs, and Huma/OpenAPI generated types are the wire source of
  truth.

### MC algorithm inventory

MC has two relevant layers:

1. `server/src/services/gc_message_format/*` translates raw provider frames
   into richer app blocks.
2. `server/src/services/render_enrichment/*` adds display-oriented augments
   such as HTML and syntax-highlighted snippets.

Only the data algorithms belong in Gas City structured normalization.

| MC area | Algorithm to port to GC data | GC target |
|---|---|---|
| Codex tool invocation | Canonicalize names (`apply_patch` → edit, shell commands → bash, web search aliases), parse JSON-string arguments, preserve call context for results. | `internal/sessionlog/codex_reader.go` |
| Codex shell classification | Reclassify `cat`, `sed -n`, `nl -ba ... \| sed -n`, `rg`, and `grep` shell commands into provider-neutral Read/Grep inputs, after unwrapping shell launchers such as `/usr/bin/env bash -lc "..."`, `/bin/bash -lc "..."`, and nested variants. | Codex reader/sessionlog normalization |
| Codex output normalization | Strip `Output:` wrappers, parse nested JSON-string outputs, extract stdout/stderr/exit code/error state, and avoid treating no-match grep as a provider failure. | Codex reader/sessionlog normalization |
| Codex event filtering and errors | Preserve user/assistant/reasoning/error events as provider-neutral messages, normalize error/stream-error/turn-aborted events into typed `system_event` data, and skip unknown provider event frames such as shutdown/diagnostic events instead of exposing native `event_msg` records on the structured wire. | Codex reader/sessionlog normalization |
| Codex Read result | Strip `nl -ba` line number prefixes, compute `start_line`, `num_lines`, and `total_lines`. | typed Read result |
| Codex Grep result | Distinguish `mode: "content"`, `mode: "files_with_matches"`, and `mode: "count"`; expose filenames, counts, and matched content. | typed Grep result |
| Codex image blocks | Preserve user `input_image` evidence as provider-neutral image blocks with file path, external image URL, and MIME type; keep inline image bytes out of `format=structured`. | typed Image block |
| Patch parsing | Parse raw `*** Begin Patch` / unified diff text into `{file_path, old_start, old_lines, new_start, new_lines, lines}` hunks, including multi-file patches. | typed Edit result |
| Edit diff computation | When a provider result contains old/new/original edit fields or replace-all/user-modified flags, expose those facts as typed data and compute patch hunks from result-side old/new text when no provider hunk exists. Do not expose MC's HTML. | provider-specific Edit result normalizer |
| Claude SDK tool results | Normalize Bash, Read, Edit, Write, Glob, Grep, TodoWrite, WebSearch, WebFetch, BashOutput, KillShell, TaskOutput, AskUserQuestion, and plan-mode result schemas. | Claude reader/sessionlog normalization |
| Async shell stdin correlation | Link `WriteStdin` / stdin tools back to the parent shell command by normalizing provider shell/session identifiers to `task_id` and attaching the parent command as typed `linked_command` input data. | typed Stdin input/result |
| Search result items | Flatten provider result arrays such as MC's nested WebSearch result `content` items into provider-neutral `{ title, url, snippet? }` items; parse URL-prefixed search output only when the tool context is a web query, not grep. | typed Search result `result_items` |
| Gemini result display | Translate `resultDisplay.fileDiff`, file path, original content, and new content into typed edit/write data. | Gemini reader/sessionlog normalization |
| Tool error classification | Classify explicit tool failures into provider-neutral categories such as user rejection, command failure, file error, validation error, timeout, network error, and unknown; expose cleaned messages and user rejection reasons as typed data, not UI badges. | typed `StructuredToolError` |
| User prompt metadata | Extract opened IDE files, IDE selections, uploaded-file sections, and cleaned prompt text from user messages so clients do not parse prompt boilerplate. | typed `StructuredUserPrompt` |
| Markdown/code rendering | Preserve source text, file path, language/truncation hints when derivable; do not pre-render HTML. | typed `language` / `truncated` data only; dashboard owns rendering |

### Typed result families

The first structured result families are sketched in the schema section. To
reach MC richness without HTML, GC needs these provider-neutral DTOs over time:

- **Command**: `stdout`, `stderr`, `exit_code`, `interrupted`, `truncated`,
  `is_image`, command text, optional shell/task identifiers, status, stream
  line counts, and timestamp when providers expose async shell output polling.
  Shell-control results such as kill/stop messages expose the shell identifier
  as neutral `task_id` and the provider result message as `stdout`/`content`;
  they do not leak provider-native shell-control field names.
- **Stdin**: async shell input tools expose the provider-neutral shell
  identifier as `task_id`, the submitted stdin text as `text`, and the parent
  shell command as `linked_command` when it can be derived from an earlier Bash
  result. Provider-native identifiers such as `sessionId` and `shellId` stay in
  `format=raw`.
- **Tool errors**: failed tool results carry provider-neutral error metadata:
  `category`, cleaned `message`, and optional `user_reason`.
  Categories are `user_rejection`, `user_rejection_with_reason`,
  `command_failure`, `file_error`, `validation_error`, `timeout`,
  `network_error`, and `unknown`. This ports MC's data classification
  algorithm without shipping MC's UI rendering.
- **System events**: provider transcript events such as provider errors,
  stream errors, and turn-aborted notices carry typed `system_event` metadata
  with neutral `kind`, `category`, optional semantic `code`, and cleaned
  `message`. Provider-native frame names and fields such as `event_msg` and
  `codex_error_info` stay in `format=raw`.
- **User prompts**: user messages carry cleaned prompt text, opened IDE file
  paths, uploaded-file metadata, and IDE selection text as typed
  `user_prompt` data. Raw provider/user text remains available through the
  text block and `format=raw`; clients that want MC-style display can use the
  typed prompt metadata without parsing IDE tags or upload sections.
- **Read/Write**: `file_path`, `content`, `language`, `num_lines`,
  `start_line`, `total_lines`, optional result-side `patch` / `patch_hunks`
  for providers that report write diffs, and optional image metadata where
  providers return images. Write result patches still come only from
  result-side evidence, never from the write input text.
- **Edit/Patch**: operation type, `file_path`, `file_paths`, optional raw
  neutral patch text, `patch_hunks`, `old_string`, `new_string`,
  `original_file`, `replace_all`, and `user_modified` when the provider result
  reports them.
- **Grep/Glob**: mode/pattern where applicable, filenames, counts, matched
  content, applied limit, `duration_ms`, and truncation state when present.
- **Search/Fetch**: query/url, result items, `status_code`, `status_text`,
  bytes, `duration_ms`, and response content where present.
- **Todo**: todo input lists plus result-side `old_todos` / `new_todos`
  before/after lists.
- **Plan**: plan text, explanation text, and provider-neutral plan step lists.
- **Question**: full question arrays, headers, option labels/descriptions,
  multi-select state, selected answer, and answer maps.
- **Task/Subprocess output**: task id/type/status/description/output/exit code.
  Bash results that launch background work may also carry the neutral task id
  while remaining `kind: "bash"`.
- **Fallback text**: neutral text with `kind: "text"` for providers or tools
  without a known typed result.

Adding a new result family is a schema change: update Go wire structs, OpenAPI,
generated clients, dashboard types, and golden fixtures together.

### Provider normalization status

| Provider family | Current GC state | Required gap closure |
|---|---|---|
| Claude | Reads JSONL blocks; Claude `toolUseResult` sidecar evidence is normalized by the sessionlog entry layer into provider-neutral result evidence before worker inference, covering initial command/edit/read/glob/search/fetch/todo/plan/question/task/text data. Read sidecars with nested `file` evidence flatten to neutral file/range/content fields; WebSearch sidecar `results[].content[]` items flatten to neutral `result_items` with title, URL, and snippet; read/write file path evidence derives provider-neutral `language` hints before API projection. | Expand Claude SDK result parsing in sessionlog/worker for the remaining MC tool family set; continue shrinking raw Claude key handling outside the reader/sessionlog boundary. |
| Codex | Reads rollout JSONL; the reader normalizes JSON-string tool arguments, canonicalizes common tool input/result keys, preserves user image blocks as provider-neutral `image` blocks (`file_path`, external `image_url`, `mime_type`), preserves web-search queries as typed search input while keeping provider-native actions and unrecognized extra fields raw-only, derives provider-neutral command/read/grep input evidence for recognized shell commands including file `language`, unwraps common shell launchers (`/usr/bin/env bash -lc`, `/bin/bash -lc`, nested variants) before read/grep classification, strips Codex `Output:` wrappers, parses nested JSON command results, derives result `is_error` from explicit flags/status, JSON/text exit-code forms, and error-like text, normalizes read result content/ranges/language and grep result mode/count/filename summaries, treats no-match grep JSON results as zero-result search data rather than provider failures, parses URL-prefixed web-query output into neutral `result_items`, and carries patch-apply evidence before worker inference. Worker normalization prefers those neutral result fields and keeps typed edit hunk inference plus generic read/grep fallback for older fixtures. | Continue porting the remaining MC Codex output variants and real-provider fixtures, especially richer stderr/status forms and richer web/search result payloads beyond current URL/text result summaries. |
| Gemini | Current JSONL message `model`, nested `tokens.cache.read/write`, `type:"error"` messages, and tool-result status flow to provider-neutral structured metadata, typed `system_event`, error/text fields. Whole-file write inputs normalize as `kind: "write"` with file path/content text instead of fabricated input patches; `resultDisplay.fileDiff` and result-side file path evidence normalize in the Gemini reader into neutral patch data for current edit/write fixtures. | Normalize the remaining current Gemini `resultDisplay` and tool-response variants to typed edit/write/search/fetch data in the Gemini reader; add incremental parsing for live frame-granular streaming. |
| Kimi | Useful tool calls/results via reader; common tool input/result object keys normalize to provider-neutral names before worker inference, native `is_error` / `isError` / status fields normalize to provider-neutral `is_error`, and result-side patch evidence produces typed edit hunks. | Expand typed command/read/edit parsing where newer Kimi wire evidence supports richer execution, search, and artifact data. |
| OpenCode/MiMo/Groq/Cerebras | OpenCode-compatible reader gives useful blocks, neutralizes common camel-case tool input/output keys before worker inference, and export `info.modelID` / `info.tokens` now flow to provider-neutral structured model/usage metadata, including reasoning tokens when present. Result-side patch evidence from OpenCode tool output states produces typed edit hunks; input-only edit evidence still does not fabricate result diffs. | Expand OpenCode tool output-state normalization beyond the current common command/edit/file fields where native evidence supports richer read/grep/artifact data. |
| Pi/OMP | Pi-family reader provides messages/tool records, preserves generic image-part metadata as provider-neutral `image` blocks, normalizes common tool input/result object keys before worker inference, and OMP `bashExecution` / `pythonExecution` records emit provider-neutral execution fields that normalize through worker history into typed Bash/Python structured results. Result-side Pi patch evidence produces typed edit hunks. | Expand beyond current command/read/edit/image support where Pi-family evidence exposes richer search/artifact data; per-invocation usage is still absent. |
| Antigravity | Reader provides messages/tools/interactions, normalizes common tool input/result object keys before worker inference, maps native status fields to provider-neutral `is_error`, preserves whole-file write inputs as `kind: "write"` with file path/content text, and result-side diff/patch evidence produces typed edit hunks. | Expand typed result extraction for command/artifact/search shapes where Antigravity exposes richer evidence. |
| Copilot | Dedicated reader maps `~/.copilot/session-state/<sessionId>/events.jsonl` into provider-neutral messages and tool use/result blocks, including command stdout/stderr/exit code, failed-tool error content, and result-side patch/edit evidence when present. Keyed discovery uses the session-state directory and verifies `workspace.yaml` or `session.start.data.context.cwd` against the workdir. | Expand against real provider fixtures as Copilot event variants evolve, including any separate terminal-output chunk events and richer file-change records beyond current `tool.execution_complete` result evidence. |
| Kiro | Dedicated reader maps Kiro ACP JSONL `session/update` events under `~/.kiro/sessions/cli/<session-id>.jsonl` into provider-neutral assistant text, `tool_use`, and `tool_result` blocks. ACP `rawInput` / `rawOutput` fields normalize to neutral command/file/edit/result keys; ACP diff content normalizes to result-side patch data. Keyed discovery validates the JSON sidecar or JSONL context cwd against the workdir. Persisted `AssistantMessage` / `ToolResults` history frames are supported where they carry message content. | Capture real Kiro ACP fixtures to harden field variants and chunk coalescing; sample the default chat database/export path before claiming retrospective rich chat-session parity. |
| Amp | Dedicated reader maps captured `amp --execute --stream-json` JSONL into provider-neutral user/assistant/system messages, tool-use blocks, tool-result blocks, thinking blocks when present, usage metadata, and final result system events. Tool inputs/results normalize common command/file/edit/result keys before worker inference; JSON-string tool result payloads are decoded and neutralized before reaching `format=structured`. Discovery uses configured GC-owned capture paths only, because Amp does not document a stable retrospective local transcript store. | Add managed stdout capture before enabling stream-json in the builtin profile; validate against real Amp tool-result fixtures and subagent `parent_tool_use_id` cases. |
| Grok | Dedicated reader maps captured Grok ACP JSONL `session/update` events into provider-neutral assistant text, `tool_use`, and `tool_result` blocks by reusing the ACP normalization path. ACP `rawInput` / `rawOutput` fields normalize to neutral command/file/edit/result keys; ACP diff content normalizes to result-side patch data. Discovery uses configured GC-owned capture paths only, because the documented `~/.grok/sessions` persisted schema and `streaming-json` event schema still need fixture validation before use. | Add managed ACP/headless capture for builtin sessions if GC should launch Grok in that mode; collect real Grok ACP and streaming-json fixtures before claiming persisted/headless parity outside configured captures. |
| Auggie | Dedicated reader maps captured Auggie ACP JSONL `session/update` events into provider-neutral assistant text, `tool_use`, and `tool_result` blocks by reusing the ACP normalization path. ACP `rawInput` / `rawOutput` fields normalize to neutral command/file/edit/result keys; ACP diff content normalizes to result-side patch data. Discovery uses configured GC-owned capture paths only, because GC does not yet rely on a stable retrospective local Auggie transcript store. | Add managed ACP, SDK, or hook capture for builtin sessions; collect real Auggie ACP/hook/SDK fixtures before claiming saved-session, hook, or live SDK parity outside configured captures. |
| Cursor | Dedicated reader maps Cursor stream JSON and configured hook captures into provider-neutral user/assistant messages, `tool_use`, `tool_result`, and final result system events. Cursor stream tool calls normalize read/write/shell evidence into neutral file, command, output, and edit/result keys; configured capture discovery validates cwd and session ID. Native local Cursor transcripts intentionally omit tool outputs, so they are not a complete rich source by themselves. | Add managed stream/hook capture for builtin Cursor sessions; collect real Cursor stream/hook fixtures across CLI versions before claiming parity beyond configured captures. |

### Implementation sequence

1. Extend structured result DTOs with typed patch hunks and file path lists.
   Keep the existing flat `patch` field temporarily for compatibility.
2. Port MC patch parsing as typed data and update dashboard rendering to prefer
   typed hunks when present.
3. Tighten Codex read/grep/output normalization, including line-number
   stripping and grep mode detection.
4. Move provider-specific result extraction from `internal/api` into
   `internal/sessionlog` and `internal/worker` carriers.
5. Add first-class and high-coverage typed result normalizers from real
   provider fixtures, including Claude, Codex, Gemini, OpenCode-compatible
   providers, Grok ACP, Auggie ACP, and existing Pi-family evidence.
6. Build a golden fixture corpus from real `format=raw` transcripts and assert
   provider-neutral structured output for every first-class provider.
7. Expand long-tail providers with reader/capture work while preserving typed
   graceful fallback for all 17 profiles.

Each step should land with tests that prove both positive fidelity and negative
provider-neutrality: rich data is present, HTML is absent, and native provider
field names do not appear on the structured wire.

Current implementation status: steps 1-3 are partially implemented for the
initial
command/stdin/read/grep/glob/edit/search/fetch/todo/plan/question/task/text families.
The worker/API carrier now also projects direct-entry `model`, `stop_reason`,
and typed token usage when provider evidence is present; Codex `token_count`
events are correlated to structured assistant messages in worker history. Step
4 is complete for the API edge: structured API projection no longer performs
provider-native tool inference. Step 4 remains incomplete at the
provider-normalization layer until every extraction algorithm lives in
provider/sessionlog/worker code and the remaining first-class provider result
families are covered by real fixtures.

## Gap analysis: spirit vs. implementation

This gap list is part of the spec, not a transient review note. The structured
stream is not done when the API can return typed frames; it is done when a
client can build MC-class presentation, alternate GUIs, CLIs, analytics, and
programmatic automations from typed provider-neutral data without provider
parsers, native JSON escape hatches, or HTML.

### Weakest current areas

- **Provider parity is not proven until it is fixture-backed.** The code now
  has typed paths and graceful downgrade for every built-in profile, but the
  evidence must be a real provider/version matrix: Claude, Codex, Gemini,
  OpenCode-compatible providers, Pi-family providers, Copilot, Kiro, Cursor,
  Amp, Grok, Auggie, Kimi, and Antigravity. Synthetic fixtures are useful unit
  tests, not proof that current live provider sessions produce rich structured
  output.
- **Capture is the highest live-session risk.** Providers whose retrospective
  local transcript is incomplete or undocumented must have GC-managed capture
  for the source that actually contains rich data: stdout stream-json, ACP
  `session/update`, hook output, SDK event streams, or provider event logs. A
  profile that depends on a manually configured capture path should be reported
  as a degraded structured source, not described as complete.
- **The central normalizer must keep shrinking.** Shared helpers for patch
  parsing, command output normalization, shell unwrapping, and common
  provider-neutral key canonicalization are useful. Provider-specific semantic
  evidence should be emitted by provider readers/adapters, then carried by
  worker history. A generic worker/API normalizer that guesses semantics from
  arbitrary native-looking fields recreates the client-side parser problem on
  the server.
- **Provider-native key leakage is still the easiest regression.** Fallback
  `arguments: [{name, value}]` is only acceptable when names are
  provider-neutral or intentionally generic. Native field names such as
  `toolUseResult`, `resultDisplay`, `call_id`, `tool_use_id`, `sessionId`, and
  provider SDK object names belong in `format=raw`, fixtures, and adapters, not
  in structured DTOs.
- **Structured pagination and cursors must stay first-class.** Structured
  history and stream consumers need the same navigability as raw/conversation:
  `before`, `after`, stable entry cursors, and explicit pagination metadata.
  This branch adds structured cursor support, but provider-conformance tests
  must include cursor behavior so future structured work does not silently fall
  back to whole-session dumps.
- **Inline-subagent lineage is deferred beyond v1.**
  `session.structured.v1` has no reserved lineage fields. `ga-mb46n3` owns a
  later, evidence-backed version that may add typed parent/child relationships,
  parent tool call references when available, and transcript linkage without
  confusing provider inline subagents with Gas City formulas, drains, convoys,
  or beads.
- **Dashboard rendering must not be the evidence source.** Dashboard tests
  should prove that typed data renders rich results and colorized diffs, but the
  provider truth must be proven below the dashboard: native transcript/capture
  fixture -> provider reader -> worker history -> typed API/SSE. A beautiful
  dashboard over mocked payloads does not prove `format=structured`.
- **Spec claims must be mechanically tied to coverage.** Provider counts,
  first-class status, and supported result families should be backed by fixture
  manifests or conformance tables that fail tests when a provider/profile is
  added without structured evidence.

## Architectural alignment

Scoped to respect Gas City's load-bearing invariants (AGENTS.md):

- **Object model at the center.** The per-provider translation and the new
  carrier fields land in the domain (`internal/sessionlog`,
  `internal/worker`); `internal/api` only *projects* the normalized history
  into the structured wire shape. No domain logic moves into the API layer,
  and the carrier extends the canonical normalized-history model rather than
  forking a parallel one. The stream path already uses `worker.Handle.History`;
  the transcript path still reads `sessionlog` directly today, and AGENTS.md
  explicitly says the worker-boundary migration is not a sessionlog read-site
  inventory. Structured transcript implementation should prefer the worker
  history/transcript boundary where practical, but any remaining direct
  `sessionlog` use in `internal/api` must stay a projection over the domain
  parser, not a second parser.
- **Transport, not judgment.** Frame→block translation and Codex
  command-shape classification must remain deterministic adapter
  normalization, not role logic or user-work reasoning. `format=structured`
  is a projection of existing domain data, not a new SDK primitive.
- **Upstream alignment.** The carrier touches upstream-owned files
  (`sessionlog/entry.go`, `worker/types.go`, `internal/api` handlers). Keep
  each edit minimal and idiomatic so it rebases cleanly, and treat this
  feature as a strong candidate to propose upstream rather than carry as
  fork-only divergence.
- **Required reading before implementing.**
  `engdocs/architecture/api-control-plane.md` (typed-wire rules and the
  raw-frame opacity exception) and `engdocs/contributors/huma-usage.md`
  (SSE/Huma registration), per AGENTS.md.

## Proposed schema

Typed wire only — no bare `map[string]any` / `json.RawMessage` on the API or
SSE wire path. The concrete Huma-registered Go wire structs are the source for
the generated OpenAPI; the pseudocode below is a design sketch for those
structs, and `TestOpenAPISpecInSync` must pass.

`format=structured` has an additional provider-neutrality contract:
**it must not send provider-native transcript frames, provider-native JSON
leaves, or provider-specific dialect shapes over the wire.** The point of the
format is that clients can render rich session output without knowing that
Codex calls a field `call_id`, Claude calls it `tool_use_id`, or Gemini stores
tool arguments under a different object shape. Exact provider frames remain
available only through `format=raw`.

Every tool input, tool result, interaction, usage record, and diagnostic exposed
by `format=structured` must therefore be one of:

- a provider-neutral typed shape Gas City owns (preferred for known tool
  families such as Bash/Grep/Read/Edit/Search and known interaction fields);
- or a provider-neutral text fallback such as `{ kind: "text", text }`.

The `arguments` carrier is not a generic fallback for unknown provider fields.
Its input names are schema-approved scalar fields; v1 admits `path` for repeated
search paths. Unknown names and object or array values are omitted from the
structured projection. Consumers that need byte-level provider evidence must
ask for `format=raw`.

For edit results specifically, `tool_result.structured.patch` must come from
result-side evidence: a provider's structured tool-result field, a provider's
file-diff result display, or textual result content that is itself a patch.
Gas City may normalize those result-side shapes into one provider-neutral patch
string for the wire. It must not synthesize a result patch from the preceding
tool input (`old_string` / `new_string`, `apply_patch` input, write contents,
or similar). Tool inputs can still expose their own provider-neutral input
shape, but the result must represent what the provider reported after running
the tool.

Do not use anonymous maps, raw JSON fields, unregistered unions, or "any JSON"
carriers directly on the structured API types.

```
StructuredHistory
  gc_session_id           string?
  logical_conversation_id string?
  provider_session_id     string?
  transcript_stream_id    string
  generation              { id, observed_at? }
  cursor                  { after_entry_id?, resume_token }
  continuity              { status, compaction_count?, has_branches?, note? }
  tail_state              { activity, last_entry_id?, open_tool_call_ids?, pending_interaction_ids?, degraded?, degraded_reason? }
  diagnostics           []StructuredDiagnostic?

StructuredMessage
  id                  string
  role                "user" | "assistant" | "system" | "tool" | "unknown"
  provider            string                 // claude, codex, gemini, ...
  timestamp           string (RFC3339Nano)
  model               string?                // assistant turns
  stop_reason         string?
  usage               StructuredUsage?
  user_prompt         StructuredUserPrompt?
  system_event        StructuredSystemEvent?
  status              string                 // final | partial | superseded | unknown
  blocks              []StructuredBlock

StructuredUserPrompt
  text          string?
  opened_files  []string
  uploaded_files []UploadedFile
  selections    []IDESelection

UploadedFile
  original_name string?
  size         string?
  mime_type    string?
  file_path    string?
  preview_url  string?

IDESelection
  text string?

StructuredSystemEvent
  kind     string          // error | turn_aborted | ...
  category string          // usage_limit | stream_error | provider_error | turn_aborted | ...
  code     string?         // semantic provider code when useful, never the provider field name
  message  string?

StructuredUsage
  input_tokens          int
  output_tokens         int
  reasoning_tokens      int?
  cache_read_tokens     int?
  cache_creation_tokens int?
  context_window_tokens int?           // when derivable server-side
  context_used_tokens   int?
  context_percent       int?

StructuredBlock  (discriminated on `type`)
  type "text"        => { text }
  type "thinking"    => { thinking, signature? }     // gated, see policy
  type "tool_use"    => { id, name, input, caller? } // input = provider-neutral typed shape
  type "tool_result" => { tool_call_id, content, is_error, structured? }
  type "interaction" => { request_id, kind, state, prompt, options, action }
  type "image"       => { file_path?, image_url?, mime_type? }

StructuredToolInput  (the `input` on tool_use; discriminated on `kind`)
  kind "command"   => { command, args? }
  kind "stdin"     => { task_id, text, linked_command? }
  kind "code"      => { code }
  kind "patch"     => { patch, file_path? }
  kind "glob"      => { pattern, file_path? }
  kind "fetch"     => { url, prompt? }
  kind "search"    => { query?, pattern?, file_path?, command?, arguments?: [{ name: "path", value }] }
  kind "file"      => { file_path, language? }
  kind "write"     => { file_path, language?, text } // whole-file write input, not a fabricated patch
  kind "todo"      => { todos: TodoItem[] }
  kind "plan"      => { plan?, explanation?, steps: PlanStep[] }
  kind "question"  => { question, options[] }
  kind "task"      => { task_id?, task_type?, task_status?, description?, prompt? }
  kind "arguments" => { arguments: [{ name: "path", value }] } // schema-approved scalar search paths only
  kind "text"      => { text }

StructuredToolResult  (the `structured?` on tool_result; discriminated on `kind`)
  common optional => { error?: StructuredToolError }
  kind "bash"   => { command?, stdout, stderr, exit_code?, interrupted, truncated?, is_image?, task_id?, task_status?, stdout_lines?, stderr_lines?, timestamp?, content? }
  kind "stdin"  => { task_id?, text?, content? }
  kind "python" => { code?, stdout, stderr, exit_code?, interrupted, truncated? }
  kind "grep"   => { mode, filenames[], counts: [{ name, value }], num_files, num_results?, content, num_lines, applied_limit? }
  kind "glob"   => { filenames[], num_files, duration_ms?, truncated?, content? }
  kind "fetch"  => { url?, status_code?, status_text?, bytes?, duration_ms?, content? }
  kind "read"   => { file_path, language?, content, num_lines, start_line?, total_lines? }
  kind "write"  => { file_path, language?, content?, num_lines?, start_line?, total_lines?, text?, patch?, patch_hunks[]? }
  kind "edit"   => { file_path?, file_paths[], patch?, patch_hunks[], old_string?, new_string?, original_file?, replace_all?, user_modified?, content? }
  kind "search" => { query?, mode?, filenames[], counts: [{ name, value }], result_items[], duration_ms?, content, num_results? }
  kind "todo"   => { old_todos[], new_todos[], content? }
  kind "plan"   => { plan?, explanation?, steps: PlanStep[], content? }
  kind "question" => { question?, questions: Question[], options[], answer?, answers: [{ name, value }], content? }
  kind "task"   => { task_id?, task_type?, task_status?, description?, total_duration_ms?, total_tokens?, total_tool_use_count?, output?, stdout?, stderr?, exit_code?, content? }
  kind "text"   => { text, content? }                 // provider-neutral fallback

StructuredToolError
  category   "user_rejection" | "user_rejection_with_reason" | "command_failure" |
             "file_error" | "validation_error" | "timeout" | "network_error" | "unknown"
  message    string?       // cleaned provider text, with wrappers/prefixes removed
  user_reason string?      // only when the provider reports a user-supplied rejection reason

PatchHunk
  file_path? string
  old_start  int
  old_lines  int
  new_start  int
  new_lines  int
  lines      []string       // unified diff lines prefixed with " ", "-", or "+"

TodoItem
  id?        string
  content?   string
  status?    string
  active_form? string
  priority?  string

PlanStep
  step?   string
  status? string

SearchResultItem
  title?   string
  url?     string
  snippet? string

Question
  question?    string
  header?      string
  options?     QuestionOption[]
  multi_select? bool

QuestionOption
  label?       string
  description? string
```

The concrete Go wire structs should use Gas City's normal JSON spelling shown
above (`schema_version`, `stop_reason`, `tool_call_id`, `is_error`, etc.). Use `tool_call_id`
even when the native provider calls the value `tool_use_id`, `call_id`, or
something else; native spelling belongs only in `format=raw`.

The structured transcript response and the structured stream preserve the
worker history envelope, not only individual messages. REST returns
`operation: "snapshot"` and an opaque `history.cursor.resume_token`. The client
passes that token as `after_cursor` when opening SSE; browser reconnects send
the latest SSE `id` as `Last-Event-ID`. Structured SSE frames use three typed
operations: `snapshot` replaces the initial projection, `upsert` replays the
previous mutable tail inclusively plus later messages, and `reset` carries a
full replacement (including an empty array) with a typed reason. Generation
changes remain evidence, but the volatile observation value is not itself a
reset discriminator. Cursor invalidation, history rewrites, stream rotation,
degraded continuity, and tail state remain visible on the wire, matching
`worker-conformance.md` §4.3 rather than silently replaying or dropping history.

The stream keeps the lifecycle event kinds (`activity`, `pending`,
`pending_cleared`, `heartbeat`) and adds a versioned structured message payload.
Huma SSE registration maps **one concrete Go payload type to one SSE event name**
(`internal/api/sse.go`; `internal/api/supervisor_city_routes.go`), so Phase 1
uses a distinct `event: structured` frame for
`SessionStreamStructuredMessageEvent`. The existing `turn` event remains the
conversation payload, `message` remains the raw provider-frame payload, and the
structured payload carries `schema_version` inside `data`. `conversation` and
`raw` payloads remain unversioned for back-compat.

Pending interactions form one authoritative slot, not an append-only event
history. After the initial structured projection, every connection immediately
reseeds its current pending interaction. A `pending` frame replaces the slot,
including same-ID replays and request A changing to request B;
`pending_cleared` removes the resolved request. On every EventSource open or
reconnect, clients clear any locally cached pending interaction before accepting
the server reseed. This prevents a stale interaction from surviving when the new
connection has no pending request and therefore no prior request ID for the
server to clear.

When the transcript path has no structured history yet, `format=structured`
still honors the requested structured wire contract. The response or stream
frame must use `format: "structured"`, include `schema_version`, and include a
degraded `history` envelope with `continuity.status = "degraded"` plus a
`transcript_unavailable` diagnostic. If live pane text is the only observable
source, it may appear only as a provider-neutral `text` block on a degraded
`assistant` message in `structured_messages`; the server must not return
`format: "text"` to a structured request and must not forward provider-native
frames through the structured format. This graceful downgrade is mandatory for
every built-in provider, including providers whose rich transcript parser has
not landed yet, so clients can render one provider-neutral shape without
learning provider file formats.

### Thinking-exposure policy

`conversation` redacts thinking by deliberate policy
(`handler_agent_output_turns.go:46-49`). `format=structured` is itself
opt-in, but to avoid silently reversing that stance for consumers who don't
want reasoning, thinking blocks are gated behind an explicit
`include_thinking=true` query parameter; default omits both the `thinking`
block's text and its signature (keeping a typed placeholder block so ordering
is preserved). The parameter is declared on the Huma input structs so it
appears in OpenAPI; do not read it through raw URL inspection. City read
authorization applies to both REST and SSE. Maintainer/product-security owner
Julian Knutsen approved this v1 policy on 2026-07-14.

## Phasing

### Phase 1 — Structured, frame-granular format

**1A. Carrier + schema + stop-flatten (cross-cutting; currently unlocks 12
transcript families and 16 profiles).**

- `internal/sessionlog/entry.go` — carry `Thinking` and `Signature` on
  `ContentBlock`; map provider thinking/reasoning fields into canonical
  thinking text and neutral signature markers.
- `internal/worker/types.go` — add `Usage`, `Model`, `StopReason` to
  `HistoryEntry`; add signature and structured-result carriers to
  `HistoryBlock`.
- `internal/worker/sessionlog_adapter.go:293-368` — decode
  `message.usage` / `model` / `stop_reason` (reuse the cache-aware parser in
  `tail_usage.go`) and populate the new fields instead of only `cloneRaw`.
  Preserve context-window/fullness data from `TailMeta.ContextUsage` when the
  server can derive it; do not make clients reimplement model-window lookup.
- `internal/api/` — register `format=structured` on the Huma session stream
  (`huma_handlers_sessions_stream.go`) and transcript
  (`huma_handlers_sessions_query.go`) handlers, emitting `StructuredMessage`;
  add the typed wire types, the Huma input docs/validation, and the OpenAPI
  SSE schema; leave `conversation`/`raw` untouched.
- `cmd/gc/dashboard/web/src/generated/` and `internal/api/genclient/` —
  regenerate clients from the Huma spec. The dashboard uses generated
  `streamSession` SSE types and currently bridges heterogeneous frame payloads
  opaquely, so this is a generated-client/API-shape change, not just a server
  change.
- Live delivery reuses the existing tail/cursor stream machinery — frames
  are emitted structured as they commit (frame-granular streaming).

Outcome: claude + codex + gemini + kimi + opencode + mimocode + groq +
cerebras + pi + omp + antigravity emit useful structured blocks immediately,
with fidelity bounded by the carrier fields they can populate and by targeted
reader gaps closed below.

**1B. Codex structured tool results** (`internal/sessionlog/codex_reader.go`).

The load-bearing prerequisite is a `call_id → {toolName, readShellInfo}`
context map threaded through `ReadCodexFile`'s loop — without it a
`function_call_output` cannot know whether it was Bash/Grep/Read, which is
exactly why Codex results are opaque today. Then:

- tool-name canonicalization (`apply_patch`→Edit, `shell_command` /
  `exec_command`→Bash, `web_search_call` / `search_query`→WebSearch);
- command-shape reclassification (`cat`/`sed`/`nl`→Read, `rg`→Grep) with a
  quote-aware tokenizer;
- structured result parsers: Bash (strip `Output:\n`, split stdout/stderr,
  exit code), ripgrep (content vs files-with-matches; no-match ≠ error),
  read (line numbering, strip `nl` prefixes);
- exit-code / `is_error` derivation from text and fields;
- `web_search_call` handling; reasoning fallback to the item's `content`
  when `summary` is empty, and `encrypted_content` → `signature: "encrypted"`;
- `token_count` event → per-entry usage; capture `model` and the reported
  context window (currently implemented in worker history via
  `ExtractCodexTailUsage` correlation).
- `codexResponseItem` already carries typed `content`, `summary`, `arguments`,
  `input`, and `output`; `encrypted_content` is carried only as neutral
  signature evidence. Keep encrypted provider bytes off the structured wire.

**1C. Gemini and Kimi current-format gaps.**

- Gemini: current JSONL session format is supported; `model`, nested
  `tokens.cache.read/write`, `type:"error"` messages, and tool-result status
  now normalize to provider-neutral metadata/usage/text/error fields. Remaining
  work: cover every current `resultDisplay` variant and add an **incremental
  parser** so live frame-granular streaming works (the legacy reader is
  whole-file).
- Kimi: prefer current `~/.kimi-code/sessions/<workDirKey>/<sessionId>/agents/main/wire.jsonl`
  and subagent `wire.jsonl` files, while retaining legacy context-log support
  until it is no longer useful.

**1D. Alias and Pi-family hardening.**

- Route `groq` and `cerebras` through the OpenCode adapter. They are Gas City
  OpenCode-backed profiles, not distinct transcript dialects.
- Route `omp` through the Pi-family adapter and normalize OMP `bashExecution`
  / `pythonExecution` messages to provider-neutral Bash/Python tool-result
  shapes with output, exit code, cancellation/interruption, and truncation
  (covered in worker history).
- Kimi and Antigravity status/error fields normalize to provider-neutral
  `is_error`; remaining work is richer typed result extraction where their
  native evidence exposes command/edit/artifact/file shapes.
- Note: per-invocation usage is absent for some long-tail readers today. Pi
  exposes compaction `PreTokens`, but that is context-boundary evidence, not
  response usage.

**1E. Copilot reader hardening.**

- Copilot: `~/.copilot/session-state/<sessionId>/events.jsonl` is now mapped
  through the sessionlog provider layer into neutral user/assistant/system
  messages, `tool.execution_start` / assistant `toolRequests` tool-use blocks,
  and `tool.execution_complete` tool results. Command output fields normalize
  to `stdout`, `stderr`, and `exit_code`; failed tool errors normalize to
  neutral error content; result-side patch/edit evidence normalizes to
  `file_path`, `patch`, old/new/original text, and edit flags when present.
  Remaining work: validate against a real fixture corpus for newer Copilot
  event variants, including any separate terminal-output chunk events and richer
  file-change records beyond completion-result evidence.

**1F. Kiro ACP structured reader.**

- Kiro: `~/.kiro/sessions/cli/<session-id>.jsonl` is now mapped through the
  sessionlog provider layer into neutral assistant text, tool-use, and
  tool-result blocks. ACP `rawInput` / `rawOutput` normalize to provider-neutral
  command/file/edit/result fields, failed statuses become neutral error result
  content, and ACP diff content normalizes to result-side `file_path` + `patch`
  data. Keyed discovery uses the ACP session file plus JSON sidecar/JSONL cwd
  evidence to verify the workdir. Remaining work: capture real ACP fixture
  variants for chunk coalescing and sample the default Kiro chat DB/export path
  before relying on retrospective chat sessions.

**1G. Amp captured stream-json reader.**

- Amp: configured GC-owned captures of `amp --execute --stream-json` output are
  now mapped through the sessionlog provider layer into neutral user/assistant
  messages, tool-use blocks, tool-result blocks, and final result system
  events. Assistant usage metadata flows to worker history. Tool inputs and
  JSON-string result payloads normalize to provider-neutral command/file/edit
  fields before worker inference, including typed Bash/Edit structured results
  when the captured stream contains stdout/stderr/exit-code or result-side patch
  evidence. Remaining work: add managed stdout capture before changing the
  builtin interactive Amp profile to stream-json/headless mode, and validate
  against real Amp fixtures for subagents and richer tool-result variants.

**1H. Grok ACP captured reader.**

- Grok: configured GC-owned captures of ACP JSONL `session/update` events are
  now mapped through the sessionlog provider layer into neutral assistant text,
  tool-use blocks, and tool-result blocks. ACP `rawInput` / `rawOutput`
  normalize to provider-neutral command/file/edit/result fields, and ACP diff
  content normalizes to result-side `file_path` + `patch` data before worker
  inference. Discovery uses configured capture roots and validates cwd from
  `session/new` / nested context frames. Remaining work: add managed ACP or
  headless capture before changing the builtin Grok profile, and validate
  documented `--output-format streaming-json` plus persisted `~/.grok/sessions`
  formats with real fixtures before relying on them.

**1I. Auggie ACP captured reader.**

- Auggie: configured GC-owned captures of ACP JSONL `session/update` events are
  now mapped through the sessionlog provider layer into neutral assistant text,
  tool-use blocks, and tool-result blocks. ACP `rawInput` / `rawOutput`
  normalize to provider-neutral command/file/edit/result fields, and ACP diff
  content normalizes to result-side `file_path` + `patch` data before worker
  inference. Discovery uses configured capture roots and validates cwd from
  `session/new` / nested context frames. Remaining work: add managed ACP, SDK,
  or hook capture before changing the builtin Auggie profile, and validate saved
  session `chatHistory` / node graph, hook `tool_output` / `file_changes`, and
  SDK `tool_call_update` / `rawOutput` formats with real fixtures before
  relying on them.

**1J. Cursor capture hardening.**

- Cursor: native local transcripts intentionally omit tool outputs, so rich
  structured support requires Cursor stream JSON or Gas City-managed hook
  capture (for example `postToolUse`) rather than the native local JSONL alone.
  GC now has a dedicated Cursor reader for stream/hook captures; remaining work
  is managed builtin capture and real-provider fixture hardening.

**1K. Golden-fixture test corpus (cross-cutting; do alongside 1A–1J).**

Capture real `format=raw` transcripts per provider/version into a fixture
corpus and snapshot-test the structured producer. A provider CLI bump
becomes "regenerate fixtures + diff." This is the regression guard that
makes one-place server-side translation safe — and is exactly the
discipline consumers reverse-engineering frames never had.

Extend the existing gates, not just new golden tests:

- API session stream/transcript tests for thinking redaction defaults,
  `include_thinking`, raw preservation, `text` fallback, and structured
  no-history behavior (`internal/api/handler_sessions_test.go` already covers
  adjacent stream cases).
- Worker conformance helpers for open-tail history, tool calls/results,
  interactions, and the new usage/model/stop-reason carriers
  (`internal/worker/workertest/phase2_result_helpers_test.go`).
- Huma SSE schema coverage (`internal/api/huma_sse_test.go`), OpenAPI sync
  (`internal/api/openapi_sync_test.go`), generated Go client sync
  (`internal/api/genclient/genclient_test.go`), and dashboard TypeScript type
  generation (`make dashboard-check` when generated web types change).

### Phase 2 — Sub-frame streaming (future)

Tap providers' live API output for token/delta-level streaming
(`content_block_delta` / `input_json_delta` equivalents). Materially larger
than Phase 1: it steps outside the transcript-reader model and needs a live
provider-wire tap plus delta-assembly state. Documented here so the schema
(`StructuredBlock` deltas, `schema_version`) is designed to admit it later
without a breaking change; not built in this spec.

## Consumer impact

Any consumer that today reads `raw` and re-derives structure can switch to
`format=structured` and **delete its per-provider transcript translation
layer**, keeping only genuine presentation (markdown→HTML, highlighting, diffs,
visual pairing). The division of labor is explicit:

- **Gas City owns** canonical structured *data* — typed blocks, structured
  tool results, usage, and gated thinking.
- **Consumers own** presentation.

This is mode-agnostic: a self-hosted supervisor on loopback and a future
remote/managed control plane forwarding streams to many clients both
benefit — a multi-tenant forwarder wants a clean versioned typed format even
more than a loopback one does.

The session stream/transcript endpoints are city-scoped today and inherit
city/tenant identity from the `/v0/city/{cityName}/...` route context. Any
future aggregated forwarder must add its own source-city/tenant envelope and
composite cursor semantics, like the machine-wide event stream, rather than
pretending a per-city structured message is globally unique on its own.

It is still an observability/data projection. It is not checkpointing, durable
execution, formula/drain correlation, inter-session messaging, or an
authorization/redaction layer.

## Risks & open questions

- **Thinking exposure (settled for v1)** — `include_thinking` is explicit and
  city-read-authorized; the default retains an ordering placeholder but omits
  both reasoning text and signature.
- **Inline-subagent lineage (deferred)** — v1 exposes no reserved lineage
  fields. `ga-mb46n3` owns a versioned contract backed by real provider
  evidence and end-to-end tests.
- **Schema churn** — `StructuredToolInput` and `StructuredToolResult` kinds
  (bash/grep/read/edit/search/text/etc.) need fixture pressure before freezing
  the wire. Unknown result data may use provider-neutral `kind:"text"`; input
  arguments remain restricted to schema-approved scalar search fields, not
  arbitrary provider names or raw JSON.
- **Sensitive payload exposure** — full tool inputs, tool results,
  interaction prompts/options, code/diffs, and thinking can contain secrets.
  Any remote or multi-tenant deployment needs the same auth, redaction,
  log-scrubbing, and file-permission discipline used for projected MCP files
  and other secret-bearing runtime surfaces.
- **Provider-neutrality boundary** — `format=structured` must normalize
  provider-specific fields into typed provider-neutral shapes. Do not put bare
  `json.RawMessage`, `map[string]any`, unregistered unions, "any JSON" carriers,
  or provider-native field objects directly on structured API structs.
- **SSE event-name compatibility** — preserving the current default SSE
  `message` event semantics for raw frames while adding structured frames
  requires either a new event name or an intentional SSE helper refactor, as
  described in the schema section. If raw and structured share the same
  semantic event key, the replacement discriminator must remain visible to
  generated clients; otherwise dashboard consumers lose `switch (frame.event)`
  narrowing.
- **`worker-conformance.md` alignment** — the carrier change extends the
  canonical normalized model that doc designates as core; the new fields
  should land as conformance assertions, not incidental observability.
- **Provider capture completeness** — Cursor, Auggie saved-session/hook/SDK
  paths, streaming-json/persisted Grok paths, and some Kiro paths are not safely
  solved by generic local transcript discovery. Their rich structured coverage
  depends on managed hook/stream/ACP capture, and tests must distinguish
  "message/tool-call intent available" from "full rich result/diff trace
  available."

## Appendix: source facts

- Formats accepted today: the Huma transcript and stream inputs declare
  `format` as `conversation,raw,structured`
  (`huma_types_sessions.go`). `conversation` remains the default when the
  query parameter is omitted; `raw` emits provider-native frames through the
  typed raw-frame wrapper; `structured` emits the provider-neutral structured
  envelope. The legacy unscoped `/v0/session/...` routes preserve the same
  `format=structured` semantics for compatibility, but the city-scoped Huma
  routes are the canonical OpenAPI surface.
- `text` fallback: the default conversation transcript and live peek can
  return `format: "text"` when there is no provider transcript yet and pane
  output is the only available source. A client that explicitly asks for
  `format=structured` receives the degraded structured fallback instead
  (`huma_handlers_sessions_query.go`; `streamSessionPeekStructuredHuma` in
  `handler_session_stream.go`).
- Worker-boundary caveat: the migration governs session creation/lifecycle;
  AGENTS.md explicitly says stream and transcript readers in `internal/api/`
  still read session logs directly (`AGENTS.md:248-265`;
  `huma_handlers_sessions_query.go:156-209`).
- Flattening: `entryToTurn` / `historyEntryToTurn`
  (`handler_agent_output_turns.go:26-101`).
- Typed block fidelity retained pre-flatten: `ContentBlock`
  (`entry.go:60-77`), `HistoryBlock` (`worker/types.go:199-210`).
- Per-family dispatch: `reader.go` `ReadProviderFile` and `ProviderFamily`.
- Tail-only usage: `tail_usage.go:12-98`, `codex_usage.go:31-123`,
  `worker/invocation_telemetry.go:188-225`.
- Codex opacity: `codex_reader.go:254-270,315-327`.
- Gemini whole-file: `gemini_reader.go:18-28`.
- Raw-frame "honest opacity" rationale: `engdocs/architecture/api-control-plane.md`
  §3.6. Normalized-history alignment: `engdocs/design/worker-conformance.md`
  §4.1–4.2.
- Provider-format research:
  - Claude documents continuous local JSONL transcript storage under
    `~/.claude/projects/<project>/<session-id>.jsonl`, with each line a JSON
    object for message, tool use, or metadata:
    <https://code.claude.com/docs/en/sessions>.
  - Codex documents `--json` as newline-delimited JSON events and
    `--ephemeral` as disabling persisted rollout files:
    <https://developers.openai.com/codex/cli/reference>.
  - Gemini CLI session management records prompts/responses, tool execution
    inputs/outputs, token usage, and assistant thoughts:
    <https://developers.googleblog.com/pick-up-exactly-where-you-left-off-with-session-management-in-gemini-cli/>.
  - Kimi Code stores sessions under `~/.kimi-code/sessions/...`, with
    `agents/main/wire.jsonl` as the main agent communication record:
    <https://www.kimi.com/code/docs/en/kimi-code-cli/configuration/data-locations.html>.
  - OpenCode exports session data as JSON and exposes session/database
    commands, including `opencode export [sessionID]`:
    <https://opencode.ai/docs/cli/>.
  - MiMo Code persists session data in `MIMOCODE_HOME` and supports JSON
    export/import:
    <https://mimo.xiaomi.com/mimocode/sessions>.
  - Oh My Pi documents JSONL session storage under
    `~/.omp/agent/sessions/...` and hook events for tool calls/results:
    <https://github.com/can1357/oh-my-pi/blob/main/docs/session.md>,
    <https://github.com/can1357/oh-my-pi/blob/main/docs/hooks.md>.
  - Copilot CLI documents local session data and full-history resume behavior:
    <https://docs.github.com/en/copilot/concepts/agents/copilot-cli/chronicle>.
  - Kiro documents local database-backed chat sessions and ACP JSONL session
    logs with `ToolCall`, `ToolCallUpdate`, and `TurnEnd` updates:
    <https://kiro.dev/docs/cli/chat/session-management/>,
    <https://kiro.dev/docs/cli/acp/>.
  - Cursor staff state that local JSONL transcripts include messages,
    assistant text, and tool-call inputs but intentionally omit tool-call
    outputs; they recommend hooks for full output capture:
    <https://forum.cursor.com/t/accessing-the-full-agent-transcript-in-cursor/157311>.
  - Cursor documents `--output-format stream-json` as line-delimited JSON
    events with `tool_call` started/completed frames:
    <https://cursor.com/docs/cli/reference/output-format>.
  - Cursor documents hooks as local processes configured through Cursor hook
    manifests:
    <https://cursor.com/docs/hooks>.
  - Amp documents `--execute --stream-json` as line-delimited structured
    output, plus `--stream-json-input` for programmatic conversations:
    <https://ampcode.com/manual>.
  - Grok documents headless sessions under `~/.grok/sessions`,
    `--output-format streaming-json`, and ACP `session/update` chunks:
    <https://docs.x.ai/build/cli/headless-scripting>.
  - Auggie documents `--print --output-format json`, `--acp`, saved session
    resume/list commands, cache relocation, and diagnostic log-file controls;
    Auggie ACP uses JSON-RPC over stdio; Auggie hooks expose `tool_output`,
    `tool_error`, and `file_changes`; and the TypeScript SDK exposes
    `agent_message_chunk`, `tool_call`, `tool_call_update`, and `rawOutput`
    session updates:
    <https://docs.augmentcode.com/cli/reference>,
    <https://docs.augmentcode.com/cli/acp/agent>,
    <https://docs.augmentcode.com/cli/hooks>,
    <https://docs.augmentcode.com/cli/sdk-typescript>.
  - Google documents Antigravity CLI as the Gemini CLI successor with hooks,
    subagents, and extensions:
    <https://developers.googleblog.com/an-important-update-transitioning-gemini-cli-to-antigravity-cli/>.
