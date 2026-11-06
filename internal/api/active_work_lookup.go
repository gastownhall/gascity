package api

import (
	"context"
	"fmt"
	"log"
)

// activeWorkLookup bounds the active-work enrichment of one list response
// (GET /sessions, GET /agents). Every row's uncached work-store read shares
// one statusStoreReadTimeout window derived from the request, so a stalled
// store costs the response that window at most, not the backend's own
// timeout once per row. The first failed read ends the lookup: later rows
// read nothing and resolve no work-store bead, since an unread
// higher-priority store may hold the winning one, and the response reports
// the failure as partial. Cache answers stay usable after the window closes
// until a read has failed. One request's row loop uses it sequentially.
type activeWorkLookup struct {
	ctx    context.Context
	cancel context.CancelFunc
	err    error
}

// newActiveWorkLookup starts the lookup's window from the request context;
// the caller defers close.
func newActiveWorkLookup(ctx context.Context) *activeWorkLookup {
	ctx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
	return &activeWorkLookup{ctx: ctx, cancel: cancel}
}

// close cancels any read still in flight under the lookup's window.
func (l *activeWorkLookup) close() {
	l.cancel()
}

// failed reports whether a read has failed, ending the lookup.
func (l *activeWorkLookup) failed() bool {
	return l.err != nil
}

// fail records and logs the read failure that ends the lookup.
func (l *activeWorkLookup) fail(rig string, err error) {
	l.err = fmt.Errorf("looking up active work in rig %q: %w", rig, err)
	log.Printf("api: %v", l.err)
}

// partialErrors is the failure a list response reports as partial_errors.
func (l *activeWorkLookup) partialErrors() []string {
	if l.err == nil {
		return nil
	}
	return []string{l.err.Error()}
}
