package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file holds the store door of `gc prime --hook`. A hook runs on every
// session start and prompt, so on a city whose store is stopped, stale or
// unreachable it must neither start that store nor wait on it: each store read
// the hook makes opens under the observation policy, and an unavailable store
// leaves the hook with the static prompt it can render without one.

// primeHookStoreState is the one-invocation memory of a hook's store opens: the
// writer that receives the unavailable notice, and the failure the first open
// found. The notice is written once; later opens of the same city return the
// recorded failure without asking the backend again, which for an unreachable
// store would repeat the wait.
var primeHookStoreState struct {
	mu       sync.Mutex
	stderr   io.Writer
	cityPath string
	failure  error
	// ctx bounds every read of the invocation's stores: the first read that
	// waits on an unreachable server spends the one deadline, and every later
	// read fails at once.
	ctx    context.Context
	cancel context.CancelFunc
}

// beginPrimeHookStores starts the invocation's store memory, reporting an
// unavailable store to stderr. The returned func ends it.
func beginPrimeHookStores(stderr io.Writer) (end func()) {
	ctx, cancel := observationContext()
	primeHookStoreState.mu.Lock()
	primeHookStoreState.stderr, primeHookStoreState.cityPath, primeHookStoreState.failure = stderr, "", nil
	primeHookStoreState.ctx, primeHookStoreState.cancel = ctx, cancel
	primeHookStoreState.mu.Unlock()
	return func() {
		primeHookStoreState.mu.Lock()
		primeHookStoreState.stderr, primeHookStoreState.cityPath, primeHookStoreState.failure = nil, "", nil
		primeHookStoreState.ctx, primeHookStoreState.cancel = nil, nil
		primeHookStoreState.mu.Unlock()
		cancel()
	}
}

// openPrimeHookStore opens cityPath's work store for a hook read under the
// observation policy. An unavailable store is the typed *storeUnavailableError,
// written once to the invocation's stderr.
func openPrimeHookStore(cityPath string) (beads.Store, error) {
	primeHookStoreState.mu.Lock()
	if primeHookStoreState.failure != nil && primeHookStoreState.cityPath == cityPath {
		err := primeHookStoreState.failure
		primeHookStoreState.mu.Unlock()
		return nil, err
	}
	ctx := primeHookStoreState.ctx
	primeHookStoreState.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	store, err := openObservationStoreAt(ctx, cityPath, cityPath, nil)
	if err == nil {
		return store, nil
	}
	recordPrimeHookUnavailable(cityPath, err)
	return nil, err
}

// notePrimeHookReadFailure reports a failed read of an open hook store when the
// failure is outage-shaped (an unreachable server or an expired deadline), in
// the same once-only notice an unavailable open produces. It is a no-op outside
// a hook invocation. scopeRoot names the store that was read.
func notePrimeHookReadFailure(cityPath, scopeRoot string, err error) {
	recordPrimeHookUnavailable(cityPath, observationReadFailure(err, observationScopeLabel(cityPath, scopeRoot, nil)))
}

// recordPrimeHookUnavailable remembers an unavailable-store failure and writes
// its notice to the invocation's stderr, once. Any other error is ignored.
func recordPrimeHookUnavailable(cityPath string, err error) {
	if _, ok := asStoreUnavailable(err); !ok {
		return
	}
	primeHookStoreState.mu.Lock()
	defer primeHookStoreState.mu.Unlock()
	if primeHookStoreState.failure != nil || primeHookStoreState.stderr == nil {
		return
	}
	primeHookStoreState.cityPath, primeHookStoreState.failure = cityPath, err
	fmt.Fprintf(primeHookStoreState.stderr, "gc prime: %v\n", err) //nolint:errcheck // best-effort hook diagnostics
}

// primeHookSessionName is cliSessionName for a hook: the session-name template
// consults the session beads only when the verify-only open yields a store.
func primeHookSessionName(cityPath, cityName, agentName, sessionTemplate string) string {
	if strings.TrimSpace(cityPath) == "" {
		return sessionName(nil, cityName, agentName, sessionTemplate)
	}
	store, err := openPrimeHookStore(cityPath)
	if err != nil {
		return sessionName(nil, cityName, agentName, sessionTemplate)
	}
	return sessionName(observationSessionStore(store, nil, cityPath), cityName, agentName, sessionTemplate)
}
