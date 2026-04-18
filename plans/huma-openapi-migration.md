# Plan: Type-Safe HTTP + SSE via Huma + OpenAPI 3.1

## Goal

Make the HTTP + SSE surface a pure projection of the core object model
(`internal/{beads,mail,convoy,formula,sling,agent,events,session,...}`),
where the spec is the engine: Go types and handler annotations are the
single source of truth, and the framework handles every byte on the wire.
No hand-written networking. No hand-written JSON. No hand-written OpenAPI.

## Architecture context

Gas City has a typed core object model. The CLI (`cmd/gc/cmd_*.go`) and
the HTTP/SSE API (`internal/api/handler_*.go`, `huma_handlers_*.go`) are
both projections over it. This plan governs the HTTP/SSE API projection
only.

The CLI does not consume the HTTP API as a generic remote client. The
CLI and the supervisor share process-local state coordination: CLI
commands call the core library directly, and route mutations through
the local HTTP API only when a mutable supervisor is running in the
same city (to avoid lock conflicts). Remote access is not a first-class
CLI concern; the HTTP surface exists for non-Go consumers (the TS
dashboard SPA, third-party tooling).

## Core principles

These are the invariants. Every one is load-bearing — violating any of
them reintroduces the hand-written-wire problem this plan exists to
solve.

### 1. Annotations drive the live implementation

Each endpoint is a Go function whose signature (typed input struct,
typed output struct) plus a `huma.Operation` value IS the endpoint
definition. Huma binds it, validates it, routes it, serializes it,
schema-describes it. There is no second description of the endpoint
anywhere — not in a router table, not in an OpenAPI YAML, not in a
client stub.

### 2. Spec is generated, never hand-written

`internal/api/openapi.json` and `docs/schema/openapi.json` are outputs
of `cmd/genspec`, which reads the live Huma registration from a
`SupervisorMux`. The pre-commit hook regenerates both on every Go-file
commit. `TestOpenAPISpecInSync` fails CI if the committed spec drifts
from what the supervisor serves.

### 3. The routes we register ARE the routes we expose

Per-city operations live at `/v0/city/{cityName}/...`. Supervisor-scope
operations live at their top-level paths. No shadow mapping. No
`prefix-strip-and-forward`. No client-side path-rewrite helpers. The
existence of such a helper is direct evidence the spec disagrees with
reality and is a bug to fix.

### 4. Zero hand-written JSON in the typed control plane

No `json.Marshal` or `json.Unmarshal` in any HTTP or SSE code path that
touches bytes owned by our API contract. No `json.NewEncoder` /
`json.NewDecoder` writing or reading wire bodies. No
`MarshalJSON` / `UnmarshalJSON` on wire types. Huma owns every byte
that enters or leaves the socket for a typed operation.

Edge cases that are NOT wire:

- SQL/BLOB (de)serialization in storage packages.
- Hashing request bodies for idempotency keys.
- Parsing stored JSONL transcript/log files from disk.
- Parsing external-tool output we don't own (provider CLI stdout,
  provider auth files like `~/.codex/auth.json`).
- Internal event-bus `[]byte` payloads between in-process emitters and
  consumers (but see Principle 7 — these become typed at the wire).

### 5. Typed structs for every shape knowable at compile time

Every response field, every SSE event payload, every input body is a
named Go struct with real fields and Huma tags. No `json.RawMessage` or
`map[string]any` in the typed control plane, with exactly one class of
exception (Principle 6).

"Heterogeneous", "opaque", "clients render it generically", "we'll
figure out the union later", and "it's just internal" are not
qualifying exceptions. If our code constructs the map, we know the
keys. Make it a struct.

### 6. Raw pass-through only for shapes unknowable at compile time

The single legitimate case for `json.RawMessage` on the wire is content
authored outside our source tree that we forward verbatim and cannot
enumerate statically. The canonical example is third-party provider
session transcript frames: Gas City is an SDK, users plug in providers
via config, and their frame shapes are not in our source tree.

The rule for this case:

- Every first-party provider's frame shapes ARE modeled as named
  schemas in the spec (see `internal/api/session_frame_types.go` for
  Codex/Gemini types). Consumers code against the typed cases for the
  common path.
- Only truly unknown third-party frames fall through to the raw hatch.
- The raw hatch is a single named type with a documented reason in its
  doc comment explaining why it cannot be typed.

Passing through externally-authored shapes is not a license to also
opacify our own shapes that happen to be nested near them.

### 7. Event-bus payloads are typed at the wire

The internal event bus (`internal/events`) stores event payloads as
`[]byte` to stay domain-agnostic. That is fine inside the bus. At the
wire, events are discriminated by `event.Type`; the SSE stream decodes
the `[]byte` into the concrete Go type registered for that event type
and emits a proper discriminated-union schema (oneOf by `type`). Event
emitters take typed structs, never `map[string]any`.

### 8. Error responses are typed too

Every error returned by a Huma handler is a `huma.StatusError`-producing
call with a real problem-details body. No `apiError{}` shortcuts. No
hand-written `writeError`. For the outermost panic-recovery middleware
(which must run before Huma enters the stack), error bodies are
pre-serialized `application/problem+json` constants — one `var`
declaration per well-known error, no runtime `json.Marshal`.

### 9. The `/svc/*` workspace-service proxy is the only exclusion

`/svc/*` is a raw pass-through to external service processes that own
their own contracts. It is explicitly not a typed API surface. This is
the single carved-out path inside `internal/api/`. If `/svc/*` ever
becomes typed, it gets its own migration.

## Testing discipline (invariants)

Three layers of spec-driven coverage keep the principles enforced
rather than aspirational.

**Layer 1 — schema-driven response validation.** For every typed GET
operation, the test calls the real handler via `httptest.NewServer`
and validates the response body against the operation's declared
response schema using `pb33f/libopenapi` + `libopenapi-validator`.
Catches any handler that returns a field the spec doesn't declare or
omits a required field. Huma does not validate responses at runtime;
this test does.

**Layer 2 — generated-client round-trip.** `cmd/gen-client` generates
a typed Go client from the committed spec. Round-trip tests spin up a
real supervisor via `httptest.NewServer` and call every generated
method, asserting the decoded response shape. The generated client is
not a product surface; it is a conformance probe proving the spec
matches reality.

**Layer 3 — binary integration.** Build the `gc` binary into a tempdir,
run a real supervisor, run real CLI subcommands against it, assert
exit codes and stdout shapes. Validates the whole stack wires
end-to-end through a real process and a real socket. Build-tagged
(`//go:build integration`) so it doesn't run by default.

## Spec publishing

`cmd/genspec` writes `internal/api/openapi.json` (drift-check source)
and `docs/schema/openapi.json` (Mintlify-served copy) in one run. The
`.githooks/pre-commit` hook regenerates on every Go-file commit and
stages both. The Mintlify "API" navigation group publishes the
user-facing copy at `docs/reference/api.md`.

## Generated Go client: role and scope

`cmd/gen-client` generates a typed Go HTTP client (`internal/api/genclient/`)
from the committed OpenAPI spec via `oapi-codegen`. This client has two
in-tree consumers — both legitimate, neither a shipped product surface.

### Consumer 1: multi-process coordination for the CLI

The CLI and the supervisor can run as separate processes in the same
city. When the supervisor is running, it holds in-process mutexes and
open handles to the bead/mail/convoy stores. A second process (the
CLI) cannot safely mutate that state concurrently by calling the core
library directly — it would race the supervisor's writes.

The coordination rule, implemented in `cmd/gc/apiroute.go:apiClient()`:

- No running local supervisor → CLI calls the core library directly
  against the on-disk stores.
- Running local supervisor with mutations allowed → CLI routes the
  mutation through HTTP via the generated client. The supervisor
  executes the mutation under its own locks; the CLI's result is
  consistent with the supervisor's state.

Remote access is not the reason this path exists. A
`--base-url http://remote:port` invocation is a side effect of the
same mechanism, not its purpose. The generated client is "library
calls dispatched over HTTP when we have to cross a process boundary
we didn't create."

Consequence: the generated client is part of the CLI's ordinary
execution path and must stay in sync with the spec. Regenerate via
`go generate ./internal/api/genclient` after any server-side spec
change. CI enforces sync (`TestGeneratedClientInSync`).

### Consumer 2: Layer 2 conformance probe

The same generated client is exercised by
`genclient_roundtrip_test.go` to prove every published operation
decodes cleanly against a real supervisor. This is how we catch
spec/reality drift that the pure schema-validation test (Layer 1)
would miss — mismatches in method names, request encoding, or
status-code contracts.

### Not a consumer: external Go SDK

We do not promote `internal/api/genclient/` as a public Go package for
third-party projects. External Go consumers, if they ever appear, get
their own supported surface at that point; until then the `internal/`
location is load-bearing — the generated client's API is free to
change based on what our two in-tree consumers need.

## Known gaps against these principles

The principles above are the target. The current codebase violates
several of them. Closing these is the concrete work under this plan.

### Opacity that must become structs (Principle 5)

- `huma_types_convoys.go:65-66` — `logicalNodeResponse = json.RawMessage`
  and `scopeGroupResponse = json.RawMessage` are aliases to raw JSON.
  The underlying shapes are convoy domain objects constructed in our
  own code (see `convoy_sql.go:312,314`, `handler_convoy_dispatch.go:190,192`).
  Model them as real structs.
- `huma_types_formulas.go:86` — `formulaDetailResponse.Steps []map[string]any`
  is constructed in `handler_formulas.go:192-225` with fixed keys
  (`id`, `title`, `kind`, `type`, `assignee`, `labels`, `metadata`).
  The outer frame is fully typed at the construction site. Replace
  with a struct; if a subfield (e.g. `metadata`) truly holds
  per-user-configured data, keep THAT subfield narrow and typed as
  much as possible.
- `convoy_event_stream.go:27` — `ConvoyEvent.AttemptSummary map[string]any`
  is built in `workflowAttemptSummary` with fixed fields
  (`attempt_count`, `active_attempt`, `max_attempts`). Convert to a
  typed struct with pointer-valued optionals.

### Event-bus emitters (Principle 7)

- `handler_mail.go:recordMailEvent` marshals `map[string]any{"rig": ..., "message": ...}`.
  Replace with a typed `MailEventPayload` struct; emit typed; register
  it in the event-type → Go-type map consumed by the SSE projection.
- `handler_extmsg.go:extmsgEmitEvent` takes `map[string]any` and
  marshals it. Callers in `huma_handlers_extmsg.go` construct known
  shapes (ExtMsgBound, ExtMsgUnbound, ExtMsgGroupCreated,
  ExtMsgAdapterAdded, ExtMsgAdapterRemoved) — one typed struct per
  event type.
- Audit `internal/api/` for every other emission-site that passes
  `map[string]any` to the event bus. Each corresponds to a discovered
  event-type; each gets a typed payload struct.

### Middleware error body (Principle 8)

- `middleware.go:24, 42` hand-marshals an error body because it runs
  outside Huma's stack. Convert to pre-serialized
  `application/problem+json` constants — one `var` per well-known
  error, no runtime `json.Marshal`.

### Raw pass-through review (Principle 6)

- `session_frame_types.go` — the `SessionRawMessageFrame` hatch is
  legitimate (third-party provider frames). Verify the doc comment
  explains WHY the type cannot be enumerated; verify all first-party
  provider frame shapes are modeled (Codex and Gemini are; audit that
  the set is complete as new providers land).
- `handler_provider_readiness.go:123` — `codexAuthFile.Tokens json.RawMessage`
  is parsing an external-tool-owned file, not a wire type. Confirm no
  handler response puts this value on the wire as-is; if it does,
  model the typed cases we care about.

### Dead code (general principle: delete what we don't need)

- `cmd/gc/dashboard/handler.go`, `serve.go`, `handler_test.go` — the
  legacy hand-written Go dashboard proxy. The dashboard is now a TS
  SPA consuming the published API contract. `gc dashboard` must still
  launch and serve the SPA, but via a static file server only. Delete
  all `/v0/*` routing from this path.

### Consumer alignment (ongoing)

- The TS SPA consumes the published API contract via generated TS
  types from `internal/api/openapi.json` and `openapi-fetch`. Any
  `window.prompt` / `window.confirm` interactions and any remaining
  ad-hoc endpoint strings go through typed helpers.
- `gc events` (CLI) reflects the API's event-list and SSE-stream
  contracts exactly. The SSE `event:` field is a transport envelope
  (`event`, `tagged_event`, `heartbeat`); the semantic event type is
  the JSON payload's `type` field. This mapping is documented in
  `docs/reference/api.md`.

## Out of scope

- **`/svc/*` proxy.** See Principle 9.
- **Outbound HTTP** (`internal/extmsg/http_adapter.go`,
  `internal/workspacesvc/proxy_process.go`). Not a typed API endpoint;
  consumes someone else's contract.
- **Storage-layer (de)serialization** (SQL BLOBs, JSONL log files,
  external-tool auth files). Not on our wire.
