---
title: "API Enrichment Audit"
---

**Goal:** Make the GC API rich enough that any dashboard (Mission Control,
custom UIs, monitoring tools) can build a complete agent monitoring
experience from GC alone — without needing to scrape OS process tables or
talk to provider-specific APIs like YepAnywhere.

**Principle:** The agent abstraction owns the data. Provider and session
details stay hidden behind the abstraction. If a dashboard needs to know
something about an agent, GC should expose it as a first-class field on the
agent, not force the consumer to reverse-engineer it from PIDs and cwds.

---

## Current agent response (`GET /v0/agents`, `/v0/agent/{name}`)

```json
{
  "name": "rig/agent-1",
  "running": true,
  "suspended": false,
  "rig": "rig",
  "pool": "rig/agent",
  "session": {
    "name": "city--rig--agent-1",
    "last_activity": "2026-03-06T...",
    "attached": false
  },
  "active_bead": "abc123"
}
```

This is structurally correct but data-poor. A dashboard builder has to make
N+1 calls (fetch agent list, then fetch each bead, then peek each session)
to build a useful display. The agent abstraction should carry enough state
that a single `GET /v0/agents` call gives you everything you need.

---

## Gaps — organized by what the agent abstraction should own

### Gap 1: Agent identity metadata

The agent knows its name and rig, but not its provider or what it's running.
This is static config data that should be on every agent response.

**Add to `agentResponse`:**

| Field | Type | Source | Notes |
|-------|------|--------|-------|
| `provider` | `string` | `config.Agent.Provider` | `"claude"`, `"codex"`, `"gemini"`, etc. |
| `display_name` | `string` | `ProviderSpec.DisplayName` | `"Claude Code"`, `"Codex CLI"`, etc. |

**Why:** Every dashboard wants to show what kind of agent this is. Today
you'd have to cross-reference the agent name against the config to find the
provider. The API should just tell you.

**Effort:** Trivial — the config is already loaded; add two fields to the
response builder in `handleAgentList`.

---

### Gap 2: Agent activity state (beyond running/not-running)

`running: true` is a binary. Dashboards need a richer state model to show
what the agent is actually doing.

**Add to `agentResponse`:**

| Field | Type | Source | Notes |
|-------|------|--------|-------|
| `state` | `string` | Derived (see below) | Enum: `"idle"`, `"working"`, `"waiting"`, `"stopped"`, `"suspended"`, `"quarantined"` |

**Derivation logic (in API handler, not Go business logic — pure data mapping):**

```
if suspended        → "suspended"
if quarantined      → "quarantined"
if !running         → "stopped"
if active_bead != "" {
  if last_activity recent (< threshold) → "working"
  else                                  → "waiting"
} else              → "idle"
```

The threshold for "working" vs "waiting" can be a reasonable default (10min)
or configurable. This replaces the crude `running` boolean with a
human-meaningful state without adding decision logic to Go — it's a pure
data derivation from fields we already have.

**Effort:** Small — all inputs already exist in the handler.

---

### Gap 3: Process-level metadata

Dashboards want PID, memory usage, and uptime per agent. The tmux provider
already has `GetPanePID()` and can query `/proc/{pid}/status` for RSS. This
data belongs on the agent response, not discovered by the consumer via `ps`.

**Add to `agentResponse`:**

| Field | Type | Source | Notes |
|-------|------|--------|-------|
| `process` | `*processInfo` | Session provider | `null` when not running |

```json
"process": {
  "pid": 12345,
  "rss_mb": 280,
  "elapsed_sec": 3600
}
```

**New session.Provider method:**

```go
// ProcessInfo returns OS-level process metadata for the named session.
// Returns nil if the session isn't running or info is unavailable.
ProcessInfo(name string) *ProcessInfo

type ProcessInfo struct {
    PID        int
    RSSBytes   int64
    ElapsedSec int
}
```

The tmux provider implements this via `GetPanePID` + reading
`/proc/{pid}/stat` (or `ps -p {pid} -o rss=,etimes=`). Non-tmux providers
return nil.

**Effort:** Medium — new Provider interface method, tmux implementation,
wire into API handler. The building blocks exist; this is plumbing.

---

### Gap 4: Active work context

`active_bead: "abc123"` is an opaque ID. Dashboards have to fetch the bead
separately to learn what the agent is working on.

**Add to `agentResponse`:**

| Field | Type | Source | Notes |
|-------|------|--------|-------|
| `active_work` | `*workContext` | Bead store lookup | `null` when no active bead |

```json
"active_work": {
  "bead_id": "abc123",
  "title": "implement user auth",
  "type": "task",
  "started_at": "2026-03-06T..."
}
```

**Why:** The agent handler already calls `findActiveBead()` which iterates
bead stores. It currently returns only the ID. Extend it to return title,
type, and created_at from the same bead it already found.

**Effort:** Trivial — the bead is already loaded; return more fields from it.

---

### Gap 5: Last output / peek preview

Dashboards want a quick preview of what the agent is doing without a
separate peek call. MC uses this for question detection and status display.

**Add to `agentResponse`:**

| Field | Type | Source | Notes |
|-------|------|--------|-------|
| `last_output` | `string` | `session.Peek(name, 5)` | Last ~5 lines, truncated. Empty when not running. |

**Concern:** Peek is not free (tmux capture-pane). For the agent list
endpoint, this could be expensive with many agents. Two options:

- **Option A:** Only include when `?peek=true` query param is set.
  Default list call stays fast; detail call includes it.
- **Option B:** Always include on single-agent `GET /v0/agent/{name}`,
  never on list endpoint.

Recommend **Option A** for flexibility.

**Effort:** Small — Peek already works; add optional inclusion in list handler.

---

### Gap 6: Rig/project enrichment

Rigs are the GC equivalent of "projects" but lack activity metadata.
Dashboards want to know when a rig was last active and its git state.

**Add to `rigResponse`:**

| Field | Type | Source | Notes |
|-------|------|--------|-------|
| `last_activity` | `string` | Max of agent last_activity times for rig | ISO8601 or empty |
| `agent_count` | `int` | Count of agents assigned to this rig | Includes pool expansion |
| `running_count` | `int` | Count of running agents in this rig | |

**Git status** — new optional sub-object, populated when `?git=true`:

| Field | Type | Source | Notes |
|-------|------|--------|-------|
| `git` | `*gitStatus` | `git -C {path} ...` | `null` unless requested |

```json
"git": {
  "branch": "main",
  "clean": false,
  "ahead": 2,
  "behind": 0,
  "changed_files": 3
}
```

**Effort:** Medium — agent counts are cheap (already computed). Git status
requires shelling out to `git`, so it must be opt-in (`?git=true`) and
have a short timeout.

---

### Gap 7: City-level overview stats

The status endpoint is minimal. Dashboards want a single call that gives
the full picture.

**Enrich `GET /v0/status`:**

| Field | Type | Source | Notes |
|-------|------|--------|-------|
| `version` | `string` | Build-time constant | GC binary version |
| `uptime_sec` | `int` | `time.Since(startTime)` | Controller uptime |
| `agents` | `object` | Counts | `{ "total": N, "running": N, "suspended": N, "quarantined": N }` |
| `rigs` | `object` | Counts | `{ "total": N, "suspended": N }` |
| `work` | `object` | Bead store summary | `{ "in_progress": N, "ready": N, "open": N }` |
| `mail` | `object` | Mail store summary | `{ "unread": N, "total": N }` |

**Effort:** Small-medium — all data sources exist; this is aggregation.

---

### Gap 8: Health endpoint enrichment

`GET /health` returns `{"status": "ok"}`. This is fine for liveness probes
but useless for dashboards.

**Enrich `GET /health`:**

```json
{
  "status": "ok",
  "version": "0.11.0",
  "city": "bright-lights",
  "uptime_sec": 86400
}
```

Keep it lightweight — no expensive queries. This is a probe endpoint
that also gives enough context for dashboard connection verification.

**Effort:** Trivial — add three string/int fields.

---

## Summary: implementation order

| # | Gap | Fields | Effort | Priority |
|---|-----|--------|--------|----------|
| 1 | Agent identity | `provider`, `display_name` | Trivial | P0 |
| 2 | Agent state enum | `state` | Small | P0 |
| 3 | Process metadata | `process.{pid, rss_mb, elapsed_sec}` | Medium | P0 |
| 4 | Active work context | `active_work.{bead_id, title, type, started_at}` | Trivial | P0 |
| 5 | Peek preview | `last_output` (opt-in) | Small | P1 |
| 6 | Rig enrichment | `last_activity`, `agent_count`, `running_count`, `git` | Medium | P1 |
| 7 | Status overview | Aggregate counts + version + uptime | Small | P1 |
| 8 | Health enrichment | `version`, `city`, `uptime_sec` | Trivial | P2 |

**P0** = needed for any useful dashboard integration (Gaps 1-4)
**P1** = makes dashboards significantly better (Gaps 5-7, 9)
**P2** = nice-to-have polish (Gap 8)

---

### Gap 9: Session log viewer (model, context usage, conversation)

GC already has `internal/sessionlog` (merged to main as `1a7ae398`) — a Go
package that reads Claude Code's JSONL session files, resolves the DAG to
the active conversation branch, and provides compact-boundary pagination.
This is the "container log" observation layer. It currently supports:

- DAG resolution (uuid/parentUuid chain walking, tip selection)
- Compact boundary handling (logicalParentUuid bridging)
- Pagination (slice at compact boundaries for incremental loading)
- Tool pairing (orphaned tool_use detection)
- Session discovery (find most recent JSONL by working directory slug)

What it does NOT yet extract (but can, from the same JSONL data):

- **Model name** — stored in `message.model` on assistant entries (e.g.,
  `"claude-opus-4-5-20251101"`). Just needs a helper that scans for the
  first assistant entry with a non-synthetic model field.

- **Context usage %** — computed from the last assistant message's
  `message.usage` fields (`input_tokens + cache_read_input_tokens +
  cache_creation_input_tokens`), adjusted by compaction overhead
  (`compactMetadata.preTokens`), divided by a model context window lookup.

YepAnywhere computes context % like this (from `reader.ts`):

```
1. Look up context window size by model name (hardcoded table:
   claude → 200K, gemini → 1M, codex/gpt-5 → 258K, gpt-4 → 128K)
2. Compute compaction overhead:
   overhead = last compact_boundary.preTokens - last pre-compaction assistant usage
3. Find last assistant message with non-zero usage
4. totalInput = input_tokens + cache_read + cache_creation + overhead
5. percentage = round(totalInput / contextWindowSize * 100)
```

Our sessionlog package already parses `CompactMeta.PreTokens` and has the
full active branch. Adding model + context usage extraction is
straightforward — the JSONL has all the data, we just need to decode two
more fields from `message`.

**Add to agent API — two layers:**

**Layer A: Agent-level summary fields (on `agentResponse`):**

| Field | Type | Source | Notes |
|-------|------|--------|-------|
| `model` | `string` | sessionlog extraction | `"claude-opus-4-5-20251101"` or empty |
| `context_pct` | `*int` | sessionlog extraction | 0-100, null if unavailable |
| `context_window` | `*int` | Model lookup table | Token count, null if unknown |

These are populated by reading the agent's most recent session JSONL file.
Discovery: the agent's working directory maps to a Claude projects slug
under `~/.claude/projects/`. The sessionlog package already has discovery
logic for this.

**Layer B: Full session log endpoint:**

```
GET /v0/agent/{name}/log
GET /v0/agent/{name}/log?tail=1    (last compaction segment only)
GET /v0/agent/{name}/log?before={uuid}  (pagination cursor)
```

Returns the resolved conversation branch with pagination. This is
`sessionlog.ReadFile()` / `ReadFileOlder()` exposed over HTTP. Provider-
agnostic in concept (any provider that writes structured logs could be
supported), Claude-specific in initial implementation.

**Context window lookup table** — a Go map mirroring YA's:

```go
var modelContextWindows = map[string]int{
    "opus":   200_000,
    "sonnet": 200_000,
    "haiku":  200_000,
    "gemini": 1_000_000,
    "gpt-5":  258_000,
    "codex":  258_000,
    "gpt-4":  128_000,
    "gpt-4o": 128_000,
}
```

Parse model ID → extract family → lookup. Same regex approach as YA. This
table is provider-aware but lives in the sessionlog package, not in the
agent abstraction — the API handler just calls
`sessionlog.ExtractContextUsage(session)` and surfaces the result.

**Is this a ZFC violation?** No. The context window table is a fact table
(like a timezone database), not a decision tree. It maps model IDs to known
token limits. The Go code doesn't decide anything based on context % — it
just reports the number. Dashboards decide what to do with it.

**Effort:** Medium — extend sessionlog with model/usage extraction (small),
add context window lookup table (small), add `/v0/agent/{name}/log`
endpoint (medium), wire summary fields into agent response (small).

---

## Summary: updated implementation order

| # | Gap | Fields | Effort | Priority |
|---|-----|--------|--------|----------|
| 1 | Agent identity | `provider`, `display_name` | Trivial | P0 |
| 2 | Agent state enum | `state` | Small | P0 |
| 3 | Process metadata | `process.{pid, rss_mb, elapsed_sec}` | Medium | P0 |
| 4 | Active work context | `active_work.{bead_id, title, type, started_at}` | Trivial | P0 |
| 5 | Peek preview | `last_output` (opt-in) | Small | P1 |
| 6 | Rig enrichment | `last_activity`, `agent_count`, `running_count`, `git` | Medium | P1 |
| 7 | Status overview | Aggregate counts + version + uptime | Small | P1 |
| 8 | Health enrichment | `version`, `city`, `uptime_sec` | Trivial | P2 |
| 9 | Session log + model/context | `model`, `context_pct`, `/v0/agent/{name}/log` | Medium | P1 |

---

## What this does NOT include (and why)

- **AI-generated summaries.** This is a consumer-layer feature. MC
  generates summaries by calling Claude on session data. GC could store
  summaries as bead metadata, but generating them is not an SDK concern.

- **Stale/orphan process detection.** Once GC owns process metadata (Gap
  3), a dashboard can compare GC's agent list against its own OS process
  scan. But GC shouldn't scan for orphans itself — it knows exactly which
  agents it manages. "Stale" is an MC concept for processes outside any
  orchestrator's control.

- **System stats (RAM, CPU, disk).** OS-level monitoring is not GC's job.
  A separate system monitoring service/API is the right home for this. MC
  gets this via `free`, `os.loadavg()`, `df` and should continue to.

---

## Compatibility note

All new fields use `omitempty`. Existing consumers see no breaking changes.
New fields appear only when populated. The `?peek=true` and `?git=true`
query params are additive — default behavior is unchanged.
