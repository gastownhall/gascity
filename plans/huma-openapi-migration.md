# Plan: Type-Safe HTTP + SSE via Huma + OpenAPI 3.1

## Goal

Make the HTTP + SSE surface a pure projection of the core object model
(`internal/{beads,mail,convoy,formula,sling,agent,events,session,...}`),
where the spec is the engine: Go types and handler annotations are the
single source of truth, and the framework handles every byte on the
wire. No hand-written networking. No hand-written JSON. No hand-written
OpenAPI.

The developer-facing consequence: changing types, adding operations,
or adding new event variants is Go code only. Struct definitions and
handler signatures are the contribution; spec, generated clients, TS
types, and docs regenerate from them. CI gates every form of drift.

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

The dashboard is a static TypeScript SPA served by a tiny Go binary
(`cmd/gc/dashboard/`) whose only jobs are to embed the compiled bundle
and inject the supervisor URL into `index.html`. The SPA talks
directly to the supervisor's typed OpenAPI endpoints from the browser
— the dashboard server is NOT an API proxy. The dashboard server also
hosts one narrow operational debug endpoint (`POST /api/clientlog`)
that accepts browser-side error logs for centralized debugging. This
endpoint is intentionally outside the typed HTTP + SSE control plane
these principles govern: it is a one-way sink for diagnostic text, not
a domain API. It may use standard `encoding/json` for body decoding
without violating Principle 4, because it lives outside `internal/api/`
and outside the published OpenAPI contract.

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
`json.NewDecoder` writing or reading wire bodies. Huma owns every byte
that enters or leaves the socket for a typed operation.

Edge cases that are NOT wire:

- SQL/BLOB (de)serialization in storage packages.
- Hashing request bodies for idempotency keys.
- Parsing stored JSONL transcript/log files from disk.
- Parsing external-tool output we don't own (provider CLI stdout,
  provider auth files like `~/.codex/auth.json`).
- Internal event-bus `[]byte` payloads between in-process emitters and
  consumers (but see Principle 7 — these become typed at the wire).

Custom `MarshalJSON` / `UnmarshalJSON` on wire types are forbidden
with two narrow, documented exceptions:

- **`SessionRawMessageFrame`** (Principle 6's third-party provider
  frame hatch) — forwards arbitrary JSON the provider wrote.
- **`EventPayloadUnion`** (`internal/api/convoy_event_stream.go`) —
  the wire wrapper around `events.Payload` that makes the payload
  field a named `oneOf` component in the spec while preserving
  Go-side type safety. Its `MarshalJSON` emits the concrete variant
  directly (so the wire sees `{"rig":...}` rather than a wrapper
  object); its Schema method registers and refs the named component.
  Both are required to get typed oneOf wire emission past the
  limitations of current Go OpenAPI codegen (see Principle 7).

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

### 7. Every event type has a typed wire payload

Every surface that emits events — the SSE streams
(`/v0/events/stream`, `/v0/city/{cityName}/events/stream`) and the
list endpoints (`GET /v0/events`, `GET /v0/city/{cityName}/events`)
— describes its `payload` field as a named `oneOf` union schema
covering every registered `events.Payload` shape. There is no opaque
`payload: {}` anywhere on the wire. The spec enumerates the full
catalog of possible payload shapes; generated clients get a typed
union rather than an interface-to-anything.

The internal event bus (`internal/events`) stores payloads as
`[]byte` so it stays domain-agnostic. That is fine inside the bus.
The event-payload registry (`internal/events/payload.go`) holds the
event-type → Go-type mapping: emitters take values of the sealed
`events.Payload` interface, and the wire projection calls
`events.DecodePayload` to turn bus bytes back into typed Go values
before emission.

**Discrimination design.** The envelope itself carries a plain
`type: string` field; the `payload` field is the discriminated
`oneOf` union. Consumers switch on `type` and narrow `payload`
explicitly (e.g. `if (event.type === "mail.sent") { use(event.payload
as MailEventPayload); }`). We would prefer envelope-level discrimination —
each event-type constant pinned as a `type` const in its own
envelope variant, with OpenAPI 3.1 discriminators giving consumers
automatic narrowing — but no current Go OpenAPI client generator
produces a workable Go type from envelope-level `oneOf`
(oapi-codegen collapses the envelope to a `json.RawMessage` wrapper
that loses all field access; ogen drops SSE endpoints entirely). The
payload-level-union design is the current ceiling given the tooling;
every payload variant is still fully typed on the wire, so consumer
code stays compile-time checked against the full shape catalog.

**Registry coverage.** Every constant in `events.KnownEventTypes`
must have a registered payload. Events that carry no structured data
register `events.NoPayload` — a typed empty struct that still
produces a named schema variant so the wire stays uniform across
event types. `TestEveryKnownEventTypeHasRegisteredPayload` fails CI
if a new constant is added without registration; that's how the
registry discipline stays load-bearing rather than best-effort.

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

## Developer workflow

The invariants above exist so the developer's contribution to the
HTTP + SSE surface is Go code only. Tooling produces everything else.

### Adding or changing a REST operation

1. Edit or add input/output struct types with Huma tags (`json:"..."`,
   `minLength:"1"`, `required:"true"`, etc.).
2. Write the handler function; register via `huma.Register` (or the
   `cityGet` / `cityPost` / `cityPatch` / etc. helpers for
   per-city scoped operations).
3. Commit. Pre-commit regenerates `internal/api/openapi.json`,
   `docs/schema/openapi.json`, `internal/api/genclient/`, and the TS
   types under `cmd/gc/dashboard/web/src/generated/`. Mintlify
   publishes the spec on the next docs build.

### Adding or changing an event type

1. Add the constant to `internal/events/events.go` and append it to
   `events.KnownEventTypes`.
2. Define a typed payload struct implementing `events.Payload` (a
   trivial `IsEventPayload()` method), or use `events.NoPayload` for
   events whose envelope fields alone capture the semantics.
3. Call `events.RegisterPayload(constant, sample)` from an `init()`
   in the domain package that owns the event (e.g.
   `internal/api/event_payloads.go` for mail/bead,
   `internal/extmsg/events.go` for extmsg).
4. Commit. Pre-commit regenerates the discriminated-union wire
   schema; generated clients gain the new typed variant
   automatically.

### Failure modes and their guards

Skipping any step lands on a CI failure, not a production bug:

| Miss | Caught by |
|---|---|
| Spec not regenerated after Go-type change | `TestOpenAPISpecInSync` |
| Generated Go client out of sync with spec | `TestGeneratedClientInSync` |
| Handler response field undeclared in spec | Layer 1 response-validation tests |
| Spec/client method-shape drift | Layer 2 round-trip tests |
| End-to-end binary wire regression | Layer 3 integration tests |
| New event-type constant without registered payload | `TestEveryKnownEventTypeHasRegisteredPayload` |
| Hard-coded SPA `/v0/...` path outside typed client | TypeScript build (`satisfies SpecPath`) |

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

None open. Every principle holds end-to-end including the events
surface — both SSE streams (`/v0/events/stream`,
`/v0/city/{cityName}/events/stream`) and list endpoints
(`GET /v0/events`, `GET /v0/city/{cityName}/events`) emit typed
`payload` fields as a named `oneOf` union over every registered
`events.Payload` shape. `event.Event` and `event.TaggedEvent` (the
bus-internal types with opaque payload bytes) are no longer wire
types — the handlers convert to `WireEvent` / `WireTaggedEvent`
before returning. The registry-coverage test
(`event_payloads_coverage_test.go`) fails CI if a new event-type
constant is added without registering a payload.

**Tooling ceiling, not a plan gap.** Envelope-level `oneOf`
discrimination (each event-type constant as a `type` const on its
own envelope variant) would give consumers automatic discriminator
narrowing, but no current Go OpenAPI client generator renders that
design into usable Go types — see the note under Principle 7 and
`cmd/gen-client/main.go` for the details. If a generator lands that
fixes this, revisit.

## Consumer alignment (ongoing)

- The TS SPA consumes the published API contract via generated TS
  types from `internal/api/openapi.json` and `openapi-fetch`. SSE
  path templates are checked against the spec at compile time via
  `sseSupervisorEventsURL` / `sseCityEventsURL` / `sseSessionStreamURL`
  in `cmd/gc/dashboard/web/src/api.ts`.
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
