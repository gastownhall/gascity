# internal/api — change guide

The HTTP + SSE API is a projection over the domain packages
(`internal/{beads, mail, convoy, formula, events, session, worker, sling, ...}`);
it never re-implements domain logic.

## Read first

Read **`engdocs/architecture/api-control-plane.md`** and
**`engdocs/contributors/huma-usage.md`** before touching:

- `internal/api/` (HTTP + SSE API layer)
- `cmd/gc/` (CLI) — especially anything that constructs events,
  calls `apiroute.go:apiClient()`, or uses
  `internal/api/genclient`
- `internal/events/` (event bus, registry)
- `internal/extmsg/` (external-messaging emitters)
- Anything that affects `internal/api/openapi.json`,
  `docs/reference/schema/openapi.json`, or the generated TS types under
  `internal/api/dashboardspa/web/shared/src/generated/`

## Load-bearing invariants enforced by CI

Violating any fails the build; full rationale is in the architecture docs.

- **Object model at the center.** `internal/{beads, mail, convoy,
  formula, events, session, worker, sling, ...}` is the canonical
  domain. The CLI (`cmd/gc/`) and the HTTP+SSE API
  (`internal/api/`) are projections over it. Neither re-implements
  domain logic. `internal/agent/` is a small helper package
  (session-name utilities, startup hints) — not a primitive.
- **Typed wire.** No hand-written JSON on any HTTP or SSE wire
  path; no `map[string]any` or `json.RawMessage` on wire types
  (documented exceptions live in the API control-plane doc). All
  endpoints are Huma-registered; the OpenAPI spec is generated,
  never hand-written (`TestOpenAPISpecInSync`).
- **Typed events.** Every constant in `events.KnownEventTypes`
  must have a registered payload via
  `events.RegisterPayload(constant, sample)`. Use
  `events.NoPayload` for events whose envelope fields alone
  capture the semantics. Enforced by
  `TestEveryKnownEventTypeHasRegisteredPayload`.

## Worker boundary exception

`session_manager.go` constructs `session.Manager` values for API handlers —
a sanctioned bypass of the worker boundary. Do not add others; see
`internal/worker/AGENTS.md`.

## Dashboard and API-schema gates

- `make dashboard-check` passes for any change touching `internal/api/`,
  `internal/api/openapi.json`, `docs/reference/schema/openapi.*`,
  `internal/api/dashboardspa/`, or generated dashboard types. It runs the
  dashboard's Bazel targets (`//internal/api/dashboardspa/...`, which CI's
  `bazel test //...` gates on): typecheck, Vitest, the SPA build, and the
  diff tests that fail when the committed `dist/`, generated API client, or
  `pnpm-lock.yaml` is stale (`make dashboard-build`,
  `make dashboard-generate-client`, `make dashboard-lock` fix them)
- The dashboard starts locally and serves the app for dashboard/API-schema
  changes (a manual step, not a CI gate); use
  `npm run preview -- --host 127.0.0.1 --port <port>` from
  `internal/api/dashboardspa/web/frontend` after `make dashboard-build-npm`

## Fresh active-work lookup

Batch a session's assignee aliases into one fresh query per bead store. Preserve
assignee priority across stores and newest-first order within a store, so a
concrete session's work wins over a template fallback.
