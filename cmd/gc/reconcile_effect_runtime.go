package main

import (
	"context"
	"slices"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The effects' fresh runtime read (v5 O1, O2): presence and identity on the
// routed leaf alone, so they never mix backends; absence proven through
// every composite hop that falls through to its other backend (auto), so a
// stale route cannot fake it (mc-zndi7.24; PLUMBING). The effect
// transaction (effectTx A2) makes it its runtime read; PR B seals it as
// fresh.Runtime.

// runtimeClass is what one fresh read proves of a runtime name.
type runtimeClass uint8

const (
	rtUnsupported runtimeClass = iota // the leaf cannot read fresh with errors: proves nothing
	rtUnknown                         // a read failed, expired, or two reads disagree
	rtAbsent                          // nothing under the name, on the leaf and every fall-through hop
	rtCorpse                          // listed, no pane alive
	rtZombie                          // a pane alive, the agent dead
	rtAlive                           // a pane and the agent alive
)

// txRuntime is one fresh read: presence since the attempt began, then
// identity, then presence again (Again). Same reports that both presence
// reads found one session object, so the identity is that object's. Class is
// the latest read's.
type txRuntime struct {
	Class       runtimeClass
	Live, Again runtime.Liveness
	Identity    runtimeIdentity
	Same        bool
	leaf        runtime.Provider // the routed leaf read, for the fence legs
}

// Alive reports a runtime proven alive, its identity read on that object.
func (r *txRuntime) Alive() bool { return r.Class == rtAlive && r.Same }

// readRuntime reads name fresh on sp since since, with processNames (the
// row's template's, processNamesFor), the bracketing read at now(); or
// refuses with a cause when no backend resolves the name.
func readRuntime(ctx context.Context, sp runtime.Provider, processNames []string, name string, since time.Time, now func() time.Time) (*txRuntime, string) {
	leaf, _, known := runtime.ResolveBackend(sp, name)
	if sp == nil || !known || leaf == nil {
		return nil, causeRouteUnknown
	}
	if !freshReadable(leaf) {
		return &txRuntime{Class: rtUnsupported}, ""
	}
	read := func(sp runtime.Provider, since time.Time) (runtime.Liveness, bool) {
		l, status, err := runtime.ObserveLivenessBoundedSince(ctx, sp, name, processNames, since, fenceProbeTimeout)
		return l, status == runtime.ObservationComplete && err == nil
	}
	rt := &txRuntime{Class: rtUnknown, leaf: leaf}
	live, ok := read(leaf, since)
	switch rt.Live = live; {
	case !ok:
		return rt, ""
	case !live.Running:
		// Absent, or a corpse: the leaf's verdict stands only when every
		// fall-through hop finds nothing running elsewhere (and, for
		// absence, nothing at all), each backend that cannot read fresh
		// answering by an attested, error-free listing.
		for _, hop := range fallThroughHops(sp, name) {
			if l, ok := read(hop, since); !ok || l.Running || !live.Present() && l.Present() || !unlistedElsewhere(ctx, hop, name) {
				return rt, ""
			}
		}
		if !live.Present() {
			rt.Class = rtAbsent
			return rt, ""
		}
	}
	rt.Identity = readRuntimeIdentity(ctx, leaf, name)
	again, ok := read(leaf, now())
	if rt.Again = again; !ok {
		return rt, ""
	}
	rt.Same = again.Present() && again.ObjectID == live.ObjectID && again.ObjectCreated == live.ObjectCreated
	switch {
	case !again.Present():
	case again.Corpse:
		rt.Class = rtCorpse
	case !again.Alive:
		rt.Class = rtZombie
	default:
		rt.Class = rtAlive
	}
	return rt, ""
}

// processNamesFor is row's template's process names, by which a read tells a
// zombie (its agent dead) from a live runtime; none when unresolved.
func processNamesFor(w *World, row session.Info) []string {
	if res, ok := w.Templates.lookup(row); ok {
		return res.TP.Hints.ProcessNames
	}
	return nil
}

// freshReadable reports a leaf whose liveness read reports errors and is
// fresh: it implements the fresh read, or reads fresh by construction (acp,
// subprocess). Any other leaf (a bool-only one: herdr, k8s, exec) proves
// neither presence nor absence.
func freshReadable(leaf runtime.Provider) bool {
	if _, ok := leaf.(runtime.LivenessObserverWithError); !ok {
		return false
	}
	_, fresh := leaf.(runtime.FreshLivenessObserver)
	m, byConstruction := leaf.(runtime.FreshByConstruction)
	return fresh || byConstruction && m.LivenessReadsFresh()
}

// unlistedElsewhere reports whether every backend hop falls through to that
// cannot read fresh itself (a bool-only or caching herdr, k8s or exec
// default, or a hybrid, whose read is its leaves' cached one) proves name
// absent the way the inventory does: an attested, error-free listing
// without it, bounded as every effect read is. The routed backend is the
// leaf's, already read fresh; a backend that reads fresh answered the hop.
func unlistedElsewhere(ctx context.Context, hop runtime.Provider, name string) bool {
	b, ok := hop.(runtime.BackendsProvider)
	if !ok {
		return true
	}
	var routed string
	if r, ok := hop.(runtime.Router); ok {
		routed = r.RouteFor(name).Label
	}
	type listing struct {
		names []string
		err   error
	}
	for _, backend := range b.Backends() {
		if backend.Label == routed || freshReadable(backend.Provider) {
			continue
		}
		l, ok := boundedProbe(ctx, func() listing { n, err := backend.Provider.ListRunning(name); return listing{n, err} })
		if !ok || l.err != nil || !runtime.ListRunningAttested(backend.Provider) || slices.Contains(l.names, name) {
			return false
		}
	}
	return true
}

// fallThroughHops are the composite hops from sp to name's leaf whose fresh
// read falls through to their other backend on absence (auto). A hop
// without the fresh read routes by name alone (hybrid), and adds nothing.
func fallThroughHops(sp runtime.Provider, name string) []runtime.Provider {
	var hops []runtime.Provider
	for {
		router, ok := sp.(runtime.Router)
		if !ok {
			return hops
		}
		if _, fresh := sp.(runtime.FreshLivenessObserver); fresh {
			hops = append(hops, sp)
		}
		sp = router.RouteFor(name).Provider
	}
}
