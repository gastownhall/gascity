---
title: "Messaging"
---

> Last verified against code: 2026-03-04

## Summary

Messaging is a Layer 2-4 derived mechanism that provides inter-agent
communication without introducing new primitives. Mail is composed
from the Bead Store (`TaskStore.Create(bead{type:"message"})`), and
nudge is composed from the Agent Protocol
(`runtime.Provider.Nudge()`). No new infrastructure is needed — messaging
is a thin composition layer proving the primitives are sufficient.

## Key Concepts

- **Mail**: A message bead — a bead with `Type="message"`. `From` is the
  sender, `Assignee` is the recipient, `Title` holds the subject line, and
  `Description` holds the message body.
  Open beads without a "read" label are unread; beads with the "read"
  label are read but still accessible; closed beads are archived.

- **Inbox**: The set of open, unread message beads assigned to a
  recipient. Queried by filtering for `Type="message"`, `Status="open"`,
  `Assignee=recipient`, and absence of the "read" label.

- **Read vs Archive**: Reading a message adds the "read" label but
  keeps the bead open — the message remains accessible via `Get` or
  `Thread`. Archiving closes the bead permanently. This matches
  upstream Gastown behavior.

- **Threading**: Each message gets a `thread:<id>` label. Replies
  inherit the parent's thread ID and add a `reply-to:<id>` label.
  `Thread(threadID)` queries all messages sharing a thread.

- **Archive**: Closing a message bead. Idempotent via
  `ErrAlreadyArchived`.

- **Nudge**: Text sent directly to an agent's session to wake or
  redirect it. Delivered via `runtime.Provider.Nudge()`. Configured
  per-agent in `Agent.Nudge`. Not persisted — fire-and-forget.

- **Provider**: The pluggable mail backend interface. Two
  implementations: beadmail (default, backed by `beads.Store`) and
  exec (user-supplied script).

## Architecture

```
                    ┌─────────────┐
                    │ gc mail CLI │
                    └──────┬──────┘
                           │
                    ┌──────▼──────┐
                    │ mail.Provider│
                    └──────┬──────┘
                    ┌──────┴──────┐
              ┌─────▼─────┐ ┌────▼────┐
              │  beadmail  │ │  exec   │
              │ (default)  │ │ (script)│
              └─────┬──────┘ └─────────┘
                    │
              ┌─────▼──────┐
              │ beads.Store │
              └────────────┘
```

### Data Flow

**Sending a message (beadmail path):**

1. `gc mail send agent-1 -s "Hello" -m "body text"` invokes `Provider.Send("sender", "agent-1", "Hello", "body text")`
2. beadmail calls `store.Create(Bead{Title:"Hello", Description:"body text", Type:"message", Assignee:"agent-1", From:"sender", Labels:["thread:<id>"]})`
3. Store assigns ID, sets Status="open", returns the bead
4. beadmail converts to `mail.Message` and returns

**Checking inbox:**

1. `gc mail inbox` invokes `Provider.Inbox("agent-1")`
2. beadmail calls `store.List()` and filters for `Type="message"`, `Status="open"`, `Assignee="agent-1"`, no "read" label
3. Returns matching messages as `[]Message` with Subject, Body, ThreadID, etc.

**Reading a message:**

1. `Provider.Read(id)` retrieves the bead via `store.Get(id)`
2. Adds "read" label via `store.Update(id, UpdateOpts{Labels: ["read"]})`
3. The bead remains open — still accessible via Get, Thread, Count
4. Returns the message

**Replying to a message:**

1. `Provider.Reply(id, from, subject, body)` retrieves the original bead
2. Inherits the original's `thread:<id>` label, adds `reply-to:<original-id>`
3. Creates a new message bead addressed to the original sender

### Key Types

- **`mail.Provider`** — interface with Send, Inbox, Get, Read, MarkRead,
  MarkUnread, Archive, Delete, Check, Reply, Thread, Count methods.
  Defined in `internal/mail/mail.go`.
- **`mail.Message`** — ID, From, To, Subject, Body, CreatedAt, Read,
  ThreadID, ReplyTo, Priority, CC. The transport struct returned by all
  Provider methods.
- **`beadmail.Provider`** — default implementation backed by
  `beads.Store`. Defined in `internal/mail/beadmail/beadmail.go`.
- **`mail.ErrAlreadyArchived`** — sentinel error for idempotent
  archive calls.
- **`mail.ErrNotFound`** — sentinel error for Get/Read of nonexistent
  messages.

## Invariants

1. **Messages are beads.** Every message has a corresponding bead with
   `Type="message"`. No separate message storage exists. `Type="message"`
   is the authoritative discriminator — the legacy `gc:message` label is
   neither written nor read.
2. **Inbox returns only open, unread messages.** Read messages (with
   "read" label) and closed (archived) beads are excluded from inbox.
3. **Read does not close.** `Read(id)` adds the "read" label but keeps
   the bead open. The message remains accessible via `Get`, `Thread`,
   and `Count`. Only `Archive`/`Delete` closes the bead.
4. **Archive is idempotent.** Archiving an already-archived message
   returns `ErrAlreadyArchived`, not a generic error.
5. **Check and Get do not mutate state.** Unlike Read, Check and Get
   return messages without adding the "read" label.
6. **Threading is label-based.** Each message has a `thread:<id>` label.
   Replies inherit the parent's thread ID. `Thread(id)` queries by label.
7. **Nudge is fire-and-forget.** There is no delivery guarantee,
   persistence, or retry for nudges. If the session is not running,
   the nudge is lost.

## Interactions

| Depends on | How |
|---|---|
| `internal/beads` | beadmail stores messages as beads |
| `internal/runtime` | Nudge delivered via Provider.Nudge() |

| Depended on by | How |
|---|---|
| `cmd/gc/cmd_mail.go` | CLI commands: send, inbox, read, peek, reply, archive, delete, mark-read, mark-unread, thread, count |
| `cmd/gc/cmd_hook.go` | Hook checks for unread mail via Check() |
| Agent prompts | Templates reference `gc mail` commands |

## Code Map

- `internal/mail/mail.go` — Provider interface, Message struct, ErrAlreadyArchived
- `internal/mail/fake.go` — test double
- `internal/mail/fake_conformance_test.go` — conformance tests for fakes
- `internal/mail/beadmail/beadmail.go` — bead-backed implementation
- `internal/mail/exec/` — script-based mail provider
- `internal/mail/mailtest/` — test helpers
- `cmd/gc/cmd_mail.go` — CLI commands

## Configuration

```toml
[mail]
provider = "beadmail"   # default; or "exec" for script-based
```

The exec provider runs a user-supplied script for each mail operation,
allowing integration with external messaging systems.

## Testing

- `internal/mail/fake_conformance_test.go` — verifies the fake
  satisfies the Provider contract
- `internal/mail/beadmail/` — unit tests for bead-backed provider
- `test/integration/mail_test.go` — integration tests with real beads

## Message Lifecycle

```
Send → [unread, open]
  ├── Read → [read label, open] (still in Get/Thread/Count)
  │     ├── MarkUnread → [unread, open] (back to inbox)
  │     └── Archive → [closed] (permanent)
  ├── Peek/Get → [unread, open] (no state change)
  └── Archive/Delete → [closed] (permanent, skips read)
```

## Known Limitations

- **beadmail.Inbox scans all beads.** Uses `store.List()` with
  client-side filtering. No server-side query for type + status +
  assignee. Acceptable for current scale.
- **No delivery confirmation.** Neither mail nor nudge provides
  read receipts or delivery guarantees.

## See Also

- [Bead Store](beads.md) — messages are stored as beads; understanding
  bead lifecycle explains mail lifecycle
- [Agent Protocol](agent-protocol.md) — Nudge() delivery mechanism
- [Glossary](glossary.md) — authoritative definitions of mail, nudge,
  and related terms
