package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
)

// sessionLegs are the two backends a session provider serves names from: the
// base built for the selection name and, when the city composes, the ACP leg
// (resolveSessionTransportProvider). A nil acp means no ACP leg.
type sessionLegs struct {
	base runtime.Provider
	acp  runtime.Provider
}

// sessionProviderLegs splits a session provider into its legs. Only the auto
// composition has an ACP leg; any other provider is a bare base, whatever
// backends it composes itself (hybrid's local and remote are one base).
func sessionProviderLegs(sp runtime.Provider) sessionLegs {
	autoSP, ok := sp.(*sessionauto.Provider)
	if !ok {
		return sessionLegs{base: sp}
	}
	var legs sessionLegs
	for _, b := range autoSP.Backends() {
		switch b.Label {
		case "default":
			legs.base = b.Provider
		case "acp":
			legs.acp = b.Provider
		}
	}
	return legs
}

// carriedSessionLegs returns the legs of the running provider that a provider
// swap carries into the new one (CONTRACT v5.7 P7): the base when its
// selection name, pack runtime declaration and socket/endpoint configuration
// are unchanged, and the ACP leg when its configuration is. A carried leg is
// the same instance, so every runtime it serves stays reachable.
func carriedSessionLegs(old sessionLegs, oldCfg, newCfg *config.City, oldName, newName string) sessionLegs {
	var carried sessionLegs
	if oldCfg == nil || newCfg == nil {
		return carried
	}
	if oldName == newName && !packRuntimeDeclarationChanged(oldCfg, newCfg, newName) &&
		oldCfg.Session.Socket == newCfg.Session.Socket && oldCfg.Session.RemoteMatch == newCfg.Session.RemoteMatch {
		carried.base = old.base
	}
	if acpProviderConfig(oldCfg.Session.ACP) == acpProviderConfig(newCfg.Session.ACP) {
		carried.acp = old.acp
	}
	return carried
}

// listSessionLegs lists the running names on each leg of sp. Any leg's error,
// partial or not, fails the listing: a swap needs a complete ListRunning.
func listSessionLegs(sp runtime.Provider) ([]runtime.BackendListing, error) {
	legs := sessionProviderLegs(sp)
	backends := []runtime.Backend{{Label: "default", Provider: legs.base}}
	if legs.acp != nil {
		backends = append(backends, runtime.Backend{Label: "acp", Provider: legs.acp})
	}
	listings := runtime.ListBackends(backends, "")
	for _, l := range listings {
		if l.Err != nil {
			return nil, l.Err
		}
	}
	return listings, nil
}

// sessionRouteFor returns the leg of sp that serves name.
func sessionRouteFor(sp runtime.Provider, name string) runtime.Provider {
	if router, ok := sp.(*sessionauto.Provider); ok {
		return router.RouteFor(name).Provider
	}
	return sp
}

// swapStop is one runtime a provider swap stops, on the leg that listed it.
type swapStop struct {
	name    string
	backend runtime.Provider
}

// providerSwapStops returns exactly the listed runtimes the new provider
// cannot reach (CONTRACT v5.7 P7): those whose name it routes to a backend
// other than the one serving them now.
func providerSwapStops(listings []runtime.BackendListing, newSP runtime.Provider) []swapStop {
	var stops []swapStop
	for _, l := range listings {
		for _, name := range l.Names {
			if sessionRouteFor(newSP, name) != l.Provider {
				stops = append(stops, swapStop{name: name, backend: l.Provider})
			}
		}
	}
	return stops
}

// stopProviderSwapRuntimes stops each runtime with its own backend's Stop
// under the provider-swap exception (CONTRACT v5.7 F2). It writes no session
// row: the next complete pass reads them gone and the ordinary arms (v2) or
// the reconciler's heal and wake (legacy) take the rows.
func stopProviderSwapRuntimes(stops []swapStop, stdout, stderr io.Writer) {
	backends := make(map[string][]runtime.Provider, len(stops))
	targets := make([]stopTarget, 0, len(stops))
	for i, s := range stops {
		if len(backends[s.name]) == 0 {
			targets = append(targets, stopTarget{name: s.name, order: i})
		}
		backends[s.name] = append(backends[s.name], s.backend)
	}
	results := executeTargetWave(targets, defaultMaxParallelStopsPerWave, stopPerTargetTimeoutDefault, func(target stopTarget) error {
		for _, backend := range backends[target.name] {
			if err := backend.Stop(target.name); err != nil && !runtime.IsSessionGone(err) {
				return err
			}
		}
		return nil
	})
	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(stderr, "provider swap: stopping %s: %s\n", r.target.name, formatLifecycleError(r.err)) //nolint:errcheck // best-effort stderr
			continue
		}
		fmt.Fprintf(stdout, "Stopped agent '%s'\n", r.target.name) //nolint:errcheck // best-effort stdout
	}
}
