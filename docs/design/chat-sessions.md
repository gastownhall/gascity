# Chat Sessions

| Field | Value |
|---|---|
| Status | Draft |
| Date | 2026-03-07 |
| Author(s) | Claude, Steve |
| Issue | — |
| Supersedes | — |

## Summary

Gas City gains a **chat session** resource: a persistent, resumable
conversation between a human and an agent configuration. Sessions are
backed by beads (type `"session"`) for persistence and use existing
`session.Provider` infrastructure for runtime. The key optimization:
suspended sessions consume zero runtime resources (no tmux session, no
provider process) while preserving the ability to resume the exact
conversation via provider-native resume facilities (`claude --resume`,
etc.). Agent configs serve as templates — multiple concurrent sessions
can exist for the same template. New CLI commands (`gc session new`,
`gc session list`, `gc session attach`, `gc session suspend`, `gc session close`)
and API endpoints (`/v0/sessions`, `/v0/session/{id}`) provide the user-facing
surface.

## Motivation

Today Gas City agents are either **persistent** (reconciler keeps them
alive forever) or **multi-instance** (manually started/stopped, but still
run continuously while active). Neither model fits interactive human
conversations:

**Problem 1: Resource waste.** A developer chats with an agent for 20
minutes, then walks away for 3 hours. The tmux session and provider
process sit idle, consuming memory and (for cloud providers) API
connections. With 5 idle conversations, that's 5 orphaned processes.

**Problem 2: No conversation persistence.** If someone kills the tmux
session to free resources, the conversation is lost. Restarting the agent
gives a fresh context — the human must re-explain everything.

**Problem 3: One agent = one conversation.** An agent config maps 1:1 to
a runtime session. To have two conversations with the same agent
template (e.g., one about auth, one about the build system), you need
two separate agent configs.

**Example:** Alice defines a `helper` agent in `city.toml` — Claude Code
with a coding prompt. She starts a conversation about the auth system,
works for 30 minutes, then switches to a build system question. Today
she either (a) continues in the same context (polluting it) or (b) kills
the agent and loses the auth conversation. With chat sessions, she runs
`gc session new helper` twice, getting two independent conversations she
can suspend and resume independently.

**NDI alignment:** Chat sessions extend nondeterministic idempotence to
interactive work. The conversation state (bead) survives independently
of the runtime session. Kill the process, restart the machine — the bead
knows which provider session to resume.

## Guide-Level Explanation

### Creating a session

Any agent config can serve as a session template. Pick your template and
start chatting:

```bash
$ gc session new helper
Session s-1 created from template "helper". Attaching...

# [Claude conversation starts]
# Work on auth system...
# Detach with Ctrl-b d
```

### Listing sessions

```bash
$ gc session list
ID    TEMPLATE  STATE      TITLE                  AGE   LAST ACTIVE
s-1   helper    active     —                      10m   2m ago
s-2   helper    suspended  auth system debugging   2d   2h ago
s-3   review    closed     sprint 12 review        5d   1d ago
```

### Resuming a session

`gc session attach` is the single entry point. It handles all states:

```bash
# Reattach to an active session (tmux session still alive)
$ gc session attach s-1
# [Reconnects to running tmux session]

# Resume a suspended session (no process — restarts with --resume)
$ gc session attach s-2
Resuming session s-2 (helper)...
# [Claude resumes with full conversation history]
```

### Suspending to free resources

```bash
$ gc session suspend s-1
Session s-1 suspended. Resume with: gc session attach s-1
```

Or let it happen automatically — when the controller is running and a
session has been detached longer than `idle_timeout`, it auto-suspends:

```toml
[sessions]
idle_timeout = "30m"
```

### Ending a conversation

```bash
$ gc session close s-2
Session s-2 closed.
```

### Peeking without attaching

```bash
$ gc session peek s-1 --lines 30
```

### Naming sessions

Sessions get auto-generated IDs (`s-1`, `s-2`, ...) but can be titled:

```bash
$ gc session new helper --title "auth system debugging"
```

### Configuration

No new config section is required to use sessions. Any `[[agents]]`
entry works as a template. The optional `[sessions]` section controls
auto-suspend:

```toml
[sessions]
idle_timeout = "30m"    # auto-suspend detached sessions (0 = disabled)
```

## Reference-Level Explanation

### 1) Identity Model

A chat session is identified by a **bead ID** with a `s-` prefix
(e.g., `s-1`, `s-42`). The bead store assigns the numeric suffix; the
`s-` prefix distinguishes sessions from task beads (`gc-1`).

The template is referenced by the agent's qualified name (e.g.,
`helper`, `my-rig/coder`). Multiple sessions can reference the same
template simultaneously.

### 2) Persistence: Sessions as Beads

A chat session is a bead with `type = "session"`:

```go
beads.Bead{
    ID:     "s-1",
    Type:   "session",
    Status: "open",       // or "closed"
    Title:  "auth system debugging",
    Labels: []string{
        "gc:session",
        "template:helper",
    },
    Metadata: map[string]string{
        "template":     "helper",
        "state":        "active",    // or "suspended"
        "provider":     "claude",
        "session_key":  "<opaque>",  // provider-specific resume handle
        "work_dir":     "/home/alice/my-project",
        "created_by":   "human",
    },
}
```

**Bead status vs session state:**

| Bead status | Metadata state | Meaning |
|---|---|---|
| `open` | `active` | Conversation live, runtime session exists |
| `open` | `suspended` | Conversation paused, no runtime resources |
| `closed` | — | Conversation ended, historical record |

Two levels of state are needed because bead status (`open`/`closed`)
tracks the conversation lifecycle (is this conversation still relevant?)
while metadata state (`active`/`suspended`) tracks runtime resources
(is a process running?).

### 3) State Machine

```
                 gc session new
                      |
                      v
                 +---------+
          +----->| active  |------+
          |      +---------+      |
          |        |     |        |
     resume|  detach+idle | close  |
          |  or suspend   |        |
          |        |      |        v
          |        v      |    +--------+
          +-- suspended   +--->| closed |
              +---------+     +--------+
```

**Transitions:**

| From | To | Trigger | Action |
|---|---|---|---|
| (none) | active | `gc session new` | Create bead, start runtime session, attach terminal |
| active | active | `gc session attach` (already active) | Reattach to existing tmux session |
| active | suspended | `gc session suspend` or idle timeout | Capture session key, kill runtime session |
| active | closed | `gc session close` | Kill runtime session, close bead |
| suspended | active | `gc session attach` | Start runtime session with resume, attach terminal |
| suspended | closed | `gc session close` | Close bead (no runtime to kill) |

**Invalid transitions:** `closed -> *` (terminal state).

### 4) Runtime Session Management

Each active chat session maps to a tmux session. The tmux session name
follows the pattern `{city}--chat-{bead-id}` (e.g., `bright-lights--chat-s-1`).
This avoids collisions with agent sessions (`{city}--{agent-name}`).

**Starting a new session:**

1. Create the session bead
2. Generate a UUID for the session key (`session_key` in bead metadata)
3. Look up the template agent config
4. Build a `session.Config` from the template (same as `agent.SessionConfig()`)
5. If provider supports `SessionIDFlag`: inject `--session-id <uuid>` into command
6. Call `session.Provider.Start(ctx, tmuxName, cfg)`
7. Store tmux session name in bead metadata
8. Call `session.Provider.Attach(tmuxName)`

The session key is known from step 2 — no capture needed on suspend.

**Suspending:**

1. Update bead metadata: `state = "suspended"`
2. Call `session.Provider.Stop(tmuxName)`

The session key is already in the bead (stored at creation). The
provider saves its conversation state on SIGTERM. Nothing to capture.

**Resuming:**

1. Read bead metadata → get template, session key, work dir
2. Build resume command: `claude --resume <session_key>`
   (or `codex resume <key>`, etc., per provider's `ResumeFlag`/`ResumeStyle`)
3. Call `session.Provider.Start(ctx, tmuxName, resumeCfg)`
4. Update bead metadata: `state = "active"`
5. Call `session.Provider.Attach(tmuxName)`

**Closing:**

1. If active → `session.Provider.Stop(tmuxName)`
2. Close bead

### 5) Provider Session Key Management

The **session key** is the opaque handle (typically a UUID) that lets us
resume the correct conversation. There are five possible strategies for
obtaining it, analyzed here from most to least reliable:

#### Strategy 1: Generate & Pass (primary — Claude)

Gas City generates a UUID at session creation and passes it to the
provider via a `--session-id` flag. The provider creates/continues a
conversation with that exact ID. No capture step needed — we own the
ID from birth.

```bash
# First start — we generate the UUID, provider uses it
claude --session-id $OUR_UUID "prompt"

# Resume — we pass the same UUID back
claude --resume $OUR_UUID
```

Claude Code supports both `--session-id <uuid>` (create with specific
ID) and `--resume <uuid>` (resume that specific session). This gives
Gas City full control: no parsing, no scanning, no race conditions.

**Reliability: High.** We control both sides of the contract.

#### Strategy 2: Hook stdin capture (gastown approach)

Claude Code sends `{"session_id":"uuid","transcript_path":"...","source":"startup"}`
on stdin to SessionStart hooks. Gastown's `gt prime --hook` reads this
JSON, persists the session ID to `.runtime/session_id`, and uses it
for handoff/resume cycles.

Gas City already installs hooks via `gc prime --hook`. The hook can
read the session ID from stdin and store it in tmux session metadata
(`SetMeta("session_key", id)`), confirming or supplementing the
Generate & Pass ID.

**Reliability: High.** The provider explicitly sends the ID. Requires
hooks to be installed. Useful as a belt-and-suspenders validation
alongside Strategy 1, and as the primary strategy for providers that
support hooks but not `--session-id`.

#### Strategy 3: JSONL log filename scan

Claude stores session logs at
`~/.claude/projects/<hash>/<session-id>.jsonl`, where the project hash
is a simple path transform (`/data/projects/foo` → `-data-projects-foo`).
List files, find the most recent, extract the session ID from the
filename.

**Reliability: Medium.** Races when multiple sessions share the same
project directory — exactly the failure mode the requirements call out
("MUST NOT depend on latest JSONL file in this directory"). Only viable
as a last resort when Strategies 1 and 2 are unavailable.

#### Strategy 4: history.jsonl tail

`~/.claude/history.jsonl` logs every interaction with a `sessionId`
field. Tail it and read the most recent entry.

**Reliability: Low.** Racy when multiple Claude instances run
concurrently (a city with 3 agents = wrong session ID). Not viable
for Gas City.

#### Strategy 5: Environment variable

Check if the provider exports a session ID env var (e.g.,
`CLAUDE_SESSION_ID`). Claude Code does **not** currently set this.

**Reliability: None.** Dead end for Claude today. May be viable for
future providers that export session IDs to the environment.

#### Chosen approach

**Strategy 1 (Generate & Pass) is the primary mechanism for Claude.**
It requires no capture logic, no hooks, and no fragile filesystem
scanning. The session bead stores the UUID from creation — the same
UUID is passed to Claude on start and on resume.

**Strategy 2 (Hook capture) is the fallback** for providers that don't
support `--session-id` but do fire session hooks. It's also available
as validation for Claude (the hook can confirm the ID matches).

**For providers with no resume capability at all**, sessions still
provide resource management (suspend frees the process) — resume
just starts a fresh conversation. The user gets the suspend/resume UX
regardless of provider capabilities.

#### Graceful degradation chain

```
1. Generate & Pass   → full conversation continuity
2. Hook capture      → full conversation continuity
3. JSONL scan        → best-effort continuity (may resume wrong session)
4. No capture        → fresh conversation with warning
```

Each level falls through to the next. The user always gets a working
session; the quality of resume degrades gracefully.

#### Provider resume capabilities

To support this, `ProviderSpec` gains new fields:

```go
type ProviderSpec struct {
    // ... existing fields ...

    // ResumeFlag is the CLI flag for resuming a session by ID.
    // Empty means the provider does not support resume.
    // Examples: "--resume" (claude), "resume" (codex)
    ResumeFlag string `toml:"resume_flag,omitempty"`

    // ResumeStyle controls how ResumeFlag is applied:
    //   "flag"       → command --resume <key>              (default)
    //   "subcommand" → command resume <key>
    ResumeStyle string `toml:"resume_style,omitempty"`

    // SessionIDFlag is the CLI flag for creating a session with a
    // specific ID. Enables the Generate & Pass strategy.
    // Example: "--session-id" (claude)
    SessionIDFlag string `toml:"session_id_flag,omitempty"`
}
```

Built-in provider updates:

```go
"claude": {
    // ... existing ...
    ResumeFlag:    "--resume",
    ResumeStyle:   "flag",
    SessionIDFlag: "--session-id",
},
"codex": {
    // ... existing ...
    ResumeFlag:    "resume",
    ResumeStyle:   "subcommand",
    SessionIDFlag: "",  // hook capture fallback
},
```

#### What could go wrong?

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| `--session-id` semantics change in Claude | Low | High | Pin to documented CLI contract; detect failures and fall back |
| `--resume <id>` fails (session data cleared) | Medium | Low | Fall back to fresh conversation with warning |
| Non-Claude providers lack resume entirely | High | Low | Sessions still useful for resource management; fresh start on resume |
| Claude's `~/.claude/` cleared between sessions | Low | Low | Same as any user clearing their history — graceful fresh start |
| Multiple machines / containers | Medium | Medium | Session data is local; document limitation. Future: sync via bead metadata |

No strategy is fragile enough to block the feature. The worst case is
always "fresh conversation with a warning" — never a crash or data loss.

### 7) Auto-Suspend

When the controller/daemon is running and `[chat].idle_timeout` is set,
the controller watches active chat sessions:

```
for each active chat session bead:
    if session.Provider.IsAttached(tmuxName):
        continue   // human is active
    activity, _ := session.Provider.GetLastActivity(tmuxName)
    if time.Since(activity) > idleTimeout:
        suspend(bead)
```

This runs as part of the existing reconciliation tick. No new goroutine
needed — the controller already iterates agents each tick.

### 8) Bead Store: Session ID Prefix

Session beads use the `s-` prefix to distinguish them from task beads
(`gc-`). The `FileStore` and `MemStore` gain a configurable ID prefix:

```go
store := beads.NewFileStore(path, beads.WithPrefix("s-"))
```

Alternatively, the chat session manager creates beads with a
post-creation ID rewrite. The simpler approach: the chat session manager
uses the city's existing bead store and stores sessions as regular
beads with `type = "session"`. The ID prefix is cosmetic — any bead
ID works. The `s-` prefix is a convention enforced by the CLI display,
not the store.

**Chosen approach:** Use the existing bead store with no prefix changes.
Session beads get regular IDs (`gc-42`). The CLI display can alias them
as `s-42` by stripping the `gc-` prefix, or we accept `gc-` IDs for
sessions. This avoids any bead store changes.

### 9) Error Handling

| Error | Behavior |
|---|---|
| Template not found | CLI error: `template "xyz" not found in city.toml` |
| Bead store unavailable | CLI error: `cannot create session: store unavailable` |
| Session key capture fails | Warning + empty key. Resume starts fresh. |
| Resume fails (provider error) | Fall back to fresh start. Warn user. |
| Attach to closed session | CLI error: `session s-1 is closed` |
| Tmux session died (active bead) | `gc session attach` detects it, resumes automatically |

### 10) Events

| Event type | When | Subject |
|---|---|---|
| `session.created` | New session | bead ID |
| `session.attached` | Terminal attached | bead ID |
| `session.detached` | Terminal detached | bead ID |
| `session.suspended` | Session suspended | bead ID |
| `session.resumed` | Suspended session resumed | bead ID |
| `session.closed` | Session closed | bead ID |

### 11) Configuration

**New `[sessions]` section (optional):**

```go
type SessionsConfig struct {
    // IdleTimeout is the duration after which a detached chat session
    // is auto-suspended. Zero disables auto-suspend.
    IdleTimeout string `toml:"idle_timeout,omitempty"`
}
```

Progressive activation: if `[sessions]` is absent, chat sessions still work
(no auto-suspend). The `[sessions]` section activates the auto-suspend
controller feature.

**New `ProviderSpec` fields:**

```go
ResumeFlag    string `toml:"resume_flag,omitempty"`
ResumeStyle   string `toml:"resume_style,omitempty"`
SessionKeyEnv string `toml:"session_key_env,omitempty"`
```

### 12) Concurrency

- **Bead creation** is atomic (store-level mutex or Dolt transaction).
- **Suspend/resume** uses the bead as the lock: check state, then
  transition. If two `gc session attach` race on the same suspended
  session, the first one that starts the tmux session wins (Provider.Start
  returns error if session name exists).
- **Auto-suspend** checks `IsAttached` before suspending. If the user
  attaches between the check and the kill, the tmux session is already
  attached — `Stop()` will fail or the user will see a disconnect.
  Acceptable: the user simply re-attaches.

### 13) Backward Compatibility

- No existing config changes. `[sessions]` is purely additive.
- No existing agent behavior changes. Persistent agents and
  multi-instance agents work exactly as before.
- The `ProviderSpec` gains optional fields — existing configs are
  unaffected.
- Session beads are a new type in the bead store. Existing bead queries
  that filter by type won't accidentally include sessions.

## CLI Commands

### `gc session new <template> [--title <title>]`

Create a new chat session from an agent template and attach.

```
Arguments:
  template    Agent name from city.toml (required)

Flags:
  --title     Human-readable session title (optional)
  --no-attach Create session but don't attach (background start)

Exit codes:
  0  Success (after detach)
  1  Template not found, store error, or start failure
```

### `gc session list [--state <state>] [--template <name>]`

List chat sessions.

```
Flags:
  --state     Filter: "active", "suspended", "closed", "all" (default: active+suspended)
  --template  Filter by template name
  --json      JSON output

Output columns: ID, TEMPLATE, STATE, TITLE, AGE, LAST ACTIVE
```

### `gc session attach <session-id>`

Attach to (or resume) a chat session.

```
Arguments:
  session-id  Bead ID of the session (required)

Behavior:
  - Active + tmux alive  → reattach
  - Active + tmux dead   → resume via provider
  - Suspended            → resume via provider
  - Closed               → error

Exit codes:
  0  Success (after detach)
  1  Session not found, closed, or resume failure
```

### `gc session suspend <session-id>`

Suspend an active session (capture state, kill process).

```
Exit codes:
  0  Suspended (or already suspended)
  1  Session not found, closed, or capture failure
```

### `gc session close <session-id>`

End a conversation permanently.

```
Exit codes:
  0  Closed (or already closed)
  1  Session not found
```

### `gc session peek <session-id> [--lines <n>]`

View session output without attaching.

```
Flags:
  --lines  Number of lines (default: 50)

Exit codes:
  0  Success (prints output)
  1  Session not found or not active
```

## API Endpoints

Following the existing `/v0/` convention from the API server design.

### `GET /v0/sessions`

List chat sessions.

**Query parameters:**

| Param | Description | Default |
|---|---|---|
| `state` | `active`, `suspended`, `closed`, `all` | `active,suspended` |
| `template` | Filter by template name | — |
| `limit` | Max results | 50 |
| `continue` | Pagination cursor | — |

**Response:**

```json
{
  "items": [
    {
      "id": "gc-42",
      "template": "helper",
      "state": "active",
      "title": "auth system debugging",
      "provider": "claude",
      "work_dir": "/home/alice/my-project",
      "created_at": "2026-03-07T10:00:00Z",
      "last_active": "2026-03-07T10:20:00Z",
      "runtime_session": "bright-lights--chat-gc-42",
      "attached": false
    }
  ],
  "total": 1
}
```

### `GET /v0/session/{id}`

Get a single chat session.

**Response:** Same shape as a list item.

Returns `404` if the session bead does not exist or is not type
`"session"`.

### `POST /v0/sessions`

Create a new chat session.

**Request body:**

```json
{
  "template": "helper",
  "title": "auth system debugging"
}
```

**Response:** `201 Created` with the session object. The session starts
in `active` state (runtime session created). The API does not attach —
that's a terminal operation, CLI-only.

### `POST /v0/session/{id}/suspend`

Suspend an active session. Captures session key and kills the runtime
session.

**Response:** `200 OK` with updated session object. `409 Conflict` if
already suspended or closed.

### `POST /v0/session/{id}/resume`

Resume a suspended session. Starts a new runtime session with provider
resume.

**Response:** `200 OK` with updated session object. `409 Conflict` if
already active or closed.

### `POST /v0/session/{id}/close`

Close a session permanently. Kills runtime if active, closes bead.

**Response:** `200 OK` with updated session object. Idempotent — closing
a closed session returns `200`.

### `GET /v0/session/{id}/output`

Get session output (same pattern as `/v0/agent/{name}/output`).

**Response:** Session output with `format` field (`"conversation"` or
`"text"`). `404` if session is suspended (no runtime to peek).

### `POST /v0/session/{id}/nudge`

Send a message to an active session.

**Request body:**

```json
{
  "message": "check the test failures"
}
```

**Response:** `200 OK`. `409 Conflict` if suspended or closed.

## Primitive Test

Not applicable — chat sessions are a derived mechanism composing
existing primitives:

- **Task Store (Beads)** — session persistence, metadata, lifecycle
- **Agent Protocol** — runtime session start/stop/attach via
  `session.Provider`
- **Event Bus** — session lifecycle events
- **Config** — agent templates, `[sessions]` section, provider resume fields

No new primitive. No new persistence substrate. No new state machine
outside what beads already provide (open/closed). The `active/suspended`
sub-state is metadata on the bead, not a new status value.

**Derivation proof:** Every chat session operation decomposes into
existing primitive calls:

| Operation | Primitives used |
|---|---|
| Create | `beads.Store.Create()` + `session.Provider.Start()` + `session.Provider.Attach()` |
| Suspend | `beads.Store.SetMetadata()` + `session.Provider.Stop()` |
| Resume | `beads.Store.Get()` + `session.Provider.Start()` + `session.Provider.Attach()` |
| Close | `beads.Store.Close()` + `session.Provider.Stop()` |
| List | `beads.Store.ListByLabel("gc:session")` |
| Auto-suspend | `session.Provider.IsAttached()` + `session.Provider.GetLastActivity()` + Suspend |

## Drawbacks

**New user-facing concept.** Users must learn "chat session" alongside
agents, beads, and molecules. Mitigated by the simple mental model:
sessions are conversations, agents are templates.

**Provider resume coupling.** Session resume quality depends entirely on
the provider's resume implementation. If Claude's `--resume` is flaky,
sessions feel flaky. Gas City can't control the provider's resume
quality. Mitigated by the fallback: failed resume → fresh conversation
with a warning.

**Provider resume contract dependency.** The Generate & Pass strategy
relies on `--session-id` and `--resume` being stable Claude CLI flags.
For non-Claude providers, resume support varies — some may have no
resume at all. Mitigated by: (a) `--session-id` and `--resume` are
documented, stable Claude CLI flags, (b) hook-based capture is available
as a fallback, and (c) failed resume degrades to a fresh conversation,
never a crash.

**Bead store growth.** Every conversation creates a bead that persists
forever (even after close). For heavy users with hundreds of sessions,
the bead store grows. Mitigated by: beads are small (metadata only,
not transcript), and a future `gc session prune` command can clean old
closed sessions.

## Alternatives

### A: Do nothing — use multi-instance agents

Users can already `gc session new helper my-chat-1` to create named
instances. Each instance is a separate tmux session.

Advantages: No new code. Existing feature.

Rejected because: Multi-instance agents have no suspend/resume. Killing
the session loses the conversation. There's no resource reclamation for
idle conversations. The UX is imperative (`start`/`stop`/`destroy`)
rather than conversational (`new`/`attach`/`suspend`).

### B: Dedicated working directory per session

Each session gets its own directory (e.g., `.gc/sessions/s-1/`). The
provider runs in that directory, so `--resume` (without explicit session
ID) naturally finds the right conversation.

Advantages: Simpler session key capture — no provider-specific logic
needed. Provider state is isolated per session.

Rejected because: The requirements explicitly state "MUST NOT require a
dedicated working directory per chat session." Users want sessions to
share the same project directory. Dedicated directories also duplicate
project files or require symlink complexity.

### C: Sessions as a separate store (not beads)

A dedicated `sessions.json` file instead of reusing the bead store.

Advantages: Clean separation. No risk of session beads appearing in
bead queries.

Rejected because: Beads are the universal persistence substrate. Adding
a parallel store violates the architectural principle and duplicates
CRUD, concurrency, and persistence logic. Session beads are cleanly
separated by `type = "session"` and the `gc:session` label.

### D: Workspace-scoped sessions (one per agent per workspace)

Instead of multiple sessions per template, limit to one session per
agent config. The agent config IS the session — no separate session
resource.

Advantages: Simpler model. No session management UI needed.

Rejected because: The requirements explicitly state "more than one chat
session can exist for the same agent template." Single-session-per-agent
forces users to create duplicate agent configs for parallel
conversations.

## Unresolved Questions

### Before accepting this design

1. **Session bead ID format.** Should session beads use the standard
   `gc-N` prefix or a distinct `s-N` prefix? Distinct prefix aids
   readability (`gc session attach s-1` vs `gc session attach gc-42`) but
   requires bead store changes. See §8 for trade-offs.

### During implementation

4. **Auto-suspend interaction with agent attach.** If a human has both
   `gc session attach worker` and `gc session attach s-1` running, does
   auto-suspend only apply to chat sessions? Yes — auto-suspend is
   scoped to beads with `type = "session"`.

5. **Session listing performance.** `ListByLabel("gc:session")` scans
   all beads. For stores with thousands of beads, this may need an
   index. Defer optimization until measured.

6. **Title auto-generation.** Should the system ask the provider to
   generate a title from conversation content? Defer to a follow-up —
   manual titles and no-title are sufficient for v1.

## Implementation Plan

### Phase 1: Core lifecycle (medium)

- Add `type = "session"` bead convention
- Implement session manager: `New()`, `Suspend()`, `Resume()`, `Close()`
- `gc session new`, `gc session list`, `gc session attach`, `gc session suspend`,
  `gc session close`, `gc session peek`
- No auto-suspend, no session key capture (resume always starts fresh)

Delivers: Working chat sessions with suspend/resume UX. Resume starts
a fresh conversation (no provider resume yet). Already useful for
resource management — suspend frees the tmux session.

### Phase 2: Provider resume (medium)

- Add `ResumeFlag`, `ResumeStyle`, `SessionIDFlag` to `ProviderSpec`
- Implement Generate & Pass: create UUID at session birth, pass via
  `--session-id` on start, pass via `--resume` on resume
- Add hook-based capture as fallback for providers without `SessionIDFlag`
- Wire resume into session manager — suspended sessions resume the
  correct conversation

Delivers: True conversation continuity across suspend/resume for Claude
Code. Other providers get it as resume support is added to their specs.

### Phase 3: Auto-suspend + API (small)

- Add `[sessions]` config section with `idle_timeout`
- Controller watches active chat sessions on each reconcile tick
- Add API endpoints (`/v0/sessions`, `/v0/session/{id}`, etc.)

Delivers: Hands-free resource management. Dashboard can show chat
sessions.

### Phase 4: Polish (small)

- `gc session rename <id> <title>` for renaming sessions
- `gc session prune [--before <date>]` for cleaning old sessions
- Session count in `gc status` output
- Tab completion for session IDs
