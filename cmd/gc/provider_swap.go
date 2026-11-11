package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// reloadOpenCityStore opens a standalone city's bead store on reload; a test
// seam.
var reloadOpenCityStore = openCityStoreAt

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
		case sessionauto.DefaultBackendLabel:
			legs.base = b.Provider
		case sessionauto.ACPBackendLabel:
			legs.acp = b.Provider
		}
	}
	return legs
}

// carriedSessionLegs returns the legs of the running provider that a provider
// swap carries into the new one (CONTRACT P7): the base when its selection
// name, pack runtime declaration and socket/endpoint configuration are
// unchanged, and the ACP leg when its configuration is. A carried leg is the
// same instance, so every runtime it serves stays reachable.
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
	backends := []runtime.Backend{{Label: sessionauto.DefaultBackendLabel, Provider: legs.base}}
	if legs.acp != nil {
		backends = append(backends, runtime.Backend{Label: sessionauto.ACPBackendLabel, Provider: legs.acp})
	}
	listings := runtime.ListBackends(backends, "")
	for _, l := range listings {
		if l.Err != nil {
			return nil, l.Err
		}
	}
	return listings, nil
}

// sessionRouteFor returns the leg of sp that serves name. A bare provider
// serves every name.
func sessionRouteFor(sp runtime.Provider, name string) runtime.Route {
	if router, ok := sp.(*sessionauto.Provider); ok {
		return router.RouteFor(name)
	}
	return runtime.Route{Backend: runtime.Backend{Provider: sp}, Known: true}
}

// swapStop is one runtime a provider swap stops, on the leg that listed it.
type swapStop struct {
	name    string
	backend runtime.Provider
}

// providerSwapStops returns exactly the listed runtimes the new provider
// cannot reach (CONTRACT P7): those whose name it routes to a backend other
// than the one serving them now. A name it cannot route is an error: whether
// its runtime stays reachable is unknown, so the swap must not go ahead.
func providerSwapStops(listings []runtime.BackendListing, newSP runtime.Provider) ([]swapStop, error) {
	var stops []swapStop
	for _, l := range listings {
		for _, name := range l.Names {
			route := sessionRouteFor(newSP, name)
			if !route.Known {
				return nil, fmt.Errorf("the new provider's route for %q is unknown", name)
			}
			if route.Provider != l.Provider {
				stops = append(stops, swapStop{name: name, backend: l.Provider})
			}
		}
	}
	return stops, nil
}

// stopProviderSwapRuntimes stops each runtime with its own leg's Stop under the
// provider-swap exception (CONTRACT F2), one target per (leg, name), and
// records legacy's session.stopped event for each. It writes no session row:
// the next complete pass reads them gone and the ordinary arms (v2) or the
// reconciler's heal and wake (legacy) take the rows. Any failed or timed-out
// stop is an error, and the caller must not publish the new provider: the old
// one still reaches every runtime.
func stopProviderSwapRuntimes(stops []swapStop, cfg *config.City, store beads.Store, rec events.Recorder, stdout, stderr io.Writer) error {
	names := make([]string, len(stops))
	for i, s := range stops {
		names[i] = s.name
	}
	// Read-only: the rows' IDs and templates for the event payload.
	targets := stopTargetsForNames(names, cfg, store, stderr)
	results := executeTargetWave(targets, defaultMaxParallelStopsPerWave, stopPerTargetTimeoutDefault, func(target stopTarget) error {
		if err := stops[target.order].backend.Stop(target.name); err != nil && !runtime.IsSessionGone(err) {
			return err
		}
		return nil
	})
	var failed []string
	for _, r := range results {
		if r.err != nil {
			fmt.Fprintf(stderr, "provider swap: stopping %s: %s\n", r.target.name, formatLifecycleError(r.err)) //nolint:errcheck // best-effort stderr
			failed = append(failed, r.target.name)
			continue
		}
		fmt.Fprintf(stdout, "Stopped agent '%s'\n", r.target.name) //nolint:errcheck // best-effort stdout
		rec.Record(events.Event{
			Type: events.SessionStopped, Actor: "gc", Subject: r.target.subject,
			SessionID: r.target.lifecycleCorrelationID(),
			Payload:   api.SessionLifecyclePayloadJSON(r.target.lifecycleCorrelationID(), r.target.template, "stopped"),
		})
		telemetry.RecordAgentStop(context.Background(), r.target.name, firstNonEmptyGCString(r.target.agentName, r.target.template), "stopped", nil)
	}
	if len(failed) > 0 {
		return errors.New("stopping " + strings.Join(failed, ", ") + " failed")
	}
	return nil
}

// goStart runs f on a new goroutine that launched counts until f returns.
// The count rises right before the goroutine starts and falls in its first
// deferred call, so no panic can leave it raised. A nil tracker counts nothing.
func (t *asyncStartTracker) goStart(f func()) {
	if t == nil {
		go f()
		return
	}
	t.mu.Lock()
	t.launched++
	t.mu.Unlock()
	go func() {
		defer func() {
			t.mu.Lock()
			t.launched--
			t.mu.Unlock()
		}()
		f()
	}()
}

// waitLaunchedStarts waits up to timeout, or until ctx is done, for every
// launched async start goroutine to return, so a provider swap's listing
// cannot miss a runtime a legacy start is still creating. It refuses nothing:
// the swap runs on the controller loop goroutine, the only one that launches
// starts, so none can begin while it waits.
func (t *asyncStartTracker) waitLaunchedStarts(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		t.mu.Lock()
		n := t.launched
		t.mu.Unlock()
		if n == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%d async session start(s) still running after %s", n, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}
