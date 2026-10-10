package main

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// postStartObservationBound is the longest runPreparedStartCandidate waits for
// any one observation it makes after provider.Start has returned: the
// ErrStateSync recovery, the ErrSessionExists collision check and the
// post-stability liveness check. The call under the wait can hang on a wedged
// tmux subprocess or a stalled store and nothing cancels it, so on expiry the
// wait is abandoned and the start defers as runtime-unavailable
// (SESSION-RUNTIME-009) instead of holding its async start slot.
const postStartObservationBound = 10 * time.Second

// startFailureProbeBudget is the total time a failed start spends on the two
// probes it makes to classify the failure, the rate-limit screen peek and the
// pending-create identity reads, together. A peek that does not answer reads as
// "no rate-limit screen". A read that does not answer leaves the attribution
// unknown, so the start defers instead of rolling the create back.
const startFailureProbeBudget = 10 * time.Second

// postStartObservationKey names what ObserveBounded keeps at one in-flight
// observation: one session in one city. The city is part of it because two
// cities run sessions of the same name, and one city's wedged runtime must not
// defer the other's. A path and a session name never contain NUL, so the
// joined key is unambiguous.
func postStartObservationKey(cityPath, sessionName string) string {
	return cityPath + "\x00" + sessionName
}

// observeSessionBounded runs observe, an observation of the session sessionName
// in cityPath, for no longer than ctx allows. It goes through
// worker.ObserveBounded, so at most one observation of the session is in flight
// and an expired wait is an error wrapping runtime.ErrRuntimeUnavailable. A
// blank session name is not bounded: it never reaches the runtime (the
// unbounded observation answers runtime.ErrSessionNotFound without any I/O), so
// there is no call to wedge.
func observeSessionBounded[T any](ctx context.Context, cityPath, sessionName string, observe func() (T, error)) (T, error) {
	if strings.TrimSpace(sessionName) == "" {
		return observe()
	}
	return worker.ObserveBounded(ctx, postStartObservationKey(cityPath, sessionName), func(context.Context) (T, error) {
		return observe()
	})
}

// workerObserveSessionTargetBounded is
// workerObserveSessionTargetWithRuntimeHintsWithConfig held to
// postStartObservationBound. The bound is derived from ctx, so it also ends
// when the controller does, and it covers the handle's store reads as well as
// the runtime probes behind them.
func workerObserveSessionTargetBounded(ctx context.Context, cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, target string, processNames []string) (worker.LiveObservation, error) {
	ctx, cancel := context.WithTimeout(ctx, postStartObservationBound)
	defer cancel()
	return observeSessionBounded(ctx, cityPath, target, func() (worker.LiveObservation, error) {
		return workerObserveSessionTargetWithRuntimeHintsWithConfig(cityPath, store, sp, cfg, target, processNames)
	})
}

// observeRuntimeProviderLivenessBounded is observeRuntimeProviderLiveness held
// to postStartObservationBound, for a start with no session bead to resolve a
// handle from.
func observeRuntimeProviderLivenessBounded(ctx context.Context, cityPath string, sp runtime.Provider, name string, processNames []string) (running bool, alive bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, postStartObservationBound)
	defer cancel()
	type liveness struct{ running, alive bool }
	got, err := observeSessionBounded(ctx, cityPath, name, func() (liveness, error) {
		isRunning, isAlive, observeErr := observeRuntimeProviderLiveness(sp, name, processNames)
		return liveness{running: isRunning, alive: isAlive}, observeErr
	})
	return got.running, got.alive, err
}

// readPendingCreateIdentityBounded is readPendingCreateIdentity with every
// metadata read held to ctx. A read that does not answer in time is
// unverifiable like any other failed read, so the attribution comes out
// unknown unless the reads that did answer already decide it.
func readPendingCreateIdentityBounded(ctx context.Context, cityPath string, info sessionpkg.Info, sessionName string, sp runtime.Provider) pendingCreateIdentity {
	return readPendingCreateIdentityVia(info, sp, func(key string) (string, error) {
		return observeSessionBounded(ctx, cityPath, sessionName, func() (string, error) {
			return sp.GetMeta(sessionName, key)
		})
	})
}
