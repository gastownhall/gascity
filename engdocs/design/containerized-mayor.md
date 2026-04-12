---
title: "Containerized Interactive Mayor"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-04-12 |
| Author(s) | Mark Kim |
| Issue | N/A |
| Supersedes | N/A |

## Summary

Design a fully containerized **Claude Code** mayor agent that preserves
every capability of a bare-metal tmux-based mayor — interactive
streaming, multi-turn context, human attach/detach, crash recovery —
**without tmux**.

The mayor runs headless inside Docker using the `@anthropic-ai/claude-agent-sdk`
persistent `query()` stream. Controller nudges are delivered through a
file-based inbox. Human operators attach an Ink terminal UI (`mayor-chat`)
from outside the container via a Unix domain socket, getting
token-by-token streaming. When the daemon crashes, the controller
restarts it and the SDK's `resume` option reconnects to the existing
conversation.

**Harness scope.** This design targets the **Claude Code CLI and the
Anthropic Agent SDK specifically**. Whether an equivalent pattern works
for other harnesses (Codex, OpenCode, Cursor, Aider, etc.) is an open
question — it depends on whether each harness exposes a persistent
streaming API, a session-resume mechanism, and a subprocess model
compatible with a Node daemon wrapper. Gas City's broader session
provider model is harness-agnostic (any exec script can implement the
protocol), but **this specific containerized-interactive pattern
currently targets Claude Code only**. Support for other harnesses is
future work and explicitly out of scope for v1.

## Intent

**Problem.** Claude Code's TUI hangs in Docker containers (Docker's PTY
multiplexing bridge deadlocks Ink's React renderer). Today, the only way
to run an interactive mayor is on bare metal with tmux. This blocks
multi-tenant deployments, isolated sandboxing, and clean crash recovery.

**Goal.** Replace the tmux + bare-metal model with a pure-Docker model
that is:

- **Fully containerized** — no tmux, no host-level state, clean sandbox
- **Fully interactive** — token-by-token streaming, multi-turn context
- **Attachable on demand** — human can attach/detach an Ink UI at will
- **Crash-recoverable** — daemon crashes do not lose conversation history
- **Protocol-native** — fits Gas City's existing session provider model

**Non-goals.**

- **Generalizing to non-Claude-Code harnesses.** The daemon relies on
  Claude Code-specific features: the Agent SDK's `query()` AsyncIterable
  API, the `--resume <sessionId>` semantics, Claude's on-disk session
  storage at `~/.claude/projects/...`, and the `stream_event`/`result`
  message schema. Other harnesses (Codex, OpenCode, Cursor, Aider, etc.)
  have not been evaluated for equivalent capabilities. A harness-agnostic
  version would need a separate design informed by what each harness
  actually exposes.
- Replacing the worker agent model. Workers already work headless with
  the `gc-session-docker-headless` provider (#552) and the discrete
  `claude -p` nudge pattern.
- Supporting arbitrary frontend clients in the initial version. The Ink
  UI is the reference client; a plugin architecture for other frontends
  (Telegram, Slack, web) is future work.
- Mid-turn interrupts and priority queueing. FIFO only in v1.

## Background

Three PRs lay the foundation:

- **#552 `gc-session-docker-headless`** — exec provider for headless
  workers. Container runs `sleep infinity`; each nudge is a separate
  `claude -p --resume <id>` invocation. Works for workers because they
  receive discrete tasks and produce structured output. Does not
  support streaming or persistent conversation state.

- **#553 `contrib/mayor-chat`** — a V1 `query()` transport (`transport.mjs`)
  that keeps a single Claude Code subprocess alive across turns with
  token-by-token streaming via `stream_event` messages. The Ink UI
  (`chat.mjs`) is currently a standalone client that creates its own
  transport instance — suitable for one-user-one-container, not for
  controller integration.

- **#555 per-agent session provider** — generalizes the auto provider
  from a 2-backend router (default + ACP) to an N-backend router,
  enabling per-agent provider selection. Unblocks the mixed-provider
  deployment but does not itself define a mayor-specific provider.

This design proposes **PR #5XX (future)**: a new exec provider script
(`gc-session-mayor-chat`) that bridges the exec protocol to the
persistent SDK transport, plus a `daemon.mjs` that wraps `transport.mjs`
with the plumbing needed to make it work in a controller-driven context.

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     Gas City controller                       │
│                                                               │
│  nudge "plan the day"                                         │
│      └──► gc-session-mayor-chat nudge mayor                   │
│              └──► writes /run/gc-mayor/inbox/<ts>.json        │
└───────────────────────────┬───────────────────────────────────┘
                            │ (docker exec)
┌───────────────────────────▼───────────────────────────────────┐
│                     Container (mayor)                          │
│                                                               │
│   ┌──────────────────────────────────────────┐               │
│   │          daemon.mjs  (PID 1 child)       │               │
│   │                                          │               │
│   │  - watches /run/gc-mayor/inbox/          │               │
│   │  - maintains MayorTransport              │               │
│   │  - persists session_id to disk           │               │
│   │  - writes /run/gc-mayor/log/output.log   │               │
│   │  - listens on /run/gc-mayor/daemon.sock  │               │
│   │                                          │               │
│   │  on crash: new query({resume: sid})      │               │
│   └──────────┬───────────────────────┬───────┘               │
│              │                       │                        │
│              ▼                       ▼                        │
│      claude subprocess        Unix socket listener           │
│         (via SDK)             (for chat.mjs)                  │
│              │                                                │
│              ▼                                                │
│       Anthropic API                                           │
│                                                               │
│   Volume mounts:                                              │
│     /root/.claude  → host HOME/.claude (session persistence)  │
│     /run/gc-mayor  → container-local tmpfs                    │
└───────────────────────────────────────────────────────────────┘
                            ▲
                            │ (docker exec -it)
                            │
              ┌─────────────┴──────────────┐
              │   Host — chat.mjs (Ink UI) │
              │   connects to daemon.sock  │
              │   sends messages, streams  │
              │   responses in real time   │
              └────────────────────────────┘
```

### Components

**`gc-session-mayor-chat`** (bash, exec provider). Implements the Gas
City exec session provider protocol. Operations:

- `start` — `docker run` the mayor-chat image, start `daemon.mjs` inside
- `nudge` — write message to inbox directory via `docker exec`
- `peek` — `tail` the output log
- `process-alive` — check if `daemon.mjs` is running (not just container)
- `stop`, `interrupt`, `set-meta`, `get-meta`, etc. — standard exec ops

**`daemon.mjs`** (Node.js, inside container). Wraps `MayorTransport`
with the plumbing needed for controller-driven use:

- Watches `/run/gc-mayor/inbox/` for new message files (sorted by name)
- For each message: calls `transport.send(text)`, streams `stream_event`
  messages to `/run/gc-mayor/log/output.log`, writes final text response
- Persists session ID to `/run/gc-mayor/session_id` on first response
- On startup, reads persisted session ID (if any) and initializes
  transport with `{resume: sessionId}`
- Listens on `/run/gc-mayor/daemon.sock` for human clients
- Tracks retry count; after N consecutive crashes, writes a failure
  marker and exits (controller paging/alerting left to Gas City)

**`transport.mjs`** (existing, from #553). No changes required — its
`_queryFn` injection point lets `daemon.mjs` pass SDK options including
`resume`.

**`chat.mjs`** (rewrite). No longer creates its own transport. Connects
to `/run/gc-mayor/daemon.sock` and speaks a small JSON protocol:

```jsonc
// Client → daemon
{ "type": "send", "text": "hello" }
{ "type": "subscribe" }     // begin receiving stream events
{ "type": "history", "limit": 50 }

// Daemon → client
{ "type": "stream_event", "event": { ... } }
{ "type": "assistant", "message": { ... } }
{ "type": "result", "subtype": "success", ... }
{ "type": "session", "sessionId": "abc-123" }
```

### Queue model

**FIFO file-based inbox.** Each nudge creates one file in
`/run/gc-mayor/inbox/`, named `<unix-ts>-<seq>.json`. daemon reads the
oldest, processes it (one full turn), deletes the file, repeats.

Why file-based:

- **Survives daemon crash.** Messages in the inbox are not lost.
- **Observable.** `ls /run/gc-mayor/inbox/` shows pending work.
- **Simple.** No shared-memory or in-process queue to reason about.
- **Natural serialization.** Alphabetical ordering = chronological.

**Priority and mid-turn interrupt are explicitly out of scope for v1.**
A priority queue adds cancellation semantics, partial-response handling,
and billing edge cases that are not worth the complexity before the
FIFO path is proven.

### Crash recovery

The SDK exposes `resume: sessionId` as a first-class `query()` option.
Claude stores session history at `~/.claude/projects/<dir>/<sessionId>/`.
If `~/.claude` is volume-mounted from the host (or a persistent volume),
session history survives container restart.

**Recovery flow:**

1. `daemon.mjs` dies (subprocess error, SDK issue, OOM, etc.)
2. `gc-session-mayor-chat process-alive` returns `false`
3. Gas City's reconciler detects dead session, triggers restart
4. Container's `daemon.mjs` restarts
5. `daemon.mjs` reads `/run/gc-mayor/session_id`
6. New `query({ prompt: inputStream, options: { resume: sessionId } })`
7. SDK loads conversation history, stream is live
8. Daemon processes next message from inbox → continues as normal

**Retry limits.** daemon.mjs tracks consecutive crashes (persisted to
`/run/gc-mayor/crash_count`). After N crashes (default 3), it writes a
failure marker file and exits without restarting. The reconciler
escalates — today that means marking the session as stuck; future work
may integrate with Gas City's alerting.

**Known cost.** Resume re-sends conversation history to the model on the
first post-restart message. For a long conversation (100K+ tokens), this
is one full-context API call. Prompt caching mitigates the cost within
its 5-minute TTL, but a crash + restart usually exceeds the TTL. This is
an **accepted limitation** — crashes are expected to be rare, and the
cost is bounded per crash. Future work could fork the session at
recovery time to avoid re-reading the entire history.

### Session persistence

Mount `~/.claude` from the host (or a named Docker volume) into the
container at `/root/.claude` (or wherever `HOME` points inside the
container). Without this, session history lives on the container's
ephemeral layer and `resume` has nothing to resume.

The existing `GC_DOCKER_HOME_MOUNT=true` env var in the headless
provider already does this. The mayor-chat provider will default this
to `true` (headless workers can opt out).

## Known limitations and deferred work

These are documented non-goals for v1; none block the design.

**Testing strategy.** Unit-testing the transport with a mock `query`
works (PR #553). Unit-testing the daemon — named pipe semantics, inbox
watching, signal handling, restart-with-resume logic — needs a test
harness we have not built. End-to-end crash recovery (kill daemon
mid-turn, verify next nudge continues the conversation) needs either a
live Claude or a deep SDK mock. **This is deferred.** The initial PR
will ship with manual E2E validation on forge and the existing unit
tests for the transport layer.

**chat.mjs rewrite.** The current Ink UI creates its own `MayorTransport`
instance. Under this design it becomes a socket client. This is not a
minor change — it is effectively a new chat.mjs that happens to share
the Ink components. Scoped to the mayor-chat provider PR.

**Interrupt semantics.** The exec protocol's `interrupt` operation is
fire-and-forget via SIGINT. For a persistent transport, interrupt needs
to cancel the current turn's AsyncIterable without killing daemon.mjs.
The SDK likely has a cancel mechanism; wiring it through requires
careful state management. **Deferred to a follow-up PR**; v1's interrupt
will just kill the current `claude` subprocess and let the daemon
restart the transport with resume.

**Exec protocol is a lossy fit.** We are forcing a streaming,
persistent-connection model through a protocol designed for discrete
command-line nudges. The file-based inbox, log format, and socket
listener are all shims around this mismatch. This works for v1 but is
not the right long-term abstraction.

## Future abstraction

The exec protocol assumes each session backend is a shell script with a
fixed verb-based interface (`start`, `stop`, `nudge`, `peek`). This
fits tmux (send-keys, capture-pane) and discrete CLI agents (claude -p).
It does not fit persistent streaming transports.

A better abstraction for the streaming case might be:

**Native Go interface for streaming providers.** The `runtime.Provider`
interface already supports `runtime.StreamingProvider` (see
`internal/runtime/`). A future PR could:

1. Define a `runtime.StreamingProvider` contract with `Send(ctx, msg)`,
   `Subscribe() <-chan Event`, `SessionID() string`
2. Implement it directly in Go for the mayor-chat case (no shell-script
   detour, no named pipes, no shim daemon)
3. The mayor-chat transport becomes a Go-managed process; the SDK stream
   is consumed directly in the controller's address space
4. Human clients connect via an externally-exposed version of the same
   streaming interface (HTTP/SSE, gRPC, or socket)

This is a significant refactor — it replaces the exec protocol with a
real streaming API — but it would eliminate the daemon.mjs indirection,
the inbox plumbing, and the protocol-translation overhead. It would
also make provider authoring more rigorous: instead of "write a shell
script that implements 14 operations," it would be "implement this
interface in Go."

**Recommendation for the medium term:** ship the exec-protocol-based
mayor-chat provider first (this doc's design), learn from running it in
production, then propose the native Go streaming abstraction as a
follow-up design doc once requirements are clear.

**Harness generalization.** A future native Go streaming provider would
also be the right place to generalize beyond Claude Code. If `StreamingProvider`
is defined in terms of generic capabilities (send message, subscribe to
events, resume by session ID), then concrete implementations for Codex,
OpenCode, Cursor, etc. can be added as their respective harnesses mature
and expose the necessary APIs. That generalization is explicitly **not**
part of this design — it belongs in the Go-streaming-provider design doc.

## Scope clarity for current PRs

| PR | Scope |
|---|---|
| #552 | Headless exec provider for workers only. No mayor-chat involvement. |
| #553 | `contrib/mayor-chat` as a reference standalone client. Transport library. |
| #555 | Per-agent session provider routing. Unlocks mixed-provider cities, validated with the existing `claude -p` nudge model for both agents. |
| **Future PR** | **This design.** `gc-session-mayor-chat` exec provider + `daemon.mjs` + chat.mjs socket-client rewrite + crash recovery. |

PR #555's E2E test validates routing, not persistent streaming. Routing
works when both agents use the existing `claude -p` model (controller
nudges mayor and worker through their respective exec providers). The
mayor-chat-as-provider work is orthogonal.

## Open questions

1. **Does Gas City's reconciler automatically restart dead sessions,
   or does it mark them stuck and wait?** The crash recovery design
   assumes automatic restart. Needs verification in the reconciler code
   before we commit to this design.

2. **What is the right retry limit before escalation?** Default 3 is a
   guess. Depends on typical failure modes (transient network error vs
   auth failure vs SDK bug). Should be configurable.

3. **Should the socket protocol be JSON-RPC 2.0 or a bespoke schema?**
   JSON-RPC gives us request IDs and structured errors for free.
   Bespoke is simpler but lacks standardization. Leaning JSON-RPC.

4. **How does this interact with Gas City's auto-title feature?** The
   controller generates titles from session content by reading the
   transcript. With a persistent transport, there is no per-turn
   transcript file — transcripts are in Claude's session storage.
   Integration TBD.

5. **How should this generalize to non-Claude-Code harnesses, if at all?**
   This design is Claude Code-specific by intent. Do other harnesses
   (Codex, OpenCode, Cursor, Aider, etc.) need the same capability? If
   so, do they expose the required primitives (persistent stream,
   resume-by-ID, stable subprocess interface)? Answering this is a
   prerequisite for the future Go-streaming-provider design.

## References

- PR #552: `gc-session-docker-headless` — headless worker provider
- PR #553: `contrib/mayor-chat` — V1 streaming transport
- PR #555: per-agent session provider routing
- `@anthropic-ai/claude-agent-sdk` — `sdk.d.ts` documents `resume`,
  `sessionId`, `resumeSessionAt`, `forkSession` options on `query()`
- `engdocs/architecture/agent-protocol.md` — existing provider model
- `docs/reference/exec-session-provider.md` — exec protocol operations
