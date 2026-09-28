# Mayor Chat

An Ink terminal UI for holding a multi-turn conversation with a
containerized Claude Code agent, built on the Claude Agent SDK's streaming
input mode.

This is contrib reference code: it demonstrates a pattern, it is not part of
the `gc` binary and is not on the same support tier as Gas City itself.

## Layout

| File | Responsibility |
|------|----------------|
| `transport.mjs` | `MayorTransport` — session and streaming plumbing. No UI. |
| `chat.mjs` | The Ink terminal UI. No SDK knowledge beyond the transport. |

The split is the useful part. `MayorTransport` holds one `query()` stream
open for the life of the process, so conversation context survives across
turns, and it exposes a single `async *send(prompt)` that yields SDK
messages until the turn's `result` arrives. Everything the terminal UI does
with those messages — rendering deltas, tool rows, cost summaries — a
Telegram bot, a Slack app, or an HTTP endpoint could do instead, without
touching the transport. `chat.mjs` is one frontend, not the point.

## Tests

```bash
npm ci
npm test    # 13 tests
```

`npm ci` is required first: `transport.mjs` statically imports
`@anthropic-ai/claude-agent-sdk`. The tests never *invoke* the SDK — they
inject a mock `query` via the `_queryFn` option — but the import must
resolve.

## Running it

Build the image:

```bash
docker build -t mayor-chat -f contrib/mayor-chat/Dockerfile contrib/mayor-chat/
```

Run standalone:

```bash
docker run -d --name mayor-chat --init mayor-chat sleep infinity
docker exec -it mayor-chat node /app/chat.mjs
```

The image sets `CMD`, not `ENTRYPOINT`, so the keepalive command above
replaces it. That is also what makes the image usable with
`scripts/gc-session-docker`, which starts every container as
`<image> sleep infinity` and drives the agent through `tmux` inside it —
hence the `tmux` install in the Dockerfile.

Credentials are discovered by the Agent SDK from `$HOME/.claude/` at
runtime. Mount them (`GC_DOCKER_HOME_MOUNT=true` under `gc-session-docker`,
or your own `-v`). Never bake them into the image.

### `docker exec` needs `-it`

Ink puts stdin into raw mode. Without an attached TTY it throws
`Raw mode is not supported`, so the `-it` flags on `docker exec` are
required, not cosmetic. This is why the UI cannot be the container's
foreground process under `gc-session-docker` — `tmux` supplies the TTY.

## Tool permissions

Stock defaults deny every tool call:

```js
new MayorTransport();   // permissionMode: "dontAsk", allowedTools: []
```

`permissionMode: "dontAsk"` means "deny anything not pre-approved", and
`allowedTools` is the auto-**approve** list — not a restriction list. So a
default mayor can converse and nothing else. That is deliberate: a headless
container has nobody to answer a permission prompt.

Grant capability explicitly:

```js
new MayorTransport({ allowedTools: ["Bash", "Read", "Edit"] });
```

To restrict which tools exist at all rather than which are auto-approved,
pass the SDK's `tools` option instead. Any option other than
`includePartialMessages` is forwarded to `query()` as given;
`includePartialMessages` stays pinned to `true` because token-by-token
streaming is the whole premise of this transport.

## `send()` is not re-entrant

Concurrent `send()` calls share one output iterator and will interleave
each other's messages. `chat.mjs` is safe because its `busy` flag blocks
input during a turn. **A bot frontend must serialize turns itself** — queue
per conversation, or run one transport per conversation.

Call `transport.close()` when done. It ends the prompt iterable *and* calls
`Query.close()`, which terminates the `claude` CLI subprocess; ending the
iterable alone leaves an in-flight generation running.
