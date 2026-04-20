# Dashboard SPA

This directory is the TypeScript source for the Gas City dashboard.
It replaces the hand-written JSON API proxy in `cmd/gc/dashboard/`.

The SPA talks directly to the supervisor's OpenAPI-typed endpoints
(`/v0/...`) using a client generated from `internal/api/openapi.json`.
The Go service exists only to serve the compiled static bundle.

## Dev workflow

Requires Node 20, 22, or 24+ and npm.

```bash
npm install          # one-time
npm run gen          # regenerate src/generated/schema.d.ts from the spec
npm run typecheck    # tsc --noEmit
npm run build        # Vite production build → dist/
npm run dev          # Vite dev server with HMR on :5173
```

`npm run test`, `npm run typecheck`, `npm run build`, and `npm run dev`
all regenerate `src/generated/` first so they work from a clean checkout.

`dist/` is git-ignored. It is built fresh by CI and by the pre-commit
hook (see `.githooks/pre-commit`). Run `npm run build` before handing
a branch to a reviewer if they don't have Node available — or let the
hook do it.

`src/generated/` is also git-ignored. Run `npm run gen` after any change
to `internal/api/openapi.json`, or rely on the lifecycle hooks baked into
the main scripts above. The pre-commit hook also regenerates these files
when the spec changes.

## Layout

```
web/
├── index.html                 shell; the Go static server injects <meta name="supervisor-url">
├── package.json
├── tsconfig.json
├── vite.config.ts
└── src/
    ├── main.ts                entry
    ├── api.ts                 openapi-fetch client
    ├── sse.ts                 EventSource wrappers
    ├── panels/                one module per UI panel
    │   ├── status.ts
    │   ├── crew.ts
    │   ├── mail.ts
    │   ├── issues.ts
    │   ├── ready.ts
    │   ├── convoys.ts
    │   ├── activity.ts
    │   └── options.ts
    ├── util/                  small helpers
    └── generated/             openapi-typescript output (git-ignored)
        └── schema.d.ts
```

## Principle

The SPA owns zero hand-written networking: every request to the
supervisor goes through `openapi-fetch` typed on the generated
schema. If you find yourself writing `fetch("/v0/...")` directly or
parsing a response body with `JSON.parse`, stop — the typed client
already covers it.

The only endpoints the SPA talks to are under `/v0/`. `/api/*` is
gone. Any `gc` command that used to run via the old `/api/run`
endpoint is reachable directly through a typed supervisor operation
from the relevant panel.
