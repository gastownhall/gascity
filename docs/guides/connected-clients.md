---
title: Connect an external LLM client
description: Register an external LLM client, open a reply stream, and send turns to a Gas City session over HTTP and SSE.
---

Gas City's external messaging API lets any HTTP client act as a participant in
a city session — registering itself, opening a durable SSE reply stream, and
sending turns that the session agent receives as inbound messages. This guide
walks the full protocol: register → subscribe → send, plus reconnection and
error handling.

## Overview

Three HTTP calls form the entire protocol:

1. **Register** — `POST /v0/extmsg/clients` — obtain a bearer token that
   identifies your client to the city.
2. **Subscribe** — `GET /v0/extmsg/{provider}/{account_id}/{conversation_id}/subscribe`
   — open a long-lived SSE stream to receive the session's replies.
3. **Send** — `POST /v0/extmsg/inbound` (with `provider: "llm-client"`) —
   deliver a turn from your client to the session.

The subscribe stream stays open between turns. You send a turn via the inbound
endpoint, and the session's reply arrives as an SSE `message` event on the
already-open stream.

## Prerequisites

- A running city with external messaging enabled in `city.toml`:
  ```toml
  [extmsg]
  enabled = true
  ```
- The session you want to connect must exist in the city's active config.
- The `gc` supervisor must be reachable at its HTTP address (default: `http://localhost:7375`).

## Step 1 — Register and get a client token

```http
POST /v0/extmsg/clients
Content-Type: application/json
X-GC-Request: 1

{
  "provider": "llm-client",
  "display_name": "my-bot"
}
```

**Response (201 Created):**

```json
{
  "client_id": "lcl-a1b2c3d4",
  "token": "eyJ..."
}
```

Store the `client_id` and `token`. The `token` is a bearer credential used on
the subscribe endpoint. The `client_id` appears as the `account_id` path segment
in subscribe and inbound requests.

Tokens do not expire automatically. An operator can revoke a token via the API
or `city.toml` allowlist changes; your client receives a `token_revoked` SSE
error event if this happens while a stream is open.

## Step 2 — Subscribe to the reply stream

Open the SSE stream before sending any turns, so you do not miss the reply:

```http
GET /v0/extmsg/llm-client/{account_id}/{conversation_id}/subscribe
Authorization: Bearer {token}
Accept: text/event-stream
```

Replace `{account_id}` with your `client_id` and `{conversation_id}` with any
stable string identifying this conversation (e.g., a UUID you generate).

**On success** the server responds `200 Content-Type: text/event-stream` and
holds the connection open, emitting events as the session agent produces replies.

Message events arrive as:

```
id: 42
event: message
data: {"version":"1","seq":42,"role":"assistant","content":"Hello from the session."}

```

The `id:` field is the sequence number of the last successfully delivered message.
Pass it as `Last-Event-ID` on reconnect to resume from that point.

## Step 3 — Send a turn

Once the stream is open, deliver your turn:

```http
POST /v0/extmsg/inbound
Content-Type: application/json
X-GC-Request: 1

{
  "provider": "llm-client",
  "account_id": "{account_id}",
  "conversation_id": "{conversation_id}",
  "content": "What is the status of the city?"
}
```

The first inbound turn for a `(account_id, conversation_id)` pair implicitly
creates the conversation binding. Subsequent turns reuse it.

**Response (202 Accepted):** the turn has been queued. The session agent's
reply will arrive on the SSE stream opened in Step 2.

## Reconnecting after a drop

If your SSE connection drops, reconnect by including `Last-Event-ID` with the
sequence number of the last `message` event you received:

```http
GET /v0/extmsg/llm-client/{account_id}/{conversation_id}/subscribe
Authorization: Bearer {token}
Accept: text/event-stream
Last-Event-ID: 42
```

The server replays all messages after sequence 42 before resuming live delivery.
Error and heartbeat events do not advance the replay cursor and are not replayed.

## Error catalog

### HTTP errors — before the stream is established

These are standard HTTP responses returned before the server commits
`Content-Type: text/event-stream`.

| Status | Code | Condition |
|--------|------|-----------|
| `401 Unauthorized` | `unauthorized` | Bearer token missing or unrecognized. |
| `403 Forbidden` | `session_forbidden` | Token valid but not permitted to bind to the requested session. |
| `404 Not Found` | `session_not_found` | Session name does not exist in the city's active config. |
| `404 Not Found` | `binding_not_found` | ConversationRef presented but no binding exists; send a first inbound turn to create it. |
| `503 Service Unavailable` | `extmsg_unavailable` | External messaging is not enabled, or the controller is not ready. |

Error body shape:

```json
{
  "code": "session_forbidden",
  "message": "Token not authorized to bind to session 'mayor'."
}
```

### SSE error events — after the stream is established

Once the stream is open, errors arrive as SSE events:

```
id: error
event: error
data: {"version":"1","code":"session_stopped","message":"...","retryable":true,"retry_after_ms":5000}

```

`id: error` is a literal sentinel — it does not advance your replay cursor.

| Code | Retryable | Retry hint | Trigger |
|------|-----------|------------|---------|
| `session_stopped` | yes | 5 000 ms | Session stopped cleanly; may restart. |
| `session_not_found` | yes | 10 000 ms | Session removed from config while stream was open. |
| `server_shutdown` | yes | 3 000 ms | Controller process shutting down; honor the SSE `retry:` field too. |
| `idle_timeout` | yes | 0 ms | Idle stream closed (reserved; not active in v1). |
| `token_revoked` | no | — | Operator revoked the token; re-register with `POST /v0/extmsg/clients`. |
| `binding_removed` | no | — | Conversation binding removed; send a new inbound turn to recreate. |
| `account_mismatch` | no | — | `account_id` in the URL does not match the token's `client_id`; programming error. |

After emitting an error event the server closes the connection. For retryable
codes, reconnect after the `retry_after_ms` hint (with exponential backoff).

## Heartbeat

The server emits a heartbeat every 30 s when no message or error has been sent:

```
event: heartbeat
data: {"version":"1","ts":"2026-06-19T19:45:00Z"}

```

Heartbeats carry no `id:` field and do not advance the replay cursor. Use them
to distinguish a quiet-but-alive connection from a truly dead one.

## Configuration

The `[extmsg]` section in `city.toml` controls the connected-client surface:

```toml
[extmsg]
enabled = true

# Optional: restrict which sessions connected clients may bind to.
# If omitted, any session in the city is bindable.
allowed_sessions = ["mayor", "deacon"]
```

Tokens issued before `allowed_sessions` is narrowed retain access until the
controller reloads config; the next subscribe attempt will fail with
`session_forbidden`.

## Go example

A minimal end-to-end example using `net/http`:

```go
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const base = "http://localhost:7375"

func main() {
	// 1. Register.
	regBody, _ := json.Marshal(map[string]string{
		"provider":     "llm-client",
		"display_name": "example-bot",
	})
	resp, _ := http.Post(base+"/v0/extmsg/clients", "application/json", bytes.NewReader(regBody))
	var reg struct {
		ClientID string `json:"client_id"`
		Token    string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()

	accountID := reg.ClientID
	convID := "conv-001"

	// 2. Subscribe.
	req, _ := http.NewRequest("GET",
		fmt.Sprintf("%s/v0/extmsg/llm-client/%s/%s/subscribe", base, accountID, convID), nil)
	req.Header.Set("Authorization", "Bearer "+reg.Token)
	req.Header.Set("Accept", "text/event-stream")
	stream, _ := http.DefaultClient.Do(req)

	// 3. Send a turn.
	inbound, _ := json.Marshal(map[string]string{
		"provider":        "llm-client",
		"account_id":      accountID,
		"conversation_id": convID,
		"content":         "Hello from the external client!",
	})
	sendReq, _ := http.NewRequest("POST", base+"/v0/extmsg/inbound",
		bytes.NewReader(inbound))
	sendReq.Header.Set("Content-Type", "application/json")
	sendReq.Header.Set("X-GC-Request", "1")
	http.DefaultClient.Do(sendReq)

	// Read one reply event.
	scanner := bufio.NewScanner(stream.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			fmt.Println("Reply:", strings.TrimPrefix(line, "data:"))
			break
		}
	}
	stream.Body.Close()
}
```
