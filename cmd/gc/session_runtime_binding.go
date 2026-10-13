package main

import (
	"context"
	"log"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// startedRuntimeBinding reads the thread sp hosts the just-started session
// name in and returns the patch that records it in the start commit
// (sessionpkg.RuntimeBindingPatch). Like every observation after
// provider.Start, it is held to postStartObservationBound. The binding is
// advisory: a read that fails or does not answer is logged and records
// nothing, and the start it describes stands without it.
func startedRuntimeBinding(ctx context.Context, cityPath string, sp runtime.Provider, name string) sessionpkg.MetadataPatch {
	if _, ok := sp.(runtime.SessionBindingProvider); !ok {
		return sessionpkg.MetadataPatch{}
	}
	ctx, cancel := context.WithTimeout(ctx, postStartObservationBound)
	defer cancel()
	patch, err := observeSessionBounded(ctx, cityPath, name, func() (sessionpkg.MetadataPatch, error) {
		return sessionpkg.RuntimeBindingPatch(sp, name)
	})
	if err != nil {
		log.Printf("session %q started; not recording its runtime binding: %v", name, err)
		return sessionpkg.MetadataPatch{}
	}
	return patch
}
