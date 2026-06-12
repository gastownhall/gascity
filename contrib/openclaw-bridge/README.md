# openclaw-bridge — hosting openclaw connectors on the Gas City extmsg fabric

**Proof of concept.** This bridge runs [openclaw](https://github.com/openclaw/openclaw)'s
shipped **iMessage connector — unmodified, straight from npm —** as a Gas City
extmsg out-of-process adapter. It answers the question *"can we import
openclaw's connectors with almost no work?"* with a working demo:

```
incoming iMessage                                            agent session
      │                                                            ▲
      ▼                                                            │ nudge (<system-reminder> with text)
 [imsg CLI] ◄─JSON-RPC─► [openclaw connector code] ◄──► [bridge.mjs] ◄──HTTP──► [gc extmsg fabric]
 (fake on Linux,          probeIMessage                  ~250 LoC                bindings / transcript
  real on a Mac)          createIMessageRpcClient        normalize + forward     / delivery receipts
                          sendMessageIMessage
```

Run it:

```bash
cd contrib/openclaw-bridge
./demo.sh            # builds gc if needed; GC_BIN=<path> to reuse a binary
```

The demo boots an isolated gc supervisor (own `GC_HOME`, port 9870), creates a
city, starts this bridge, binds a DM conversation to an agent session, then
shows: an inbound iMessage landing in the agent session as a nudge, and the
session's reply delivered back out through openclaw's send pipeline — including
markdown→native-formatting conversion done by *their* code. Everything is
sandboxed under `/tmp/gc-openclaw-bridge-demo` and torn down on exit.

## What is openclaw's vs ours

From the published `openclaw` npm package (pinned in `package-lock.json`), the
bridge imports and runs as-is:

| openclaw export | role |
|---|---|
| `probeIMessage` | imsg CLI handshake/capability probe at startup |
| `createIMessageRpcClient` | persistent `imsg rpc --json` JSON-RPC client; watch-mode inbound notifications |
| `sendMessageIMessage` | full outbound pipeline: target parsing, markdown formatting runs, chunking, receipts |
| `resolveIMessageInboundConversationId`, `normalizeIMessageHandle`, `formatIMessageChatTarget` | the iMessage conversation/handle id model |

Ours is the thin glue (`bridge.mjs`, ~250 lines): map openclaw's inbound
payload to gc's `ExternalInboundMessage`, map gc's `PublishRequest` to a
connector send, serve the `/publish` callback, register the adapter. Routing
policy (which session owns a conversation, fan-out, transcripts) stays
entirely in gc — openclaw's own routing/pairing/agent-dispatch layer is
deliberately *not* hosted, which is exactly the split gc's external-messaging
fabric design intends (`engdocs/design/external-messaging-fabric.md`).

## Wire mapping (openclaw ⇄ gc)

| openclaw `IMessagePayload` | gc `ExternalInboundMessage` |
|---|---|
| `guid` / `id` | `provider_message_id`, `dedup_key` |
| `sender` (normalized handle) | `actor.id`, dm `conversation_id` |
| `chat_id` (groups) | room `conversation_id` |
| `text` | `text` |
| `reply_to_id` | `reply_to_message_id` |
| `created_at` | `received_at` |

| gc `PublishRequest` | openclaw send |
|---|---|
| `conversation_id` (dm) | handle target (`+1555...`) |
| `conversation_id` (room) | `chat_id:N` target |
| `text` | `sendMessageIMessage(to, text, ...)` |
| `reply_to_message_id` | `replyToId` (native iMessage reply) |
| receipt `message_id` | `IMessageSendResult.messageId` (+ `guid` in metadata) |

## The fake imsg CLI

openclaw's connector drives a local `imsg` binary (macOS, talks to
Messages.app). `fake-imsg/imsg` implements the same protocol on Linux —
line-delimited JSON-RPC 2.0 daemon (`rpc --json`), probe surface
(`status --json`, `rpc --help`, `send-rich --help`) — backed by two files:

- append a line to `$FAKE_IMSG_DIR/inbox.jsonl` → connector receives it as an
  incoming message (`{"text":"hi","sender":"+1555...","is_group":false}` or plain text)
- every send lands in `$FAKE_IMSG_DIR/outbox.jsonl`

To run against **real iMessage**, point `IMSG_CLI_PATH` at a real `imsg`
binary on a signed-in Mac (or an SSH wrapper script; openclaw's send path
auto-detects remote-host wrappers) and start `bridge.mjs` with `GC_CITY`,
`GC_BASE_URL` set. Nothing else changes.

## Bridge configuration (env)

| var | default | meaning |
|---|---|---|
| `GC_CITY` | (required) | city name for `/v0/city/{name}/...` |
| `GC_BASE_URL` | `http://127.0.0.1:9871` | gc API base |
| `GC_SCOPE_ID` | `$GC_CITY` | `scope_id` stamped on every ConversationRef |
| `BRIDGE_PORT` | `8930` | callback server gc publishes to |
| `BRIDGE_PROVIDER` / `BRIDGE_ACCOUNT_ID` | `imessage` / `default` | adapter identity |
| `IMSG_CLI_PATH` | `./fake-imsg/imsg` | imsg binary (must be an explicit path on non-Mac — openclaw rejects a bare `imsg` off-macOS) |

## Findings (the actual point of the PoC)

What "import openclaw connectors" costs, learned by building this:

1. **The published npm package is enough.** `dist/extensions/imessage` ships in
   the tarball with clean entry-module exports for send/probe/id-model. No
   monorepo checkout, pnpm, or build step needed.
2. **One seam is not exported:** `createIMessageRpcClient` (the watch/inbound
   client) only exists in a hash-named dist chunk under a mangled alias.
   `lib/openclaw.mjs` resolves it by scanning dist export statements. Upstream
   ask: re-export the RPC client (and the notification parser) from the
   extension's public API.
3. **Don't host their monitor/dispatch layer.** `monitorIMessageProvider` is
   hard-wired into openclaw's own pairing/agent-reply pipeline (and its dm
   policy would fight gc's routing). The right seam is one level down: RPC
   client + notification payloads. This maps cleanly onto gc's
   adapter-normalizes/core-routes split.
4. **gc contract gaps to close for parity** (tracked in beads): outbound is
   text-only (`PublishRequest` has no media/typed-payload variant), receipts
   are single-id (openclaw's `MessageReceipt` is multi-part with edit/delete
   tokens), and `AdapterCapabilities`' three booleans can't express openclaw's
   capability vocabulary. Reactions/tapbacks are skipped by this bridge for
   the same reason — no gc representation yet. Known PoC simplifications on
   the bridge itself: every publish failure is reported as `transient`, and
   the `/publish` callback has no auth token — fine against the fake backend,
   close both before pointing it at a real iMessage account.
5. **Latency note:** `POST /extmsg/inbound` took ~5s per call in the demo
   environment (bead-store side, unrelated to the connector path, which
   measured ~30ms; tracked in beads).

A Slack/Discord/Telegram bridge would follow the same shape; their plugins are
bigger but the bridge-facing surface (send adapter + inbound normalization +
id model) is the same family of exports.
