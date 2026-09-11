# Migration & Implementation Plan: Fold the New React/Vite Dashboard into the `gc` Supervisor (Single Self-Contained Binary)

> **Worktree:** `/data/projects/gascity/.claude/worktrees/new-dashboard`
> **Source repo being absorbed:** `/data/projects/gascity-dashboard` (npm workspaces: `shared` / `backend` / `frontend`)
> **Chosen strategy:** **Hybrid-Thin-BFF** (unanimous across all three adversarial lenses; top self-score 7.8)

---

## 1. Executive Summary

The new dashboard is a React 18 + Vite 7 SPA fronted by a ~7,900-LOC Express BFF. The brief's framing ("the BFF shells `gc sling`/`gc exec`") is **wrong** per the code survey: the BFF shells only `git`, `gh`, `bd`, and `gc version`; **all rich GC work (sessions/beads/mail/formulas/agents reads+writes + SSE) is pure passthrough** to already-typed Huma `/v0/*` endpoints via a same-origin `/gc-supervisor` proxy. That is the load-bearing fact that makes this tractable.

**The plan:** Make the Go supervisor (`internal/api.SupervisorMux`, served by `cmd/gc/cmd_supervisor.go`) host the SPA **same-origin on its existing listener (default `127.0.0.1:8372`)**. The browser then calls `/v0/*` **directly** — deleting the entire `/gc-supervisor` transport proxy (290 LOC) and the BFF's read-only/allowlist gating, because the supervisor's typed Huma layer **already enforces** X-GC-Request CSRF, the `readOnly` auto-gate, Host-allowlist (421), and per-request audit. Only the **irreducible host-side gaps** (config projection, git log, builds/deploy log, run-diff, system/local-tool health, and 3 slow-status samplers) are ported into a small fork-owned Go `/api` plane registered as a non-Huma exception (the `serveCitySvcProxy` precedent). The **maintainer module is dropped** (its ~2,600 LOC embed hardcoded product judgment that violates AGENTS.md "keep judgment out of Go"). The entire Node BFF is deleted; **no Node at deploy time**.

**Why this strategy (critique consensus):** All three lenses — upstream-maintainer, security, it-just-works/DX — ranked Hybrid-Thin-BFF **#1**. It is the only option that reaches the literal one-binary end state while (a) confining new code to NEW fork-owned packages with only 3–4 surgical edits to upstream-owned files, (b) **shrinking** divergence by deleting the Node BFF + dropping the role-violating maintainer module + collapsing dual OpenAPI sources, and (c) preserving `TestOpenAPISpecInSync`/`TestGeneratedClientInSync` **by construction** (zero new Huma ops). The dominant traffic repoints with a single build-time flag and zero per-file edits.

**Verified seams (against live code in this worktree):**
- `internal/api/supervisor.go:213-233` — `WithAllowedOrigins`/`WithAllowedHosts`/`WithAnyHostAllowed` all rebuild `sm.server = &http.Server{Handler: sm.Handler()}`. New builders must follow this discipline and be called **before** `Serve`.
- `internal/api/supervisor.go:169,179` — `humaMux.HandleFunc("/v0/city/{cityName}/svc/", sm.serveCitySvcProxy)` is the documented non-Huma registration precedent.
- `internal/api/supervisor.go:21-34` — `CityInfo{Name,Path,Running,Status,Error,PhasesCompleted}` + `CityResolver.ListCities()/CityState(name)` already expose the per-city host `Path` the BFF needs.
- `cmd/gc/cmd_supervisor.go:1248` — `readOnly := nonLocal && !supCfg.Supervisor.AllowMutations`; `:1257` — `api.NewSupervisorMux(...)`; `:1277-1289` — `net.JoinHostPort(bind,port)` → `net.Listen("tcp", addr)` → `apiMux.Serve(apiLis)`. This is the wiring seam.
- `Makefile:700-746` — `dashboard-build`/`dashboard-check`/`dashboard-smoke`/`dashboard-ci`/`spec-ci` hardcode the old `cmd/gc/dashboard/web` layout; rewritten in lockstep.

---

## 2. Strategy Comparison

| Strategy | itJustWorks | maintainability | upstream | effort⁻¹ | Total | Decisive tradeoff |
|---|---|---|---|---|---|---|
| **Hybrid-Thin-BFF** ✅ | 9 | 8 | 9 | 5 | **7.8** | Reaches one-binary by **deleting** the maintainer module + Node BFF; dominant traffic repoints with one flag; zero new Huma ops. |
| Staged-Incremental | 9 | 8 | 9 | 3 | 7.0 | Never-broken UI + true single-binary end state, but pays for throwaway Node-sidecar + reverse-proxy scaffolding to reach where Hybrid lands directly. |
| Managed-Node-Sidecar | 6 | 5 | 9 | 8 | 6.6 | Fastest to green, zero porting — but **permanent Node at deploy**; structurally fails the single-binary target. |
| Full-Port-Single-Binary | 9 | 6 | 8 | 3 | 6.4 | Cleanest end state, but reimplements ~7,900 LOC incl. the maintainer judgment Go must not contain; 2× the work for Hybrid's destination. |

**Why not the rejected three:**

- **Full-Port-Single-Binary** — Same one-binary destination as Hybrid for ~2× the work. It **keeps** the maintainer module, forcing the AGENTS.md "keep judgment out of Go" violation head-on (classifier tiers, triage scoring bands, One Mark Rule, contributor trust tiers, topic dictionary). Largest fork-owned Go surface with no upstream counterpart → heaviest permanent divergence against an upstream that owns its own dashboard. Highest probability of silently mis-porting one of the 7 security controls.
- **Managed-Node-Sidecar** — The right *fallback* under a hard deadline (reuses the proven `internal/workspacesvc/proxy_process.go` pattern, reimplements zero domain logic). But it explicitly **does not** yield one self-contained binary: deploy still needs Node 22 + built `frontend/dist` + `backend/dist`, breaking the npm-free `go build` guarantee. Two processes in one cgroup under `KillMode=process` → orphaned Node children. Dual OpenAPI sources persist. A dead end if the single-binary target is later wanted.
- **Staged-Incremental** — Converges to the exact Hybrid posture but spends an 8-phase window in a dual-process / dual-CSRF / dual-enforcement state that is harder to reason about than either stable endpoint, and builds disposable Node-sidecar + reverse-proxy scaffolding purely to keep the UI green mid-migration. If it stalls, the fork freezes in the very Node-dependent state it set out to escape — having paid *more* than the sidecar option to get there.

---

## 3. Current-State vs Target-State Architecture

### Current state (two processes, two origins)

```
Browser ──cross-origin──▶ gc dashboard server  (cmd/gc/dashboard, :8080)
   │                         go:embed web/dist (thin vanilla-TS SPA)
   │                         injects <meta name="supervisor-url">
   └──cross-origin /v0/*──▶ gc supervisor       (internal/api.SupervisorMux, :8372)
                              typed Huma OpenAPI ONLY, no static, no proxy
```

Plus, *in the separate `gascity-dashboard` repo*, the NEW dashboard runs as a third stack: Node Express BFF (:8082 prod) serving the React SPA via `express.static`, proxying `/gc-supervisor` → supervisor, and exposing a per-city `/api/city/:cityName/*` plane.

### Target state (one process, one origin)

```
Browser ──same-origin──▶ gc supervisor  (internal/api.SupervisorMux, :8372)  ── single net.Listener ──┐
                            ├── /v0/*, /health, /openapi.json   → Huma (typed; CSRF + readOnly enforced)
                            ├── /v0/city/{name}/svc/            → serveCitySvcProxy (existing)
                            ├── /api/*  (host-side)             → dashboardbff (non-Huma exception, self-enforces readOnly+X-GC-Request)
                            └── /  + /assets/*  (SPA fallback)  → dashboardspa (go:embed, CSP headers)
```

- **No `/gc-supervisor` proxy.** The SPA's generated `@hey-api` client base flips from `/gc-supervisor` to same-origin `''` → it calls `/v0/city/{name}/...` directly. All 33 supervisor methods + 3 SSE streams become direct same-origin typed calls.
- **No Node at deploy.** `go build` of `gc` yields a working dashboard (committed `dist` embedded).

### Multi-city reconciliation

The supervisor's `*cityRegistry` (`cmd/gc/city_registry.go`, implementing `api.CityResolver`) is the **single source of truth** for which cities exist (`ListCities()` → `[]CityInfo{Name,Path,...}`, exposed at `GET /v0/cities`). The BFF's parallel `backend/src/city/registry.ts` is **deleted**. The Go `/api/city/:cityName/*` handlers resolve a city's untrusted host `Path` directly from `CityResolver.ListCities()` (validating `cityName` against `CITY_NAME_RE` **before** any `path.Join`). The frontend stays single-active-city (URL segment `/city/:name` → `cityBase.ts` singleton); city discovery comes from `GET /v0/cities` (already the case in `CityBootstrap.tsx`).

---

## 4. Code-Move Plan

### 4.1 New fork-owned Go packages (all NEW files — AGENTS.md preferred)

| New path | Contents |
|---|---|
| `internal/api/dashboardspa/handler.go` | `NewStaticHandler()` — SPA static serve + index fallback + reserved-prefix 404 + CSP/X-Frame/nosniff middleware (CSP `script-src` sha256 read from the embedded `index.html` at boot). |
| `internal/api/dashboardspa/embed.go` | `//go:embed all:dist` → `embed.FS` (Vite hashed assets under `dist/assets/`). |
| `internal/api/dashboardspa/dist/` | **Committed** built Vite bundle (go:embed target). |
| `internal/api/dashboardspa/web/` | Vendored SPA source: the `shared` + `frontend` workspaces only (the `backend` workspace is discarded). |
| `internal/api/dashboardbff/plane.go` | `New(deps)` http.Handler for `/api/*`; ONE shared middleware self-enforcing `readOnly` + `X-GC-Request` on every mutation. |
| `internal/api/dashboardbff/config.go` | `GET /api/city/{name}/config` → synthesize `DashboardRuntimeConfig` from `CityResolver` path + gc config/env. |
| `internal/api/dashboardbff/exec.go` | Hardened exec helper (ports `exec-core.ts`+`exec.ts` guarantees: argv enum-whitelist, clean env, output caps, MAX_CONCURRENT semaphore, cwd segment-boundary allowlist, OSC/CSI/C1/bidi terminal sanitizer for CVE-2021-42574). |
| `internal/api/dashboardbff/git.go` | `GET /api/git/commits` (enum-whitelisted `git log` views). |
| `internal/api/dashboardbff/builds.go` | `GET /api/builds` (`~/.dev-deploy-log` + `~/.dev-deploy-FAILED` parse). |
| `internal/api/dashboardbff/rundiff.go` | `POST /api/city/{name}/runs/{runId}/diff` (ports `runs/diff.ts` + `run-diff-policy.ts`; cwd prefix-allowlist preserved). |
| `internal/api/dashboardbff/health.go` | `GET /api/health/system` + `/api/health/local-tools` (dolt/bd/gc version probe). |
| `internal/api/dashboardbff/clientlog.go` | `POST /api/client-errors` (+ legacy `/__client-log`) → stderr/event sink. |
| `internal/api/dashboardbff/sampler.go` | Generic goroutine sampler: interval + last-good cache + start/stop lifecycle. |
| `internal/api/dashboardbff/supervisor_status.go` | 60s sampler caching `/status` (dodges 10-38s slow-status tail); serves `{available,sampledAt,status}`. |
| `internal/api/dashboardbff/dolt_trend.go` | 24h / 144-slot ring buffer over `store_health.size_bytes`. |
| `internal/api/dashboardbff/rig_store_health.go` | 5min sampler: per-rig `.beads` `os.Stat` + `dolt-server.port` TCP probe + `bd doctor --readonly` via exec helper. |
| `internal/api/dashboardbff/links/` | Port of the pure, stateless `shared/src/links/*` (relation-index, build-link-view, node-ref) — **only if a route consumes it** (defer otherwise). |

### 4.2 What moves from `gascity-dashboard` → here

- `gascity-dashboard/shared/src/**` + `gascity-dashboard/frontend/**` → `internal/api/dashboardspa/web/{shared,frontend}` (SPA source). Keep `vite.config.ts`, `tailwind.config.js`, `postcss.config.js`, `index.html`, `public/`.
- `gascity-dashboard/backend/src/**` → **NOT moved**; its host-side logic is reimplemented in Go (`dashboardbff`); its proxy + per-city registry + maintainer module are **dropped**.
- `gascity-dashboard/backend/openapi/gc-supervisor.openapi.json` + `scripts/update-gc-supervisor-openapi.mjs` → **dropped**; the SPA generates its client from the canonical `internal/api/openapi.json`.
- `gascity-dashboard/deploy/gas-city-dashboard.service` → **dropped** (no separate systemd unit).
- `gascity-dashboard/frontend/src/views/modules/maintainer/**` + `backend/src/views/modules/maintainer/**` → **dropped** (see Open Decision #1).

### 4.3 Deleted from `cmd/gc/dashboard` (LATE, to minimize upstream merge conflict)

`cmd/gc/dashboard/handler.go`, `serve.go`, `handler_test.go`, `testenv_import_test.go`, `web/` — all retired in the final phase. `cmd/gc/cmd_dashboard.go` (`gc dashboard` / `gc dashboard serve`) is aliased to print/redirect to the supervisor-hosted URL rather than launching the old cross-origin static server (see Open Decision #5).

> **Critical (upstream lens must-fix):** `cmd/gc/dashboard` is **upstream-owned** (last touched by upstream PR 877). Do NOT big-bang delete it. Keep it deletable late / behind an alias and **do NOT reuse `cmd/gc/dashboard/web/dist` for the new committed bundle** — upstream commits its own bundle there and it WILL collide on rebase. Use the distinct fork-owned `internal/api/dashboardspa/dist`.

### 4.4 Upstream-owned files touched — minimally and additively

| File | Edit | Footprint |
|---|---|---|
| `internal/api/supervisor.go` | ADD `WithStaticHandler(http.Handler)` + `WithAPIPlane(http.Handler)` builders (mirror `WithAllowedOrigins`, rebuild `sm.server`) + register on `humaMux` like `serveCitySvcProxy`. **No edits to `NewSupervisorMux`/`Handler`/`ServeHTTP` bodies** → rebases as pure insertions. | ~2 methods + 1-2 HandleFunc |
| `cmd/gc/cmd_supervisor.go` | After `:1257` `NewSupervisorMux(...)`, slot `apiMux.WithStaticHandler(...).WithAPIPlane(...)` into the existing `WithAllowedOrigins` block (the hottest file — keep to ONE line; push lifecycle into the package's `Mount`/`Start`). | ~5 lines |
| `internal/supervisor/config.go` | Optional `[supervisor]` (or `[dashboard]`) fields: `Dashboard bool` (default true), `OperatorAlias`, `OperatorWireAlias`, `DecisionLabel`, `EnabledModules`, `DefaultView`, `MaintainerGithubRepo` (if maintainer kept), `RunCwdAllowedRoots`. | new fields only |
| `cmd/gc/cmd_dashboard.go` | Alias `gc dashboard serve` to a redirect/hint. | thin edit |
| `Makefile`, `.github/workflows/ci.yml` | Retarget dashboard targets to the new web dir + embed path. | rewrite |

---

## 5. The Hosting Change (precise seam)

### 5.1 Supervisor mux: add static + `/api` on the same listener

In `internal/api/supervisor.go`, add (following `WithAllowedOrigins` at `:213`):

```go
// WithStaticHandler registers the embedded SPA as the root catch-all.
func (sm *SupervisorMux) WithStaticHandler(h http.Handler) *SupervisorMux {
    sm.humaMux.Handle("/", h)                       // Go 1.22 specificity: /v0/*, /health win
    sm.server = &http.Server{Handler: sm.Handler()} // same rebuild discipline as the With* builders
    return sm
}

// WithAPIPlane registers the non-Huma /api/* host-side plane.
func (sm *SupervisorMux) WithAPIPlane(h http.Handler) *SupervisorMux {
    sm.humaMux.Handle("/api/", h)                   // precedent: serveCitySvcProxy at supervisor.go:169
    sm.server = &http.Server{Handler: sm.Handler()}
    return sm
}
```

Both inherit the existing mux chain `withLogging → withRecovery → withRequestID → withHostAllowing → withCORSAllowing` (`Handler()` at `:202`). **They do NOT inherit Huma's `UseMiddleware` CSRF/readOnly** — the `/api` plane self-enforces (see §8).

Go 1.22 `ServeMux` specificity guarantees the registered `/v0/...`, `/health`, `/openapi.json`, `/debug/`, and `/v0/city/{cityName}/svc/` patterns win over `/`. The SPA handler's index fallback must additionally 404 those reserved prefixes (inverted from the old `reservedNonSPAPrefixes` — SPA is now the catch-all). Reserved set: `/v0/`, `/health`, `/openapi.json`, `/debug/`, `/svc/`, `/api/`.

### 5.2 cmd_supervisor.go wiring (one slot)

```go
apiMux := api.NewSupervisorMux(registry, cityInitSvc, readOnly, version, commit, startedAt) // :1257
if supCfg.Supervisor.Dashboard {                                                            // default true
    bff := dashboardbff.New(dashboardbff.Deps{Resolver: registry, ReadOnly: readOnly, /* config/env */})
    apiMux.WithStaticHandler(dashboardspa.NewStaticHandler()).WithAPIPlane(bff)
    // bff.Start(ctx) starts the 3 samplers; bff.Stop wired into the ctx.Done() shutdown block (LIFO defer)
}
// ... existing WithAllowedOrigins/WithAllowedHosts ...
```

`bff.Start/Stop` integrate with the supervisor's existing shutdown so sampler goroutines drain cleanly.

### 5.3 Runtime-config injection for the SPA

The SPA reads `DashboardRuntimeConfig` via **`GET /api/city/:cityName/config`** (a fetch, not a meta tag). The Go handler in `dashboardbff/config.go` synthesizes the **exact** shape `decodeRuntimeConfig` requires (`shared/src/snapshot/types.ts`): `cityName`, `cityRoot` (from `CityResolver` path), `useFixtures`, `readOnly` (from the mux flag), `operatorAlias`, `operatorWireAlias`, `decisionLabel`, `enabledModules`, `defaultView`, optional `maintainer`. Operator-identity fields come from gc config/env with **neutral defaults** (ZERO hardcoded roles). With maintainer dropped, `enabledModules` is core-only by default.

### 5.4 `gc dashboard serve` reconciliation

Aliased to print `now served by gc supervisor at http://127.0.0.1:8372` (or remove the cobra command). The separate cross-origin static server and `dashboardServeHook = dashboard.Serve` are retired. See Open Decision #5.

---

## 6. BFF Disposition (per-endpoint-class)

| BFF endpoint(s) | Class | Disposition |
|---|---|---|
| `/gc-supervisor/*` catch-all proxy (`supervisor-transport-proxy.ts`, 290 LOC) | pure-proxy-to-supervisor | **DELETED.** SPA calls `/v0/*` same-origin directly. |
| All sessions/beads/mail/formulas/agents reads+writes + `sling`/`nudge`/`prime`/`respond`/`bead-close` | direct supervisor (passthrough) | **Direct same-origin `/v0/*`** (typed Huma; CSRF+readOnly already enforced). Zero Go work. |
| SSE `/v0/city/{name}/events/stream`, `/session/{id}/stream` | direct supervisor SSE | **Direct same-origin** EventSource. Zero Go work. |
| `GET /api/city/:c/config` | aggregation (config projection) | **Go handler** `dashboardbff/config.go`. |
| `GET /api/git/commits` | host-exec (`git log`) | **Go handler** `dashboardbff/git.go` (enum views). |
| `GET /api/builds` | host-fileread (deploy log) | **Go handler** `dashboardbff/builds.go`. |
| `POST /api/city/:c/runs/:runId/diff` | host-exec (run-diff git) | **Go handler** `dashboardbff/rundiff.go` (cwd allowlist + policy). |
| `GET /api/health/system`, `/api/health/local-tools` | infra-telemetry + host-exec | **Go handlers** `dashboardbff/health.go`. |
| `POST /api/client-errors`, `/__client-log` | infra-telemetry sink | **Go handler** `dashboardbff/clientlog.go`. |
| `GET /api/csrf` | infra (token) | **Dropped/repointed** — standardize on `X-GC-Request`; SPA mutations carry it. |
| `GET /api/city/:c/supervisor-status` | aggregation (slow-status workaround) | **Go sampler** `supervisor_status.go`. |
| `GET /api/city/:c/dolt-noms/trend` | aggregation (ring buffer) | **Go sampler** `dolt_trend.go`. |
| `GET /api/city/:c/rig-store-health` | host-exec + fileread + socket | **Go sampler** `rig_store_health.go` (see Open Decision #6). |
| `GET/POST /api/city/:c/maintainer/*` (triage/refresh/events SSE/sling-record/contributor) | host-exec (gh) + fileread + SSE + judgment | **DROPPED** (Open Decision #1). |
| Links / relation-index (`shared/src/links/*`) | pure data-shaping | **Port only if a route needs it** (deferred). |

### Frontend data-layer changes required

- `internal/api/dashboardspa/web/frontend/src/supervisor/url.ts` — set `SUPERVISOR_PROXY_BASE_URL = '/'` (or build-time `VITE_GC_SUPERVISOR_URL=''`) so the generated client hits `/v0/*` on `location.origin`. **One constant; the 22 direct-plane files need zero edits.**
- `vite.config.ts` — dev proxy: replace `/gc-supervisor` with `/v0` + `/health` → `127.0.0.1:8372` so dev stays same-origin.
- `src/CityBootstrap.tsx` — city discovery already uses `supervisorApi().listCities()` (`GET /v0/cities`); no change beyond the base flip.
- `src/api/client.ts` — `/api/*` paths unchanged; now Go-served. CSRF header read from cookie standardizes to whatever the Go `/api` plane self-enforces (`X-GC-Request`).
- `src/App.tsx` — drop the maintainer route; `enabledModules` core-only.
- OpenAPI client generator input → `../../../internal/api/openapi.json` (collapses dual source).

---

## 7. Build + Embed + CI Integration

### 7.1 Build & embed

1. Build order: `build:shared` (tsc) → `build:frontend` (`tsc && vite build`) inside `internal/api/dashboardspa/web`.
2. Vite output (`dist/`, hashed assets under `dist/assets/`, `sourcemap:false`, `emptyOutDir:true`) is copied to `internal/api/dashboardspa/dist/` and **committed**.
3. `internal/api/dashboardspa/embed.go`: `//go:embed all:dist` (the `all:` prefix captures dotfiles + nested asset dirs — precedent: `internal/bootstrap/packs/core/embed.go`). `NewStaticHandler` does `fs.Sub(embedFS, "dist")` + `http.FileServer` + index fallback.
4. **Commit policy:** reverse the new dashboard's `dist` gitignore; commit `internal/api/dashboardspa/dist`. This preserves the npm-free `go build` guarantee for Node-less contributors (the property the current `.gitignore` comment explicitly protects).

### 7.2 OpenAPI typed-client generation

- The SPA generates its `@hey-api` client from the **canonical** `internal/api/openapi.json` (the same file `cmd/genspec` writes and `TestOpenAPISpecInSync` guards). Drop `backend/openapi/gc-supervisor.openapi.json` + `update-gc-supervisor-openapi.mjs`.
- **No new Huma operations** — the `/api` plane is the documented non-Huma exception (like `/svc/`). Therefore `internal/api/openapi.json` stays byte-identical, and `TestOpenAPISpecInSync` + `TestGeneratedClientInSync` are **untouched by construction**.
- Keep the `@hey-api` response validator **OFF** (incident r43k — browser must not reject evolved supervisor responses).

### 7.3 Makefile / CI rewrites (lockstep)

- Rewrite `dashboard-build` → `cd internal/api/dashboardspa/web && npm ci && npm run gen && npm run build && cp -rf frontend/dist ../dist`.
- Rewrite `dashboard-check` → `dashboard-build` + `npm run typecheck` + `go test ./internal/api/dashboardspa/... ./internal/api/dashboardbff/...`.
- Rewrite `dashboard-ci` → `dashboard-check` + `git diff --quiet -- internal/api/dashboardspa/dist` (staleness gate at the NEW path).
- `.github/workflows/ci.yml` dashboard job → point at the new web dir, Node 22, drop the `backend` workspace.
- `spec-ci` unchanged (still guards `internal/api/openapi.json` + Go genclient).

### 7.4 Node version

Standardize on **Node 22** (worktree `.node-version`/`.nvmrc` = `22`; new dashboard `engines >=22.13.0`). Add `.node-version`/`.nvmrc` to the vendored web dir if needed. Node is a **build-time-only** dependency now; absent at deploy.

---

## 8. Security Re-Implementation Matrix (same-origin)

| BFF control | Where today | Disposition under same-origin Go host |
|---|---|---|
| **1. Host allowlist (421)** | `security.ts::hostHeaderAllowlistFactory` | **Already in Go** — `middleware.go::withHostAllowing` emits the same 421 (`problemHostNotAllowed`). Static + `/api` routes sit behind it via `Handler()`. Reuse as-is. |
| **2. Origin check on writes (403)** | `security.ts::originCheck` | **Already in Go** — `withCORSAllowing` + `originAllowed`. Mostly moot same-origin. NOTE: Go CORS does not *hard-403* a bad-Origin write — the **X-GC-Request CSRF header is the actual write gate** (Control 4). |
| **3. CSP + frame/nosniff headers** | `security.ts::securityHeaders` | **NEW Go work** — small response-header middleware on `dashboardspa` routes. `script-src` sha256 of the inline theme-boot script must be **read from the embedded `index.html` at boot** (not hardcoded) so it tracks the bundle; `connect-src 'self'` suffices same-origin. |
| **4. CSRF** | `csrf.ts` cookie double-submit | **Standardize on `X-GC-Request`** (existing Go convention, `city_scope.go`). The `/api` plane self-enforces a non-empty `X-GC-Request` on mutations. **Do NOT** port the cookie double-submit (avoids two CSRF models on one origin). |
| **5. Read-only proxy + read-allowlist** | `supervisor-transport-proxy.ts` + `supervisor-read-allowlist.ts` | **Proxy DELETED** (unnecessary same-origin). Read-only posture **already in Go** — `humaReadOnlyMiddleware` (403). The 18-template default-deny read-allowlist + 405 method-gate are **ported onto `/v0` ONLY IF the binary is fronted** (Open Decision #2); for loopback-only, `humaReadOnlyMiddleware` suffices (documented). |
| **6. Shell-exec sandbox** | `exec-core.ts` + `exec.ts` | **NEW Go work** — `dashboardbff/exec.go`: `os/exec` (shell-free), clean env, enum-whitelisted argv, output caps + kill-on-overflow, `MAX_CONCURRENT` semaphore, cwd **segment-boundary** allowlist, OSC/CSI/C1/bidi terminal sanitizer (CVE-2021-42574). Go removes the shell-injection vector but NOT the argv-whitelist/cwd-allowlist/cap/sanitizer needs. |
| **7. Audit + bind posture** | per-exec `dashboard.exec` row; bind 127.0.0.1 | HTTP audit **already in Go** (`recordSupervisorRequest`). **NEW:** a per-exec audit row (endpoint, parsed argv, exit_code, duration) for every subprocess. Bind-127.0.0.1 is the supervisor's `net.Listen` deployment concern. |

**The single biggest correctness trap (all lenses):** non-Huma `/api/*` routes registered on `humaMux` **bypass** Huma's `UseMiddleware` CSRF + readOnly. Mitigation: **ONE shared middleware** wrapping the entire `/api` plane that rejects mutations when `readOnly` and requires non-empty `X-GC-Request` — never per-handler checks that can be forgotten.

---

## 9. Phased Execution (TDD-first; ≤5 files/phase where feasible; each ends green)

> Green-gate after every phase: `go build ./cmd/gc/`, `go vet ./...`, `make test` (fast unit baseline), and the relevant new package tests. The dashboard build/embed gates apply from Phase 3 on.

### Phase 0 — Reconcile OpenAPI source + scaffold the embed package (no behavior change)
**Files:** `internal/api/dashboardspa/web/` (vendored `shared`+`frontend`), `internal/api/dashboardspa/web/openapi-ts.config.ts` (input → `internal/api/openapi.json`), `internal/api/dashboardspa/handler.go` (stub `NewStaticHandler` + `reservedNonSPAPrefixes`), `internal/api/dashboardspa/embed.go` (`//go:embed all:dist` over a placeholder).
**Test:** `handler_test.go` — placeholder index served, reserved prefixes 404. SPA `npm run gen` succeeds against the canonical spec.
**Acceptance:** package compiles; `go build ./cmd/gc/` works with empty placeholder dist; client generates from `internal/api/openapi.json`; dual OpenAPI snapshot deleted.

### Phase 1 — Repoint SPA to same-origin `/v0` + delete proxy dependency (frontend-only, zero Go)
**Files:** `internal/api/dashboardspa/web/frontend/src/supervisor/url.ts` (base `'/'`), `.../vite.config.ts` (dev proxy `/v0`+`/health`→8372), `.../src/hooks/useGcEvents.ts` (verify SSE URL `/v0/.../events/stream`), `.../src/hooks/useSessionStream.ts`.
**Test:** Vitest — generated client base resolves to `location.origin` + `/v0`; SSE URL builders emit same-origin paths.
**Acceptance:** all 22 direct-plane files now target `/v0/*` with zero per-file edits; `/gc-supervisor` no longer referenced.

### Phase 2 — Port the per-city `/config` projection into Go (SPA bootstrap dependency)
**Files:** `internal/api/dashboardbff/config.go`, `internal/api/dashboardbff/config_test.go`, `internal/api/dashboardbff/plane.go` (shared readOnly+X-GC-Request middleware; GET-only for config), `internal/supervisor/config.go` (operator-alias/decision-label/enabled-modules/default-view fields).
**Test:** `config_test.go` — synthesized `DashboardRuntimeConfig` passes the SPA's `decodeRuntimeConfig` shape; neutral operator defaults (ZERO hardcoded roles); `cityRoot` sourced from `CityResolver`.
**Acceptance:** `GET /api/city/{name}/config` returns the exact validator-accepted shape.

### Phase 3 — Wire static SPA + `/api` plane onto the supervisor mux (single binary takes shape)
**Files:** `internal/api/supervisor.go` (`WithStaticHandler`/`WithAPIPlane` + `humaMux` registration; rebuild `sm.server`), `cmd/gc/cmd_supervisor.go` (call builders after `:1257`; wire `bff.Start/Stop` into shutdown), `internal/api/supervisor_static_test.go`, `internal/api/dashboardspa/dist/` (first committed built bundle).
**Test:** `supervisor_static_test.go` — `GET /` serves SPA; `GET /v0/cities` still typed; `GET /api/city/x/config` served; reserved prefixes 404; mutation through `/v0` still requires `X-GC-Request`; bad-Origin/no-CSRF `POST /api/...` rejected when readOnly.
**Acceptance:** `gc supervisor run` serves SPA + `/v0` + `/api` on one origin; `TestOpenAPISpecInSync` still green.

### Phase 4 — Port host-side exec/file endpoints (git, builds, run-diff, local-tools, system-health)
**Files:** `internal/api/dashboardbff/exec.go` (sandbox + sanitizer + cwd allowlist), `git.go`, `builds.go`, `rundiff.go`, `health.go` (+ co-located `*_test.go`, spread across two phases if the ≤5-file rule binds).
**Test:** exec sandbox rejects non-whitelisted argv, caps output, sanitizes bidi/OSC; `rundiff` rejects cwd outside `RUN_CWD_ALLOWED_ROOTS` and traversal `cityName`; `git`/`builds` golden outputs; `local-tools` version probe.
**Acceptance:** every host-side route works behind the shared readOnly+CSRF middleware; per-exec audit row emitted.

### Phase 5 — Port the three slow-status samplers
**Files:** `internal/api/dashboardbff/sampler.go` (generic lifecycle), `supervisor_status.go`, `dolt_trend.go` (ring buffer), `rig_store_health.go`, `sampler_test.go`.
**Test:** sampler serves last-good snapshot with availability/freshness on upstream error (degrade-not-blank); ring buffer retains 144 slots / 24h; rig probe rolls up red/green; concurrency capped by the exec semaphore (no fork-storm).
**Acceptance:** `/api/city/:c/{supervisor-status,dolt-noms/trend,rig-store-health}` serve cached data; goroutines drain on supervisor shutdown.

### Phase 6 — Delete the Node BFF + retire old dashboard + finalize CI
**Files:** delete `cmd/gc/dashboard/{handler.go,serve.go,web/,...}`; `cmd/gc/cmd_dashboard.go` (alias `gc dashboard serve`); `Makefile` (retarget `dashboard-build/check/ci` to the new web dir; commit-dist gate at new path); `.github/workflows/ci.yml` (point dashboard job at new dir; drop backend workspace). Drop the maintainer route from `App.tsx` (Phase 0/1 if preferred). Delete the `gascity-dashboard` `backend/` + maintainer files from the vendored tree.
**Test:** full `make dashboard-check` green at new path; `make spec-ci` green; `go test ./cmd/gc/...` green; fresh-checkout `go build ./cmd/gc/` works npm-free.
**Acceptance:** no Node at deploy; one binary serves everything; all CI invariants green; old dashboard package removed.

---

## 10. Test Plan, Quality Gates, Rollback, Risks

### Test plan
- **New unit tests:** `dashboardspa/handler_test.go` (serve/fallback/reserved-404/CSP-header), `dashboardbff/config_test.go`, `exec_test.go` (whitelist/caps/sanitizer/cwd-allowlist), `rundiff_test.go` (traversal), `sampler_test.go` (degrade-not-blank/ring-buffer), `supervisor_static_test.go` (mux integration + CSRF/readOnly self-enforcement).
- **Frontend Vitest:** base-URL resolution, SSE URL builders, `decodeRuntimeConfig` round-trip.
- **Existing invariants that MUST stay green:** `TestOpenAPISpecInSync`, `TestGeneratedClientInSync`, `TestEveryKnownEventTypeHasRegisteredPayload`, `TestGCNonTestFilesStayOnWorkerBoundary`, plus `make test` / `make test-fast-parallel`, `go vet ./...`, `make spec-ci`, `make dashboard-check` (rewritten).

### Quality gates (per AGENTS.md, before any task is "done")
Fast unit baseline + sharded process/integration targets + `go vet` clean + active `.githooks/pre-commit` + `make dashboard-check` for any `internal/api/`/dashboard change + SPA serves locally via `npm run preview --host 127.0.0.1` + every exported func has a doc comment.

### Rollback
- The `[supervisor] dashboard` toggle (default true) and a `--no-dashboard` flag disable the static + `/api` surface, reverting the supervisor to typed-API-only with one config flip — no rebuild.
- Each phase is independently revertable; the old `cmd/gc/dashboard` stays present until Phase 6, so `gc dashboard serve` remains a working escape hatch until the very end.
- Because no Huma ops are added, the OpenAPI contract never changes — a dashboard rollback never affects the typed API.

### Risks & mitigations
| Risk | Mitigation |
|---|---|
| Non-Huma `/api` bypasses Huma CSRF/readOnly | ONE shared self-enforcing middleware (readOnly + `X-GC-Request`) wrapping the whole plane; `supervisor_static_test.go` asserts rejection. |
| Dropping maintainer = feature regression | Explicit product decision (Open Decision #1) before Phase 0; re-home as non-SDK service if required. |
| Committed hashed dist churns the diff / breaks npm-free build | Single committed dist at the NEW path + `dashboard-ci` staleness gate; never reuse upstream `cmd/gc/dashboard/web/dist`. |
| CSP `script-src` hash drift → white screen | Read sha256 from embedded `index.html` at boot (not hardcoded). |
| Samplers move host-FS/subprocess/socket into supervisor | exec semaphore cap + cwd segment-boundary allowlist + untrusted-path validation; or defer to upstream `/status` perf fix (Open Decision #6). |
| Traversal via untrusted city path | `CITY_NAME_RE` validate BEFORE `path.Join`; gate on cleaned/resolved path, never raw `req.URL.Path`. |
| Lifecycle coupling (supervisor crash kills dashboard shell) | Explicitly waive the outlive-supervisor design intent (Open Decision #3); document the URL move. |
| Exposed-bind read-surface widening | Port the 18-template read-allowlist + 405 gate onto `/v0` only if fronted (Open Decision #2). |

---

## 11. Resolved Decisions (settled — this is now the build contract)

All eight forks are resolved. Decisions 1, 2, 6, 8 were confirmed by the operator;
decisions 3, 4, 5, 7 were defaulted by the architect (sensible-default, no objection raised).

1. **Maintainer module → DROP.** ✅ confirmed. Remove `frontend/src/views/modules/maintainer/**` and the entire `backend/src/views/modules/maintainer/**`; `enabledModules` is core-only by default; drop the maintainer route from `App.tsx`. Accepted as a shipped-feature regression. No maintainer config fields are added to `internal/supervisor/config.go`. (This also removes the only surviving `gc sling`-shelling exec path, shrinking the host-exec surface to `git`/`gh`/`bd`/`gc version`.)
2. **Exposure posture → LOOPBACK-ONLY.** ✅ confirmed. Bind `127.0.0.1`. The existing `humaReadOnlyMiddleware` suffices; the 18-template default-deny read-allowlist + 405 method-gate are **intentionally omitted** and this omission is documented in the package doc. (If a future change fronts the binary, re-open this and add the hardening phase.)
3. **Lifecycle-coupling → WAIVED.** Defaulted. Folding the SPA into the supervisor process couples crash domains (a supervisor crash takes the dashboard shell down). This is inherent to the explicit "supervisor hosts the website" goal and is accepted; the "dashboard outlives supervisor" design intent from the old `deploy/README.md` is waived. Document the URL move in the new package README.
4. **Dashboard port/URL → existing `:8372`.** Defaulted. Serve the SPA + `/api` on the supervisor's existing listener. Combined with loopback-only (decision 2), the `readOnly := nonLocal && !AllowMutations` auto-gate stays in its mutations-allowed state, so the SPA can mutate.
5. **`gc dashboard serve` → alias/redirect, delete LATE.** Defaulted. Keep `cmd/gc/dashboard` present until Phase 6; alias `gc dashboard serve` to print/redirect to `http://127.0.0.1:8372`. Do **not** big-bang delete (upstream-owned, PR 877) and do **not** reuse `cmd/gc/dashboard/web/dist` for the new bundle.
6. **Host-side samplers → INCLUDE.** ✅ confirmed. Port all three (`rig-store-health`, `supervisor-status` cache, dolt 24h trend) in-process, gated by the exec semaphore + cwd segment-boundary allowlist + untrusted-path validation. Preserves the slow-status workaround and the dolt trend ring buffer.
7. **Default-city / operator identity → neutral config/env, pure multi-city.** Defaulted. Operator identity (`operatorAlias`/`operatorWireAlias`/`decisionLabel`/`enabledModules`/`defaultView`) comes from gc config/env with neutral defaults (ZERO hardcoded roles). No hardcoded default city — the UI is pure multi-city off `GET /v0/cities`; resolve the BFF's `racoon-city` vs `gas-city` disagreement by having neither (no baked default).
8. **Multi-city UI → single-active-city-per-URL is sufficient.** ✅ confirmed. The existing `/city/:name` model + `GET /v0/cities` satisfies "select multiple cities" (switch which city is in view). Simultaneous multi-city aggregation is **out of scope** for this migration; file it as a separate follow-on if wanted later.
