package main

import (
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/config"
)

// controllerWiring is what a controller entry point builds before its socket
// can deliver anything: the latched reconciler mode, the reconciler's wake
// signals and the wake over them, and the channels the socket, the API and
// the city runtime share. runController and the supervisor's startOneCity
// both take it from newControllerWiring, so neither can wire a channel the
// other forgets.
type controllerWiring struct {
	mode                        reconcilerMode
	pokeCh, controlDispatcherCh chan struct{}
	wake                        *controllerWake
	reloadReqCh                 chan reloadRequest
	convergenceReqCh            chan convergenceRequest
	configDirty                 *atomic.Bool
}

// newControllerWiring latches the session reconciler and builds the wiring.
// A refused mode is a start failure: the caller must not start the city.
func newControllerWiring(cfg *config.City) (*controllerWiring, error) {
	mode, err := latchReconcilerMode(cfg)
	if err != nil {
		return nil, err
	}
	w := &controllerWiring{
		mode:                mode,
		pokeCh:              make(chan struct{}, 1),
		controlDispatcherCh: make(chan struct{}, 1),
		reloadReqCh:         make(chan reloadRequest),
		convergenceReqCh:    make(chan convergenceRequest, 16),
		configDirty:         &atomic.Bool{},
	}
	w.wake = newLegacyWake(w.pokeCh, w.controlDispatcherCh)
	return w, nil
}
