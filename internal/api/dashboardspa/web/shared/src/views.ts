// Modular dashboard contract — Phase 1 of specs/requirements/modular-dashboard-prd.md.
//
// These descriptors are the single source of truth for "what is a view"
// and "what is a backend module" on BOTH sides of the wire. Living in
// `shared/` means the compile-time edit (registering a module) is the
// design-review checkpoint — premortem #6 mitigation.
//
// `shared/` declares no dependency on `react` or `express`, so the
// React-specific element type and the Express Router type are passed in
// via generic parameters from each side's wrapper. Frontend re-types
// `ViewDescriptor` with `LazyExoticComponent<ComponentType>`; backend
// re-types `BackendModule` with `express.Router`.

// ── Supervisor capability seam ────────────────────────────────────────────
//
// `shared/` cannot import the concrete GcClient from `backend/` — that
// would create a backend→shared→backend cycle and force every consumer of
// these types to drag the full HTTP-client surface into scope. `shared/`
// therefore publishes no supervisor method shape at all. Under the
// direct-supervisor migration, first-party modules must not use `ctx.gc` for
// GC-owned resources; browser code calls the generated supervisor client
// directly and modules keep only dashboard-local host capabilities.
//
// Modules wanting a wider surface declare it in their own `needs(config)`
// closure, or specialize `CityContext<TGc>` in backend-only code. The default
// `ctx.gc` type is `unknown` so accidental supervisor facade calls fail at
// compile time.

// ── View descriptor (frontend) ───────────────────────────────────────────

/** Nav entry. null = routable but hidden from Header. */
export interface ViewNavEntry {
  label: string;
  order: number;
}

/** A view in the frontend bundle. Generic over the element type so
 *  `shared/` need not depend on React; the frontend re-exports a narrowed
 *  alias `ViewDescriptor = ViewDescriptorOf<LazyExoticComponent<ComponentType>>`. */
export interface ViewDescriptor<TElement = unknown> {
  /** Stable id; matches MODULES_ENABLED entries; lowercase, hyphen-allowed. */
  id: string;
  /** core = always mounted, cannot appear in MODULES_ENABLED filter.
   *  firstParty = optional, in-tree, ships with core. */
  kind: 'core' | 'firstParty';
  /** React Router path. Unique. '/' allowed iff defaultRoute resolves here. */
  path: string;
  /** Nav entry. null = routable but hidden from Header. */
  nav: ViewNavEntry | null;
  /** Lazy-loaded route element. Frontend uses React.lazy; the generic
   *  keeps `shared/` React-free. */
  element: TElement;
  /** Module's declared candidacy for `/`. Operator may override via
   *  DEFAULT_VIEW env. Registry validates exactly-one resolution. */
  defaultRoute?: boolean;
}

// ── Backend module ───────────────────────────────────────────────────────

/** Background worker contract — bound by the host runtime. Shape matches
 *  the cleaned-up MaintainerRefresher signature (premortem #2: previous
 *  `DashboardRuntime` mismatch). */
export interface BackgroundWorker {
  start(): void;
  stop(): Promise<void>;
}

/** Per-resource lifetime declaration so Phase 2 multi-city verifies
 *  lifetime invariants statically (premortem #5). */
export interface ModuleResourceEntry {
  name: string;
  scope: 'perProcess' | 'perCity';
}

/** Resource posture a module declares at descriptor time. */
export interface ModuleResources {
  filesystem?: ReadonlyArray<ModuleResourceEntry>;
  network?: ReadonlyArray<ModuleResourceEntry>;
  memory?: ReadonlyArray<ModuleResourceEntry>;
}

/** Per-city-scoped runtime view passed to every BackendModule at mount
 *  time. Realized as a per-city registry in ucc: the backend keeps a lazy
 *  `Map<cityName, CityRuntime>` (each runtime owns one CityContext) keyed
 *  off `GET /v0/cities`, selected per request via the `/api/city/:cityName/`
 *  path segment. The per-module mount signature is unchanged from the
 *  single-city phase — a module still receives exactly one CityContext. */
export interface CityContext<TGc = unknown, TConfig = unknown> {
  cityName: string;
  cityPath: string;
  /** Per-city data directory. Modules MUST derive paths from this, not
   *  from config-dirname operations. Closes the leak surface premortem
   *  #5 found (per-city MaintainerRefresher writing the same global path).
   *
   *  OWNERSHIP: the host constructs the path but does NOT create the
   *  directory. Each module is responsible for `fs.mkdir(path.dirname(myFile),
   *  { recursive: true })` before writing its own sub-paths. The host's
   *  `cityName` is validated against `^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/i` at
   *  config-load time so this path segment can never escape the
   *  cities/ root via path.join's `..` normalization. */
  cityDataDir: string;
  /** Host-owned supervisor handle. Module contracts must not assume GC-owned methods. */
  gc: TGc;
  /** Read-only runtime config exposed to modules. Backend re-types this
   *  as `DashboardRuntimeConfig` via the local wrapper. */
  config: TConfig;
}

/** A backend module. Generic over Deps (closed at `bind<D>()` time so the
 *  iterator never sees them) and over TRouter (kept opaque so `shared/`
 *  need not depend on `express`). Backend re-types as
 *  `BackendModule<Deps, express.Router>`. */
export interface BackendModule<Deps = void, TRouter = unknown, TGc = unknown, TConfig = unknown> {
  /** Matches ViewDescriptor.id. */
  id: string;
  /** `core` modules cannot be omitted via MODULES_ENABLED. `firstParty`
   *  ships in-tree and is enable/disable-able. */
  kind: 'core' | 'firstParty';
  /** Resource lifetime posture. Required (premortem #5). */
  resources: ModuleResources;
  /** Owns its config slice. REQUIRED — modules with `Deps = void` return
   *  `undefined` explicitly: `needs: () => undefined`. Premortem #3 banned
   *  optional-`?` + `as never` cast as a type-erasure hazard. */
  needs: (config: TConfig) => Deps;
  /** Mounts under /api/<id>. */
  mount: (ctx: CityContext<TGc, TConfig>, deps: Deps) => TRouter;
  /** Optional background worker. start()/stop() called by host runtime. */
  workers?: (ctx: CityContext<TGc, TConfig>, deps: Deps) => BackgroundWorker | undefined;
}
