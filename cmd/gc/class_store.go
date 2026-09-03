package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// This file is the controller/CLI-side seam of the per-class store refactor.
// It gives each coordination class a named accessor so a future per-class
// backend becomes a change here rather than at every call site. On a
// single-store city every class collapses to the same concrete store, so these
// are identity helpers today: each returns the exact wrapped+cached store the
// call site already uses, never a re-wrapped instance, so optional-capability
// type assertions (GraphApplyFor, HandlesFor, StorageCreateStore, Counter, ...)
// keep working.

// graphBeadStore returns the store that owns graph (workflow/v2) beads. It
// delegates to the exported GraphBeadStore() accessor so the api.State surface
// and the controller's own callers share one resolver. Identity to the work
// store at the default bd backend; returned as the strongly-typed
// beads.GraphStore so the graph class stays statically visible.
func (cs *controllerState) graphBeadStore() beads.GraphStore {
	return cs.GraphBeadStore()
}

// sessionsBeadStore returns the store that owns session and session-wait beads.
// It delegates to the exported SessionsBeadStore() accessor so the api.State
// surface and the controller's own callers share one resolver. Identity to the
// work store at the default bd backend; returned as the strongly-typed
// beads.SessionStore so the session class stays statically visible.
func (cs *controllerState) sessionsBeadStore() beads.SessionStore {
	return cs.SessionsBeadStore()
}

// mailBeadStore returns the store that owns mail (message) beads: the configured
// messaging class store when [beads.classes.messaging] relocates messaging, else
// the work store. Identity to the work store at the default bd backend; returned
// as the strongly-typed beads.MailStore so the messaging class stays statically
// visible.
func (cs *controllerState) mailBeadStore() beads.MailStore {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return beads.MailStore{Store: resolveMailMessagesStore(cs.storageRoutes, cs.cityBeadStore, cs.cfg, cs.cityPath, cs.eventProv)}
}

// nudgesBeadStore returns the store that owns nudge beads. It delegates to the
// exported NudgesBeadStore() accessor so the api.State surface and the
// controller's own callers share one resolver. Identity to the work store at the
// default bd backend; returned as the strongly-typed beads.NudgesStore so the
// nudges class stays statically visible.
func (cs *controllerState) nudgesBeadStore() beads.NudgesStore {
	return cs.NudgesBeadStore()
}

// ordersBeadStore returns the store that owns order-tracking bookkeeping beads
// for the given scope (rig name, or "" for the city). It delegates to the
// exported OrdersBeadStore() accessor so the api.State surface and the
// controller's own callers share one resolver. The scope is accepted so a future
// per-scope orders backend can route without a call-site change. Identity to the
// work store at the default bd backend; returned as the strongly-typed
// beads.OrdersStore so the orders class stays statically visible. This is the
// city-scope simple case; per-order scope (rig/pool-routed orders) resolves PER
// ORDER through resolveOrderStoreTarget (the federated dispatch/sweep paths in
// order_store.go / order_dispatch.go).
func (cs *controllerState) ordersBeadStore(_ string) beads.OrdersStore {
	return cs.OrdersBeadStore()
}

// cityWorkStore returns the city-level store for ordinary WORK-class beads that
// are not scoped to a named rig. Work is the default/residual coordination class
// (everything Classify does not route elsewhere), so this is the typed accessor
// for the work class — distinct from CityBeadStore(), which stays beads.Store as
// the federation/by-id/default root. Returned as the strongly-typed
// beads.WorkStore so the work class stays statically visible; the wrapper carries
// the exact same underlying store value CityBeadStore() returns today, so it is
// byte-identical. Pass the embedded .Store field to any generic beads.Store
// helper shared across classes.
func (cs *controllerState) cityWorkStore() beads.WorkStore {
	return beads.WorkStore{Store: cs.CityBeadStore()}
}

// workBeadStores returns all rig WORK-class stores keyed by rig name, including
// the HQ city store, as strongly-typed beads.WorkStore values. Each wrapper
// carries the exact same underlying store value BeadStores() returns today, so it
// is byte-identical; pass the embedded .Store field to any generic beads.Store
// helper shared across classes.
func (cs *controllerState) workBeadStores() map[string]beads.WorkStore {
	return toWorkStores(cs.BeadStores())
}

// graphBeadStore returns the runtime's graph (workflow/v2) bead store: the
// dedicated graph store when [beads.classes.graph] relocates graph, else the
// work store. Byte-identical to cityBeadStore() at the default bd backend.
// Returned as the strongly-typed beads.GraphStore so the graph class stays
// statically visible; the wrapper carries the same underlying store value.
func (cr *CityRuntime) graphBeadStore() beads.GraphStore {
	return beads.GraphStore{Store: resolveGraphStore(cr.storageRoutes, cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)}
}

// sessionsBeadStore returns the runtime's session/session-wait bead store: the
// configured session class store when [beads.classes.sessions] relocates
// sessions, else the work store. The recorder is passed for signature parity
// and is not what makes a write observable — the controller's emission comes
// from the CachingStore around its work ledger, and a relocated class store has
// no such layer on this side (class_store_emit.go covers the one-shot CLI's).
// Byte-identical to cityBeadStore() at the default bd backend.
// Returned as the strongly-typed beads.SessionStore so the session class stays
// statically visible; the wrapper carries the same underlying store value.
func (cr *CityRuntime) sessionsBeadStore() beads.SessionStore {
	return beads.SessionStore{Store: resolveSessionStore(cr.storageRoutes, cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)}
}

// mailBeadStore returns the runtime's mail (message) bead store: the configured
// messaging class store when [beads.classes.messaging] relocates messaging, else
// the work store. Byte-identical to cityBeadStore() at the default bd backend.
// Returned as the strongly-typed beads.MailStore so the messaging class stays
// statically visible; the wrapper carries the same underlying store value.
func (cr *CityRuntime) mailBeadStore() beads.MailStore {
	return beads.MailStore{Store: resolveMailMessagesStore(cr.storageRoutes, cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)}
}

// nudgesBeadStore returns the runtime's nudge bead store: the configured nudges
// class store when [beads.classes.nudges] relocates nudges, else the work store.
// Byte-identical to cityBeadStore() at the default bd backend. Returned as the
// strongly-typed beads.NudgesStore so the nudges class stays statically visible;
// the wrapper carries the same underlying store value.
func (cr *CityRuntime) nudgesBeadStore() beads.NudgesStore {
	return beads.NudgesStore{Store: resolveNudgesStore(cr.storageRoutes, cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)}
}

// ordersBeadStore returns the runtime's order-tracking bead store for the given
// scope: the configured orders class store when [beads.classes.orders] relocates
// orders, else the work store. The scope is accepted for forward compatibility.
// Byte-identical to cityBeadStore() at the default bd backend; returned as the
// strongly-typed beads.OrdersStore so the orders class stays statically visible;
// the wrapper carries the same underlying store value. This is the city-scope
// simple case; per-order scope resolution flows through resolveOrderStoreTarget
// in the federated dispatch/sweep paths.
func (cr *CityRuntime) ordersBeadStore(_ string) beads.OrdersStore {
	return beads.OrdersStore{Store: resolveOrderStore(cr.storageRoutes, cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)}
}

// relocatedOrdersStore returns the runtime's ORDERS-class binding store when
// [storage] relocates the orders class, and nil when it does not — the
// controller-side twin of relocatedOrdersClassStore (order_store.go), resolved
// through the routes this process opened at boot rather than the one-shot CLI
// funnel. nil is what keeps a federation on a single-store city byte-identical:
// there is no second store to add.
func (cr *CityRuntime) relocatedOrdersStore() beads.Store {
	return resolveOrderStore(cr.storageRoutes, nil, cr.cfg, cr.cityPath, cr.rec)
}

// cityWorkStore returns the runtime's city-level WORK-class bead store. Work is
// the default/residual coordination class; this is its typed accessor, distinct
// from the federation/by-id/default cityBeadStore() root. Returned as the
// strongly-typed beads.WorkStore carrying the same underlying store value
// cityBeadStore() returns today, so it is byte-identical; pass the embedded
// .Store field to any generic beads.Store helper shared across classes.
func (cr *CityRuntime) cityWorkStore() beads.WorkStore {
	return beads.WorkStore{Store: cr.cityBeadStore()}
}

// workBeadStores returns the runtime's per-rig WORK-class stores keyed by rig
// name as strongly-typed beads.WorkStore values. Each wrapper carries the exact
// same underlying store value rigBeadStores() returns today, so it is
// byte-identical; pass the embedded .Store field to any generic beads.Store
// helper shared across classes.
func (cr *CityRuntime) workBeadStores() map[string]beads.WorkStore {
	return toWorkStores(cr.rigBeadStores())
}

// toWorkStores wraps each store in a rig→store map as a strongly-typed
// beads.WorkStore, carrying the same underlying store value so the result is
// byte-identical to the input map.
func toWorkStores(stores map[string]beads.Store) map[string]beads.WorkStore {
	if stores == nil {
		return nil
	}
	out := make(map[string]beads.WorkStore, len(stores))
	for name, store := range stores {
		out[name] = beads.WorkStore{Store: store}
	}
	return out
}

// unwrapWorkStores unwraps a rig→work-store map back to a generic
// rig→beads.Store map for passing into helpers shared across coordination
// classes. Each value carries the same underlying store, so the result is
// byte-identical.
func unwrapWorkStores(stores map[string]beads.WorkStore) map[string]beads.Store {
	if stores == nil {
		return nil
	}
	out := make(map[string]beads.Store, len(stores))
	for name, store := range stores {
		out[name] = store.Store
	}
	return out
}

// createTarget returns the inner store that owns creates of the given
// coordination class for this policy-wrapped store. It is the create-side seam:
// the create chokepoint (Create / ApplyGraphPlan / the wisp-root lookup in
// policyForCreate) routes through it instead of reaching for the embedded store
// directly, so a future per-class split changes only this method. A
// beadPolicyStore wraps exactly one underlying store today, so every class
// collapses to that same embedded store and createTarget is identity — it
// returns the exact store the create chokepoint already used, preserving the
// StorageCreateStore / GraphApplyStore optional-capability assertions that the
// create path relies on.
func (s *beadPolicyStore) createTarget(_ coordclass.Class) beads.Store {
	return s.Store
}

// graphApplierFor returns the graph-apply capability that owns graph creates of
// the given coordination class for this graph-policy-wrapped store. It is the
// graph-create arm of the create-side seam: ApplyGraphPlan routes through it
// instead of reaching for the cached applier directly. A beadPolicyGraphStore
// wraps exactly one underlying applier today, so every class collapses to that
// cached instance — graphApplierFor returns the exact GraphApplyStore the apply
// path already used, preserving the StorageGraphApplyStore optional-capability
// assertion. A future per-class split derives the applier from
// createTarget(class) here.
func (s *beadPolicyGraphStore) graphApplierFor(_ coordclass.Class) beads.GraphApplyStore {
	return s.applier
}

// resolveClassStore returns the beads.Store backing a coordination class. It is
// the single dispatch point for per-class backend selection, and the one place
// a resolved storage plan reaches the running city.
//
// routes is the opened non-work binding this process resolved at boot
// (storage_boot.go). A nil routes value — every city that authors no [storage]
// section, and every one-shot CLI caller — takes the identity branch and
// returns the exact workStore value it was handed, so the optional-capability
// assertions this file's header names keep working on the value they already
// worked on.
//
// A class the routes do not relocate also returns workStore unchanged. That is
// how work stays on the work ledger in a split city: the routes carry only the
// classes the plan assigned to a non-work binding, so "not relocated" and "no
// routes at all" are the same instruction rather than two branches that have to
// agree.
//
// cfg, cityPath and rec stay in the signature for the per-scope work routing
// that resolves elsewhere; they are not read here. rec in particular does NOT
// make a relocated write observable, for any class: a class store is a bare
// bead engine with no emitting layer, and what a caller passes here changes
// nothing about that. Emission is decided where the ROUTES are built, once —
// the one-shot CLI funnel gives its stores an emit target
// (storageRoutes.withCLIEmission), and the controller's boot does not, because
// its own emitter already covers it. See class_store_emit.go.
func resolveClassStore(routes *storageRoutes, workStore beads.Store, cfg *config.City, cityPath, class string, rec events.Recorder) beads.Store {
	_ = cfg
	_ = cityPath
	_ = rec
	if store, relocated := routes.storeFor(coordclassFor(class)); relocated {
		return store
	}
	return workStore
}

// coordclassFor maps a config class name to its coordination class. An
// unrecognized name resolves to work, which is the residual class and the one
// the routes never relocate — so a name this build does not know cannot be
// silently routed at a binding.
func coordclassFor(class string) coordclass.Class {
	for _, candidate := range coordclass.Classes() {
		if candidate.String() == class {
			return candidate
		}
	}
	return coordclass.ClassWork
}

// resolveMailMessagesStore returns the message-persistence store for mail
// (messaging-class) beads. Identity today: the work store. When messaging
// relocates, this is the seam that diverges from session reads (which stay on
// the work store until sessions relocate); the divergence plugs in at
// resolveClassStore.
func resolveMailMessagesStore(routes *storageRoutes, workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(routes, workStore, cfg, cityPath, config.BeadClassMessaging, rec)
}

// resolveOrderStore returns the order-tracking store. Identity today: the work
// store. When orders relocate, the embedded order store plugs in at
// resolveClassStore; returned as a beads.Store so the dispatch path can use it
// both as the order-tracking seam and, when distinct from the work store, as an
// extra gate-read store.
func resolveOrderStore(routes *storageRoutes, workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(routes, workStore, cfg, cityPath, config.BeadClassOrders, rec)
}

// resolveNudgesStore returns the nudge-shadow store. Identity today: the work
// store. When nudges relocate, the class store plugs in at resolveClassStore;
// returned as a beads.Store, which satisfies the nudge-store seam for free, so
// only the leaf nudge-bead operations route here.
func resolveNudgesStore(routes *storageRoutes, workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(routes, workStore, cfg, cityPath, config.BeadClassNudges, rec)
}

// resolveSessionStore returns the session-lifecycle store. Identity today: the
// work store. Session-class beads are session lifecycle beads and durable
// session waits; only those bead ops route here. When sessions relocate, the
// class store plugs in at resolveClassStore.
func resolveSessionStore(routes *storageRoutes, workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(routes, workStore, cfg, cityPath, config.BeadClassSessions, rec)
}

// resolveGraphStore returns the beads.Store backing the GRAPH coordination
// class. Identity today: the work store. When graph relocates, the dedicated
// graph-store dispatch plugs in at resolveClassStore. rec is accepted for
// signature parity with the other resolve*Store helpers and ignored here, as it
// is for every class: see resolveClassStore.
func resolveGraphStore(routes *storageRoutes, workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
	return resolveClassStore(routes, workStore, cfg, cityPath, config.BeadClassGraph, rec)
}

// scopeIsCity reports whether a scope store the caller opened at storePath is
// the city's own store rather than a rig's.
//
// This is the predicate behind every graph-class routing decision, and it is
// deliberately one function. Graph bindings are city-keyed: resolveClassStore
// holds a single city-level store per class, so there is no per-rig binding to
// route to, and `gc storage migrate` copies only the city work store. Any
// coordination surface that answers "does this scope take the graph class?"
// must answer it the same way, or two sibling surfaces end up reading different
// databases for beads of one class.
func scopeIsCity(cityPath, storePath string) bool {
	return samePath(resolveStoreScopeRoot(cityPath, storePath), cityPath)
}

// scopeGraphStore routes a scope store to the city's graph-class binding when
// the scope IS the city, and returns the store it was handed otherwise.
//
// Control beads and convergence roots are both ClassGraph and both live under a
// city-keyed binding, so they share this one rule rather than each spelling it
// out. When the routes relocate nothing — every city with no [storage] section,
// and every rig scope — this returns the exact store value it was given, so
// optional-capability type assertions the callers make against it keep working.
func scopeGraphStore(cityPath, storePath string, cfg *config.City, scopeStore beads.Store) beads.Store {
	if !scopeIsCity(cityPath, storePath) {
		return scopeStore
	}
	return resolveGraphStore(cliStorageRoutes(cityPath), scopeStore, cfg, cityPath, nil)
}

// moleculeClassStore returns the store a compiled recipe's molecule must be
// materialized in: graphStore when the beads instantiating it produces are
// graph class, and the caller's own scope/work store otherwise.
//
// The question is the CLASSIFIER'S, not the compiler's. Routing on "did this
// formula use the v2 compiler" is wrong in both directions. A v1 formula that
// is root-only — `phase = "vapor"`, or no [[steps]] at all — compiles to a root
// carrying gc.kind=wisp (internal/formula/compile.go), and that is the first
// arm coordclass.Classify tests, so the bead is ClassGraph and belongs in the
// binding. A v1 POURED formula compiles to a molecule container whose every
// bead is ClassWork, and work stays on the work ledger even in a split city —
// relocating it hides the steps from every work-scope reader, `gc hook`
// included.
func moleculeClassStore(recipe *formula.Recipe, workStore, graphStore beads.Store) beads.Store {
	if recipeCoordClass(recipe) == coordclass.ClassGraph {
		return graphStore
	}
	return workStore
}

// cookOnClassRouted compiles a formula and instantiates it in the store the
// compiled recipe's class demands; molecule.Cook picks its store before compiling.
func cookOnClassRouted(ctx context.Context, workStore, graphStore beads.Store, formulaName string, searchPaths []string, opts molecule.Options) (*molecule.Result, error) {
	if opts.ParentID == "" {
		return nil, fmt.Errorf("cookOnClassRouted requires Options.ParentID")
	}
	compileVars := opts.Vars
	if compileVars == nil {
		compileVars = map[string]string{}
	}
	recipe, err := formula.CompileWithoutRuntimeVarValidation(ctx, formulaName, searchPaths, compileVars)
	if err != nil {
		return nil, fmt.Errorf("compiling formula %q: %w", formulaName, err)
	}
	if err := molecule.ValidateRecipeRuntimeVars(recipe, opts); err != nil {
		return nil, err
	}
	return molecule.Instantiate(ctx, moleculeClassStore(recipe, workStore, graphStore), recipe, opts)
}

// recipeCoordClass returns the coordination class of the beads that
// instantiating recipe produces.
//
// molecule.Instantiate materializes a recipe as one atomic plan, so the plan is
// classified wholesale exactly as coordclass.ClassifyGraphPlan does: a single
// graph-marked node makes the whole molecule graph class, which keeps its
// intra-plan edges inside one store. Steps a RootOnly recipe never creates are
// skipped, matching the instantiate loop's own `if recipe.RootOnly && i > 0`
// break.
func recipeCoordClass(recipe *formula.Recipe) coordclass.Class {
	if recipe == nil {
		return coordclass.ClassWork
	}
	for i, step := range recipe.Steps {
		if recipe.RootOnly && i > 0 {
			break
		}
		stepType := step.Type
		if stepType == "" {
			stepType = "task"
		}
		bead := beads.Bead{Type: stepType, Labels: step.Labels, Metadata: step.Metadata}
		if coordclass.Classify(bead) == coordclass.ClassGraph {
			return coordclass.ClassGraph
		}
	}
	return coordclass.ClassWork
}

// graphClassBinding returns the store these routes serve the graph class from,
// and whether they relocate it at all — the same question resolveClassStore asks
// to choose its branch, exposed for the callers that must BEHAVE differently
// rather than merely read elsewhere. A reader that answers by shelling `bd` in
// the work directory cannot follow a relocated class, so it has to know it must
// go in-process instead; resolveGraphStore alone cannot tell it, because a
// relocated store and an unrelocated one are both just a beads.Store.
func graphClassBinding(routes *storageRoutes) (beads.Store, bool) {
	return routes.storeFor(coordclassFor(config.BeadClassGraph))
}

// cityQueryTopology answers the two questions a generated work_query or
// pool-demand command has to be built against: the bd semantics the city is
// configured for, and whether its claimable work is spread across stores a
// single `bd ready` in the agent's work directory cannot reach.
//
// The second question is the RESOLVER'S, and it is answered as a projection of
// Plan(RoutedWork) — the same plan the demand surface reads. A generated query
// is the one consumer that cannot take a plan: its legs are bd subprocesses in a
// workspace, and a relocated class binding is not a bd workspace at all. What it
// can take is the plan's DECISION, which is exactly one bit: does the claimable
// set live anywhere a bd workspace cannot reach? A binding leg in the plan means
// yes, and the query is built around the federated reader.
//
// Projecting rather than re-asking is what stops the query from disagreeing with
// the reader it drives. It also keeps the cityQueryTopology lesson intact:
// storageSplitShapeOf reads [storage] alone and answers "no split" for a city
// whose section was DELETED after it had already served one. That city's graph
// beads are in a binding, its boot refuses, and Plan REFUSES over it — which is
// projected here as FederatedReady, so the query is the federated one and fails
// loud with the refusal that names the remedy, rather than a `bd ready` that
// reads the work ledger and reports "no work" forever.
//
// A nil cfg still resolves the routes: the topology constructor reads the city's
// own city.toml rather than the caller's snapshot, precisely because where a
// city serves its classes from is a property of the CITY.
func cityQueryTopology(cityPath string, cfg *config.City) config.QueryTopology {
	topo := config.QueryTopology{}
	if cfg != nil {
		topo.Beads = cfg.Beads
	}
	topo.FederatedReady = routedWorkNeedsFederatedReader(cityPath, cfg)
	return topo
}

// routedWorkNeedsFederatedReader reports whether this city's claimable set spans
// a store no bd workspace can reach.
//
// The work legs of Plan(RoutedWork) are bd workspaces — the city work store and
// the rigs — and the hook already fans out across them. The binding is not one,
// so its presence in the plan is the whole answer. A REFUSED city answers yes:
// the refusal is about a relocated class, and only the federated reader carries
// it to the operator instead of silently reading the wrong ledger.
//
// The topology is built with no work legs on purpose. This asks about the SHAPE
// of the city, not about a caller's opened stores, and every caller here — `gc
// hook`, `gc prime`, `gc agent list`, the dispatch runtime — holds a different
// set or none at all. Bindings do not depend on which work stores the caller
// opened, so the answer is the same for all of them.
func routedWorkNeedsFederatedReader(cityPath string, cfg *config.City) bool {
	topo := residencyTopologyForCity(cityPath, cfg, queryTopologyWorkProbe{}, nil)
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		return true
	}
	return plan.TouchesBinding()
}

// queryTopologyWorkProbe stands in for the work leg of a topology built to be
// PLANNED over and never read. Plan refuses a legless topology — a Union that
// reported zero rows as a complete answer is the silent-shrink shape — so the
// leg has to exist; nothing in this file executes the plan, so it never answers
// a call.
type queryTopologyWorkProbe struct{ beads.Store }

// newCityMailProvider builds the controller's mail provider as a two-store mail
// provider: message beads persist in the messaging-class store, and mail's
// session reads/writes for addressing/identity resolution go to the session-class
// store. Both resolve to the work store at the single-store bd backend, so this is
// byte-identical to newMailProvider(workStore) today and diverges only once
// [beads.classes.messaging] or [beads.classes.sessions] relocates a class.
func newCityMailProvider(routes *storageRoutes, workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) mail.Provider {
	msgStore := resolveMailMessagesStore(routes, workStore, cfg, cityPath, rec)
	sessStore := resolveSessionStore(routes, workStore, cfg, cityPath, rec)
	return newMailProviderWithSessionStore(msgStore, sessStore)
}

// newCityExtMsgServices builds the controller's external-messaging services as
// the split Messaging/Sessions form extmsg already exposes: bindings, groups,
// participants, delivery contexts, memberships and transcripts persist in the
// messaging-class store, while identity and liveness reads go to the
// session-class directory.
//
// extmsg records are messaging class — they hide under type=task carrying a
// gc:extmsg-* label, which is exactly why coordclass.Classify is a runtime
// function and not a type filter. Both classes resolve to the work store at the
// single-store bd backend, so this is byte-identical to the prior
// extmsg.NewServices(workStore) and diverges only once a class relocates.
//
// A nil session directory is the one and only thing extmsg refuses, and it
// cannot happen here: session.NewStore always returns a non-nil *session.Store,
// whatever store it was handed, so the directory this passes is never nil.
// TestCityExtMsgServicesCannotReachTheRefusalPath pins that, with a control
// that a genuinely nil directory IS refused so the pin cannot pass vacuously.
//
// The unreachable path therefore refuses rather than falling back to the work
// store. Falling back was the residency bug in miniature: on a city that
// relocated the messaging class it would persist bindings, groups and
// transcripts in the work ledger, where the class's own readers never look —
// external messaging would appear to work and deliver nothing, which is worse
// than not having it. Refusing surfaces the cause on every operation instead.
func newCityExtMsgServices(routes *storageRoutes, workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) *extmsg.Services {
	msgStore := resolveMailMessagesStore(routes, workStore, cfg, cityPath, rec)
	sessStore := resolveSessionStore(routes, workStore, cfg, cityPath, rec)
	svc, err := extmsg.NewServicesWithSessionDirectory(msgStore, session.NewStore(beads.SessionStore{Store: sessStore}))
	if err != nil {
		fmt.Fprintf(os.Stderr, "api: external messaging services: %v\n", err) //nolint:errcheck // best-effort stderr
		return refusedExtMsgServices(err)
	}
	return &svc
}

// refusedExtMsgServices builds external-messaging services whose every store
// operation reports why messaging could not be wired, so a caller that reaches
// them fails where it stands instead of reading and writing a store that does
// not serve the messaging class.
func refusedExtMsgServices(cause error) *extmsg.Services {
	refused := extmsg.NewServices(refusedClassStore{
		err: fmt.Errorf("external messaging is not available on this city: %w", cause),
	})
	return &refused
}

// warnFederationBlindOverrides tells an operator that this agent's own
// work_query or scale_check will not see the city's relocated coordination
// class.
//
// A custom query is returned verbatim, which is the contract and is not being
// changed here. What is being changed is the SILENCE around it: on a split city
// the generated query reads every store and the operator's does not, so the two
// disagree about what is claimable and the override's answer is a short array
// with nothing to distinguish it from an empty queue. That is the precise
// failure the federated reader exits non-zero to close, and an override walks
// straight back into it.
//
// Nothing is printed on a city that relocates nothing, which is every city with
// no [storage] section: FederationBlindOverrides returns nil there, so the
// diagnostic cannot become per-tick noise on a legacy deployment.
func warnFederationBlindOverrides(stderr io.Writer, a *config.Agent, topo config.QueryTopology) {
	if stderr == nil || a == nil {
		return
	}
	for _, key := range a.FederationBlindOverrides(topo) {
		fmt.Fprintf(stderr, "gc hook: agent %q sets a custom %s, which reads one store; this city serves a coordination class from a relocated binding, so that command cannot see graph-class work (the generated query uses %q)\n", //nolint:errcheck // best-effort stderr
			a.QualifiedName(), key, "gc ready")
	}
}
