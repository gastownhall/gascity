package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// acquireRoutingSessionStore returns the city's sessions-class store for a
// one-shot graph-routing root whose own stores are scope, opened at scopeRoot.
// Session rows live in the city's sessions store, never in a rig workflow
// store (#8436): scope's own sessions handle is that store when the sessions
// class is relocated or scope is the city scope; otherwise the city store is
// opened and release closes it. A city store that fails to open is an error,
// never a fallback to scope. A cityPath with no city on disk is a standalone
// scope that keeps its sessions in its own store.
func acquireRoutingSessionStore(cityPath, scopeRoot string, scope storeref.Stores) (beads.SessionStore, func() error, error) {
	release := func() error { return nil }
	if _, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassSessions); relocated || samePath(scopeRoot, cityPath) { // residency:allow — asks only whether the sessions class is relocated, to decide whether scope's handle already is the city's
		return scope.Sessions(), release, nil
	}
	city, err := openSlingCityStore(cityPath)
	if err != nil {
		return beads.SessionStore{}, release, fmt.Errorf("opening the city store for session routing: %w", err)
	}
	if city == nil {
		return scope.Sessions(), release, nil
	}
	return cliStores(cityPath, city).Sessions(), func() error { return closeBeadStoreHandle(city) }, nil
}
