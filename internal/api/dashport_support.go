//go:build integration

// This file provides ServeSeededCity, a test-support composition seam used only
// by the //go:build integration dashboard e2e harness (test/dashport). The
// build tag keeps it out of the production binary and the normal internal/api
// surface; it is compiled only when the integration tag is set.
package api

import (
	"context"
	"net/http"
	"time"

	"github.com/gastownhall/gascity/internal/api/dashboardbff"
	"github.com/gastownhall/gascity/internal/api/dashboardspa"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/usage"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// SeededCityDeps carries the pre-seeded stores and providers a single city is
// served from by [ServeSeededCity]. Every field is supplied by the caller (an
// integration/e2e harness), so the served stack is a pure projection over the
// injected state — no Dolt spawn, no on-disk config load, no controller loop.
//
// The zero value is not usable: CityName, CityPath, Config, and CityBeadStore
// must be set. Rig stores, mail, and events are optional (a nil EventProvider
// disables the events feed; a nil mail provider disables the mail feed).
type SeededCityDeps struct {
	// CityName is the registered city name; it becomes the {cityName} path
	// segment on every /v0/city/{cityName}/... and /api/city/{cityName}/...
	// route.
	CityName string

	// CityPath is the city root directory on disk. The host-side run tailers
	// read the seeded event log from CityPath/.gc/events.jsonl, so a harness
	// that exercises the run views must write its events there.
	CityPath string

	// Config is the city config snapshot the stack projects agents, rigs, and
	// providers from. Must be non-nil.
	Config *config.City

	// CityBeadStore backs the city (HQ) scope: session beads, mail, and the
	// graph/formula topology the workflow and formula-feed endpoints read.
	CityBeadStore beads.Store

	// RigStores maps rig name to that rig's work-class bead store. It is
	// surfaced verbatim from BeadStore/BeadStores, exactly as the controller
	// exposes per-rig stores.
	RigStores map[string]beads.Store

	// MailProvider is the city-scoped mail provider. Nil leaves the /mail feed
	// empty rather than erroring.
	MailProvider mail.Provider

	// EventProvider is the city event provider the events feed and SSE stream
	// read from. Nil disables those endpoints (they report events off).
	EventProvider events.Provider

	// SessionProvider supplies session-lifecycle reads. Nil defaults to an
	// empty runtime.Fake so session-listing endpoints return an empty set
	// instead of panicking.
	SessionProvider runtime.Provider

	// Version is the reported GC binary version string. Empty defaults to
	// "seeded".
	Version string
}

// ServeSeededCity returns an http.Handler that serves the full supervisor stack
// — the typed /v0 API, the host-side /api plane, and the embedded dashboard SPA
// — for a single city backed by the seeded stores in deps. It reuses the exact
// production wiring the supervisor uses (singleStateResolver + NewSupervisorMux
// + dashboardbff.New + dashboardspa.NewStaticHandler + WithAPIPlane /
// WithStaticHandler), so a harness drives the real handlers, not a mock.
//
// The returned handler hosts the SPA and its same-origin /v0 and /api surfaces
// on one listener, so a harness (and the Playwright fake supervisor) can load
// "/" and let its relative fetches resolve to the same origin.
//
// The plane's per-city run tailers and status samplers are started against ctx;
// cancel ctx (e.g. via the harness's test context) to drain them. baseURL is the
// loopback origin the host-side status samplers dial to read this stack's own
// /v0 status; pass the httptest.Server URL once known, or "" to leave the
// status samplers dark (the run tailers, which read the event log off disk, do
// not need it).
//
// For integration and e2e harnesses only. It performs no access control beyond
// the production middleware and must not be exposed on an untrusted listener.
func ServeSeededCity(ctx context.Context, deps SeededCityDeps, baseURL string) (http.Handler, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	state := newSeededState(deps)

	mux := NewSupervisorMux(
		&singleStateResolver{state: state},
		nil, false, state.Version(), "", time.Now(),
	)
	// A harness dials an arbitrary httptest host; permit any Host so the
	// production allowlist does not 421 the seeded stack.
	mux.WithAnyHostAllowed()

	spa, err := dashboardspa.NewStaticHandler()
	if err != nil {
		return nil, err
	}

	plane := dashboardbff.New(dashboardbff.Deps{
		Resolver:          singleCityPathResolver{name: state.CityName(), path: state.CityPath()},
		SupervisorBaseURL: baseURL,
	})
	plane.Start(ctx)
	mux.WithAPIPlane(plane.Handler()).WithStaticHandler(spa)

	return mux.Handler(), nil
}

// singleCityPathResolver resolves exactly one city name to its seeded root path
// for the host-side /api plane, mirroring the supervisor's dashboardCityResolver
// without importing the cmd/gc registry.
type singleCityPathResolver struct {
	name string
	path string
}

func (r singleCityPathResolver) CityPath(name string) (string, bool) {
	if name == r.name {
		return r.path, true
	}
	return "", false
}

// seededState is a minimal, read-only State implementation built from injected
// stores and providers. It is the production analog of the _test.go-only
// fakeState: an immutable snapshot with no controller loop, no hot-reload lock,
// and no Dolt/subprocess wiring, so an external harness can serve a realistic
// city without the cmd/gc controllerState machinery. It implements only the read
// surface (State); mutation endpoints are inert because it does not implement
// StateMutator.
type seededState struct {
	cfg        *config.City
	sp         runtime.Provider
	rigStores  map[string]beads.Store
	cityStore  beads.Store
	mailProv   mail.Provider
	eventProv  events.Provider
	cityName   string
	cityPath   string
	version    string
	startedAt  time.Time
	adapterReg *extmsg.AdapterRegistry
	extmsgSvc  *extmsg.Services
}

func newSeededState(deps SeededCityDeps) *seededState {
	sp := deps.SessionProvider
	if sp == nil {
		sp = runtime.NewFake()
	}
	version := deps.Version
	if version == "" {
		version = "seeded"
	}
	s := &seededState{
		cfg:        deps.Config,
		sp:         sp,
		rigStores:  deps.RigStores,
		cityStore:  deps.CityBeadStore,
		mailProv:   deps.MailProvider,
		eventProv:  deps.EventProvider,
		cityName:   deps.CityName,
		cityPath:   deps.CityPath,
		version:    version,
		startedAt:  time.Now(),
		adapterReg: extmsg.NewAdapterRegistry(),
	}
	if s.rigStores == nil {
		s.rigStores = map[string]beads.Store{}
	}
	if s.cityStore != nil {
		svc := extmsg.NewServices(s.cityStore)
		s.extmsgSvc = &svc
	}
	return s
}

func (s *seededState) Config() *config.City              { return s.cfg }
func (s *seededState) SessionProvider() runtime.Provider { return s.sp }
func (s *seededState) BeadStore(rig string) beads.Store  { return s.rigStores[rig] }

// BeadStores returns the rig stores plus the city (HQ) store keyed by city name,
// so /beads federates the city root alongside every rig — the same shape
// controllerState.BeadStores exposes.
func (s *seededState) BeadStores() map[string]beads.Store {
	m := make(map[string]beads.Store, len(s.rigStores)+1)
	if s.cityStore != nil {
		m[s.cityName] = s.cityStore
	}
	for k, v := range s.rigStores {
		m[k] = v
	}
	return m
}

func (s *seededState) MailProvider(_ string) mail.Provider { return s.mailProv }

func (s *seededState) MailProviders() map[string]mail.Provider {
	if s.mailProv == nil {
		return map[string]mail.Provider{}
	}
	return map[string]mail.Provider{s.cityName: s.mailProv}
}

func (s *seededState) EventProvider() events.Provider { return s.eventProv }
func (s *seededState) UsageSink() usage.Sink          { return usage.Discard }
func (s *seededState) CityName() string               { return s.cityName }
func (s *seededState) CityPath() string               { return s.cityPath }
func (s *seededState) Version() string                { return s.version }
func (s *seededState) StartedAt() time.Time           { return s.startedAt }
func (s *seededState) IsQuarantined(string) bool      { return false }
func (s *seededState) ClearCrashHistory(string)       {}
func (s *seededState) CityBeadStore() beads.Store     { return s.cityStore }

// ScopedStoreLike returns (nil, nil): the seeded stores are in-memory, so there
// is no bd-CLI subprocess to scope. Callers keep reading through the existing
// store directly, matching the contract for non-bd-backed stores.
func (s *seededState) ScopedStoreLike(context.Context, beads.Store) (beads.Store, error) {
	return nil, nil
}

// NudgesBeadStore, SessionsBeadStore, and GraphBeadStore all collapse to the
// city store on a seeded single-store city, exactly as they do on a default
// (non-relocated) controller city.
func (s *seededState) NudgesBeadStore() beads.NudgesStore {
	return beads.NudgesStore{Store: s.cityStore}
}

func (s *seededState) SessionsBeadStore() beads.SessionStore {
	return beads.SessionStore{Store: s.cityStore}
}

func (s *seededState) GraphBeadStore() beads.GraphStore {
	return beads.GraphStore{Store: s.cityStore}
}

func (s *seededState) Orders() []orders.Order    { return nil }
func (s *seededState) OrdersAll() []orders.Order { return nil }
func (s *seededState) Poke()                     {}

func (s *seededState) ServiceRegistry() workspacesvc.Registry   { return nil }
func (s *seededState) ExtMsgServices() *extmsg.Services         { return s.extmsgSvc }
func (s *seededState) AdapterRegistry() *extmsg.AdapterRegistry { return s.adapterReg }
func (s *seededState) MaintenanceLoop() MaintenanceProvider     { return nil }

// RawConfig returns the same snapshot as Config: a seeded city has no separate
// raw (pre-expansion) config, so provenance reads see the expanded config.
func (s *seededState) RawConfig() *config.City { return s.cfg }

// Compile-time proof the seam satisfies the read surface the supervisor stack
// dispatches against.
var (
	_ State             = (*seededState)(nil)
	_ RawConfigProvider = (*seededState)(nil)
)
