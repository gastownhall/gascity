package main

// The controller's CachingStore over a split city's relocated class binding.
//
// A split city serves graph, sessions, messaging, orders and nudges from one
// binding engine that storage boot opens (openStorageRoutes). The work and rig
// ledgers have always sat under a CachingStore in the controller; the binding
// sat bare. So every class read on a split city was a live engine read, the
// controller's own writes to the binding appended no bead.* event, and the
// sessions leg had no census or watermark (CacheRevision, WriteRev) at all.
// withControllerCache gives the binding the same layer the work ledger has.
//
// # Layering: the cache sits directly on the engine, and it is the emitter
//
// The one-shot CLI emits through emittingClassStore (class_store_emit.go). The
// controller must not: its CachingStore already records every write-through
// mutation, and a second emitter on the same write is a double row. The cache
// is also the one layer that can see a bead engine's ready projection, which it
// finds by type-asserting its backing, so any wrapper between the two turns the
// projection off. Cache over engine, emitting through the cache's own onChange,
// satisfies both. The rows carry the cache-reconcile actor, the one the work
// ledger's cache stamps on the identical write on a city that relocates
// nothing, so a split city's events poke, enqueue and fold exactly as a
// single-store city's do.
//
// # Freshness
//
// A read that asks for live (HandlesFor(store).Live, ListQuery.Live,
// beads.ReadyLive) still reads the engine. The legacy demand reads that exist
// for cross-process freshness are all spelled that way, so they are unchanged.
// A cached read sees:
//   - this process's writes immediately (write-through);
//   - a one-shot CLI write when the bead-event watcher delivers the event it
//     emitted (applyBeadEventToStores routes every namespace the binding serves
//     here);
//   - a write that emitted nothing (a Tx-shaped CLI write, a failed best-effort
//     emission, a raw bd write to a workspace binding) at the cache's next
//     re-scan: at most the adaptive cadence (30/60/120s by active-row count)
//     plus one 5s poll.
//
// Those are the bounds a single-store city's cached class reads already live
// with, minus the gap bd's missing hooks leave there. A poked tick still loads
// its session snapshot live (loadTickSessionBeadSnapshot).
//
// One known limit: a relic — a bead `gc storage migrate` carried into the
// binding under its original work-ledger id — refreshes only at the re-scan.
// Its events route by prefix to the work store, and the work ledger keeps a
// frozen same-id copy of it, so applying those events to the binding as well
// would let the two caches trade the copy and the row back and forth forever.
// Relics are rarely written; the re-scan bound (above) is theirs.
//
// # Lifecycle
//
// Wrapped once, at controller-state construction and before any lane runs, so
// nothing has read the routes concurrently yet. The routes are immutable for
// the process (a [storage] change requires a restart), so a config reload does
// NOT rebuild this cache: its epoch is the process's, while the work and rig
// caches take a new epoch per reload. Priming and the reconcile loop are the
// work ledger's (wrapWithCachingStore). routes.close stops the loop before the
// engine closes.

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// withControllerCache replaces every distinct engine these routes serve with
// one CachingStore, shared by every class that engine serves, and returns the
// routes. Store identity is load-bearing, as it is for withCLIEmission: callers
// dedup legs by store, so a cache per class would turn one binding into five.
//
// Routes that already emit (the one-shot funnel's) are returned untouched: a
// cache over an emitter would emit twice and hide the engine's ready
// projection. A store that is already a cache is left as it is.
func (r *storageRoutes) withControllerCache(ctx context.Context, ep events.Provider) *storageRoutes {
	if r == nil || len(r.stores) == 0 || r.emitCityPath != "" {
		return r
	}
	cached := make(map[beads.Store]beads.Store, 1)
	for class, store := range r.stores {
		if store == nil {
			continue
		}
		if _, already := store.(*beads.CachingStore); already {
			continue
		}
		wrapped, ok := cached[store]
		if !ok {
			wrapped = wrapWithCachingStore(ctx, store, ep, true, beads.WithEventIDPrefixes(r.namespacesServedBy(store)...))
			if wrapped == nil {
				continue
			}
			cached[store] = wrapped
			if cache, isCache := wrapped.(*beads.CachingStore); isCache {
				r.caches = append(r.caches, cache)
			}
		}
		r.stores[class] = wrapped
	}
	return r
}

// isBindingCache reports whether cache is one the controller put over these
// routes' engines. The list is written once at controller-state construction,
// before any reader runs, so it is read without a lock.
func (r *storageRoutes) isBindingCache(cache *beads.CachingStore) bool {
	if r == nil {
		return false
	}
	for _, c := range r.caches {
		if c == cache {
			return true
		}
	}
	return false
}

// namespacesServedBy returns every reserved id prefix of every class routed to
// store: the namespaces whose rows the engine holds, and so the bead events its
// cache must apply.
func (r *storageRoutes) namespacesServedBy(store beads.Store) []string {
	var prefixes []string
	for class, routed := range r.stores {
		if routed == store {
			prefixes = append(prefixes, config.ReservedClassPrefixesFor(class.String())...) // residency:allow — names the namespaces whose events a binding's cache applies; resolves no bead
		}
	}
	return prefixes
}

// bindingEngine returns the bead engine under a class store's process layers:
// the controller's CachingStore and the one-shot CLI's emitter. Questions about
// the engine itself — which store the boot census read, whether it is the
// SQLite ledger — are asked of this, never of the wrapper a class accessor
// hands out.
// Each layer is peeled once; neither can wrap itself, so the walk ends at the
// first store that is neither.
func bindingEngine(store beads.Store) beads.Store {
	for {
		switch layer := store.(type) {
		case *beads.CachingStore:
			store = layer.Backing()
		case *emittingClassStore:
			store = layer.Store
		default:
			return store
		}
	}
}
