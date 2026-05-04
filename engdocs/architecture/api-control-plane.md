---
title: "API Control Plane"
description: "Current-state architecture for Gas City's CLI, HTTP, SSE, generated client, and typed-wire contract."
---

> Last verified against code: 2026-04-22

This architecture doc captures the API control-plane invariants Gas
City has converged on. It is normative current-state documentation:
future contributions that violate these invariants are wrong unless a
conscious decision updates this document. Plans in `plans/archive/`
describe the journeys that produced these invariants; this document
describes the destination.

Two architectural themes run through everything below:

1. **The object model is the center; the CLI and the HTTP + SSE API
   are projections over it.** One canonical domain, two typed
   surfaces.
2. **Typed data end-to-end.** Go structs with annotations drive a
   generated OpenAPI 3.1 contract; every wire-visible shape appears
   in the OpenAPI spec; consumers in any language code against the same
   contract. Zero opacity on the wire.

## 1. The object model

`internal/{beads, mail, convoy, formula, agent, events, session,
sling, graphroute, agentutil, pathutil, cityinit, ...}` is the
canonical domain. All business logic lives there. The two surfaces
below call into it; neither re-implements validation, routing, or
invariants.

City initialization is a worked example: the HTTP handler for
`POST /v0/city` does **not** shell out to `gc init`; it calls
`cityinit.Service.Scaffold` in-process, and the CLI drives the same
`cityinit.Service.Init` contract. The scaffolded city registers with
the supervisor synchronously before `202 Accepted` returns; the
reconciler runs the slow finalize later and publishes a
`request.result` event. Both projections live on the same typed
contract and error sentinels (`cityinit.ErrAlreadyInitialized`,
`ErrInvalidProvider`, `ErrMissingDependency`, `ErrProviderNotReady`,
`ErrInvalidBootstrapProfile`). Long-running mutations in general
follow this shape: validate and create intent synchronously, return
202 with a `request_id`, run the expensive work in a background
goroutine, publish a `request.result` event on completion or
failure — subscribers watch the event stream instead of polling.
See `engdocs/design/async-request-result.md` for the full pattern.

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

`internal/api/genclient/` has three in-tree consumer categories,
governed by a structural rule: **direct consumption is allowed for
endpoints that (a) do not participate in write-side fallback (no
`ShouldFallback` path) and (b) do not require domain-type conversion
at the adapter seam.** Anything that fails either test goes through
`internal/api/client.go`.

1. **CLI mutation coordination** via `internal/api/client.go`, used
   by `cmd/gc/apiroute.go` as described above. This is the only
   consumer for paths that mutate state and could race an in-process
   supervisor, or that need domain-type conversion (e.g. typed
   `session.SubmitIntent` from a string wire field). The adapter
   also owns local-file fallback when the controller isn't running.
2. **Read/stream CLI surfaces that import genclient directly** —
   currently `cmd/gc/cmd_events.go`, which calls typed methods for
   event listing and SSE following. Events have no write-side
   fallback (no bus without a controller) and need no domain-type
   conversion, so they satisfy the structural rule. Future
   read-only CLI surfaces that meet the same two conditions are
   allowed to import genclient directly; no case-by-case approval
   needed.
3. **Layer 2 conformance probe** —
   `genclient_roundtrip_test.go` exercises every generated method
   against a real supervisor so spec/reality drift fails CI.

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

Framework-level cross-cutting wire contract (CSRF header, request-ID
response header, per-stream status headers) does not live on
per-endpoint struct annotations; it lives on the registration
helpers (`cityPost`, `cityRegister`, `registerSSE`) or on a
post-registration spec walker (`registerFrameworkHeaders`). That is
not a second description — it is the same mechanism applied
at one layer up, and the OpenAPI spec that results still describes
every operation's full contract. See §3.5.2. Patterns and Huma
quirks that inform these helpers are documented in
[Huma Usage Notes](../contributors/huma-usage.md).

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

### 3.4 No hand-constructed JSON for domain data

Every wire byte that represents a domain value comes from encoding
a typed Go struct (schema-registered with Huma) through the
standard JSON encoder, directly or via Huma's own serialization
machinery. This principle forbids three anti-patterns specifically:

- `json.Marshal(map[string]any{...})` — untyped input.
- `fmt.Sprintf`-built JSON strings — hand-constructed shape.
- `json.Marshal(anyInterfaceValue)` where the interface carries
  values whose types are not schema-registered — hides the shape
  from the spec.

The test a reviewer applies: *is there any line in your code that
produces JSON-shaped output from non-typed or map-typed input?* If
yes, violation. If every JSON byte comes from `encoder.Encode` of a
typed, schema-registered struct, the principle holds.

Protocol framing around domain data — HTTP status codes, HTTP
response headers, SSE `id:` / `event:` / `data:` / retry line
separators, chunked-encoding bytes — is not domain data and is not
in scope for this principle. The carve-out is direction-symmetric
and covers two specific files: `internal/api/sse.go` (emitter)
hand-writes the SSE protocol-text lines around a typed
`encoder.Encode(data)` call on a registered struct, and
`cmd/gc/cmd_events.go:sseDecoder` (consumer) hand-parses the same
SSE protocol-text lines and `json.Unmarshal`s the `data:` payload
into typed `genclient.*` structs. In both directions the domain
payload IS framework-encoded/decoded; the surrounding protocol
literals are not JSON at all.

New SSE endpoints must register through `registerSSE` /
`registerSSEStringID`; ad-hoc SSE handlers outside those helpers
are not covered by this carve-out.

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
  — the raw-frame pass-through for provider-native session
  transcripts; forwards arbitrary JSON the provider wrote. See §3.6.
- **`EventPayloadUnion`** (`internal/api/convoy_event_stream.go`)
  — the wire wrapper around `events.Payload` that emits the typed
  payload as a named `oneOf` component. Its `MarshalJSON` emits
  the concrete variant directly (so the wire sees `{"rig":...}`
  rather than a wrapper object); its Schema method registers and
  refs the named component. Required to get a single named
  `EventPayload` component schema that Go and TS clients can both
  consume.

### 3.5 Typed structs for every shape knowable at compile time

Every response field, every SSE event payload, every input body is
a named Go struct with real fields and Huma tags. No
`json.RawMessage` or `map[string]any` in the typed control plane,
with exactly one class of exception (§3.6).

"Heterogeneous", "opaque", "clients render it generically", "we'll
figure out the union later", and "it's just internal" are not
qualifying exceptions. If our code constructs the map, we know the
keys. Make it a struct.

### 3.5.1 No hidden inputs — every accepted parameter appears in the spec

Every input a handler reads MUST be a typed field on its Huma
input struct (`path:`, `query:`, `header:`, or `Body`). The
generated OpenAPI spec is the complete and exhaustive description
of the inputs an endpoint accepts. Running a request through a
handler must not produce a different outcome than running the same
request through the spec.

Three anti-patterns are specifically forbidden:

- **Dynamic or wildcard query parameters.** Any scheme where a
  handler accepts query keys matching a pattern (`var.*`, `meta_*`,
  `x-*`) rather than declared names. OpenAPI 3.1 cannot express
  wildcard query keys; accepting them creates a hidden contract
  the spec cannot describe. When a handler needs an open-ended
  string-to-string dictionary as input, move the input into a
  typed request body field (`Vars map[string]string` on a POST
  body). Dictionary bodies have a schema; dictionary query
  parameters do not.
- **Resolvers that read raw URL query or header values that
  aren't declared input fields.** `huma.Resolver` implementations
  may validate or normalize values the struct already declares,
  but may not read keys off `ctx.URL().Query()` or `ctx.Header()`
  that aren't present on the input struct. If a resolver needs a
  value, that value is a declared field — no exceptions.
- **Presence-vs-empty semantics via raw-URL inspection.** If a
  handler behaves differently for "parameter absent" vs "parameter
  present with empty value", the presence flag must come from
  Huma's parameter binder — not from peeking at `ctx.URL().Query()`
  inside a Resolver. Use the `huma.OptionalParam[T]`-style wrapper
  (see `internal/api/huma_optional_param.go`): a custom type with
  `Schema()`, `Receiver()`, and `OnParamSet(isSet bool, ...)` that
  emits the underlying `T`'s schema on the wire and exposes an
  `IsSet` flag to the handler. Huma v2 does not support pointer
  query parameters (they panic at registration, see
  `github.com/danielgtaylor/huma` issue #288); `OptionalParam` is
  the framework-sanctioned idiom.

  Practical corollary: Huma's parameter binder treats `?cursor=`
  (empty value) identically to an absent parameter
  (`huma.go:881-882: isSet = value != ""`). A three-state contract
  (absent / present-empty / present-nonempty) is therefore not
  expressible against Huma; the wire contract collapses to
  two states. Design APIs around that.

The test a reviewer applies: does running an undeclared query
parameter or an undeclared body field through the handler change
its behavior? If yes, violation. The spec is the contract; the
handler does not get a second, private contract the spec doesn't
know about.

Huma does not reject undeclared query parameters by default
(they are silently ignored). That is not permission to rely on
them — silent acceptance of undeclared parameters is a property
of the framework, not a blessing of hidden contract. Callers that
send undeclared parameters are sending noise; handlers that read
them are violating this principle.

### 3.5.2 Framework-level headers declared once, not per-operation

Wire contract that applies uniformly across every operation —
CSRF request headers, request-ID response headers, the custom
response headers SSE streams emit for runtime status — is still
real wire contract and must appear in the spec. OpenAPI 3.1 has
no mechanism to declare headers "globally" for all operations
(see
[speakeasy.com/openapi/responses/headers](https://www.speakeasy.com/openapi/responses/headers));
the canonical pattern is:

1. Define the header once. Request headers live as operation
   parameters; response headers get a named entry in
   `components.headers`.
2. Reference it from every operation it applies to (request
   params go on the Operation's `Parameters`; response headers
   use `{"$ref": "#/components/headers/NAME"}`).

Rather than embedding the reference in 50+ input/output structs,
attach it at the single function every operation already flows
through:

- **Request headers (e.g. `X-GC-Request`)** — `cityPost`,
  `cityPut`, `cityPatch`, `cityDelete`, and `cityRegister` in
  `internal/api/city_scope.go` pass `addMutationCSRFParam` as a
  Huma operation handler. One line at the route helper covers
  every current and future mutation endpoint.
- **Response headers (e.g. `X-GC-Request-Id`)** —
  `registerFrameworkHeaders` in
  `internal/api/huma_spec_framework.go` runs once after all
  routes are registered. It populates `components.headers` and
  walks every operation's responses to inject a `$ref` pointing
  at the named component.
- **Per-stream custom response headers (e.g. `GC-Agent-Status`,
  `GC-Session-State`, `GC-Session-Status`)** — catalogued in
  `sseStatusHeaders` (`internal/api/sse.go`) and referenced by
  name at each `registerSSE` call site via
  `sseResponseHeaders("GC-Agent-Status")`. Colocated with the
  operation where the handler emits the header, one catalog
  entry per header.

These patterns are not exceptions to §3.5.1; they are the
§3.5.1-compliant mechanism for cross-cutting concerns. The spec
still fully describes the contract — every operation's parameters
and response headers list the header explicitly — but the
declaration happens at one function call, not fifty struct
definitions. Middleware remains the single source of enforcement;
the spec remains the single source of description; the helpers
keep the two aligned.

### 3.6 Raw pass-through for provider-native session frames

Session transcript streaming and query endpoints forward
provider-native frames with full fidelity. Each response/envelope
identifies the producing provider via a `provider` field whose
value is one of the known provider keys (`claude`, `codex`,
`gemini`, `open-code`, etc.); each frame's JSON is emitted verbatim
as the provider wrote it, with no GC-side interpretation.
Consumers parse frames using provider-specific logic on their side,
keyed by the provider identifier on the envelope.

The single JSON-pass-through wire type is `SessionRawMessageFrame`
(`internal/api/session_frame_types.go`). Its Schema method emits
an "any JSON value" schema because Gas City does not own the
shape of provider frames. Publishing typed wire schemas for
provider frames would claim a contract we don't own: a provider
could change its frame shape tomorrow and the spec would silently
lie until regenerated. Honest opacity with a provider discriminator
is the right design.

Passing through externally-authored shapes is not a license to
also opacify our own shapes that happen to be nested near them.
Every GC-owned field on the same envelope as the raw frames
(envelope metadata, provider identifier, session info) stays
typed.

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

**Decode-failure policy (uniform across list and stream).** Decode
failures and unregistered event types are omitted from list and
stream output and logged via `log.Printf`; the wire never carries
a degraded envelope with nil payload. A malformed event is a CI
bug (the registry-coverage test above catches it before prod);
emitting a typed envelope with `payload: null` would train
consumers to tolerate broken payloads, defeating the point of
§3.4. Clean omission plus a loud log is the contract.

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

## 6. Tooling landscape

Principle 7's "payload-field-level discrimination rather than
envelope-level" is a Go-tooling constraint, not a principled
preference. The TypeScript and Go ecosystems differ on what they
support; this section records what we evaluated and what we use
per language.

### Go (server-side Huma, client via oapi-codegen)

- **Huma v2** — server framework. Generates OpenAPI 3.1 from
  annotated Go types; we use it for every typed endpoint. Emits a
  3.0 downgrade on request for consumers that still need 3.0.
- **oapi-codegen** — our current Go client generator. Supports
  OpenAPI 3.0 (we feed it the downgrade from Huma). When given
  envelope-level `oneOf`, it generates `struct { union
  json.RawMessage }` with `AsX`/`FromX`/`MergeX` accessor methods.
  That shape breaks field-based construction in
  `cmd/gc/cmd_events.go`. It does generate typed request methods
  for SSE endpoints, but does not parse SSE frames — the caller
  handles framing.
- **ogen** — evaluated via spike. Refuses `text/event-stream`
  content type entirely; every SSE endpoint is dropped from the
  generated client. With `ignore_not_implemented: all`, ogen
  produces clean REST types but drops SSE operations Gas City is
  built on. Not viable.
- **openapi-generator** (Java-based) — breaks the pure-Go toolchain
  and generates less-idiomatic Go.
- **Commercial SDK generators** (Speakeasy, Fern, Stainless) —
  generate typed Go SSE clients including envelope-level `oneOf`
  handling. Not open source; paid plans start at ~$250/mo.

The payload-field-union `EventPayload` design (Principle 7) is the
current ceiling under open-source Go tooling. Revisit if
oapi-codegen's experimental 3.1/3.2-aware branch stabilizes or if
another open-source Go generator ships envelope-level `oneOf` plus
SSE that works with our shape.

### TypeScript (dashboard SPA)

- **`openapi-fetch`** — typed `fetch` wrapper, the tool the
  dashboard uses for every REST call site. Typed path/body/response
  against `openapi-typescript`-generated `schema.d.ts`. Minimal
  runtime, well-documented, keeps REST call-site code short. Does
  not handle SSE — that's what drives the dual-tool design below.
- **`@hey-api/openapi-ts`** — open-source generator the dashboard
  uses exclusively for SSE. Generates typed stream functions using
  `fetch()` + `ReadableStream` (not `EventSource`), which means
  custom auth headers work, retry with exponential backoff is
  built in, and each stream has typed discriminated-union response
  types keyed by the SSE `event` name. `sse.ts` is a thin callback
  bridge over the generated `streamSupervisorEvents`,
  `streamEvents`, and `streamSession` functions; the per-frame
  JSON parsing, line buffering, and retry are all framework code.
- **`openapi-typescript-codegen`** — unmaintained.
- **OpenAPI Generator** (Java) — same pure-toolchain concern as Go.

The dual-tool design is pragmatic, not aspirational: each library
handles what it's good at. `openapi-fetch` is the minimal typed
surface for REST consumers (kept because it has zero impact on
call-site code and the ecosystem has shifted to hey-api slowly
enough that we'd gain nothing by churning every REST call today).
`@hey-api/openapi-ts` is the only open-source TS tool that
generates typed SSE stream clients, and it handles every aspect of
the SSE wire that used to be hand-rolled in `sse.ts`.

The Go-side `oneOf` ceiling described above does not apply to
TypeScript consumers. SSE frames come typed and discriminated
through the generated stream functions; consumers get automatic
`switch (frame.event)` narrowing with no hand-written parser or
type guard in the SPA.

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

Every file-path citation in this document is load-bearing. If you
rename or remove a cited symbol (`events.KnownEventTypes`,
`EventPayloadUnion`, `TestEveryKnownEventTypeHasRegisteredPayload`,
`cmd/gc/apiroute.go:apiClient()`, `addMutationCSRFParam`,
`registerFrameworkHeaders`, `sseResponseHeaders`,
`OptionalParam`, `cityinit.Service`, `cityinit.InitRequest`,
`cityinit.InitResult`, `cityinit.UnregisterRequest`,
`cityinit.UnregisterResult`, `cityinit.ErrNotRegistered`,
`TransientCityEventSource`, etc.), **update this document in the same
commit**. Stale architecture docs are worse than no docs — they
mislead future agents about what invariants hold.

Framework-specific patterns and Huma quirks are captured in
[Huma Usage Notes](../contributors/huma-usage.md); update that file
in the same commit when you touch any of: `OptionalParam`,
`addMutationCSRFParam`, `registerFrameworkHeaders`,
`sseResponseHeaders`, the SSE hand-writing zone, or the
`cityPost`/`cityRegister` helper family.

Line numbers are deliberately omitted so the spec survives
refactors. Package names, type names, and test names are stable
anchors.
