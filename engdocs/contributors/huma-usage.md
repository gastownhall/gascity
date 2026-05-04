---
title: "Huma Usage Notes"
description: "Contributor notes for Huma v2 patterns, quirks, and generated OpenAPI behavior in Gas City's HTTP and SSE API."
---

Gas City's HTTP + SSE control plane is built on Huma v2
(`github.com/danielgtaylor/huma/v2`). This document captures the
framework-level patterns and quirks we've learned the hard way, so
future contributors can find the answer here instead of re-deriving
it from the framework source or stumbling into the same traps.

Every pattern below is load-bearing in the current implementation;
removing one breaks a specific invariant described in
[API Control Plane](../architecture/api-control-plane.md).

## 1. Presence detection for query parameters

**Problem.** Some endpoints need to distinguish "query parameter
absent" from "query parameter present with empty/zero value." The
naive answer — a pointer field like `Cursor *string` — panics at
registration because Huma v2 does not support pointer query
parameters (`huma.go:189`, issue
[#288](https://github.com/danielgtaylor/huma/issues/288)).

**Solution.** Use the `OptionalParam[T]` wrapper. Gas City's copy
lives at `internal/api/huma_optional_param.go`:

```go
type OptionalParam[T any] struct {
    Value T
    IsSet bool
}

func (o OptionalParam[T]) Schema(r huma.Registry) *huma.Schema {
    return huma.SchemaFromType(r, reflect.TypeOf(o.Value))
}

func (o *OptionalParam[T]) Receiver() reflect.Value {
    return reflect.ValueOf(o).Elem().Field(0)
}

func (o *OptionalParam[T]) OnParamSet(isSet bool, _ any) {
    o.IsSet = isSet
}
```

Declared on an input struct:

```go
type SessionListInput struct {
    CityScope
    Cursor OptionalParam[string] `query:"cursor"`
}
```

`Schema()` emits the wrapped `T`'s schema unchanged, so the wire
contract is identical to a plain `query:"cursor" string` field.
`IsSet` is populated by `OnParamSet`, which Huma's binder calls
after parsing.

**The sharp edge.** Huma's parameter binder treats empty string
values as `isSet = false` (`huma.go:881-882`:
`isSet = value != ""`). `?cursor=` and no `cursor=` at all are
indistinguishable at the handler. Three-state semantics (absent /
present-empty / present-nonempty) are not expressible under Huma;
APIs must design around two-state (`IsSet && value != ""`).

This constraint is intrinsic to the framework, not a Gas City
choice. Do not work around it by reading raw URL values in a
Resolver (that's an [API Control Plane](../architecture/api-control-plane.md)
§3.5.1 violation).

## 2. Pointer query params panic; Resolvers cannot rescue them

Huma checks the kind of every query/header/path/cookie field at
registration (`huma.go:189`):

```go
if f.Type.Kind() == reflect.Pointer {
    panic("pointers are not supported for form/header/path/query parameters")
}
```

The TODO in the source acknowledges this is solvable but hasn't
been done. Until it is, every optional query parameter must be a
value type plus either `OptionalParam[T]` (for presence detection)
or a sentinel value the handler interprets.

Resolvers (`huma.Resolver` / `huma.ResolverWithPath`) can validate
or normalize values the struct already declares, but they MUST NOT
read keys off `ctx.URL().Query()` or `ctx.Header()` that are not
declared fields. Hidden contract, §3.5.1 violation.

## 3. Operation handlers — Huma's escape hatch for cross-cutting
ops-level metadata

`huma.Get`/`Post`/`Put`/`Patch`/`Delete` take `operationHandlers
...func(op *Operation)` after the handler. They run AFTER Huma has
auto-populated `OperationID` / `Summary` / `Method` / `Path`, so
the handler can append to `op.Parameters` (or `op.Responses`,
`op.Tags`, etc.) without disturbing the auto-generated identity.

Gas City uses this to declare the `X-GC-Request` CSRF header
without touching 50+ input structs. In
`internal/api/city_scope.go`:

```go
func addMutationCSRFParam(op *huma.Operation) {
    // idempotent append of the X-GC-Request header param
}

func cityPost[I, O any] (sm *SupervisorMux, tail string,
    fn func(*Server, context.Context, *I) (*O, error),
) {
    huma.Post(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn),
        addMutationCSRFParam)
}
```

**The sharp edge.** If you skip `huma.Post` and call
`huma.Register` with a manually-built `huma.Operation`, you lose
auto-`OperationID`/Summary generation — the spec ends up with
empty `operationId` fields, and oapi-codegen falls back to
path-synthesized method names (`PostV0CityCityNameRigNameAction`
instead of `PostV0CityByCityNameRigByNameByAction`). The
regenerated Go client fails to compile against existing call
sites. Lesson: use the convenience helpers, not `huma.Register`
directly, whenever the auto-generated metadata is acceptable.

## 4. OperationID generation

`huma.go:2192`:

```go
var GenerateOperationID = func(method, path string, response any) string {
    // ... produces kebab-case IDs like "post-v0-city-by-city-name-rig-by-name-by-action"
    return casing.Kebab(action + "-" +
        reRemoveIDs.ReplaceAllString(path, "by-$1"))
}
```

`{param}` segments are replaced with `by-param`. The kebab ID
becomes the operationId in the OpenAPI spec, which oapi-codegen
PascalCases into Go method names. Explicit `OperationID` values
(e.g. `"create-agent"`) are preserved.

Skipping `huma.Post` and registering via `huma.Register` directly
bypasses this generator. If you see a mutation endpoint in
`openapi.json` with no `operationId`, you're looking at a case
where someone built the Operation by hand and forgot to populate
`OperationID`.

## 5. Response headers that apply to every operation

OpenAPI 3.1 has no "global response header" mechanism. The
canonical pattern is to declare the header once in
`components.headers` and `$ref` it from each operation's
responses:

```yaml
components:
  headers:
    X-Request-Id:
      description: ...
      schema: {type: string}

paths:
  /foo:
    get:
      responses:
        "200":
          headers:
            X-Request-Id:
              $ref: "#/components/headers/X-Request-Id"
```

Gas City does this in `internal/api/huma_spec_framework.go`:
`registerFrameworkHeaders` runs once after all routes are
registered, populates `api.OpenAPI().Components.Headers`, and
walks every operation's responses to inject `{Ref: "#/..."}`
entries. One named component backs 284 `$ref` entries, so the
spec stays readable and generated clients get a single typed
accessor.

`huma.Header` is a type alias for `huma.Param`
(`openapi.go:588`), which is why `Response.Headers` is typed
`map[string]*Param`. Setting `Ref: "#/components/headers/NAME"`
on the Param produces the `$ref` emission in the generated YAML.

## 6. Per-operation custom response headers (SSE streams)

Some SSE endpoints emit custom headers like `GC-Agent-Status`
or `GC-Session-State` via `hctx.SetHeader`. These are not
global — they apply to specific streams — so they belong on
the operation's 200 response, not in
`components.headers`. But inlining the description at the
registration site means 3+ copies of the same description
string.

Gas City's pattern (`internal/api/sse.go`):

```go
var sseStatusHeaders = map[string]string{
    "GC-Agent-Status":   "...",
    "GC-Session-State":  "...",
    "GC-Session-Status": "...",
}

func sseResponseHeaders(names ...string) map[string]*huma.Response {
    // builds Responses["200"].Headers from the catalog;
    // panics if a name is not in sseStatusHeaders.
}
```

Registration site:

```go
registerSSE(sm.humaAPI, huma.Operation{
    OperationID: "stream-agent-output",
    ...
    Responses: sseResponseHeaders("GC-Agent-Status"),
}, ...)
```

The panic-on-unknown-name is load-bearing: it makes drift
between `hctx.SetHeader` call sites and the declared contract
surface at startup, not at "why isn't my client getting this
header" debug time.

## 7. Middleware ordering vs request validation

Middleware registered via `api.UseMiddleware` wraps the whole
request path; it runs BEFORE Huma's parameter validation. That's
why `humaCSRFMiddleware` can return `403 csrf: ...` when
`X-GC-Request` is missing — by the time Huma's validator sees
the request, the middleware has already short-circuited.

Without the middleware, Huma's own validator would return
`422 Unprocessable Entity` with `"required header parameter is
missing"` for the same case. Both reject, but 403 is the
semantically correct status for CSRF rejection and is more
scannable in logs.

**Implication.** A spec that declares a header `required:true`
must have a mechanism (middleware, handler code, Huma validator)
that actually enforces it. The spec describes the contract; it
does not enforce it. `TestGeneratedClientInSync` catches spec
drift but not enforcement drift.

## 8. Convenience path auto-metadata — `_convenience_id` sentinel

When `huma.Post` et al. run, they set `op.OperationID` to the
auto-generated kebab-case ID, then run user operation handlers,
then check:

```go
if operation.OperationID == opID {
    operation.Metadata["_convenience_id"] = opID
    operation.Metadata["_convenience_id_out"] = o
}
```

The metadata sentinel lets `huma.Group` regenerate the ID if the
operation gets moved under a prefix. If you want to override the
auto-ID, do it inside an operation handler:

```go
huma.Post(api, "/things", handler, func(op *huma.Operation) {
    op.OperationID = "create-thing"
})
```

The same trick applies to `Summary`.

## 9. SSE: three hand-written zones around typed payloads

`internal/api/sse.go` is the single sanctioned location for SSE
protocol framing. It hand-writes `id:` / `event:` / `data:` /
blank-line separators around a call to `encoder.Encode(payload)`
where payload is a typed, schema-registered struct. This is the
§3.4 carve-out described in the
[API Control Plane](../architecture/api-control-plane.md).

Three related hand-written helpers:

1. `beginSSEStream(hctx)` — sets `Content-Type: text/event-stream`,
   `Cache-Control: no-cache`, `Connection: keep-alive` via
   `hctx.SetHeader`, then returns the body writer, JSON encoder,
   and Flusher.
2. `writeSSEFrame(...)` — emits one frame: optional `id:` line,
   `event:` line from the type→event reverse lookup, `data:`
   line via `encoder.Encode(data)`, blank line, flush.
3. `attachSSEResponseSchema(...)` — populates
   `op.Responses["200"].Content["text/event-stream"]` with a
   `oneOf` schema listing every event variant. Called before
   `huma.Register` so the spec describes the stream's event
   shapes.

Third-party SSE adapters (e.g. Huma's built-in `sse.Register`)
do not support precheck errors; an op like `stream-events` that
needs to 503 before committing stream headers has to use our
`registerSSE` wrapper instead.

## 10. When to reach for `CreateHooks = nil`

Huma's default `Config` installs a `SchemaLinkTransformer` that
adds `$schema` properties and `Link` response headers. Gas City
disables this with `cfg.CreateHooks = nil` in
`huma_handlers_supervisor.go:newSupervisorHumaAPI`. Reason: we
don't serve the schema at the Link target, and the extra header
clutters generated clients.

If you need other CreateHooks, don't nil the slice — append your
hook to the default list.

## 11. Adapter import path

`humago` lives at
`github.com/danielgtaylor/huma/v2/adapters/humago`, not at
`github.com/danielgtaylor/huma/v2/humago`. The README examples in
some Huma versions are out of date on this; `pkg.go.dev` has the
real path.

```go
import "github.com/danielgtaylor/huma/v2/adapters/humago"
```

## What we don't use from Huma

- **`huma.Group`** — we have a single API per supervisor and a
  per-city handler dispatcher. Group's prefix/middleware
  composition would duplicate what `cityRegister` already does.
- **Huma's built-in `sse.Register`** — cannot return HTTP errors
  before stream headers commit; we use our own `registerSSE` for
  that reason (§9 above).
- **`huma.OperationTags`** convenience — we set `Tags` directly on
  the Operation literal where present.

## Upstream issues we're tracking

- **#288** — pointer query params panic. Blocks the cleanest form
  of optional parameters; `OptionalParam[T]` is the documented
  workaround and will remain the idiom here even after a fix.
- Response-header "global declaration" ergonomics — OpenAPI 3.1
  limitation, not a Huma gap. Our `registerFrameworkHeaders` is
  the `$ref` pattern applied programmatically.
