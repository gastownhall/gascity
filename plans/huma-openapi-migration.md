# Plan: Replace Network Layer with Huma + OpenAPI 3.1

## Status: Phase 1 + 2 + 3 + 3.5 Complete (server). Consumer alignment follow-on in progress.

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

**Post-3.5 consumer alignment follow-on (2026-04-17, in progress):**

- **Dashboard moved from “legacy proxy mindset” toward direct API-contract
  consumption.** The restored standalone dashboard is now a static
  HTML/CSS/TS client that treats the supervisor API as the authority,
  not a private Go adapter surface. Current work includes:
  - **Supervisor-first boot.** `gc dashboard` no longer requires a city
    directory; the UI launches in supervisor scope with no `?city=...`
    and degrades by hiding city-scoped panels instead of rendering
    confusing empty states.
  - **Parity regressions closed with frontend tests.** Palette-driven
    compose / new-issue / new-convoy / assign flows now open their real
    forms or modals again, and empty-state copy resets correctly when a
    city is selected after supervisor mode. `vitest` + `jsdom` coverage
    now guards those flows.
  - **One live event stream, not two.** The SPA is being tightened so
    activity rendering and panel invalidation both derive from the API's
    SSE contract instead of opening duplicate streams and guessing at
    semantic event names from the SSE `event:` line.
  - **API-typed client helpers.** The frontend already generates TS
    types from `internal/api/openapi.json` and uses `openapi-fetch`;
    current follow-on work is pushing more of the restored dashboard
    behavior through those typed helpers instead of scattered ad hoc
    endpoint strings and brute-force refreshes.
  - **Structured operator flows.** The remaining `window.prompt` /
    `window.confirm` interactions in the restored dashboard are being
    replaced with explicit modal flows so assignment, sling, and
    reassign actions are real UI interactions backed by the same API
    contract the docs publish.

- **`gc events` is being realigned as a reflection of the API, not an
  alternate event model.** The source of truth remains the typed API:
  city list/stream (`/v0/city/{cityName}/events`,
  `/v0/city/{cityName}/events/stream`) and supervisor list/stream
  (`/v0/events`, `/v0/events/stream`). Follow-on work on `gc events`
  is defined by that constraint:
  - **Per-scope parity.** In city scope, `gc events` must reflect the
    city event list/stream contract. In supervisor scope, it must
    reflect the supervisor tagged-event list/stream contract.
    This is now wired through typed REST + SSE calls, not direct local
    event-provider access.
  - **SSE schema awareness.** The CLI and dashboard must treat the SSE
    `event:` field as a transport envelope (`event`, `tagged_event`,
    `heartbeat`) and the semantic event type as the JSON payload's
    `type` field. That mapping is part of the contract and must be
    documented explicitly.
    The CLI now emits list-item DTOs as JSONL in default list mode and
    stream-envelope DTOs as JSONL in `--watch` / `--follow`, with
    supervisor resume via the new `--after-cursor` flag.
  - **User-facing docs.** `docs/reference/api.md` and the generated CLI
    reference need an explicit event-schema section that explains the
    relationship between list responses, SSE envelopes, and `gc events`
    JSON output so developers can match them 1:1 when building external
    tools. The API reference now carries that mapping explicitly.
  - **Dashboard invalidation contract.** The restored SPA currently
    consumes events operationally; this follow-on makes that explicit by
    deriving invalidation from the same typed event payloads that `gc
    events` and the published API docs describe.

**Out of scope for this plan:**

- **Historical dashboard Go proxy rewrite.** The original Phase 3
  inventory explicitly excluded the hand-written Go dashboard proxy
  (`cmd/gc/dashboard/api.go`, `api_fetcher.go`, `serve.go`,
  `handler.go`). That remains true as historical context: this plan did
  not require finishing a generated Go client migration for that old
  layer. The current dashboard follow-on is instead about the shipped
  static SPA consuming the published API contract correctly.

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

## Archive

Phase history, gap analyses, design research, and the Phase 3 fix catalog
live in [`plans/archive/huma-openapi-migration-history.md`](archive/huma-openapi-migration-history.md).
Start there if you want the "why" behind a current shape; start here if
you want to know what's true today.
