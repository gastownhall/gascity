package main

import (
	"context"
	"log"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// startedRuntimeBinding reads the thread sp hosts the just-started session
// name in, for the start commit to record. Like every observation after
// provider.Start it is held to postStartObservationBound. The binding is
// advisory, so a read that fails or does not answer is logged and records
// nothing: the start it describes stands without it.
func startedRuntimeBinding(ctx context.Context, cityPath string, sp runtime.Provider, name string) runtime.SessionBinding {
	if _, ok := sp.(runtime.SessionBindingProvider); !ok {
		return runtime.SessionBinding{}
	}
	ctx, cancel := context.WithTimeout(ctx, postStartObservationBound)
	defer cancel()
	binding, err := observeSessionBounded(ctx, cityPath, name, func() (runtime.SessionBinding, error) {
		return sessionpkg.ReadRuntimeBinding(sp, name)
	})
	if err != nil {
		log.Printf("session %q started; not recording its runtime binding: %v", name, err)
		return runtime.SessionBinding{}
	}
	return binding
}
