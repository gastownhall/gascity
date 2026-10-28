---
title: Connect an external client
description: Bring a chat, voice, or bot front end into a city conversation — send turns to an agent over HTTP and receive its replies on a callback.
---

A city's agents usually hear from people through `gc` or the dashboard. When
you want them reachable from somewhere else — a voice front end, a chat app, a
bot, another service — you connect that client to the city's **external
messaging** API. The client posts each turn in; the agent answers with a reply
command; the city delivers the answer to a callback URL the client serves.

This guide builds a working client for a provider named `voice`. Substitute
your own provider name throughout.

## How it works

| Step | Who | What happens |
|---|---|---|
| 1 | City | A pack imported as `voice` supplies `gc voice reply-current`, the command agents use to answer. |
| 2 | City | `[[extmsg.default_route]]` (or an explicit bind) routes `voice` conversations to an agent. |
| 3 | Client | Serves `POST /publish` and registers it as the adapter for `voice`. |
| 4 | Client | `POST /v0/city/{cityName}/extmsg/inbound` delivers a turn; the agent is notified. |
| 5 | Agent | Runs `gc voice reply-current`, which posts the reply to `/v0/city/{cityName}/extmsg/outbound`. |
| 6 | City | Calls the client's `/publish` with the reply text. |

Every endpoint is city-scoped under `/v0/city/{cityName}/extmsg/` on the
supervisor API (default `http://127.0.0.1:8372`). Mutations need the
`X-GC-Request` header — see [Supervisor REST API](/reference/api).

## Identify the conversation

Every turn and reply carries a **conversation reference**. Pick fixed values
for your client and a stable ID per conversation:

| Field | Example | Meaning |
|---|---|---|
| `provider` | `voice` | Your client's provider name. Lowercase; must match the pack import binding in step 1. |
| `account_id` | `voice-frontend` | Which deployment of the client. The adapter registers per `(provider, account_id)`. |
| `scope_id` | `voice` | Any stable string that namespaces your conversation IDs. |
| `conversation_id` | `call-001` | One conversation — a call, a chat thread, a channel. |
| `kind` | `dm` | `dm`, `room`, or `thread`. |

## 1. Give agents a reply command

When a turn arrives, the agent is told to answer by running
`gc <provider> reply-current --conversation-id <id> --body-file <path>`. That
command comes from a pack, so add a small one to the city and import it under
your provider name:

```
packs/voice/
├── pack.toml
└── commands/reply-current/
    ├── command.toml
    └── run.sh
```

```toml
# packs/voice/pack.toml
[pack]
name = "voice"
schema = 2
```

```toml
# packs/voice/commands/reply-current/command.toml
description = "Send the current agent's reply to a voice conversation"
```

`run.sh` turns the agent's reply into an outbound publish. It runs inside the
agent's session, so `GC_SESSION_ID` identifies the replying agent. `gc` sets
`GC_CITY_NAME` for pack commands, and `GC_SUPERVISOR_URL` when the supervisor
API is plain `http` on loopback. It needs `curl` and `jq`:

```sh
#!/bin/sh
# gc voice reply-current --conversation-id ID --body-file PATH
set -eu

# Supervisor API; set VOICE_GC_API where gc leaves GC_SUPERVISOR_URL unset.
api="${GC_SUPERVISOR_URL:-${VOICE_GC_API:-http://127.0.0.1:8372}}"
api="${api%/}"
conversation_id=""
body_file=""
while [ $# -gt 0 ]; do
  case "$1" in
    --conversation-id) conversation_id="$2"; shift 2 ;;
    --body-file) body_file="$2"; shift 2 ;;
    *) echo "gc voice reply-current: unknown argument $1" >&2; exit 2 ;;
  esac
done
: "${conversation_id:?--conversation-id is required}"
: "${body_file:?--body-file is required}"
: "${GC_SESSION_ID:?run this from inside an agent session}"

# The conversation reference must match what the client sends inbound.
if ! resp=$(jq -n \
      --arg session "$GC_SESSION_ID" \
      --arg conv "$conversation_id" \
      --rawfile text "$body_file" \
      '{session_id: $session, text: $text,
        conversation: {scope_id: "voice", provider: "voice",
                       account_id: "voice-frontend", conversation_id: $conv, kind: "dm"}}' |
    curl -sS --fail-with-body \
      -H 'Content-Type: application/json' -H 'X-GC-Request: 1' \
      --data-binary @- \
      "$api/v0/city/$GC_CITY_NAME/extmsg/outbound"); then
  printf '%s\n' "$resp" >&2   # the API's error, such as no adapter registered
  exit 1
fi
# A nonzero exit tells the agent the reply did not land; the failure kind says why.
printf '%s' "$resp" | jq -e '.Receipt.Delivered' >/dev/null || {
  printf 'reply not delivered: %s\n' "$(printf '%s' "$resp" | jq -r '.Receipt.FailureKind')" >&2
  exit 1
}
```

Make it executable (`chmod +x`) and import the pack in the city's `pack.toml`.
The import binding name — `voice` here — is the command namespace, so it must
equal your provider name:

```toml
[imports.voice]
source = "./packs/voice"
```

`gc voice --help` now lists `reply-current`. See
[Create and Share Packs](/guides/shareable-packs) for more on pack commands;
the Command Directory section of the
[pack specification](/reference/specs/pack-spec) lists the environment they
run with.

## 2. Route conversations to an agent

A turn reaches an agent only when its conversation is routed. The simplest
route sends every new `voice` conversation to one agent:

```toml
# city.toml
[[extmsg.default_route]]
provider = "voice"
agent = "mayor"            # a configured named session
# account_id = "voice-frontend"   # optional: route only this account
```

The first turn of a conversation binds it to that agent, and later turns
follow the binding. To route a specific conversation somewhere else, bind it
before its first turn with `gc extmsg bind` or
`POST /v0/city/{cityName}/extmsg/bind`.

## 3. Serve a callback and register the adapter

Replies arrive as `POST {callback_url}/publish`:

```json
{
  "session_id": "vo-wisp-9cb",
  "conversation": {"scope_id": "voice", "provider": "voice", "account_id": "voice-frontend",
                   "conversation_id": "call-001", "kind": "dm"},
  "text": "**mayor:** The build is green."
}
```

Answer `2xx` with a receipt that echoes the conversation. `delivered: true` is
what tells the agent its reply landed:

```json
{
  "message_id": "voice-1727630104",
  "conversation": {"scope_id": "voice", "provider": "voice", "account_id": "voice-frontend",
                   "conversation_id": "call-001", "kind": "dm"},
  "delivered": true
}
```

Then register the callback for your `(provider, account_id)`:

```http
POST /v0/city/my-city/extmsg/adapters
Content-Type: application/json
X-GC-Request: 1

{
  "provider": "voice",
  "account_id": "voice-frontend",
  "name": "voice front end",
  "callback_url": "http://127.0.0.1:9180"
}
```

`callback_url` is the base URL; omit the trailing slash, because gc appends
`/publish`.

The response is `201 Created`; registering the same `(provider, account_id)`
again replaces the earlier entry. Registrations live only in the running
city's memory, so register on every client start and again whenever the city
starts or restarts — not only when the supervisor restarts. A restart can
happen between turns without the client seeing it, so the simplest client
registers again before every turn, as the [Go example](#go-example) does.
Each registration emits an `extmsg.adapter_added` event, so that client adds
one event per turn to the city's event log.

## 4. Send a turn

```http
POST /v0/city/my-city/extmsg/inbound
Content-Type: application/json
X-GC-Request: 1

{
  "message": {
    "provider_message_id": "call-001-1",
    "conversation": {"scope_id": "voice", "provider": "voice", "account_id": "voice-frontend",
                     "conversation_id": "call-001", "kind": "dm"},
    "actor": {"id": "caller-1", "display_name": "Caller", "is_bot": false},
    "text": "Is the build green?",
    "received_at": "2026-09-29T18:34:41Z"
  }
}
```

A `200` returns the routing result. `TargetAgentName` or `TargetSessionID`
names who received the turn; both empty means no route matched and the turn
was dropped. The agent is notified in the background, so its reply arrives
later on your callback.

## Delivery semantics

Sending a turn:

| Inbound response | Meaning | What the client does |
|---|---|---|
| `404` with `code` `city-not-found` | The city is not running: it is starting, restarting, or stopped. | Wait until `GET /v0/cities` lists it with `running: true`, register the adapter again, then resend with the same `provider_message_id`. |
| No response (connection refused or dropped) | The supervisor is restarting, or the answer was lost. | Same as `city-not-found`. |
| `401` / `403` | The write was refused: a missing or rejected write grant (`X-GC-City-Write`), a missing `X-GC-Request` header, or a read-only supervisor (one bound to a non-localhost address). | Fix the credential or configuration, then resend with the same `provider_message_id`. |
| other `4xx` | The turn is malformed or its state is corrupt. | Drop it; retrying fails the same way. |
| `5xx` | A store fault; the agent was not notified. A `503` with `detail` `external messaging not enabled` does not clear on its own: the city's bead store did not open, and an operator must repair it and restart the city. | Retry with the same `provider_message_id`. |
| `200`, no target | No binding or route matched. | Add a route or bind the conversation. |

The city-scoped routes answer only while the city is running; see
[City Scope](/reference/api#city-scope) for that readiness boundary.

The transcript keeps one entry per `provider_message_id`, so a retry never
duplicates the turn. The agent is notified on every successful attempt,
though, so retrying a turn whose `200` was lost — a timeout or a dropped
connection — can notify it twice.

Receiving a reply — how your `/publish` response is reported back to the
agent's reply command:

| `/publish` response | Receipt |
|---|---|
| `2xx` with `"delivered": true` | delivered |
| `2xx` without `"delivered": true` | not delivered, with your receipt's `failure_kind` (empty if omitted) |
| `401` / `403` | `auth` |
| `404` | `not_found` |
| `429` | `rate_limited` |
| other `4xx` | `permanent` |
| `5xx`, unreachable, a 30-second timeout, or an unparseable body | `transient` |

To report a reply you received but could not deliver, answer `2xx` with
`"delivered": false` and a `failure_kind` of `transient`, `rate_limited`,
`permanent`, `auth`, `not_found`, or `unsupported`.

The city does not retry a failed publish and does not record it in the
conversation transcript; the reply command prints the failure kind and exits
nonzero, so the agent sees why its reply did not land. Delivered replies are
recorded, and
`GET /v0/city/{cityName}/extmsg/transcript` (filtered by the conversation
fields) lists a conversation's history — useful for catching up after a
client restart.

## Go example

A line-oriented client that follows the delivery rules above, except that it
gives up on a refused write: it serves `/publish`, registers itself, and sends
each line of stdin as a turn in conversation `call-001`, waiting out a city
restart instead of dropping the turn.

```go
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	apiBase     = "http://127.0.0.1:8372" // gc supervisor API
	cityName    = "my-city"
	provider    = "voice" // must match the city's pack import binding
	accountID   = "voice-frontend"
	scopeID     = "voice"
	listenAddr  = "127.0.0.1:9180"
	callbackURL = "http://" + listenAddr // no trailing slash: gc appends /publish
	retryDelay  = 2 * time.Second
)

var (
	cityAPI = apiBase + "/v0/city/" + url.PathEscape(cityName) + "/extmsg"
	client  = &http.Client{Timeout: 30 * time.Second}
	// errNoAnswer marks a request the supervisor did not answer in full: the
	// connection was refused, dropped, or timed out, as it can be mid-restart.
	errNoAnswer = errors.New("no answer from the supervisor")
)

type conversationRef struct {
	ScopeID        string `json:"scope_id"`
	Provider       string `json:"provider"`
	AccountID      string `json:"account_id"`
	ConversationID string `json:"conversation_id"`
	Kind           string `json:"kind"`
}

type publishRequest struct {
	SessionID    string          `json:"session_id"`
	Conversation conversationRef `json:"conversation"`
	Text         string          `json:"text"`
}

type publishReceipt struct {
	MessageID    string          `json:"message_id"`
	Conversation conversationRef `json:"conversation"`
	Delivered    bool            `json:"delivered"`
}

// inboundResult is the routing result of an accepted turn.
type inboundResult struct {
	TargetAgentName string
	TargetSessionID string
}

// apiError is a non-2xx answer from the supervisor API.
type apiError struct {
	Status int
	Code   string // the problem's machine-readable code, such as city-not-found
	Body   string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

func main() {
	// Replies from the city arrive here.
	http.HandleFunc("POST /publish", func(w http.ResponseWriter, r *http.Request) {
		var req publishRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		fmt.Printf("agent: %s\n", req.Text)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(publishReceipt{
			MessageID:    fmt.Sprintf("voice-%d", time.Now().UnixNano()),
			Conversation: req.Conversation,
			Delivered:    true,
		}); err != nil {
			log.Printf("writing receipt: %v", err)
		}
	})
	go func() { log.Fatal(http.ListenAndServe(listenAddr, nil)) }()

	if err := retry("registering adapter", register); err != nil {
		log.Fatalf("registering adapter: %v", err)
	}

	conv := conversationRef{
		ScopeID:        scopeID,
		Provider:       provider,
		AccountID:      accountID,
		ConversationID: "call-001",
		Kind:           "dm",
	}
	runID := time.Now().UnixNano() // turn IDs must not repeat across runs
	lines := bufio.NewScanner(os.Stdin)
	for n := 1; lines.Scan(); n++ {
		turn := map[string]any{
			"provider_message_id": fmt.Sprintf("%s-%d-%d", conv.ConversationID, runID, n),
			"conversation":        conv,
			"actor":               map[string]any{"id": "caller-1", "display_name": "Caller", "is_bot": false},
			"text":                lines.Text(),
			"received_at":         time.Now().UTC(),
		}
		// Register before every turn: a city that restarted since the last
		// turn has forgotten the adapter, and registering again is an upsert.
		err := retry("sending turn", func() error {
			if err := register(); err != nil {
				return err
			}
			return sendTurn(turn)
		})
		if err != nil {
			log.Printf("turn %d not delivered: %v", n, err)
		}
	}
	if err := lines.Err(); err != nil {
		log.Fatalf("reading stdin: %v", err)
	}
	select {} // keep receiving replies after stdin closes
}

// retry runs op until it succeeds or fails in a way resending cannot fix.
func retry(what string, op func() error) error {
	for {
		err := op()
		if err == nil || !retryable(err) {
			return err
		}
		log.Printf("%s: %v; retrying in %s", what, err, retryDelay)
		time.Sleep(retryDelay)
	}
}

// retryable reports whether resending can succeed: the supervisor did not
// answer, the city is not running, or a transient fault (5xx). Registering
// answers city-not-found until the city runs, so retrying it doubles as the
// readiness check.
func retryable(err error) bool {
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 500 || apiErr.Code == "city-not-found"
	}
	return errors.Is(err, errNoAnswer)
}

func register() error {
	return post(cityAPI+"/adapters", map[string]any{
		"provider":     provider,
		"account_id":   accountID,
		"name":         "voice front end",
		"callback_url": callbackURL,
	}, nil)
}

func sendTurn(turn map[string]any) error {
	var result inboundResult
	if err := post(cityAPI+"/inbound", map[string]any{"message": turn}, &result); err != nil {
		return err
	}
	if result.TargetAgentName == "" && result.TargetSessionID == "" {
		return errors.New("no route matched; add a route or bind the conversation")
	}
	return nil
}

// post sends a mutation and decodes a 2xx answer into out when out is not nil.
func post(u string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "1")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", errNoAnswer, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("%w: %v", errNoAnswer, err)
	}
	if resp.StatusCode >= 300 {
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &problem) // only problem+json bodies carry a code
		return &apiError{Status: resp.StatusCode, Code: problem.Code, Body: string(data)}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
```

```text
$ echo "Is the build green?" | go run .
agent: **mayor:** The build is green.
```

The example sends no `X-GC-City-Write` grant. On a city that requires one,
its first registration is refused with `401` and the client exits; a turn
refused later with `401` or `403` is logged as not delivered and skipped.
Fixing a refused write needs an operator, so a production client keeps the
turn and resends it with the same `provider_message_id` after the fix.
