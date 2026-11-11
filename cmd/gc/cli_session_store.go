package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/session"
)

// cliSessionStore routes a generic CLI one-shot work store to the session
// coordination-class store, so a [beads.classes.sessions] relocation reaches
// one-shot commands the same way it reaches the running controller (which routes
// through resolveSessionStore via CityRuntime.sessionsBeadStore). At the default
// single-store backend it is the input store, wrapped for emission when that
// store is a bead engine (see below).
//
// The recorder is nil, and passing one would change nothing: resolveSessionStore
// ignores it. What makes a relocated one-shot write observable is the emit
// target the funnel puts on the ROUTES (class_store_emit.go), which
// cliStorageRoutes has already applied by the time this returns. A session
// class left in the city's own store gets the same emission here
// (withMainStoreEmission), so a `gc session new` reaches the controller's cache
// on a single-store city too.
func cliSessionStore(store beads.Store, cfg *config.City, cityPath string) beads.Store {
	return withMainStoreEmission(resolveSessionStore(cliStorageRoutes(cityPath), store, cfg, cityPath, nil), cityPath)
}

// cliSessionFrontDoor builds the typed session write front door over the
// session-class store for a CLI one-shot command. It is the relocation-safe
// replacement for sessionFrontDoor(store) at CLI command roots. The name
// deliberately does not contain the substring "sessionFrontDoor(" so the
// relocation guard (TestSessionRelocationRootsRouteThroughSessionClassStore) can
// forbid the unrouted form while allowing this one.
func cliSessionFrontDoor(store beads.Store, cfg *config.City, cityPath string) *session.Store {
	return sessionFrontDoor(cliSessionStore(store, cfg, cityPath))
}

// cliRelocatedSessionStore returns the store a one-shot command must resolve,
// materialize and fence sessions through when it holds some OTHER store: the
// city's session-class binding when the city relocates the sessions class, and
// store itself otherwise.
//
// It is for call sites handed a store by a caller that does not know which
// class it is for — a scope work store, a rig store — and that then resolve or
// create a session bead through it. On a split city every session row lives
// only in the binding, so resolving through the handed store finds nothing and
// materializing through it mints a duplicate session bead nothing reads
// (write-audit B5, B6). A city that relocates nothing gets the exact store value
// back, so its callers' capability assertions keep matching. A city whose
// routes refuse gets the refusing store, which fails the session operation
// closed rather than letting it land in a work store.
func cliRelocatedSessionStore(store beads.Store, cfg *config.City, cityPath string) beads.Store {
	if _, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassSessions); !relocated { // residency:allow — asks only whether the sessions class is relocated; the store returned is resolveSessionStore's
		return store
	}
	return cliSessionStore(store, cfg, cityPath)
}
