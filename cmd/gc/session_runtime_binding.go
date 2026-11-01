package main

import (
	"context"
	"fmt"
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

// wokePaneMessage names the pane a started session was spawned into, for the
// message of its session.woke event: the record holds only the current
// incarnation's pane, so the event is what lets an operator join the pane of
// an earlier incarnation (a pane pid in the host journal) to the session. It
// is empty when the start recorded no pane.
func wokePaneMessage(binding sessionpkg.MetadataPatch) string {
	paneID, panePID := binding[sessionpkg.MetadataRuntimePaneID], binding[sessionpkg.MetadataRuntimePanePID]
	if paneID == "" {
		return ""
	}
	return fmt.Sprintf("pane %s pid %s", paneID, panePID)
}
