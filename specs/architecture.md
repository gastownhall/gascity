# Gas City Architecture

This spec captures the architectural invariants Gas City has
converged on. It is a normative document: future contributions that
violate these invariants are wrong unless a conscious decision in
this spec changes. Plans in `plans/archive/` describe the journeys
that produced these invariants; this spec describes the
destination.

Two architectural themes run through everything below:

1. **The object model is the center; the CLI and the HTTP + SSE API
   are projections over it.** One canonical domain, two typed
   surfaces.
2. **Typed data end-to-end.** Go structs with annotations drive a
   generated OpenAPI 3.1 contract; every wire-visible shape appears
   in the spec; consumers in any language code against the same
   contract. Zero opacity on the wire.

## 1. The object model

`internal/{beads, mail, convoy, formula, agent, events, session,
sling, graphroute, agentutil, pathutil, ...}` is the canonical
domain. All business logic lives there. The two surfaces below call
into it; neither re-implements validation, routing, or invariants.

```
cmd/gc/cmd_*.go               internal/api/handler_*.go
  (arg parsing,                 (Huma input/output types,
   text formatting,              handler bodies,
   exit codes)                   typed error returns)
        \                              /
         \                            /
          v                          v
   internal/sling/        internal/convoy/
   internal/agentutil/    internal/graphroute/
   internal/pathutil/
            |
            v
   internal/{beads, config, formula, molecule, agent, events, ...}
```

### Invariants

- **Domain code has no I/O surfacing.** No `fmt.Fprintf`, no
  `io.Writer` parameters, no HTTP responses. Domain functions
  return values and errors. Text formatting is a CLI concern; JSON
  shaping is an API concern.
- **Narrow interfaces over flag bags.** Domain-side dependencies
  use focused interfaces (`AgentResolver`, `BeadRouter`,
  `Notifier`, `BranchResolver`) validated at construction.
- **Intent-based APIs.** Callers express intent (`RouteBead`,
  `LaunchFormula`, `AttachFormula`, `ExpandConvoy`); implementation
  decides how (shell command, direct store, API call). No
  god-struct option bags passed around.
- **No upward dependencies.** A lower layer never imports from a
  higher layer.

## 2. Projections: CLI and HTTP + SSE API

### CLI projection (`cmd/gc/`)

The CLI calls the core library directly. It is not a generic
remote client; it coexists with a local supervisor in the same city
by routing through HTTP only when lock coordination requires it.

Concretely, `cmd/gc/apiroute.go:apiClient()` implements this rule:

- **No running local supervisor** → CLI calls the core library
  directly against the on-disk stores.
- **Running local supervisor with mutations allowed** → CLI routes
  the mutation through the local HTTP API via the generated Go
  client. The supervisor executes the mutation under its own
  locks; the CLI's result is consistent with the supervisor's
  state.

Remote access is not the first-class reason this path exists. A
`--base-url http://remote:port` invocation is a side effect of the
same mechanism, not its purpose. The generated client is "library
calls dispatched over HTTP when we have to cross a process
boundary we didn't create."

### API projection (`internal/api/`)

Every HTTP + SSE endpoint is registered through Huma against
annotated Go types. Huma generates the OpenAPI 3.1 spec from those
types; the spec drives everything downstream.

### The generated Go client

`internal/api/genclient/` has exactly two in-tree consumers:

1. **CLI multi-process coordination** via `internal/api/client.go`,
   used by `cmd/gc/apiroute.go` as described above.
2. **Layer 2 conformance probe** — `genclient_roundtrip_test.go`
   exercises every generated method against a real supervisor so
   spec/reality drift fails CI.

The generated client is not promoted as a public Go SDK for
external consumers. External Go consumers, if they ever appear,
get a supported surface at that point; until then the `internal/`
location is load-bearing.

### The dashboard projection

The dashboard is a static TypeScript SPA served by a tiny Go
binary (`cmd/gc/dashboard/`) whose only jobs are to embed the
compiled bundle and inject the supervisor URL into `index.html`.
The SPA talks directly to the supervisor's typed OpenAPI endpoints
from the browser — the dashboard server is NOT an API proxy. The
dashboard server also hosts one narrow operational debug endpoint
(`/__client-log`) that accepts browser error logs for centralized
debugging; this endpoint is intentionally outside the typed HTTP +
SSE control plane and may use standard `encoding/json` for body
decoding.

## 3. The typed-wire principle

The invariants below apply to every operation under `internal/api/`
except the `/svc/*` workspace-service proxy (see §5).

### 3.1 Annotations drive the live implementation

Each endpoint is a Go function whose signature (typed input struct,
typed output struct) plus a `huma.Operation` value IS the endpoint
definition. Huma binds it, validates it, routes it, serializes it,
schema-describes it. There is no second description of the endpoint
anywhere — not in a router table, not in an OpenAPI YAML, not in a
client stub.

### 3.2 Spec is generated, never hand-written

`internal/api/openapi.json` and `docs/schema/openapi.json` are
outputs of `cmd/genspec`, which reads the live Huma registration
from a `SupervisorMux`. The pre-commit hook regenerates both on
every Go-file commit. `TestOpenAPISpecInSync` fails CI if the
committed spec drifts from what the supervisor serves.

### 3.3 The routes we register ARE the routes we expose

Per-city operations live at `/v0/city/{cityName}/...`.
Supervisor-scope operations live at their top-level paths. No
shadow mapping. No `prefix-strip-and-forward`. No client-side
path-rewrite helpers. The existence of such a helper is direct
evidence the spec disagrees with reality and is a bug to fix.

### 3.4 Zero hand-written JSON in the typed control plane

No `json.Marshal` or `json.Unmarshal` in any HTTP or SSE code path
that touches bytes owned by our API contract. No
`json.NewEncoder` / `json.NewDecoder` writing or reading wire
bodies. Huma owns every byte that enters or leaves the socket for
a typed operation.

Edge cases that are NOT wire and therefore exempt:

- SQL/BLOB (de)serialization in storage packages.
- Hashing request bodies for idempotency keys.
- Parsing stored JSONL transcript/log files from disk.
- Parsing external-tool output we don't own (provider CLI stdout,
  provider auth files like `~/.codex/auth.json`).
- Internal event-bus `[]byte` payloads between in-process emitters
  and consumers (these become typed at the wire via the registry —
  see §4).

Custom `MarshalJSON` / `UnmarshalJSON` on wire types are forbidden
with two narrow, documented exceptions:

- **`SessionRawMessageFrame`** (`internal/api/session_frame_types.go`)
  — the third-party provider frame hatch; forwards arbitrary JSON
  the provider wrote. See §3.6.
- **`EventPayloadUnion`** (`internal/api/convoy_event_stream.go`)
  — the wire wrapper around `events.Payload` that emits the typed
  payload as a named `oneOf` component. Its `MarshalJSON` emits
  the concrete variant directly (so the wire sees `{"rig":...}`
  rather than a wrapper object); its Schema method registers and
  refs the named component. Both methods are necessary for the
  current Go OpenAPI tooling (see §6 for the tooling note).

### 3.5 Typed structs for every shape knowable at compile time

Every response field, every SSE event payload, every input body is
a named Go struct with real fields and Huma tags. No
`json.RawMessage` or `map[string]any` in the typed control plane,
with exactly one class of exception (§3.6).

"Heterogeneous", "opaque", "clients render it generically", "we'll
figure out the union later", and "it's just internal" are not
qualifying exceptions. If our code constructs the map, we know the
keys. Make it a struct.

### 3.6 Raw pass-through only for shapes unknowable at compile time

The single legitimate case for `json.RawMessage` on the wire is
content authored outside our source tree that we forward verbatim
and cannot enumerate statically. The canonical example is
third-party provider session transcript frames: Gas City is an
SDK, users plug in providers via config, and their frame shapes
are not in our source tree.

The rule for this case:

- First-party provider frame shapes ARE modeled as named schemas
  in the spec (see `internal/api/session_frame_types.go` for Codex
  and Gemini types). Consumers code against the typed cases for
  the common path.
- Only truly unknown third-party frames fall through to the raw
  hatch.
- The raw hatch is a single named type with a documented reason
  in its doc comment explaining why it cannot be typed.

Passing through externally-authored shapes is not a license to
also opacify our own shapes that happen to be nested near them.

### 3.7 Every event type has a typed wire payload

See §4.

### 3.8 Error responses are typed too

Every error returned by a Huma handler is a
`huma.StatusError`-producing call with a real problem-details
body. No `apiError{}` shortcuts. No hand-written `writeError`.

For the outermost panic-recovery middleware (which must run before
Huma enters the stack), error bodies are pre-serialized
`application/problem+json` byte constants — one `var` declaration
per well-known error, no runtime `json.Marshal`. The constants
live in `internal/api/middleware.go` as `problemBody` values.

### 3.9 `/svc/*` is the only exclusion

`/svc/*` is a raw pass-through to external service processes that
own their own contracts. It is explicitly not a typed API
surface. This is the single carved-out path inside `internal/api/`.
If `/svc/*` ever becomes typed, it gets its own migration.

## 4. Event typing (the registry)

Events are a first-class part of the typed wire contract. Both the
SSE streams (`/v0/events/stream`,
`/v0/city/{cityName}/events/stream`) and the list endpoints
(`GET /v0/events`, `GET /v0/city/{cityName}/events`) describe their
`payload` field as a named `oneOf` union covering every registered
`events.Payload` shape. There is no opaque `payload: {}` anywhere
on the wire.

### Mechanism

- **Bus layer (`internal/events`)** stores payloads as `[]byte` so
  it stays domain-agnostic. `events.Event` and `events.TaggedEvent`
  are bus-internal types only; they are never returned directly
  from an HTTP handler.
- **Registry (`internal/events/payload.go`)** holds the event-type
  → Go-type mapping. `events.RegisterPayload(typeConst, sample)`
  associates a constant with a sample value of a type implementing
  the sealed `events.Payload` interface. `events.DecodePayload`
  turns bus bytes back into the registered typed value.
- **Emitters** take values of `events.Payload` rather than
  `map[string]any`. The sealed interface keeps ad-hoc shapes out
  of emission sites at compile time.
- **Wire projection** — the API-layer `WireEvent` /
  `WireTaggedEvent` types (list) and `eventStreamEnvelope` /
  `taggedEventStreamEnvelope` (SSE) carry a typed `Payload` field
  wrapped in `EventPayloadUnion`. `EventPayloadUnion.Schema`
  registers a named `EventPayload` component whose schema is a
  `oneOf` of every registered payload type.

### Registry coverage

Every constant in `events.KnownEventTypes` MUST have a registered
payload. Events that carry no structured data register
`events.NoPayload` — a typed empty struct that still produces a
named schema variant so the wire stays uniform across event types.

`TestEveryKnownEventTypeHasRegisteredPayload` fails CI if a new
constant is added without registration; that's how the registry
discipline stays load-bearing rather than best-effort.

### Discrimination design

The envelope carries a plain `type: string` field; the `payload`
field is the discriminated `oneOf` union. Consumers switch on
`type` and narrow `payload` explicitly:

```typescript
if (event.type === "mail.sent") {
  use(event.payload as MailEventPayload);
}
```

Envelope-level discrimination — each event-type constant pinned
as a `type` const in its own envelope variant, with OpenAPI 3.1
discriminators giving consumers automatic narrowing — would be
nicer. It is not the design because no current Go OpenAPI client
generator produces a workable Go type from envelope-level
`oneOf`:

- **oapi-codegen** collapses the envelope to a `json.RawMessage`
  wrapper that loses all field access — `cmd/gc/cmd_events.go`'s
  field-based construction breaks.
- **ogen** drops `text/event-stream` operations entirely —
  the events streams disappear from the generated client.

The payload-field-union design is the current ceiling. Every
payload variant is still fully typed on the wire; consumers narrow
explicitly rather than getting automatic discriminator narrowing.
See §6 for the full tooling note.

## 5. Developer workflow

The invariants above exist so the developer's contribution to the
HTTP + SSE surface is Go code only. Tooling produces everything
else.

### Adding or changing a REST operation

1. Edit or add input/output struct types with Huma tags
   (`json:"..."`, `minLength:"1"`, `required:"true"`, etc.).
2. Write the handler function; register via `huma.Register` (or
   the `cityGet` / `cityPost` / `cityPatch` / etc. helpers in
   `internal/api/city_scope.go` for per-city scoped operations).
3. Commit. Pre-commit regenerates `internal/api/openapi.json`,
   `docs/schema/openapi.json`, `internal/api/genclient/`, and the
   TS types under `cmd/gc/dashboard/web/src/generated/`. Mintlify
   publishes the spec on the next docs build.

### Adding or changing an event type

1. Add the constant to `internal/events/events.go` and append it
   to `events.KnownEventTypes`.
2. Define a typed payload struct implementing `events.Payload` (a
   trivial `IsEventPayload()` method), or use `events.NoPayload`
   for events whose envelope fields alone capture the semantics.
3. Call `events.RegisterPayload(constant, sample)` from an
   `init()` in the domain package that owns the event (e.g.
   `internal/api/event_payloads.go` for mail/bead;
   `internal/extmsg/events.go` for extmsg).
4. Commit. Pre-commit regenerates the discriminated-union wire
   schema; generated clients gain the new typed variant
   automatically.

### CI guards

Skipping any step lands on a CI failure, not a production bug:

| Miss | Caught by |
|---|---|
| Spec not regenerated after Go-type change | `TestOpenAPISpecInSync` |
| Generated Go client out of sync with spec | `TestGeneratedClientInSync` |
| Handler response field undeclared in spec | Layer 1 response-validation tests |
| Spec/client method-shape drift | Layer 2 round-trip tests (`genclient_roundtrip_test.go`) |
| End-to-end binary wire regression | Layer 3 integration tests (`//go:build integration`) |
| New event-type constant without registered payload | `TestEveryKnownEventTypeHasRegisteredPayload` |
| Hard-coded SPA `/v0/...` path outside typed client | TypeScript build (`satisfies SpecPath` in `api.ts`) |

## 6. Tooling ceiling

Principle 7's "payload-field-level discrimination rather than
envelope-level" is not a principled preference; it's a tooling
ceiling. A future contributor evaluating whether to revisit
envelope-level `oneOf` should know what was tried and why it was
rejected:

- **oapi-codegen** is the current Go client generator. It supports
  OpenAPI 3.0 (we downgrade the 3.1 spec in `cmd/gen-client` before
  feeding it in). When given envelope-level `oneOf`, it generates
  `struct { union json.RawMessage }` with `AsX`/`FromX`/`MergeX`
  accessor methods. That shape breaks the CLI's field-based
  construction in `cmd/gc/cmd_events.go` and cannot be directly
  initialized with the envelope fields the list endpoint surfaces.
- **ogen** is the main alternative. It refuses `text/event-stream`
  content type entirely; every SSE endpoint gets skipped from the
  generated client. With `ignore_not_implemented: all`, ogen
  produces clean types for REST but drops the SSE operations
  Gas City is built on. Not viable.
- **openapi-generator** (Java-based) breaks the pure-Go toolchain
  and generates less-idiomatic Go.
- **Huma's own client generator** does not exist yet (project
  discussion, no code).

The payload-field-union design is the current best trade-off. If
any of the above generators fixes its limitation, envelope-level
discrimination becomes available and this spec should be revisited.

## 7. What is out of scope

- **`/svc/*` proxy.** See §3.9.
- **Outbound HTTP** (`internal/extmsg/http_adapter.go`,
  `internal/workspacesvc/proxy_process.go`). Not typed API
  endpoints; we consume someone else's contract.
- **Storage-layer (de)serialization** (SQL BLOBs, JSONL log files,
  external-tool auth files). Not on our wire.
- **Generated Go client as a Go SDK surface.** Stays in
  `internal/` until external consumers show up.
- **WebSocket transport.** HTTP + SSE only. OpenAPI 3.1 + Huma
  covers SSE end-to-end, so AsyncAPI / Modelina are not in play.

## 8. Maintenance rule

Every file-path citation in this spec is load-bearing. If you
rename or remove a cited symbol (`events.KnownEventTypes`,
`EventPayloadUnion`, `TestEveryKnownEventTypeHasRegisteredPayload`,
`cmd/gc/apiroute.go:apiClient()`, etc.), **update this spec in the
same commit**. A stale spec is worse than no spec — it misleads
future agents about what invariants hold.

Line numbers are deliberately omitted so the spec survives
refactors. Package names, type names, and test names are stable
anchors.
