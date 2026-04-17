# Plan: Replace Network Layer with Huma + OpenAPI 3.1

## Status: Phase 1 + 2 + 3 + 3.5 Complete (server + CLI). Dashboard migration out of scope.

**Phase 3.5 "real routes, real types" (shipped 2026-04-17):** Every
per-city operation is registered on the supervisor's single Huma API
at its real, user-facing scoped path (`/v0/city/{cityName}/...`). The
committed OpenAPI spec describes exactly the URLs external clients use.
No shadow mapping. No prefix-strip-and-forward. No client-side path
rewrite helper. The per-city `Server` is now a handler-host only; its
only mux registration is the `/svc/*` pass-through.

Across 7 commits (`e330e95e` → `f0644db9`) every input type embeds
`CityScope`; every registration moved from `Server.registerRoutes` to
`SupervisorMux.registerCityRoutes`; SSE streams (agent output, session,
events) wrap their precheck/streamer with per-call city resolution.
`grep '"/v0/agents"\|"/v0/beads"\|"/v0/mail"\|"/v0/convoys"'
internal/api/openapi.json` returns zero; the spec contains 94 scoped
paths and the seven supervisor-scope paths (cities, health, readiness,
provider-readiness, city-create, events, events/stream).

Plan approved 2026-04-16 after three rounds of external review (Claude
+ Codex + Gemini). Phase 3 fixes 3.0 / 3a (CLI surface) / 3b / 3c / 3d /
3e / 3f / 3g / 3h / 3j / 3k / 3l shipped across commits `0e0c1881`,
`c509ec5f`, `863a3883`, `cdd8e2dc`, plus the post-review tightening in
this branch (spec-pipeline unification, per-city middleware on Huma,
handler-side validation cleanup, supervisor topology collapse). The
typed REST/SSE control plane on the server side and the CLI client on
the consumer side are both fully spec-driven.

**Post-3.5 tightening (2026-04-17, this branch):**

- **Spec published to docs.** `cmd/genspec` now writes both
  `internal/api/openapi.json` (drift-check source of truth) and
  `docs/schema/openapi.json` (Mintlify-served copy) in one run.
  `.githooks/pre-commit` regenerates on every Go-file commit;
  `docs/reference/api.md` is the published overview and links to the
  downloadable spec.
- **Three-layer spec-driven test coverage** (see "Testing strategy"
  below): schema-driven response validation, generated-client
  round-trip, and a binary integration smoke test.
- **Fix 3k remnant closed.** Every unconditional `input.Body.X == ""`
  guard in `huma_handlers_extmsg.go` moved to `minLength:"1"` on the
  body tags; one runtime-state-dependent guard (inbound raw-payload
  path, conditioned on `Message == nil`) kept with a comment.
- **Legacy error-body fallback deleted from `client.go`.** The adapter
  now consumes the generated client's typed `*genclient.ErrorModel`
  directly; `parseProblemDetails`, `jsonUnmarshalTolerant`, and
  `client_helpers.go` are gone. `client_test.go` error-path mocks
  emit RFC 9457 Problem Details (`application/problem+json`) only.
- **Events-stream precheck hardened (Codex Critical).**
  `/v0/events/stream` now returns 503 Problem Details when no running
  city has an event provider, instead of committing `200
  text/event-stream` and closing immediately. `Multiplexer.Len()`
  exposes the provider count so the precheck runs before headers
  commit.
- **Shared response types consolidated into `huma_types_*.go`.**
  `workflowSnapshotResponse` / `workflowBeadResponse` /
  `workflowDepResponse` / the `logicalNode` + `scopeGroup` aliases
  moved from `handler_convoy_dispatch.go` to
  `huma_types_convoys.go`; every `formula*Response` type moved from
  `handler_formulas.go` to `huma_types_formulas.go`. The only
  remaining `map[string]any` response-body field is
  `formulaDetailResponse.Steps`, which is intentionally opaque and
  documented in place (formula steps are a heterogeneous DSL).
- **`SessionSubmitResponse` documented** as an intentional
  domain-facing wrapper around the generated body — keeps `cmd/gc`
  callers off the genclient package and converts the wire-level
  intent string to the typed `session.SubmitIntent` in one place.

**Out of scope for this plan:**

- **Dashboard Go HTTP layer.** 37 raw HTTP sites remain across
  `cmd/gc/dashboard/api.go` (~1,886 lines), `api_fetcher.go`,
  `serve.go`, and `handler.go`. These all go through `scopedPath()`
  and already target city-scoped paths (`/v0/city/{cityScope}/...`),
  so they work fine against the post-Fix-3b supervisor. Migrating
  them onto the generated client is a separate, dashboard-only
  plan. This plan is about the control-plane surface and its primary
  consumers (CLI + server-side REST/SSE) — not the dashboard proxy.

- **(Closed) Fix 3f remnant — bead PATCH `json.RawMessage` input.**
  Resolved: `BeadUpdateRawInput` deleted; handler now uses the typed
  `BeadUpdateInput`. The "reject `priority: null`" UX nicety was
  dropped — the only in-repo caller never sends null, so the
  rejection was preserving behavior for hypothetical third-party
  clients at the cost of a `json.RawMessage` body. `grep
  json.RawMessage internal/api/huma_handlers_*.go
  internal/api/huma_types*.go` returns only doc comments.

**Current topology (post-Phase-3.5):**

- **Single Huma API.** `SupervisorMux.humaAPI` owns every typed
  operation — supervisor-scope (`/v0/cities`, `/health`, `/v0/readiness`,
  `/v0/provider-readiness`, `POST /v0/city`, `/v0/events`,
  `/v0/events/stream`) and per-city (`/v0/city/{cityName}/...`). One
  spec, one generated client, one middleware model.
- **Per-city `Server` is a handler-host.** No Huma API, no listener,
  no `ServeHTTP`. Its only mux registration is `/svc/*` for the
  workspace-service pass-through. The supervisor resolves per-city
  state via `bindCity` / `resolveCityServer` at request time.
- **Registration helpers.** `cityGet/Post/Patch/Delete/Put/Register`
  prepend the `/v0/city/{cityName}` prefix and wrap each handler
  with `bindCity`. `sseCityPrecheck` / `sseCityStream` do the same
  for SSE registrations.
- **Remaining handler-side validations.** Three checks resist
  static Huma tags because they depend on runtime state:
  provider-builtin membership (`huma_handlers_supervisor.go`),
  extmsg conditional required fields
  (`huma_handlers_extmsg.go:70`), and convoy rig-required gate
  (`huma_handlers_convoys.go:178`).
- **`cmd/genspec` / `cmd/gen-client`.** Fetch the spec directly
  from a single-`SupervisorMux`-backed stub. No merge step —
  `internal/specmerge` is gone.

See the `## Archive` section at the bottom for the phase-by-phase
history, fix catalog, and design research that drove the migration.


Phase 1 migrated 128 operations to Huma handlers with an auto-generated
OpenAPI 3.1 spec. Phase 2 made the spec the engine for the migrated
surface — typed SSE events, real validation, typed cache keys, committed
spec artifact. Phase 3 finishes the job: the remaining hand-written
networking must go.

### Core principle (unchanged)

**The OpenAPI spec drives ALL networking in the typed REST/SSE control plane.**
Annotated Go types are the single source of truth. Huma generates the spec from
those types and drives the entire network implementation. Clients generate from
the spec. Zero hand-written networking or JSON (de)serialization in the typed
control plane — only Go endpoint implementations and Go type definitions.
Everything else is framework.

**The routes we register ARE the routes we expose.** The spec describes the
full set of real, user-facing URL shapes that the service exports — directly,
without forwarding, renaming, or backwards-compat aliasing. If the spec says
`/v0/city/alpha/agents`, the server answers at exactly that path, with no
supervisor-side prefix-strip-and-forward to a hidden bare `/v0/agents`
endpoint. No shadow mapping. No client-side path rewrite helper (e.g.
`rewriteScopedRequestPath`) — the existence of such a helper is direct
evidence the spec disagrees with reality and is a bug to fix, not a pattern
to work around. For Gas City that means every per-city operation's real,
published path is `/v0/city/{cityName}/...`; no bare `/v0/...` alias exists.

**Explicit scope exclusion:** the `/svc/*` workspace-service proxy is a
raw pass-through to external service processes. It is not a typed API
surface and cannot be spec-driven without redefining what it is. The
core principle covers everything in `internal/api/` EXCEPT the `/svc/*`
proxy layer. If `/svc/*` ever becomes a typed API, it gets its own
migration plan.

### Phase 2 progress (done / partial / deferred)

> **Historical snapshot.** This block records what was done vs. open at
> the Phase 2 → Phase 3 boundary (2026-04-16). Every item that was
> "partial" or "deferred" landed in Phase 3; the grep counts below are
> all zero today. Kept for context on why each Phase 3 fix existed.

- **2a (SSE events in spec):** Done for the 3 Huma-registered streams.
  `registerSSE` helper; `TestSSEEndpointsHaveSchemasInSpec` enforces the
  invariant. The supervisor's global `/v0/events/stream` still uses
  `writeSSE` (4 sites) — moves to Phase 3.
- **2b (real validation):** Partial. 12 required fields across 7 input
  types use `minLength:"1"`; `huma.NewError` returns 400 for validation
  errors. Remaining `omitempty` on required body fields in other input
  types — audit in Phase 3.
- **2c (error format):** Partial. Typed sentinels in `configedit` and
  `mutationError` now use `errors.Is`. But 22 `apiError{}` sites still
  bypass Huma's error model and 36 `writeError` sites still emit
  non-Huma error shapes. Moves to Phase 3.
- **2d (typed cache keys):** Done. `cacheKeyFor` derives keys from input
  struct tags via reflection.
- **2e (split types file):** Partial. Session types extracted to
  `huma_types_sessions.go`; 16 other domains remain in `huma_types.go`.
- **2f (merge handler files):** Deferred. Revisit after Phase 3 stabilizes.
- **2g (spec as artifact):** Partial. `cmd/genspec` tool + committed
  `openapi.json` + `TestOpenAPISpecInSync` landed. Typed client
  generation — the largest unmet piece of the core principle — moves to
  Phase 3.
- **2h (session state machine):** Contract defined in
  `internal/session/state_machine.go` with transition table, reducer,
  and tests. Zero handler wiring — moves to Phase 3.

### The gap against the core principle

> **Historical snapshot.** Every grep-countable item below has since
> been closed by Phase 3 fixes 3a–3l. Re-running the same greps today
> returns zero production call sites for `writeError`, `writeJSON`,
> `writeListJSON`, `writeSSE`, `apiError{`, `decodeBody`,
> `configureHumaGlobals`, or raw-byte response caches; `client.go` is
> a thin adapter over the generated client; the supervisor runs on
> Huma. The inventory is kept for audit-trail purposes.

An audit of `internal/api/` showed we were ~70% spec-driven when
Phase 3 began. Specific hand-written networking outstanding at that
time (grep-verifiable, as of 2026-04-16):

Counts below are grep-verified as of 2026-04-16. Phase 3 must re-grep
cold at start and adjust fix scopes to match reality.

- **346-line CLI client** (`internal/api/client.go`) — 3
  `http.NewRequest` + 2 `json.Marshal` + 3 `json.NewDecoder` call
  sites, all hand-written.
- **Dashboard Go HTTP layer** — 4 files with hand-written `/v0/...`
  HTTP: `cmd/gc/dashboard/api.go` (~1,886 lines, ~50 JSON touchpoints
  + shape adapters), `api_fetcher.go` (`APIFetcher`), `serve.go`
  (`ValidateAPI`, `detectSupervisor`), `handler.go`
  (`fetchCityTabs`). Enumerated in Fix 3a.
- **36 `writeError(` sites** in production `internal/api/` code:
  `handler_city_create.go` (10), `supervisor.go` (7),
  `handler_provider_readiness.go` (6), `handler_services.go` (6),
  `middleware.go` (3), `idempotency.go` (2), `envelope.go` (1
  definition + 1 usage in `writeListJSON`). Plus 1 in
  `envelope_test.go`. (An earlier count included a comment reference
  in `client.go`; that is not a call site.)
- **10 `writeJSON(` sites** across `envelope.go`, `supervisor.go`,
  `handler_provider_readiness.go`, `handler_city_create.go`.
- **22 `apiError{}` construction sites** in Huma handlers:
  `huma_handlers_sessions.go` (17), `huma_handlers_beads.go` (3),
  `huma_handlers_mail.go` (2). These bypass Huma's error encoder by
  implementing `huma.StatusError` directly. (Doc comments and the type
  definition itself also mention `apiError`; Phase 3 greps must scope
  to `&apiError{` to avoid false positives.)
- **28 manual `json.Marshal(` calls** in Huma handlers, across 7 files:
  `huma_handlers_extmsg.go` (11), `huma_handlers_sessions.go` (9),
  `huma_handlers_providers.go` (2), `huma_handlers_services.go` (2),
  `huma_handlers_config.go` (2), `huma_handlers_convoys.go` (1),
  `huma_handlers_agents.go` (1). Responses use `json.RawMessage` or
  `map[string]any`, so the spec has no body contract.
- **`json.RawMessage` response bodies** in Huma outputs:
  `huma_handlers_extmsg.go` (list/transcript/adapter),
  `huma_handlers_providers.go` (list), `huma_handlers_services.go`
  (list/get), `huma_handlers_sessions.go` (transcript/agent-list/agent-get).
- **`map[string]any` response bodies** in Huma outputs:
  `huma_handlers_convoys.go` (convoy-get, convoy-check, workflow-get).
- **Custom `MarshalJSON` wire/spec mismatch** in
  `huma_handlers_config.go:189` — the handler flattens
  `annotatedAgentResponse` / `annotatedProviderResponse`, but the spec
  models them as nested objects. The generated client is already wrong
  on this endpoint.
- **4 `writeSSE` calls** in `convoy_event_stream.go` — supervisor global
  events stream without typed event schema. Uses composite STRING
  cursor IDs via `writeSSEWithStringID`, incompatible with Huma's
  `sse.Message.ID int` (see Fix 3g for the required design choice).
- **Supervisor API** (`/v0/cities`, `/health`, `/v0/city/{name}/...`
  routing) entirely outside Huma — none of it appears in the spec.
  Current design puts `/health` and `/v0/events/stream` on BOTH
  supervisor and per-city mux at the same path — topology is
  unresolved (see Fix 3b).
- **Middleware** (`withReadOnly`, `withCSRFCheck`, `withRecovery`) emits
  errors via `writeError`. `withRecovery` must stay outermost at the
  mux layer to cover non-Huma routes; only error-emitting middleware
  migrates into Huma (see Fix 3d).
- **`decodeBody` still called** in `handler_beads.go` and
  `handler_city_create.go`.
- **Raw-byte caches** — `response_cache.go` and `idempotency.go` store
  cached responses as `[]byte`; handlers call `json.Unmarshal` on
  cache-hit paths in `huma_handlers_agents.go:31`,
  `huma_handlers_mail.go:238`, `huma_handlers_beads.go:245`. This
  violates "zero hand-written JSON (de)serialization" even after 3c–3f
  land (see Fix 3l).
- **`omitempty` on required body fields** — only 12 required fields
  across 7 input types had `minLength:"1"` added in Phase 2. The
  remaining body-input types still mark required fields as optional in
  the spec (see Fix 3k).
- **`configureHumaGlobals` rewrites 422→400** for validation errors to
  keep the hand-written `client.go` parser working. Once `client.go` is
  replaced, the override must go (see Fix 3a / 3k).
- **No generated typed client** — 128 operations, zero generated
  clients. Dashboard hand-writes fetch; CLI hand-parses responses.
- **Session state machine not wired** — no handler dispatches through
  `Transition()`. `ErrIllegalTransition` does not exist yet.

Phase 3 closes every one of these. The following are ALSO out of
scope (and should not be flagged by any Phase 3 grep):

- `/svc/*` workspace-service proxy (per the principle's explicit
  exclusion).
- `internal/extmsg/http_adapter.go` — outbound HTTP to external
  ExtMsg callback URLs. Not a typed API endpoint; consumes someone
  else's contract.
- `internal/workspacesvc/proxy_process.go` — outbound HTTP to
  spawn/manage workspace service subprocesses. Same rationale.

---

## Testing strategy: three-layer spec-driven coverage

The drift check in `TestOpenAPISpecInSync` proves the committed spec
matches what the running supervisor serves. That's necessary but not
sufficient: it says nothing about whether response bodies actually
match the schemas the spec promises, whether the generated client
round-trips correctly against a real supervisor, or whether the `gc`
binary wires end-to-end against a real socket. Three further layers
close those gaps.

### Layer 1 — schema-driven response validation

**File:** `internal/api/openapi_response_validation_test.go`

Load the committed `internal/api/openapi.json` once. For a curated
list of simple GET operations, call the real handler via
`httptest.NewServer(sm.Handler())`, then validate the response body
against the operation's `200` response schema using `pb33f/libopenapi`
+ `libopenapi-validator` (pure-Go, no CGO).

**Scope (first pass):** every simple GET — `/v0/cities`,
`/v0/readiness`, `/v0/provider-readiness`, `/v0/city/{cityName}`,
`/v0/city/{cityName}/status`, `/v0/city/{cityName}/agents`,
`/v0/city/{cityName}/beads`, `/v0/city/{cityName}/mail`,
`/v0/city/{cityName}/convoys`, `/v0/city/{cityName}/sessions`,
`/v0/city/{cityName}/services`, `/v0/city/{cityName}/formulas`,
`/v0/city/{cityName}/orders`, `/v0/city/{cityName}/config`,
`/v0/city/{cityName}/packs`.

**What this catches:** handler returns a field the spec doesn't
declare, or omits a required field. Huma doesn't validate responses
at runtime; this test does.

### Layer 2 — generated-client round-trip

**File:** `internal/api/genclient_roundtrip_test.go` (+
`internal/api/genclient_roundtrip_helpers_test.go` for `newRoundTripTest(t)`).

Spin up `httptest.NewServer(sm.Handler())` backed by a single-city
`SupervisorMux`. Construct a real `genclient.NewClientWithResponses(ts.URL, ...)`.
Call every generated method we care about, assert the decoded
response has the expected shape.

**Scope (first pass):** one test per domain —
- `TestRoundTripCitiesList` — `GetV0CitiesWithResponse`
- `TestRoundTripReadiness` — `GetV0CityByCityNameReadinessWithResponse`
- `TestRoundTripAgentList` — `GetV0CityByCityNameAgentsWithResponse`
- `TestRoundTripBeadCreate` — `PostV0CityByCityNameBeadsWithResponse`
  with a real `BeadCreateInputBody`; assert returned bead ID.
- `TestRoundTripSessionList` — `GetV0CityByCityNameSessionsWithResponse`
- `TestRoundTripMailSend` — `PostV0CityByCityNameMailWithResponse`
- `TestRoundTripConvoyCreate` — `PostV0CityByCityNameConvoysWithResponse`
- `TestRoundTripFormulaList` — `GetV0CityByCityNameFormulasWithResponse`

**What this catches:** client method name mismatch with spec, request
body encoding divergence, response status-code drift between handler
and spec-declared default.

### Layer 3 — binary integration

**Directory:** `test/integration/` with `//go:build integration` build
tag (existing convention in the repo).

**Files:**
- `test/integration/huma_binary_test.go` — test body
- `test/integration/harness.go` — helpers: `startSupervisor(t) func()`,
  `gcCmd(t, args...) *exec.Cmd`, `waitHTTP(t, url)`
- `test/integration/README.md` — run instructions

**What the test does:**

1. Build `gc` into a tempdir via `go build -o tmpdir/gc ./cmd/gc`.
2. Run `tmpdir/gc init tmpdir/city --provider claude` to make a
   throwaway city config.
3. Start `tmpdir/gc supervisor --port 0 --city tmpdir/city` in a
   goroutine; capture bound port.
4. Run CLI subcommands as subprocesses against the running supervisor
   — e.g. `tmpdir/gc --base-url http://127.0.0.1:$PORT cities list`,
   `... agents list`, `... bead create --rig myrig --title 'test'`.
   Assert exit code + stdout shape.
5. Teardown: kill the supervisor process, remove the tempdir.

**Scope (first pass):** five CLI commands that exercise different
surfaces — `cities list`, `city status`, `agents list`, `bead create`,
`mail send`. Enough to prove the whole stack wires end-to-end through
a real binary and a real socket.

**CI hook:** integration tests are build-tagged so they don't run by
default. Add a `make test-integration-huma` target for manual /
CI-opt-in runs.

### Schema publishing

Regenerating the spec is wired into `.githooks/pre-commit`, which the
repo's `make setup` target already installs. On every commit that
touches a Go file, the hook runs `go run ./cmd/genspec` and stages
`internal/api/openapi.json` and `docs/schema/openapi.json` together
— the former feeds `TestOpenAPISpecInSync`, the latter is published
by Mintlify under the "API" navigation group (added to
`docs/docs.json`). `cmd/genspec` writes both copies in one run; pass
`-out <path>` or `-stdout` to override.

---

## Archive: phase history

Everything below is historical — phase-by-phase progress, gap analyses,
design research, and the Phase 3 fix catalog. It's retained for
context on why particular decisions were made; current state lives in
the top section.

### Phase 1 summary (complete)

- 95 paths, 128 typed operations registered with Huma in the
  auto-generated spec. (The older "~169 endpoints" figure in the
  context section below counted raw mux routes including `/svc/*`
  proxy subpaths — that count is stale; 128 is the authoritative
  number of typed operations under the core principle.)
- All CRUD and per-city SSE endpoints registered through Huma
- 5,600 lines of dead old handler code removed
- 1 old mux.HandleFunc remaining: `/svc/` proxy (explicitly out of
  scope per the principle)

### Phase 2: Spec-Driven API (historical — see Phase 3 for current state)

The following Phase 2 sub-descriptions are preserved for historical
context. The authoritative view of what remains is the "gap against the
core principle" list above and the Phase 3 section below. Where this
block still says `StreamResponse`, `writeSSE`/`writeError` is
acceptable, or suggests deferring client generation, the Phase 3 fixes
supersede it.

Phase 1 left gaps that undermine the core principle:

**2a. SSE event schemas missing from spec.** All 3 SSE endpoints use
`StreamResponse` which produces empty-body responses in the spec. Event
types (`eventStreamEnvelope`, session transcript events, agent output turns)
are invisible to clients reading the spec. **Fix:** Refactor to
`sse.Register()` with typed event maps so event schemas appear in the spec.
Remove `writeSSE()`/`writeSSEComment()` helpers — use Sender callback.

**2b. Validation bypassed with `omitempty`.** All 35 body input types use
`json:"field,omitempty"` to prevent Huma's 422. The spec marks all fields
as optional even when required. Handlers validate manually. **Fix:** Remove
`omitempty` from required fields, add proper validation tags (`minLength`,
`required`). Override `huma.NewError` for consistent error format. Accept
422 as the correct status for validation errors.

**2c. Three error formats.** RFC 9457, legacy `{code,message}`, and
`apiError`. `mutationError()` uses `strings.Contains` to guess HTTP status.
**Fix:** Define typed domain errors in each package. Single `domainError()`
encoder. Eliminate string matching. Consistent error format everywhere
including middleware.

**2d. Response cache uses hand-built string keys.** Add a query param,
forget to update the key, serve stale data. **Fix:** Generic cached handler
decorator that derives cache keys from input struct fields.

**2e. huma_types.go is a 1300-line monolith.** **Fix:** Split by domain
(agents, beads, sessions, etc.). Keep only shared generics in the base file.

**2f. Dual handler file pattern.** `handler_agents.go` (helpers) and
`huma_handlers_agents.go` (handlers) are confusing. **Fix:** Merge into
single domain files.

**2g. No typed client generation.** 128 operations in the spec, but no
generated clients. CLI client hand-parses responses; dashboard proxy
calls `/v0/...` with hand-written HTTP. **Fix:** Generate typed Go
client from `/openapi.json`; both the CLI and the dashboard's Go
proxy consume it. The spec becomes the single source of truth for
server and client. (Phase 3 Fix 3.0 + 3a.)

**2h. Session state management is ad-hoc.** `huma_handlers_sessions.go` is
1200 lines with 16 handlers mixing state management, provider quirks,
naming, transcript logic, and legacy compat. State transitions are string
comparisons scattered across handlers. **Fix:** Extract an explicit session
state machine with typed states, a transition table, a single reducer for
legality, and a traceable event timeline.

## Context

Gas City's API layer (everything under `internal/api/` except the
`/svc/*` proxy) today has 128 typed Huma operations and 4 SSE
streaming endpoints. Original pre-migration state had ~169 raw
`net/http` mux routes (including `/svc/*` proxy subpaths and
endpoints that have since been consolidated). The migration goal:
annotated Go types become the single source of truth for wire
format, validation, and OpenAPI spec — no manual JSON, no separate
spec file, no drift.

## Decision Record

**Chose HTTP + SSE + OpenAPI over WebSockets + AsyncAPI** because:

- The API surface is CRUD-shaped; HTTP is the natural fit
- SSE handles the unidirectional streaming use cases
- OpenAPI tooling is vastly more mature than AsyncAPI for Go
- Performance difference is unmeasurable for a localhost dev-tool API

**Chose Huma over Fuego** because:

- OpenAPI 3.1 (Fuego is 3.0 only) — aligns with existing JSON schema generation
- Built-in SSE with typed event mapping (Fuego requires manual http.Flusher)
- Handler signature uses stdlib `context.Context` (Fuego uses custom context)
- 3x community size, more battle-tested

## Architecture

### Before (current)

```
HTTP Request
    |
    v
http.ServeMux route matching
    |
    v
middleware chain (requestID, CORS, recovery, logging, CSRF)
    |
    v
handler_*.go  (manual json.Decode → business logic → manual json.Encode)
    |
    v
envelope.go writeJSON / writeListJSON / writeSSE
```

### After Phase 3

```
HTTP Request
    |
    v
http.ServeMux route matching
    |
    v
Outer mux middleware: request-id → CORS → recovery
    |
    v
Huma adapter (humago) — single supervisor-owned API (topology 1)
    |
    v
Huma middleware: CSRF → read-only
    |
    v
Huma operation dispatch:
  - Deserialize request into typed Input struct
  - Validate against struct tag constraints
  - Call handler: func(ctx, *Input) (*Output, error)
  - Serialize Output to JSON response
  - Format errors as RFC 9457 Problem Details
    |
    v
/openapi.json served live from registered types (always in sync)
```

`/svc/*` proxy still bypasses Huma and is covered by outer recovery
only (explicit scope exclusion).

### What changes

| Layer              | Before                                                  | After                                                                        |
| ------------------ | ------------------------------------------------------- | ---------------------------------------------------------------------------- |
| Route registration | `s.mux.HandleFunc("GET /v0/agents", s.handleAgentList)` | `huma.Get(api, "/v0/agents", s.handleAgentList)`                             |
| Handler signature  | `func(w http.ResponseWriter, r *http.Request)`          | `func(ctx context.Context, input *AgentListInput) (*AgentListOutput, error)` |
| Request parsing    | `decodeBody(r, &req)` + manual query/path parsing       | Automatic from Input struct tags                                             |
| Response writing   | `writeJSON(w, 200, resp)`                               | `return &Output{Body: resp}, nil`                                            |
| Error responses    | `writeJSON(w, 4xx, Error{...})`                         | `return nil, huma.Error404NotFound("msg")` (Problem Details)                 |
| Error-emitting middleware | `writeError` in `withReadOnly`/`withCSRFCheck`   | Huma middleware via `api.UseMiddleware` + `huma.WriteErr` (Fix 3d)          |
| SSE streaming      | Manual `writeSSE()` + goroutine + ticker                | `registerSSE` with typed event maps; string-ID variant for global stream    |
| API spec           | None                                                    | Auto-generated at `/openapi.json` from registered types                      |
| Validation         | Manual checks in each handler                           | Struct tags (`minLength`, `pattern`, `enum`); no `omitempty` on required    |
| Client             | 346-line hand-written `client.go` + hand-written dashboard proxy | Generated Go client consumed by CLI and dashboard proxy (Fix 3a)      |

### What stays the same

- `http.ServeMux` as the router (Huma wraps it via `humago` adapter)
- Outer mux middleware: request-id, CORS, `withRecovery` (recovery
  stays outermost to cover `/svc/*` and any raw routes)
- Internal packages (beads, events, config, sling, convoy, etc.)
- Domain types and business logic
- Dashboard static files and HTML rendering
- Service proxy `/svc/*` — explicitly out of scope of the core
  principle; it is a pass-through to external service processes, not
  a typed API surface

## Type Design

### Principle: Go types ARE the API contract

Every endpoint has an Input struct and an Output struct. These structs:

1. Define the wire format (via `json:` tags)
2. Define validation rules (via huma struct tags)
3. Define documentation (via `doc:` and `example:` tags)
4. Generate the OpenAPI spec (via huma reflection at startup)

No separate spec file. No code generation step. The spec endpoint
serves what the code actually does.

### Reducing type proliferation with generics

Huma's reflection-based OpenAPI generation works with Go generics. Generic
types get schema names like `ListOutputAgentResponse`. This lets us define
the list envelope once:

```go
// Generic list envelope — one type covers ALL list endpoints
type ListOutput[T any] struct {
    Index int `header:"X-GC-Index" doc:"Latest event sequence number"`
    Body  struct {
        Items      []T    `json:"items"`
        Total      int    `json:"total"`
        NextCursor string `json:"next_cursor,omitempty"`
    }
}

// Usage:
// GET /v0/agents returns *ListOutput[AgentResponse]
// GET /v0/beads  returns *ListOutput[BeadResponse]
// GET /v0/rigs   returns *ListOutput[RigResponse]
```

For inputs, embed common parameter patterns:

```go
type WaitParam struct {
    Wait string `query:"wait" doc:"Block until state changes (Go duration string)"`
}

type PaginationParam struct {
    Cursor string `query:"cursor" doc:"Pagination cursor from previous response"`
    Limit  int    `query:"limit" doc:"Max results per page" minimum:"1" maximum:"1000"`
}

type AgentListInput struct {
    WaitParam
    PaginationParam
    Pool string `query:"pool" doc:"Filter by pool name"`
}
```

This eliminates ~50% of output type definitions and standardizes input patterns.

### Example: Agent endpoints

```go
// --- Input types ---

type AgentGetInput struct {
    Name string `path:"name" doc:"Agent name" example:"deacon-1"`
}

type AgentCreateInput struct {
    Body struct {
        Name     string `json:"name" minLength:"1" doc:"Agent name"`
        Provider string `json:"provider,omitempty" doc:"Provider name"`
        Dir      string `json:"dir,omitempty" doc:"Working directory"`
    }
}

type AgentUpdateInput struct {
    Name string `path:"name" doc:"Agent name"`
    Body struct {
        Provider  string `json:"provider,omitempty"`
        Suspended *bool  `json:"suspended,omitempty"`
    }
}

// --- Output types ---

type AgentResponse struct {
    Name        string       `json:"name" doc:"Agent name"`
    Description string       `json:"description,omitempty" doc:"Agent description"`
    Running     bool         `json:"running" doc:"Whether agent is actively running"`
    Suspended   bool         `json:"suspended" doc:"Whether agent is suspended"`
    Rig         string       `json:"rig,omitempty" doc:"Associated rig"`
    Pool        string       `json:"pool,omitempty" doc:"Pool membership"`
    Provider    string       `json:"provider,omitempty" doc:"Provider name"`
    State       string       `json:"state,omitempty" doc:"Current state"`
    Session     *SessionInfo `json:"session,omitempty" doc:"Active session info"`
}

// GET /v0/agents handler:
func (s *Server) handleAgentList(ctx context.Context, input *AgentListInput) (*ListOutput[AgentResponse], error) {
    // ... business logic ...
    return &ListOutput[AgentResponse]{
        Index: idx,
        Body: struct {
            Items      []AgentResponse `json:"items"`
            Total      int             `json:"total"`
            NextCursor string          `json:"next_cursor,omitempty"`
        }{Items: agents, Total: len(agents)},
    }, nil
}
```

## Error Format Migration

### Current error format (`envelope.go`)

```go
type Error struct {
    Code    string       `json:"code"`
    Message string       `json:"message"`
    Details []FieldError `json:"details,omitempty"`
}

// Usage:
writeError(w, 404, "not_found", "agent not found")
// → {"code":"not_found","message":"agent not found"}
```

### Huma error format (RFC 9457)

```go
huma.Error404NotFound("agent not found")
// → {"status":404,"title":"Not Found","detail":"agent not found"}
```

### Migration decision: single RFC 9457 format (Phase 3)

Initial Phase 2 work left a hybrid: Huma handlers emit RFC 9457, but
middleware, idempotency, and 22 `apiError{}` sites still emit the legacy
`{code, message}` shape. That hybrid violates the core principle — two
error formats means clients still need hand-written parsing.

Phase 3 target: every error emitted by any code path under
`internal/api/` is Problem Details produced by Huma's encoder.

- **Huma handlers** → `huma.Error*()` (existing). `apiError` deleted.
- **Middleware** → Huma middleware registered via `api.UseMiddleware`,
  emitting Problem Details via Huma's error path.
- **Idempotency replay** → typed Huma response or Problem Details via
  the Huma error path (no raw `w.Write`).
- **Supervisor** → moves onto Huma (see Supervisor section below); all
  errors become Problem Details.
- **Generated client** (replacing hand-written `client.go`) expects
  Problem Details only — the dual-format parser goes away.

### Custom error helper for store errors

```go
func storeError(err error) error {
    if errors.Is(err, beads.ErrNotFound) {
        return huma.Error404NotFound(err.Error())
    }
    return huma.Error500InternalServerError(err.Error())
}
```

## Idempotency Caching

### Current pattern (`idempotency.go`)

Create endpoints accept an `Idempotency-Key` header. A two-phase protocol
prevents duplicates:

1. `reserve(key, bodyHash)` — atomically reserve the key
2. Handler executes the create
3. `complete(key, status, body, hash)` — cache the response for replay

Subsequent requests with the same key replay the cached response.
Different body → 422. In-flight → 409.

### Approach considered: Huma middleware (rejected)

A Huma middleware implementation was considered — read the
`Idempotency-Key` header, hash the body, look up or reserve in the
cache, then call `next(ctx)` and capture the response for replay.
Rejected for three reasons:

1. Huma's `huma.Context` exposes `BodyReader()` but no supported
   re-buffer mechanism — once the middleware reads the body for
   hashing, Huma's decoder sees an empty stream. Working around that
   requires a response wrapper that intercepts serialization, which
   is substantially more code than the handler-level approach.
2. Idempotency applies to only a handful of create endpoints;
   middleware would intercept every request for no benefit.
3. Idempotency is a handler responsibility (semantic: "this create
   operation is retry-safe with this key"), not a transport concern.

### Decision: handler-level idempotency with typed inputs

Keep idempotency as handler-level logic. The handler calls
`cache.handleIdempotent()` before doing work, same as today but with
the `Idempotency-Key` read from the Huma input struct. Fix 3l
converts the cache's storage from `[]byte` to typed values; the
request-body hash (used to detect "same key, different body → 422")
is computed from the incoming request body before handler dispatch
and stays `[]byte`.

```go
type BeadCreateInput struct {
    IdempotencyKey string `header:"Idempotency-Key" doc:"Retry key for safe creates"`
    Body struct {
        Title  string `json:"title" minLength:"1"`
        Type   string `json:"issue_type"`
        // ...
    }
}
```

## Response Caching

### Current pattern (`response_cache.go`)

Short-lived (2-second TTL) cache for expensive responses (agent lists,
order feeds, formula feeds). Keyed by handler name + query string, tied
to the event sequence index. If the index matches and TTL hasn't expired,
raw cached JSON bytes are written directly.

### Phase 3 target: typed-struct cache (Fix 3l)

Phase 2 kept the raw-byte cache and had Huma handlers call
`json.Unmarshal` on cache-hit paths. That violates "zero hand-written
JSON (de)serialization" — the unmarshal IS hand-written JSON handling.
Phase 3 Fix 3l converts `response_cache.go` (and `idempotency.go`) to
typed-struct storage. Cache-hit handlers then return the typed value
directly; Huma serializes on every hit. At 2-second TTL on localhost,
the re-serialization cost is negligible. This is the only way to reach
the core principle.

## SSE Streaming Design (researched)

### What Huma's SSE supports

| Capability                  | Supported | Notes                                                                        |
| --------------------------- | --------- | ---------------------------------------------------------------------------- |
| Multiple event types        | Yes       | Via `eventTypeMap` — maps Go struct types to SSE event names                 |
| `Last-Event-ID` reading     | Manual    | Must declare `LastEventID string \`header:"Last-Event-ID"\`` in input struct |
| Event ID on outgoing events | Yes       | Via `sse.Message{ID: seqNum, Data: payload}`                                 |
| Keepalive comments          | No        | Must implement manually with a ticker in the stream function                 |
| Context cancellation        | Yes       | Client disconnect cancels the handler's context                              |
| Blocking stream function    | Yes       | Can block indefinitely on channels/watchers                                  |
| OpenAPI documentation       | Yes       | Event types appear in the spec                                               |

### Approach: `registerSSE` with typed event maps (every stream)

SSE endpoints that have been migrated use `registerSSE` — a thin
wrapper over `huma.Register` + `huma.StreamResponse` that publishes
typed event schemas into the spec and adds a precheck callback
(Huma's `sse.Register` can't return HTTP errors after headers
commit, so the wrapper adds that capability). Functionally
equivalent to `sse.Register` from a caller's perspective.

The earlier `huma.StreamResponse` approach was abandoned because it
left SSE event shapes out of the spec entirely. Three of four streams
were migrated in Phase 2 (events, session, agent output). The fourth —
the supervisor's global `/v0/events/stream` served by
`convoy_event_stream.go` — still uses raw `writeSSE` and moves to Phase
3 (Fix 3g below). Once that migrates, `writeSSE` / `writeSSEComment` /
`writeSSEWithStringID` are deleted.

### `registerSSE` contract (as-implemented)

`registerSSE` is a thin wrapper over `huma.Register` +
`huma.StreamResponse`. The real signature in
`internal/api/sse.go:37` is:

```go
type StreamFunc[I any] func(hctx huma.Context, input *I, send sse.Sender)

func registerSSE[I any](
    api          huma.API,
    op           huma.Operation,
    eventTypeMap map[string]any,
    precheck     func(context.Context, *I) error,
    stream       StreamFunc[I],
)
```

Semantics:

- `precheck(ctx, *I) error` runs BEFORE any response headers are
  written. Returning a `huma.StatusError` produces a proper HTTP
  status + Problem Details response. Use precheck for pure
  validation and existence checks that must fail with an HTTP error.
- `stream(hctx, *I, sse.Sender)` runs AFTER headers commit. Once
  called, it cannot return an HTTP error — only stream or stop.
- **The wrapper does NOT pass resources from precheck to stream.**
  Any resources the stream needs (event watchers, DB handles, file
  descriptors) must be either (a) acquired inside the stream
  callback — accepting that failures there degrade to
  stream-termination rather than HTTP errors — or (b) captured via
  closure over the Server struct. This is the shape the existing
  three Huma-registered streams use today.
- `sse.Message.ID` is `int`. The `send` callback emits `id: <int>`
  onto the wire. Streams that need STRING IDs (the supervisor global
  stream's composite cursor) require the string-ID variant planned
  in Fix 3g.

**Fix 3g will add a string-ID variant.** Two implementation options
remain open inside the "extend with string-ID variant" decision:

- Option A: a sibling `registerSSEStringID` with a new
  `SenderWithStringID` type. Smaller blast radius; global stream
  uses the sibling.
- Option B: make the existing `registerSSE` generic over an ID type
  (`int` or `string`). Larger refactor; affects all four streams.

Option A is the recommended starting point; Option B only if
callsite duplication becomes painful.

**Resource handoff for Fix 3g specifically.** Fix 3g refactors
`streamProjectedGlobalEvents` so that the `events.MuxWatcher` is
acquired inside the stream callback (closure over `s.state`), with
`defer mw.Close()` immediately after acquisition. Precheck validates
only — it does NOT allocate the watcher. Watcher-acquisition
failures inside the callback terminate the stream cleanly rather
than producing an HTTP error; this is acceptable because the
surface that can fail (event provider enabled / event bus
reachable) can be checked in precheck first.

**SSE endpoints (4 total, 3 on `registerSSE` today):**

- `GET /v0/events/stream` (per-city) — on `registerSSE`
- `GET /v0/session/{id}/stream` — on `registerSSE`
- `GET /v0/agent/{name}/output/stream` — on `registerSSE`
- Supervisor `GET /v0/events/stream` (global, served by
  `streamProjectedGlobalEvents` in `convoy_event_stream.go`) — still on
  raw `writeSSE`. Fix 3g migrates this one.

**Note:** `/v0/orders/feed` and `/v0/formulas/feed` are plain JSON endpoints
with response caching, not SSE streams. They were migrated as standard
Huma handlers.

## Supervisor / Multi-City Architecture (researched)

### Historical: per-city Huma API instances (superseded)

Phase 1/2 ran each city as its own `huma.API` with its own schema
registry and OpenAPI spec. That topology is superseded by Phase 3's
decision (topology 1, below): the supervisor owns a single Huma API
and per-city operations are registered as `/v0/city/{name}/...` paths
on it.

### Supervisor moves onto Huma (Phase 3, Fix 3b)

Earlier the supervisor was left on raw `net/http` on the theory that
"it's a routing layer, not an API surface." That framing conflicts with
the core principle. The supervisor is an API surface (`/v0/cities`,
`/health`, routing metadata, global events stream). Leaving it outside
Huma means:

- Its endpoints do not appear in the OpenAPI spec.
- Errors use the legacy `{code, message}` shape (7 `writeError` sites).
- Responses are hand-marshalled (3 `writeJSON` sites).
- Its SSE stream has no typed event schema (4 `writeSSE` sites in
  `convoy_event_stream.go`).

**Topology decision (must land before Fix 3b code).** Today the
supervisor mux forwards `/v0/city/{name}/...` to per-city handlers
while also serving its own `/v0/cities` and `/health`. The per-city
Huma API already serves its own `/health` and `/v0/events/stream` at
the same bare paths. Two Huma API instances coexisting on one process
is supported by Huma v2.37.3, but they must not claim the same
`(method, path)` on the same mux.

Two topologies were considered:

1. **Merged supervisor spec, city-scoped paths.** The supervisor owns
   a single Huma API. Per-city endpoints are registered as
   `/v0/city/{name}/...` on that API and dispatch internally to the
   matching city's state. One spec. One generated client. The
   supervisor always has an active city context in the path.
2. **Two specs, two clients, thin adapter.** Supervisor has its own
   Huma API serving `/v0/cities`, `/health`, global events. Per-city
   Huma APIs serve bare `/v0/...` under `/v0/city/{name}/` via the
   existing dispatcher. Two generated clients (`supervisor` and
   `city`) plus a thin adapter that knows which to call.

**Decision: topology 1.** One spec, one generated client, city name
in the path. This removes the "which client do I use?" judgment from
the CLI caller and gives Fix 3a a single stable URL shape to target.
Any prior wording elsewhere in the plan that implies per-city Huma
APIs with independent specs is superseded by this decision.

Phase 3 Fix 3b registers every supervisor route and every
city-scoped route as operations on that single supervisor-owned Huma
API.

### Dynamic city instances

Cities start/stop at runtime. Under topology 1, there is a single
Huma API owned by the supervisor; adding or removing a city does
NOT create new `huma.API` instances. The operations at
`/v0/city/{name}/...` dispatch to the named city's state at request
time. Cities starting/stopping only affects the in-memory city
registry, not the spec or the Huma API.

### Read-only mode (Phase 3 migration target)

`withReadOnly()` currently runs at the mux level and emits errors via
`writeError`. Phase 3 Fix 3d re-registers it as Huma middleware so
rejection errors come back as Problem Details. The rejection behavior
is identical — only the error shape changes.

## Blocking reads (`?wait=...` pattern) (researched)

Huma handlers can block indefinitely. No built-in request timeout
conflicts with long-polling. The handler just blocks on a channel:

```go
type AgentListInput struct {
    WaitParam  // embeds Wait string `query:"wait"`
}

func (s *Server) handleAgentList(ctx context.Context, input *AgentListInput) (*ListOutput[AgentResponse], error) {
    if input.Wait != "" {
        dur, _ := time.ParseDuration(input.Wait)
        waitCtx, cancel := context.WithTimeout(ctx, dur)
        defer cancel()
        s.waitForChange(waitCtx)  // blocks until event or timeout
    }

    agents := s.buildAgentList()
    return &ListOutput[AgentResponse]{...}, nil
}
```

Context cancellation propagates correctly — if the client disconnects
during a wait, the handler's context is cancelled.

## Migration Automation (researched)

### Strategy: hybrid AST scanner + template generator

Full AST-driven code transformation is not worth the effort (diminishing
returns on the last 15% of handlers). Instead:

**Step 1: AST scanner (4-6 hours to build)**

Scans all 31 handler files and produces `endpoints.json`:

```json
[
  {
    "func_name": "handleAgentList",
    "route": "GET /v0/agents",
    "method": "GET",
    "has_body_decode": false,
    "query_params": ["pool", "suspended", "wait"],
    "path_params": [],
    "response_type": "agentResponse",
    "response_writer": "writeListJSON",
    "has_sse": false,
    "has_custom_headers": true,
    "line_range": [45, 92]
  },
  ...
]
```

**Step 2: Stub generator (2-3 hours)**

Reads `endpoints.json`, emits for each endpoint:

- Input struct with query/path/header/body fields
- Output struct (or uses `ListOutput[T]` for list endpoints)
- Huma registration call
- Handler signature with TODO placeholder for business logic

**Step 3: Manual migration (bulk of the work)**

Developer copies business logic from old handler into new handler stub.
The scanner flags ~15-20 endpoints that need special attention (SSE,
custom headers, conditional responses). The other ~150 are mechanical.

**Why not full automation:** The business logic between "parse input" and
"write output" has too many variations (error branches, conditional
responses, multi-step queries) for reliable AST extraction. The scanner
identifies what needs to change; humans move the logic.

## Historical migration strategy (Phases 0–5, complete)

The original migration ran in phases 0–5. These are preserved below for
context. Where older Phase 3/4/5 language endorses patterns that the
current Phase 3 ("Zero Hand-Written Networking") eliminates —
`huma.StreamResponse` for SSE, keeping `writeSSE`/`writeError`/
`writeListJSON` in `envelope.go`/`sse.go`, deferring typed client
generation — the current Phase 3 section is authoritative.

### Phase 0: Setup (complete)

- Added `github.com/danielgtaylor/huma/v2` dependency
- Created `humago.New()` adapter wrapping existing mux in `server.go`
- Served `/openapi.json` and `/docs` endpoints

### Phase 1: Establish patterns (complete)

- Defined shared generic types: `ListOutput[T]`, `IndexOutput[T]`
- Defined shared input mixins: `WaitParam`, `PaginationParam`,
  `BlockingParam`
- Migrated 128 operations across all domains to Huma handlers
- Removed ~5,600 lines of dead old handler code

### Phase 2 (historical): original SSE + cleanup intent

The original Phase 2 plan intended to migrate SSE endpoints as
`huma.StreamResponse` wrappers and to keep `envelope.go` /
`sse.go`'s legacy helpers. Actual Phase 2 delivered the typed-event
`registerSSE` pattern for the three per-city streams instead. The
remaining legacy helpers (`writeSSE*`, `writeError`, `writeJSON`,
`writeListJSON`, `apiError`) are the surface Phase 3 now eliminates.

### Phase 4–5 (historical): Cleanup + Polish (complete)

- Removed unused envelope helpers (`writePagedJSON`, `writeIndexJSON`, etc.)
- Added `doc:` and `example:` tags throughout
- Served Swagger UI at `/docs`
- Committed `openapi.json` as a versioned artifact; added
  `TestOpenAPISpecInSync`

The residual `writeJSON` / `writeError` / `writeListJSON` in
`envelope.go` and `writeSSE*` in `sse.go` were not deleted then
because callers still existed. Phase 3 removes those callers and then
deletes the helpers.

## Files to modify (Phase 3 authoritative list)

The per-fix "Files:" entries under each Phase 3 fix are the
authoritative list. Summary:

- `internal/api/server.go` — Huma middleware wiring (Fix 3d), 422→400
  override removed (Fix 3k), supervisor Huma API (Fix 3b)
- `internal/api/middleware.go` — re-registered as Huma middleware (Fix 3d)
- `internal/api/supervisor.go` + `supervisor_*.go` — Huma operations (Fix 3b)
- `internal/api/huma_handlers_*.go` — typed outputs, no `apiError`,
  no raw `json.Marshal` (Fixes 3c, 3f)
- `internal/api/huma_types*.go` — typed output structs for
  currently-opaque bodies (Fix 3f); `apiError` type deleted (Fix 3c);
  `omitempty` removed from required fields (Fix 3k)
- `internal/api/client.go` — replaced by generated Go client (Fix 3a)
- `internal/api/genclient/` (new) — generated client output (Fix 3a)
- `internal/api/response_cache.go`, `internal/api/idempotency.go` —
  typed-struct storage (Fix 3l)
- `internal/api/convoy_event_stream.go` — `registerSSE` string-ID
  variant (Fix 3g)
- `internal/api/sse.go` — string-ID sibling added; legacy `writeSSE*`
  helpers deleted (Fix 3g)
- `internal/api/envelope.go` + `envelope_test.go` — deleted (Fix 3h)
- `internal/session/manager.go`, `state_machine.go` — wire `Transition()`
  (Fix 3j)
- `cmd/gc/dashboard/api.go`, `api_fetcher.go`, `serve.go`, `handler.go` —
  replaced by generated Go client (Fix 3a)
- `.github/workflows/*`, `Makefile` — regeneration + drift CI (Fix 3a)

**Unchanged:**

- `internal/api/state.go` — interface unchanged
- Outer mux middleware (request-id, CORS, `withRecovery`) — stays at
  mux level so `/svc/*` keeps panic coverage (Fix 3d)
- `/svc/*` proxy handler — explicit scope exclusion from core principle
- All internal packages outside `internal/api/` and
  `internal/session/` (beads, events, config, sling, convoy, etc.)
- Dashboard static files and HTML rendering

## Verification

At each phase:

- `go test ./...` passes
- `go vet ./...` clean
- OpenAPI spec at `/openapi.json` validates
- Dashboard still works (start dev server, test golden paths)
- SSE streaming works (subscribe to events, trigger activity, see updates)
- `curl` smoke tests against key endpoints
- Error response shapes are Problem Details (RFC 9457) everywhere
  Phase 3 has touched; legacy `{code, message}` callers are rewritten
  to match

## Risks and mitigations

| Risk                                                                 | Mitigation / Phase 3 resolution                                                                                                                                                        |
| -------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Huma SSE keepalive: no built-in comment frames                       | Manual 15s ticker in stream function (unchanged)                                                                                                                                       |
| Huma SSE string event IDs not supported                              | Phase 3 Fix 3g adds a string-ID variant of `registerSSE` (decided)                                                                                                                     |
| Response shape changes break dashboard                               | Phase 3 Fix 3a retargets the dashboard Go proxy (all dashboard files under `cmd/gc/dashboard/` that call `/v0/...`) to the generated client so shape changes are compile errors        |
| Supervisor Huma API vs per-city Huma API mux conflict                | Phase 3 topology 1 (decided): single supervisor-owned Huma API, per-city operations live at `/v0/city/{name}/...`                                                                      |
| Generic output types don't work with Huma OpenAPI                    | Verified: Huma reflection handles generics, generates schema names like `ListOutputAgentResponse`                                                                                      |
| Blocking `?wait=...` handlers conflict with Huma timeouts            | Verified: no built-in timeout, context cancellation works correctly                                                                                                                    |
| Middleware moved into Huma loses panic recovery for non-Huma routes  | Phase 3 Fix 3d keeps `withRecovery` outermost at the mux level; only error-emitting middleware (CSRF, read-only) becomes Huma middleware                                               |
| Hybrid error format breaks clients                                   | Phase 3 Fix 3a regenerates client from spec; `configureHumaGlobals` 422→400 override is removed (Fix 3k); legacy `{code,message}` parsing deleted with `client.go`                     |
| Raw-byte cache forces hand-written `json.Unmarshal` on cache hits    | Phase 3 Fix 3l converts caches to typed-struct storage; re-serialization cost is negligible at 2s TTL on localhost                                                                     |
| oapi-codegen incomplete support for OpenAPI 3.1                      | Phase 3 prerequisite Fix 3.0 validates generator choice against committed spec; Huma v2.37.3 supports a 3.0 downgrade output that most generators handle cleanly                       |

## Phase 3: Zero Hand-Written Networking (historical — all fixes shipped)

> **This entire section is archived.** Fixes 3.0, 3a, 3b, 3c, 3d, 3e,
> 3f, 3g, 3h, 3j, 3k, and 3l landed across commits `0e0c1881` →
> `309abb6b` (plus subsequent tightening: events-stream precheck,
> shared response-type consolidation, client legacy-fallback deletion).
> Top-of-document status block is authoritative for what exists today;
> the text below records the original problem statements and
> acceptance criteria for each fix so the history is auditable. If you
> are looking for what still needs doing, read the status block, not
> this section.

Phase 3 was defined against the core principle. Every fix named its
problem, fix, acceptance criteria (including grep where applicable and
behavioral tests where greps are insufficient), and files touched.

Counts below are grep-verified as of 2026-04-16. Phase 3 must re-grep
cold at start and update scopes to match reality; fixes are scoped by
outcome (all specified behavior eliminated) not by count.

### Fix 3.0: Generator prerequisite (must land first)

**Problem:** Fix 3a assumes a client generator handles the committed
`openapi.json`. Huma v2.37.3 emits OpenAPI 3.1, and
`oapi-codegen`'s 3.1 support lags — JSON Schema 2020-12, `$defs`,
null-type unions may silently lose fidelity. Huma also supports an
OpenAPI 3.0 downgrade output; some generators prefer it.

**Fix:**

- Run `oapi-codegen` (latest 2.x) and `ogen` against both the 3.1
  and 3.0-downgraded spec.
- Identify any SSE schemas, discriminators, or union types that
  regress. Record results in this plan.
- Choose and pin one Go generator + which spec variant it consumes.
  Commit the choice by recording it in the Fix 3.0 "Decision:" line
  below.
- A TypeScript generator is NOT required. The dashboard frontend
  proxies through Go (`cmd/gc/dashboard/api.go`); Fix 3a's generated
  Go client is the single source of truth for that proxy. If a
  future audit shows frontend code calling `/v0/...` directly, a TS
  generator can be added then.
- This work happens before 3a code lands; Fix 3a implements against
  the chosen generator.

**Decision (recorded 2026-04-16):**

- **Generator:** `oapi-codegen` v2.6.0 (exit 0, 20353 lines, 357
  types, SSE endpoints expose `*http.Response` for stream
  consumption).
- **Runtime:** `github.com/oapi-codegen/runtime` v1.4.0+ (older
  versions lack `StyleParamWithOptions`).
- **Spec variant consumed:** the Huma OpenAPI 3.0.3 downgrade
  (served at `/openapi-3.0.json` and accessed via `srv.ServeHTTP`
  with `GET /openapi-3.0.json` in the generator tool).
- **Required preprocessing** before handing the spec to
  `oapi-codegen`:
  1. Normalize path params: `{name...}` → `{name}` (Huma's
     rest-of-path syntax isn't recognized by the generator and the
     declared parameter name is `name`). Affects
     `/v0/agent/{name...}` and `/v0/patches/agent/{name...}`.
  2. Rename component schemas matching `^(Get|Post|Put|Patch|
     Delete|Head|Options)-.*Response$` to replace the `Response`
     suffix with `Body`. Huma auto-generates schema names
     matching `<OpId>Response`, which collide with oapi-codegen's
     per-operation `<OpId>Response` wrapper type.
- **Regeneration command:** implemented in Fix 3a as
  `go generate ./internal/api/genclient` that runs (a) genspec
  against `/openapi-3.0.json`, (b) jq/Python preprocessing to apply
  both rules above, (c) oapi-codegen on the result.

**Alternatives evaluated and rejected:**

- `ogen` v1.20.3: chokes on `text/event-stream` content type
  (reports "unsupported content types"). Would drop the SSE
  operations from the client. Rejected — SSE is a first-class part
  of the API.
- Feeding the 3.1 spec directly to `oapi-codegen`: unsupported by
  the generator (official note: issue #373). Rejected.
- Feeding the 3.1 spec directly to `ogen`: rejects
  `"type": ["x", "null"]` nullable syntax. Rejected.

**Acceptance (met):**

- Generator choice recorded above with versions pinned.
- Generated client compiles cleanly (`go build` succeeds against
  `runtime@v1.4.0`).
- SSE endpoints are present in the generated client (verified:
  `StreamEvents`, `StreamSession`, `StreamAgentOutput`,
  `StreamAgentOutputQualified` methods).
- `ErrorModel` (Problem Details) is a named type, enabling
  consistent error parsing.

**Files:** `plans/huma-openapi-migration.md`, experimental scratch
output (not committed).

### Fix 3a: Generate a typed Go client from the spec

**Status:** CLI surface SHIPPED in commit `cdd8e2dc`. Dashboard Go
HTTP layer DEFERRED — see "Deferred to future work" at the top of
this plan. The text below is the original plan; the dashboard portion
is preserved here for the future plan that picks it up.

**Problem:** `internal/api/client.go` is 346 hand-written lines using
`http.NewRequest` + `json.Marshal` + `json.NewDecoder`. A second
hand-written HTTP layer lives in the dashboard package across
multiple files:

- `cmd/gc/dashboard/api.go` — ~1,886 lines, ~50 JSON touchpoints plus
  shape adapters that reshape between `/v0/...` wire format and the
  dashboard-internal `/api/...` response DTOs.
- `cmd/gc/dashboard/api_fetcher.go` — `APIFetcher` with its own
  `http.Client`, `json.NewDecoder`, `json.Unmarshal`, and
  `apiListResponse` envelope.
- `cmd/gc/dashboard/serve.go` — `ValidateAPI` (hits `/health`) and
  `detectSupervisor` (hits `/v0/cities`) both with raw `http.Client`
  + `json.NewDecoder`.
- `cmd/gc/dashboard/handler.go` — `fetchCityTabs` hits `/v0/cities`
  directly.

Both `client.go` and the four dashboard files drift from the spec on
every new endpoint. This is the single largest violation of the core
principle.

**Fix:**

- Use the generator chosen in Fix 3.0.
- Add a `go generate` directive in `internal/api/` that produces
  `internal/api/genclient/client_gen.go` from the spec.
- Rewrite `internal/api/client.go` as a thin adapter over the
  generated client (preserving method names the CLI already calls),
  or update CLI callers to use the generated client directly.
- Rewrite the dashboard Go HTTP layer against the generated client:
  `cmd/gc/dashboard/api.go`, `api_fetcher.go`, `serve.go`,
  `handler.go`. **Shape adapters in `api.go` stay**, but their
  upstream source becomes the generated typed responses — the
  adapters map generated types to the dashboard-internal DTOs
  (`MailInboxResponse`, `CommandResponse`, `SessionPreviewResponse`,
  etc.). No raw HTTP or raw `json.Marshal`/`json.NewDecoder` in
  dashboard code talking to `/v0/...`.
- Note: the dashboard frontend (`static/dashboard.js`) calls
  `/api/...` proxied by the above files, NOT `/v0/...` directly. No
  TypeScript client is required; Fix 3.0 confirms this.
- Remove the `configureHumaGlobals` 422→400 override once the
  generated client can parse native 422 Problem Details. (Tracked
  under Fix 3k.)
- Add a CI check that regenerates the client and fails if the result
  differs from what's committed (same pattern as
  `TestOpenAPISpecInSync`).

**Acceptance:**

- `grep -nE 'http\.NewRequest|json\.Marshal\(|json\.NewDecoder'
  internal/api/client.go` returns nothing.
- `grep -nE 'http\.(Client|NewRequest|Get\()|json\.NewDecoder|json\.Unmarshal\('
  cmd/gc/dashboard/{api,api_fetcher,serve,handler}.go` returns only
  hits against the generated client package or against shape
  adapters that consume generated types (no hand-rolled
  `/v0/...` HTTP).
- All CLI and dashboard-Go HTTP talking to the typed API goes
  through the generated client.
- Generated client builds under `go build ./...`; regeneration is
  idempotent (CI check).
- Tests that asserted legacy `{code,message}` shapes are rewritten to
  assert Problem Details (see Fix 3c / 3k).

**Files:** `internal/api/client.go`, `internal/api/genclient/` (new),
`cmd/gc/dashboard/api.go`, `cmd/gc/dashboard/api_fetcher.go`,
`cmd/gc/dashboard/serve.go`, `cmd/gc/dashboard/handler.go`,
`Makefile`, `.github/workflows/*`, CLI callers in `cmd/gc/...`,
tests including `internal/api/client_test.go` and
`cmd/gc/dashboard/sse_proxy_test.go`.

### Fix 3b: Put the supervisor on Huma

**Problem:** `supervisor.go` + `supervisor_*.go` + `SupervisorMux` use
raw `net/http` with hand-written JSON. `/v0/cities`, `/health`, and
the city routing metadata endpoints are invisible to the OpenAPI spec.
7 `writeError` + 3 `writeJSON` sites. The supervisor mux also shares
paths (`/health`, `/v0/events/stream`) with per-city Huma APIs —
topology is unresolved.

**Prerequisite:** the supervisor-vs-city topology decision above
(under "Supervisor / Multi-City Architecture") must be recorded
before code lands. Recommended: topology (1) — one supervisor-owned
Huma API, all per-city operations registered as `/v0/city/{name}/...`
paths.

**Fix (assuming topology 1):**

- Create a supervisor-level Huma API via `humago.New` against the
  supervisor's mux.
- Move per-city route registration to operate under the
  `/v0/city/{name}/...` prefix on the supervisor API; the handlers
  dispatch internally to the city's state by `{name}`.
- Register supervisor-only endpoints (`/v0/cities`, `/health`,
  routing metadata, global events stream) as Huma operations on the
  same API.
- Replace every `writeJSON` with a typed Huma output struct.
- Replace every `writeError` with `huma.Error4xx/5xx` constructors
  (middleware uses `huma.WriteErr`, see Fix 3d).
- The supervisor's global events stream migrates under Fix 3g.
- Apply the Huma middleware stack from Fix 3d to the supervisor API.

**Acceptance:**

- `grep -n 'writeJSON\|writeError' internal/api/supervisor*.go`
  returns nothing.
- `/v0/cities`, `/health`, `/v0/city/{name}/...`, and the global
  events stream all appear in the committed `openapi.json`.
- Behavioral tests: existing supervisor/scoped-routing tests pass
  after rewrite (see `handler_agent_crud_test.go:177`,
  `client_test.go:205`, `cmd/gc/dashboard/sse_proxy_test.go:20`).

**Files:** `internal/api/supervisor.go`, `internal/api/supervisor_*.go`,
`internal/api/huma_types_supervisor.go` (new),
`internal/api/huma_handlers_supervisor.go` (new),
`internal/api/server.go`, relevant tests.

### Fix 3c: Eliminate `apiError{}`

**Problem:** 22 `apiError{}` construction sites inside Huma handlers
implement `huma.StatusError` directly, bypassing Huma's Problem
Details encoder. Breakdown: `huma_handlers_sessions.go` (17),
`huma_handlers_beads.go` (3), `huma_handlers_mail.go` (2). The
`idempotency.go` helper also returns `apiError` so handlers can
replay; Fix 3e owns the idempotency rewrite but this fix consumes its
output.

**Fix:**

- Replace each `apiError{Status: N, Message: "..."}` with the matching
  `huma.Error<N>...(...)` constructor, or a typed domain error
  wrapped by a shared `domainError(err)` helper.
- Consume the idempotency rewrite owned by Fix 3e (signature
  `(*TypedOutput, huma.StatusError)`) so beads/mail handlers stop
  constructing `apiError{}` directly. Fix 3e owns the rewrite; this
  fix consumes its output.
- Delete the `apiError` type from `huma_types.go` once zero callers
  remain.
- Register a single Problem Details model in Huma; callers use the
  helpers rather than constructing shapes by hand.
- Update test fixtures that asserted the legacy `{code,message}`
  shape to assert Problem Details (see Fix 3a acceptance).

**Acceptance:**

- The `apiError` type is deleted from `internal/api/huma_types.go`.
- `grep -nE '&apiError\{' internal/api/` returns nothing (scoped to
  construction to avoid doc-comment matches).
- `grep -n '"code"\s*:\s*"[^"]' internal/api/*_test.go` returns
  nothing (no test fixtures assert the legacy shape).

**Files:** `internal/api/huma_handlers_sessions.go`,
`internal/api/huma_handlers_beads.go`,
`internal/api/huma_handlers_mail.go`, `internal/api/huma_types.go`,
`internal/api/idempotency.go`, related `*_test.go` files.

### Fix 3d: Migrate error-emitting middleware to Huma-native errors

**Problem:** `middleware.go` emits errors through 3 `writeError` calls
for read-only mode, CSRF rejection, and panic recovery. These run
before Huma and emit the legacy `{code, message}` shape. Moving
everything into Huma would lose panic recovery for any remaining raw
routes (e.g. `/svc/*`).

**Fix (scoped migration, not wholesale):**

- `withCSRFCheck` and `withReadOnly` become Huma middleware
  registered via `api.UseMiddleware(...)`. Rejection emits Problem
  Details by calling `huma.WriteErr(api, ctx, 403, "...")` (or
  equivalent) and returning without calling `next(ctx)`. Huma v2 has
  no separate "abort path" — `huma.WriteErr` + early return IS the
  abort pattern.
- **Attach Huma middleware BEFORE registering any operations.**
  `api.UseMiddleware` only applies to operations registered AFTER the
  middleware is attached; an attach-after-register ordering mistake
  would silently leave existing routes ungated. This applies to both
  the supervisor Huma API in Fix 3b and any pre-Fix-3b API
  construction. Add a behavioral test that drives an existing route
  and confirms it returns 403 Problem Details under read-only mode.
- `withRecovery` stays outermost at the mux level so it covers
  non-Huma routes (`/svc/*`, health-check hooks). The existing
  implementation emits a 500 via `writeError`; replace its
  body-writer with a Problem Details body construction compatible
  with Huma's encoder (typed struct + `json.Marshal` of the Problem
  Details shape). Still no `writeError`. **Tension with the core
  principle:** `withRecovery` runs outside Huma, so the Problem
  Details body here is hand-constructed. This is the narrow
  unavoidable exception — the recovery middleware cannot use Huma's
  error path because Huma is downstream of it. The principle is
  preserved everywhere Huma runs; the recovery path is one
  documented, typed exit.
- `withRequestID` and any CORS wrapper stay outermost at the mux
  level (they set headers on every response, Huma and non-Huma
  alike).
- Target state (topology 1): attach the Huma middleware stack to
  the single supervisor-owned Huma API that Fix 3b builds. If any
  per-city Huma APIs still exist when 3d runs before 3b collapses
  them, attach to those too in the interim so the middleware gate
  is never missed.
- Ordering inside Huma: CSRF before read-only. Outside Huma (mux):
  request-id → CORS → recovery → (Huma adapter).

**Acceptance:**

- `grep -n 'writeError' internal/api/middleware.go` returns nothing.
- Middleware rejection responses match `huma.Error403Forbidden(...)`
  byte-for-byte (behavioral test).
- Panic in a `/svc/*` raw route is caught by outer recovery and
  returns a Problem Details-shaped 500.

**Files:** `internal/api/middleware.go`, `internal/api/server.go`,
`internal/api/supervisor.go`.

### Fix 3e: Migrate remaining `writeError` / `writeJSON` / `writeListJSON` callers

**Problem:** After 3b and 3d, four handler helper files still emit
hand-written responses:

- `handler_city_create.go` (10 `writeError` + 1 `writeJSON`)
- `handler_provider_readiness.go` (6 `writeError` + 2 `writeJSON`)
- `handler_services.go` (6 `writeError`)
- `idempotency.go` (2 `writeError`, plus `apiError` returns consumed
  by Fix 3c)

Plus `writeListJSON` callers (if any remain after the Huma migration)
and `decodeBody` callers in `handler_beads.go` and
`handler_city_create.go`.

These are helpers invoked from Huma handlers — they shouldn't exist
as separate response writers.

**Fix:**

- Rewrite each helper to return typed errors (`huma.Error*` or
  `domainError(err)`) instead of writing to `http.ResponseWriter`.
- Lift any remaining response construction into the calling Huma
  handler as a typed output struct.
- Rewrite `idempotency.handleIdempotent` to return
  `(*TypedOutput, huma.StatusError)` so Fix 3c's handlers can
  consume it without constructing `apiError{}` values.
- Delete `decodeBody` — Huma decodes request bodies automatically via
  the handler's `Body` field.
- Update tests that currently assert legacy `{code,message}` shapes
  (at minimum: `idempotency_test.go`, `handler_agent_crud_test.go`,
  `client_test.go`).

**Acceptance:**

- `grep -n 'writeJSON\|writeError\|writeListJSON\|decodeBody'
  internal/api/` returns only `envelope.go` definitions (which Fix 3h
  removes).
- Test suite asserts only Problem Details on error paths.

**Files:** `internal/api/handler_city_create.go`,
`internal/api/handler_provider_readiness.go`,
`internal/api/handler_services.go`, `internal/api/idempotency.go`,
`internal/api/handler_beads.go`, related `*_test.go` files.

### Fix 3f: Eliminate opaque response bodies in Huma handlers

**Problem:** 28 `json.Marshal` calls across 7 Huma handler files; every
`json.RawMessage` or `map[string]any` response body means the spec
has no contract for that endpoint. Affected files and patterns:

- `huma_handlers_extmsg.go` — 11 `json.Marshal`; `ListOutput[json.RawMessage]`
  on list/transcript/adapter endpoints
- `huma_handlers_sessions.go` — 9 `json.Marshal`; `IndexOutput[json.RawMessage]`
  on transcript/agent-list/agent-get endpoints
- `huma_handlers_providers.go` — 2 `json.Marshal`;
  `ListOutput[json.RawMessage]` on list endpoints
- `huma_handlers_services.go` — 2 `json.Marshal`;
  `ListOutput[json.RawMessage]` and `IndexOutput[json.RawMessage]`
- `huma_handlers_convoys.go` — 1 `json.Marshal`;
  `IndexOutput[map[string]any]` on convoy-get, convoy-check,
  workflow-get; plus a `structToMap` helper that does JSON round-trips
- `huma_handlers_agents.go` — 1 `json.Marshal` (cache-hit path);
  resolved by Fix 3l typed caches
- `huma_handlers_config.go` — 2 `json.Marshal` inside custom
  `MarshalJSON` methods that flatten `annotatedAgentResponse` and
  `annotatedProviderResponse`. The committed spec models these as
  nested objects — the generated client is ALREADY wrong on
  `GET /v0/config/explain`. Fix replaces the types with explicit flat
  structs and removes the custom `MarshalJSON`.

**Fix:**

- Define concrete typed output structs for every affected endpoint.
- Replace `json.RawMessage` and `map[string]any` response fields with
  the typed structs. Where `json.RawMessage` appears in
  `huma_types*.go` as a Body field (e.g.
  `huma_types_sessions.go:92`), replace with the typed struct too.
- Call out the **two-layer pattern** in sessions and extmsg
  handlers: `map[string]any{...}` literals passed to `json.Marshal`
  to build `json.RawMessage` bodies. Both layers must be replaced
  with the typed struct at once; removing only the outer
  `json.Marshal` without defining the struct leaves the map literal
  as a dangling compile error.
- Delete `structToMap` in `huma_handlers_convoys.go`.
- Delete custom `MarshalJSON` methods in `huma_handlers_config.go`;
  replace the source types with flat structs matching the spec.
- Note the one legitimate exception: `huma_handlers_beads.go:348`
  uses `map[string]json.RawMessage` as an INPUT decoder pattern to
  distinguish JSON-null from field-absent. That is not a response
  shape; leave it in place with a comment justifying the exception.
- Add a contract test that compares a real response body to the
  generated schema for `GET /v0/config/explain` (and at least one
  endpoint per fixed handler file).

**Acceptance:**

- `grep -nE 'json\.Marshal\(|json\.RawMessage|map\[string\]any'
  internal/api/huma_handlers_*.go` returns only the documented input
  decoder in `huma_handlers_beads.go:348`.
- `grep -nE 'json\.RawMessage' internal/api/huma_types*.go` returns
  nothing (Body fields are typed structs).
- Every Huma response body has a typed schema in the spec.
- Contract tests pass: real response body matches the spec-generated
  schema for the endpoints touched.

**Files:** `internal/api/huma_handlers_extmsg.go`,
`internal/api/huma_handlers_sessions.go`,
`internal/api/huma_handlers_providers.go`,
`internal/api/huma_handlers_services.go`,
`internal/api/huma_handlers_convoys.go`,
`internal/api/huma_handlers_agents.go`,
`internal/api/huma_handlers_config.go`, plus new or updated typed
output structs in the relevant `huma_types_*.go` files.

### Fix 3g: Move the supervisor's global events stream to `registerSSE`

**Problem:** `convoy_event_stream.go` contains 4 `writeSSE` calls that
serve the supervisor's `/v0/events/stream` via
`streamProjectedGlobalEvents`. No typed event schema appears in the
spec. The stream uses composite STRING cursor IDs via
`writeSSEWithStringID` because the cursor is a multi-city/multi-stream
value (`events.FormatCursor(cursors)`). Huma's `sse.Message.ID` is
`int` today, and the custom `registerSSE` wrapper hardcodes integer
IDs — so the existing `Last-Event-ID` reconnect contract cannot be
preserved by a naive drop-in.

**Decision: extend `registerSSE` with a string-ID variant.** Keeping
server-emitted event IDs in the wire format preserves
`EventSource.lastEventID` behavior across all four streams. Options
considered and rejected:

- Drop `Last-Event-ID` on the global stream and require clients to
  reconnect via an `after_cursor` query parameter. Rejected because it
  is a behavior change for clients and loses consistency with the
  other three streams.

Implementation: add an `sse.Message`-like struct with a `string ID`
and a companion `SenderWithStringID` surface on `registerSSE`. Fix 3g
implements and uses this variant for the supervisor global stream.

**Fix:**

- Implement the string-ID variant (see the `registerSSE` contract
  section). Start with Option A (sibling `registerSSEStringID`) and
  a new `SenderWithStringID` type. Escalate to Option B (generic)
  only if sibling duplication becomes painful.
- Register the supervisor stream via the string-ID variant with a
  typed event map matching the events `streamProjectedGlobalEvents`
  emits today.
- Refactor `streamProjectedGlobalEvents` to accept the string-ID
  sender callback instead of `http.ResponseWriter`.
- **Watcher lifecycle lives inside the stream callback** (see the
  contract section): precheck validates that the event provider is
  available; the stream callback opens the `events.MuxWatcher` and
  `defer mw.Close()`s it. Watcher-open failures after precheck pass
  terminate the stream cleanly (no HTTP error).
- Delete `writeSSE`, `writeSSEComment`, and `writeSSEWithStringID`
  from `sse.go` once no callers remain. Keep the `registerSSE`
  wrapper and its new string-ID sibling.

**Acceptance:**

- `grep -n 'writeSSE' internal/api/` returns nothing.
- The global events stream appears in the OpenAPI spec with typed
  event schemas.
- Reconnect test: a client disconnect + reconnect with
  `Last-Event-ID` (the string-ID wire format) resumes from the
  correct position.

**Files:** `internal/api/convoy_event_stream.go`,
`internal/api/sse.go`, `internal/api/convoy_event_stream_test.go`.

### Fix 3h: Delete `envelope.go` and legacy error helpers

**Problem:** `envelope.go` defines `writeJSON`, `writeError`,
`writeListJSON`, and the legacy `Error` / `FieldError` types. Once
3b–3g land, every caller is gone. Tests may still reference the
legacy types.

**Fix:**

- Audit `internal/api/*_test.go` for references to `api.Error`,
  `api.FieldError`, or hand-constructed legacy-shape assertions.
  Replace with Problem Details assertions.
- Delete `envelope.go` and `envelope_test.go`.
- Remove the legacy `Error` / `FieldError` types.
- Remove imports in any remaining consumers.

**Acceptance:**

- `internal/api/envelope.go` and `envelope_test.go` do not exist.
- `grep -n '\bapi\.Error\b\|api\.FieldError' internal/api/` returns
  nothing.
- `go build ./...` and `go test ./...` pass.

**Files:** `internal/api/envelope.go`,
`internal/api/envelope_test.go`, any test files referencing the
deleted types.

### Fix 3j: Wire the session state machine through the manager

**Problem:** `internal/session/state_machine.go` defines the
transition table and `Transition()` reducer; no handler or manager
calls it. Session handlers still mutate bead metadata directly,
scattering state rules across the codebase. There is also no
`ErrIllegalTransition` typed error — if/when one exists, Huma will
emit a 500 unless it maps to a proper 4xx.

**Fix:**

- Define `session.ErrIllegalTransition` as a typed error with a
  descriptive message and a target HTTP status (409 Conflict is the
  correct semantic — the session is in a state that doesn't accept
  this command).
- Extend `domainError(err)` (Fix 3c) to map `ErrIllegalTransition`
  to `huma.Error409Conflict(...)`.
- Route every session mutation in `internal/session/manager.go`
  through `Transition(from, cmd)` — Create, Ready, Suspend, Wake,
  Sleep, Quarantine, Drain, Archive, Close.
- Simplify handlers in `huma_handlers_sessions.go` to: validate
  input, dispatch a command to the manager, return a typed output.

**Acceptance:**

- Every mutation path in `internal/session/manager.go` passes
  through `Transition()` (enforced by tests asserting the transition
  table is consulted; not a grep).
- A behavioral test drives each illegal transition and confirms a
  409 Problem Details response.
- Presentation-layer state reads in handlers are isolated to a named
  helper (e.g. `presentationState(b)`); handlers no longer compare
  raw state strings inline.

**Files:** `internal/session/manager.go`,
`internal/session/state_machine.go` (add `ErrIllegalTransition`),
`internal/api/huma_handlers_sessions.go`,
`internal/api/errors.go` (new — home for the `domainError` helper
shared with Fix 3c; exact filename to be confirmed during Fix 3c
execution).

### Fix 3k: Validation audit — remove `omitempty` on required fields + the 422→400 override

**Problem:** Phase 2b added `minLength:"1"` to 12 required fields
across 7 input types. The remaining body-input types still use
`json:"field,omitempty"` on required fields, which tells Huma (and
the spec) the fields are optional. `configureHumaGlobals` also
rewrites 422→400 to keep hand-written `client.go` parsing happy —
once the generated client lands (Fix 3a), that override becomes spec
drift.

**Fix:**

- Audit every struct in `huma_types*.go` for `Body` fields that are
  required business-logically but tagged `omitempty`. For each:
  remove `omitempty`, add appropriate validation (`minLength`,
  `required`, `enum`, `pattern`).
- Regenerate the committed `openapi.json`; `TestOpenAPISpecInSync`
  confirms the spec now marks required fields required.
- Remove the 422→400 rewrite in `configureHumaGlobals`. Generated
  client and CI tests must accept 422 for validation errors.
- Delete any handler-side manual validation (`if input.Body.X ==
  "" { ... }`) that Huma now covers.

**Acceptance:**

- `grep -n 'omitempty' internal/api/huma_types*.go` returns only
  fields that are genuinely optional (reviewed individually).
- `grep -n 'StatusUnprocessableEntity\|422' internal/api/server.go`
  returns nothing indicating the override.
- Validation errors in tests assert 422 + Problem Details with
  `errors` array.

**Files:** `internal/api/huma_types.go`,
`internal/api/huma_types_*.go`, `internal/api/server.go`,
`internal/api/openapi.json`, related `*_test.go` files.

### Fix 3l: Typed caches — remove hand-written JSON in cache hits

**Problem:** `response_cache.go` and `idempotency.go` store cached
responses as `[]byte`. Handlers call `json.Unmarshal` on cache-hit
paths in `huma_handlers_agents.go:31`, `huma_handlers_mail.go:238`,
`huma_handlers_beads.go:245`. This is hand-written JSON
(de)serialization inside Huma handlers — the core principle's exact
prohibition.

**Fix:**

- Convert `response_cache.go` to generic typed storage:
  `cache.Get[T](key, index) (T, bool)` / `cache.Set[T](key, index,
  value, ttl)`. Internal representation can still be `any`; the
  generic type binds at the call site. No `json.Marshal`/`Unmarshal`
  inside the cache package.
- Same treatment for `idempotency.go` — store the typed response
  value, not its serialized bytes. The request-body hash
  (`bodyHash`) stays — it's computed from the INCOMING request body
  before handler dispatch, independent of how the response is
  cached. Only the cached response representation changes from
  `[]byte` to typed.
- Remove `json.Unmarshal` calls from the cache-hit paths in
  `huma_handlers_*.go`. Handlers return the cached typed struct
  directly; Huma re-serializes on each hit (negligible cost at 2s
  TTL + localhost).

**Acceptance:**

- `grep -n 'json\.Unmarshal\|json\.Marshal'
  internal/api/huma_handlers_*.go` returns nothing for cache paths.
- `grep -n 'json\.Marshal\|json\.Unmarshal'
  internal/api/response_cache.go internal/api/idempotency.go`
  returns nothing. Cache packages store typed values only.
- `grep -n '\[\]byte' internal/api/response_cache.go
  internal/api/idempotency.go` shows `[]byte` only for `bodyHash`
  (request-body hash input), never for stored responses.
- Cache-hit tests still pass; cache-hit behavior is
  indistinguishable from cache-miss modulo timing. Idempotency
  mismatch detection (different body, same key → 422) still works.

**Files:** `internal/api/response_cache.go`,
`internal/api/idempotency.go`, `internal/api/huma_handlers_agents.go`,
`internal/api/huma_handlers_mail.go`,
`internal/api/huma_handlers_beads.go`, related `*_test.go` files.

### Phase 3 ordering

Dependencies (not strict serial order — 3a runs continuously as a
validation loop, not a late step):

**Prerequisite (blocker):**

- **Fix 3.0 (generator prerequisite).** Must land first. Its
  generator choice feeds Fix 3a.

**Parallel first wave (independent):**

- **3c (apiError)** + **3f (opaque response bodies)** + **3l (typed
  caches)** — clean Huma handlers so the error/response story is
  typed end-to-end. 3l removes cache-hit `json.Unmarshal` that would
  otherwise persist.
- **3g (global SSE)** — implements the string-ID variant of
  `registerSSE` and migrates the supervisor stream.
- **3k (validation audit + 422→400 removal)** — safe to run anytime
  after 3c; decoupled from other fixes.

**Then:**

- **3d (middleware)** + **3e (remaining writeError/writeJSON/
  writeListJSON/decodeBody)** — collapse non-Huma error paths onto
  Problem Details. Depends on the typed error model from 3c.

**Then:**

- **3b (supervisor on Huma)** — topology 1 (decided). Depends on
  3c/3d/3k (typed error model + Huma middleware story).

**Running throughout (not a late step):**

- **3a (generated Go client).** Regenerate continuously as the
  server surface changes. 3a IS the validation tool for 3b, 3c, 3f,
  and 3k — running it early catches shape mistakes. The "final" 3a
  milestone (committed generated client + dashboard-proxy rewrite)
  lands after 3b stabilizes the supervisor surface.

**Last (once 3a–3g all land):**

- **3h (delete envelope.go)** — trivial cleanup.
- **3j (wire state machine)** — largest behavioral change; land last
  with thorough session-lifecycle test coverage.

### Phase 3 verification

The core principle is partially grep-verifiable, but greps alone are
insufficient — the migration touches test assertions, wire formats,
and behavioral contracts.

**Grep gate** (all must be empty inside `internal/api/`, excluding
`*_test.go` unless stated):

| Pattern                                   | Files allowed                                          |
| ----------------------------------------- | ------------------------------------------------------ |
| `writeError\(`                            | none                                                   |
| `writeJSON\(`                             | none                                                   |
| `writeListJSON\(`                         | none                                                   |
| `writeSSE`                                | none                                                   |
| `&apiError\{`                             | none                                                   |
| `\bapiError\b` (type definition)          | none (type is deleted)                                 |
| `decodeBody\(`                            | none                                                   |
| `json\.Marshal\(`                         | none in `huma_handlers_*.go`, `response_cache.go`, `idempotency.go` |
| `json\.Unmarshal\(`                       | none in `huma_handlers_*.go`, `response_cache.go`, `idempotency.go` |
| `json\.RawMessage`                        | only `huma_handlers_beads.go:348` (documented input decoder)        |
| `map\[string\]any` as Huma output body    | none                                                                |
| `http\.NewRequest` / `http\.Client` / `http\.Get`        | none in `internal/api/client.go` or `cmd/gc/dashboard/{api,api_fetcher,serve,handler}.go` |
| `json\.NewDecoder` / `json\.Unmarshal\(`  | none in `internal/api/client.go` or `cmd/gc/dashboard/{api,api_fetcher,serve,handler}.go` (except hits through the generated client) |
| `StatusUnprocessableEntity` override      | none in `internal/api/server.go`                       |
| `\bapi\.Error\b\|\bapi\.FieldError\b`     | none (legacy types deleted)                            |

**Behavioral + operational gate** (grep-insufficient checks):

- `go test ./...` passes including rewritten tests that now assert
  Problem Details instead of legacy `{code,message}`.
- `go vet ./...` clean.
- `openapi.json` includes every supervisor endpoint, every per-city
  endpoint, every SSE stream with typed event schemas, and typed
  response bodies on `/v0/config/explain` and every formerly-opaque
  endpoint.
- `TestOpenAPISpecInSync` passes.
- Contract tests: real response body matches the spec-generated
  schema for a representative endpoint in every fixed handler file.
- Reconnect test: SSE global stream client reconnects correctly via
  `Last-Event-ID` (the string-ID variant decided in Fix 3g).
- Generated Go client builds, regeneration is idempotent (CI check),
  and both CLI smoke tests and dashboard-proxy smoke tests pass
  using it.
- Every mutation path in `internal/session/manager.go` passes through
  `Transition()`; illegal transitions return 409 Problem Details.
- Panic in a `/svc/*` raw route is caught by outer `withRecovery` and
  returns a Problem Details-shaped 500.

**Out of scope for Phase 3 (by design):**

- `/svc/*` workspace-service proxy (per the core principle's explicit
  exclusion).
- `internal/extmsg/http_adapter.go` — outbound HTTP to external
  ExtMsg callback URLs. Consumer of someone else's contract, not a
  typed endpoint of this API.
- `internal/workspacesvc/proxy_process.go` — outbound HTTP to spawn
  or manage workspace service subprocesses. Same rationale.
- Finishing the split of `huma_types.go` by domain (Phase 2e partial;
  not blocking the core principle).
- Merging `handler_*.go` / `huma_handlers_*.go` pairs (Phase 2f;
  revisit when file layout pain is concrete).
