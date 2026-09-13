---
title: "Agent REST full CRUD + typed config-change events"
---

## Context

The supervisor's `Agent` runtime today exposes a runtime-shaped projection
of an agent over REST:

- `GET /v0/city/{cityName}/agent/{base}` returns `agentResponse`
  (`internal/api/handler_agents.go:38-64`) — a runtime snapshot
  (`name`, `running`, `suspended`, `session`, `provider`, `display_name`,
  `state`, `available`). Useful for "is the agent up?", insufficient for
  any client that wants to see or edit how the agent is **defined**.
- `PATCH /v0/city/{cityName}/agent/{base}` accepts an `AgentUpdate`
  body (`internal/api/state.go:98-102`) that exposes exactly three
  fields: `Provider`, `Scope`, `Suspended`. The handler
  (`internal/api/huma_handlers_agents.go:306-318`) silently drops
  every other field a client might send.
- `POST /v0/city/{cityName}/agents` (`huma_handlers_agents.go:242-277`)
  creates an agent from `name + provider` minimal input; richer config
  must follow as a separate PATCH.
- The session/lifecycle event stream (`/v0/city/{cityName}/events/stream`)
  carries typed payloads for `mail.*`, `bead.*`, `worker.operation`,
  and now `session.stopped` / `session.crashed`
  (see open PR #1945 `feat(events): typed SessionLifecyclePayload`).
  There is **no event** signaling that an agent's configuration on
  disk has changed.

### What exists today

- `config.Agent` (`internal/config/config.go:1717-1992`) carries ~40
  fields of definition state — identity, lifecycle, scaling, env, prompt
  template path, fragment injection, work routing, etc. All loaded from
  TOML, all kept in memory.
- `config.AgentPatch` (`internal/config/patch.go:19-137`) already
  models a structured patch covering most of those fields, used today
  by `config.ApplyPatches` for pack-defined overrides via
  `[[patches.agent]]` blocks.
- `Editor.ApplyAgentPatch` in `internal/configedit/` already round-trips
  `city.toml` patches preserving comments, dispatching by `Origin`
  (Inline vs Derived).
- The HTTP layer is Huma; `application/problem+json` (RFC 9457) is the
  established error shape with `errors[].location` field-level
  pointers.
- Event registration uses `events.RegisterPayload` keyed off a string
  constant declared in `internal/events/events.go` plus a marker
  method `IsEventPayload()` on the payload type — pattern documented
  in code by `event_payloads.go` (mail/bead/worker) and applied again
  in PR #1945 for session lifecycle.

### What's missing

1. No REST surface exposes the **definition** of an agent (the ~40
   fields above). Clients reading `GET /agent/{base}` see only
   runtime; clients wanting to render or diff configuration are forced
   to read the TOML directly.
2. `PATCH /agent/{base}` exposes 3 of ~30 editable patch fields. Every
   other safe edit (idle timeout, scaling, env, prompt template path,
   fragment injection, etc.) requires editing TOML out-of-band, with
   no optimistic-concurrency story and no observability for other
   clients.
3. No `POST` variant accepts a rich definition body — wizards that
   want to create an agent with non-default config must do a 2-step
   POST + PATCH round trip.
4. Edits made by one client (REST PATCH, TOML edit + reload, or a
   second instance of the same UI) are invisible to other clients
   until they refetch. There is no agent-config equivalent of
   `session.*` events.

This proposal addresses all four with a coherent CRUD surface for the
agent definition plus one typed event.

## Goals

1. Expose a read surface that returns the full agent definition in a
   stable, OpenAPI-typed shape on top of the existing runtime response,
   in **one** GET, with an entity tag suitable for `If-Match`
   optimistic concurrency.
2. Expose a write surface that accepts the safe editable subset of
   `config.AgentPatch` and round-trips through the same
   `Editor.ApplyAgentPatch` machinery that pack-derived patches use,
   with explicit semantics for absent vs present-empty fields,
   `If-Match` concurrency, and `application/problem+json` field-level
   errors.
3. Expose a create surface accepting the same rich body, returning
   201 + the full definition (no extra round trip), reusing the existing
   `WaitForAgentVisibility` machinery so the response reflects the
   reloaded supervisor state.
4. Emit a typed `agent.config.updated` event whenever (2) or (3) mutate
   on-disk config, carrying the city/qualified-name/etag/operation so
   subscribers can invalidate caches without polling. Same registration
   pattern as PR #1945 / `SessionLifecyclePayload`.

## Non-goals

- **Skills and MCP server registration** in the body. Both are
  tombstoned in `internal/config/patch.go:57-78` (v0.16) and excluded
  from `What Gas City does NOT contain` in `AGENTS.md`. Clients that
  want fragment injection use `inject_fragments` (already in
  `config.AgentPatch`); clients that want MCP/skills do not get a REST
  primitive from this proposal.
- **Prompt-template file content read/write.** That is a file-level
  operation with its own concurrency model (`ETag = content hash`,
  pack-derived 403, atomic write via temp+rename). Out of scope here
  to keep this proposal focused on `config.Agent` field editing.
  Possible follow-up proposal if interest emerges.
- **Bulk PATCH / multi-agent operations.** Single-agent surface only.
- **Identity-changing fields** (`Name`, `Dir`). Rename = delete + create
  by convention; this surface only accepts the safe editable subset.
- **Audit history / undo.** Out of scope. `git log` on the city repo
  remains the source of truth.

## Design

### 1. `GET /v0/city/{cityName}/agent/{base}/full`

Read-only. Returns a wrapped response combining the existing runtime
projection and the full definition:

```go
type AgentFullResponse struct {
    Runtime    agentResponse   `json:"runtime"`
    Definition AgentDefinition `json:"definition"`
}

type AgentDefinition struct {
    Name              string            `json:"name"`
    Description       string            `json:"description,omitempty"`
    Dir               string            `json:"dir,omitempty"`
    WorkDir           string            `json:"work_dir,omitempty"`
    Scope             string            `json:"scope,omitempty"`
    Suspended         bool              `json:"suspended"`
    PreStart          []string          `json:"pre_start,omitempty"`
    PromptTemplate    string            `json:"prompt_template,omitempty"`
    Nudge             string            `json:"nudge,omitempty"`
    Session           string            `json:"session,omitempty"`
    Provider          string            `json:"provider,omitempty"`
    StartCommand      string            `json:"start_command,omitempty"`
    Args              []string          `json:"args,omitempty"`
    Env               map[string]string `json:"env,omitempty"`
    OptionDefaults    map[string]string `json:"option_defaults,omitempty"`
    MaxActiveSessions *int              `json:"max_active_sessions,omitempty"`
    MinActiveSessions *int              `json:"min_active_sessions,omitempty"`
    ScaleCheck        string            `json:"scale_check,omitempty"`
    DrainTimeout      string            `json:"drain_timeout,omitempty"`
    IdleTimeout       string            `json:"idle_timeout,omitempty"`
    SleepAfterIdle    string            `json:"sleep_after_idle,omitempty"`
    WakeMode          string            `json:"wake_mode,omitempty"`
    InjectFragments   []string          `json:"inject_fragments,omitempty"`
    OverlayDir        string            `json:"overlay_dir,omitempty"`
    Namepool          string            `json:"namepool,omitempty"`
    WorkQuery         string            `json:"work_query,omitempty"`
    DefaultSlingFormula string          `json:"default_sling_formula,omitempty"`
    Origin            string            `json:"origin,omitempty"` // "inline" | "derived" | "pack"
}
```

Response carries `ETag: "<opaque>"` derived from a stable hash of the
`AgentDefinition` payload. Side-effect zero — reads from
`s.state.Config()` snapshot. Qualified-name variant
`GET /v0/city/{cityName}/agent/{dir}/{base}/full` mirrors the
qualified runtime route.

Status codes: 200 with body and ETag; 404 if the agent does not exist
in the city snapshot.

### 2. `PATCH /v0/city/{cityName}/agent/{base}/full`

Body mirrors the safe editable subset of `config.AgentPatch`. Pointer
fields distinguish "key absent in body" from "set to zero / clear":

```go
type AgentPatchRequest struct {
    Description       *string            `json:"description,omitempty"`
    WorkDir           *string            `json:"work_dir,omitempty"`
    Scope             *string            `json:"scope,omitempty"`
    Suspended         *bool              `json:"suspended,omitempty"`
    Provider          *string            `json:"provider,omitempty"`
    StartCommand      *string            `json:"start_command,omitempty"`
    Args              []string           `json:"args,omitempty"`     // nil = no-op, [] = clear
    Session           *string            `json:"session,omitempty"`
    PromptTemplate    *string            `json:"prompt_template,omitempty"`
    Nudge             *string            `json:"nudge,omitempty"`
    PreStart          []string           `json:"pre_start,omitempty"`
    IdleTimeout       *string            `json:"idle_timeout,omitempty"`
    SleepAfterIdle    *string            `json:"sleep_after_idle,omitempty"`
    WakeMode          *string            `json:"wake_mode,omitempty"`
    MaxActiveSessions *int               `json:"max_active_sessions,omitempty"`
    MinActiveSessions *int               `json:"min_active_sessions,omitempty"`
    ScaleCheck        *string            `json:"scale_check,omitempty"`
    DrainTimeout      *string            `json:"drain_timeout,omitempty"`
    Env               map[string]string  `json:"env,omitempty"`
    EnvRemove         []string           `json:"env_remove,omitempty"`
    WorkQuery         *string            `json:"work_query,omitempty"`
    DefaultSlingFormula *string          `json:"default_sling_formula,omitempty"`
    InjectFragments   *[]string          `json:"inject_fragments,omitempty"`  // see semantics below
    OverlayDir        *string            `json:"overlay_dir,omitempty"`
}
```

Concurrency: client sends `If-Match: "<etag-from-GET>"`. If the
on-disk definition has been mutated since the etag was issued, return
**409 Conflict** with `application/problem+json` (no body diff —
client re-GETs to reconcile).

Validation: Huma struct tags + handler-level checks. Failures return
**422 Unprocessable Entity** with `application/problem+json` and
`errors[].location` pointing at the offending field
(`body.idle_timeout`, etc.). Duration strings parse with
`time.ParseDuration` (with `"off"` accepted for `idle_timeout` /
`sleep_after_idle` only). `wake_mode` is a strict enum (`resume` /
`fresh`). Unknown providers return 422 with the active set in
`detail`.

Apply path: handler maps `AgentPatchRequest → config.AgentPatch`, then
calls `Editor.ApplyAgentPatchFull(cityName, name, patch)` which
dispatches by origin (`OriginInline` → `config.ApplyPatches` in place;
`OriginDerived` → merge into `[[patches.agent]]`). TOML round-trip
preserves comments via the same machinery `Editor` already uses for
pack-derived patches. Reload uses the existing `UpdateAgent` reload
machinery.

Response: 200 with the fresh `AgentFullResponse` and a new `ETag`.
Status codes: 200, 404 (no such agent), 409 (etag mismatch), 422
(validation), 5xx (apply failure with rollback).

#### `inject_fragments` semantics

The single nontrivial type choice in the patch body. Three states a
client may want, mapped to the presence-aware `*[]string` encoding
already in `config.AgentPatch.InjectFragments`:

| Client intent     | Wire shape (JSON)                      | Internal `*[]string`     |
|-------------------|----------------------------------------|--------------------------|
| Leave unchanged   | key absent (or `null`)                 | `nil`                    |
| Clear the list    | `"inject_fragments": []`               | `&[]string{}`            |
| Replace contents  | `"inject_fragments": ["a","b"]`        | `&[]string{"a","b"}`     |

This relies on the presence-aware encoding landed by
[PR #1952](https://github.com/gastownhall/gascity/pull/1952) (merged
as `da15452e`), which changed `config.AgentPatch.InjectFragments`
from `[]string` to `*[]string` plus a `config.Fragments(items ...string)
*[]string` constructor helper. The maintainer follow-up
(`2d4a00e`) extended the same pattern to `AgentOverride.InjectFragments`,
so both surfaces now honor `inject_fragments = []` as an explicit
clear on disk. The pointer (`omitempty` on the encoder) is what
distinguishes "absent" from "present-empty"; without it the TOML
encoder silently drops `[]string{}` and the clear no-ops on reload —
the TODO at `internal/config/patch.go:362-365` documents the same
limitation for the remaining list fields.

`AgentPatchRequest.InjectFragments` therefore models the wire side
as `*[]string` too: JSON `null` or absent → `nil`; JSON `[]` →
`&[]string{}`; JSON `[...]` → `&[]string{...}`. The handler maps
straight through; no special-case branching needed.

The same fix applies field-by-field to other list patches
(`PreStart`, `DependsOn`, `SessionSetup`, …) only when a real
client use case demands clear — out of scope here.

### 3. `POST /v0/city/{cityName}/agent/{base}/full`

Body shape is `AgentPatchRequest` with `Name + Dir + Provider`
required at the path/body level. Returns 201 + `AgentFullResponse` +
new `ETag` after `WaitForAgentVisibility` (same machinery the existing
POST `/agents` uses). Saves the new agent to `city.toml` via
`Editor.CreateAgent(...)` (inline `[[agents]]` block).

Status codes: 201 (created), 409 (agent name already exists in city),
422 (validation), 5xx.

Qualified-name variant `POST /v0/city/{cityName}/agent/{dir}/{base}/full`
mirrors PATCH / GET.

### 4. SSE event `agent.config.updated`

Registered the same way `mail.*`, `bead.*`, `worker.operation`,
`session.stopped`/`session.crashed` are. New constant +
`KnownEventTypes` entry in `internal/events/events.go`:

```go
const AgentConfigUpdated = "agent.config.updated"

var KnownEventTypes = []string{
    ...
    AgentConfigUpdated,
}
```

Payload registered via `events.RegisterPayload`:

```go
type AgentConfigUpdatedPayload struct {
    CityName      string `json:"city_name"`
    QualifiedName string `json:"qualified_name"`
    ETag          string `json:"etag"`
    Operation     string `json:"operation"` // "create" | "update"
}

func (AgentConfigUpdatedPayload) IsEventPayload() {}
```

Emission: best-effort after a successful PATCH or POST `/full` apply
returns. `Actor: "agentconfig"`, `Subject: qualifiedName`, body is the
JSON-marshaled payload. Failures (404/409/422/5xx) short-circuit
before the emit, so the event semantically means "a successful config
mutation happened — the ETag tells you what state to expect from a
subsequent GET".

Out-of-band TOML edits or `[[patches.agent]]` reload triggered by
filesystem changes are **not** covered by this initial slice — they
do not flow through the REST handler. A future extension could
hook the reload path; for now, clients should reconcile on reconnect.

### Prerequisite (already landed)

[PR #1952](https://github.com/gastownhall/gascity/pull/1952) (merged
2026-05-11 as `da15452e`) changed `config.AgentPatch.InjectFragments`
from `[]string` to `*[]string` with a `Fragments(items ...string)
*[]string` constructor helper, fixing the long-standing
`inject_fragments = []` no-op. The maintainer follow-up (`2d4a00e`)
extended the same pattern to `AgentOverride.InjectFragments` so
rig-scoped overrides honor the same clear semantics. This proposal
builds on that foundation: every reference to `*[]string` /
`Fragments(...)` below assumes the type is already in place.

## Validation

- **Unit / handler tests** mirror existing patterns:
  `internal/api/maestro_agentconfig_test.go`,
  `internal/api/maestro_agentconfig_patch_test.go`,
  `internal/api/maestro_agentconfig_create_test.go`,
  `internal/api/maestro_agentconfig_events_test.go`. Each handler
  covers happy path, 404 (agent missing), 409 (etag stale on PATCH;
  duplicate name on POST), 422 (validation), 5xx with rollback.
- **Apply layer tests**:
  `internal/configedit/maestro_apply_patch_full_test.go` covers
  Inline + Derived origin merge, including a canary test for
  pack-baseline + `inject_fragments` clear (relying on the
  `*[]string` encoding from PR #1952).
- **Event emission tests** verify both positive emission and "no
  event on failure" for PATCH/POST.
- **Live E2E** (manual + scriptable): GET `/full` → PATCH with
  `If-Match` → GET reflects → SSE listener sees `agent.config.updated`
  with matching etag. POST `/full` returns 201 with definition.
  Duration validation rejects malformed inputs with 422 +
  field-level error pointer.

## Implementation phasing

If the design is acceptable, implementation lands as four stacked
PRs against `gastownhall/gascity:main`, each small and reviewable:

1. **`feat(api): GET /agent/{base}/full`** — read-only, body shape +
   ETag + 404. ~1 line edit in `internal/api/supervisor.go` to
   register the route; everything else is new files.
2. **`feat(api): PATCH /agent/{base}/full + ETag/If-Match`** —
   editable subset, optimistic concurrency, 409/422 paths. Adds
   `Editor.ApplyAgentPatchFull` dispatching by `Origin`. Zero edits
   to upstream-owned files; new files in `internal/api/` (handler) +
   `internal/configedit/` (apply layer) + a small interface in
   `internal/state/` if needed.
3. **`feat(api): POST /agent/{base}/full`** — create variant.
   Builds on the `*[]string` encoding from PR #1952 (already merged).
4. **`feat(events): agent.config.updated typed payload`** — adds the
   const + `KnownEventTypes` entry + payload type + emission in PATCH
   and POST handlers. Same pattern as PR #1945 — small surface,
   reviewable in isolation.

## Open questions for reviewers

- Should the read response wrap runtime + definition (the proposed
  shape) or flatten them into a single `agentResponse` superset? The
  wrapped shape preserves backward-compat clarity (an existing
  `agentResponse` consumer can still read `response.runtime` if the
  type is also re-exposed as a top-level field), but a flattened
  superset is more idiomatic.
- For ETag derivation: stable hash of the JSON-marshaled
  `AgentDefinition`, or a hash of the underlying `config.Agent` Go
  struct? The former is what clients can independently reproduce; the
  latter is faster but opaque. Open to either.
- Does the project prefer `engdocs/proposals/` for this doc plus a
  separate `engdocs/proposals/agent-rest-full-crud-implementation-plan.md`
  before code lands (matching the `skill-materialization` precedent),
  or is design + first implementation PR acceptable?

---

**Once accepted**, the four implementation PRs above stack against
`main` in order. Each is small (the largest is ~300 LOC including
tests). The `inject_fragments *[]string` prerequisite has already
landed via PR #1952, so the four PRs above are unblocked.
