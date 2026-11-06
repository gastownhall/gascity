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
// timeout once per row. Clean cache answers need no window. The first failed
// read fails the lookup, and no store is read after it: a row whose search
// reaches a store its cache cannot answer resolves no work-store bead, since
// that unread store may hold the winning one, while a row the caches answer
// still resolves. The response reports the failure: the session lists in
// active_work_errors, the agent list as partial. One request's row loop uses
// it sequentially.
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

// failed reports whether a read has failed, after which no store is read.
func (l *activeWorkLookup) failed() bool {
	return l.err != nil
}

// fail records and logs the read failure that fails the lookup.
func (l *activeWorkLookup) fail(rig string, err error) {
	l.err = fmt.Errorf("looking up active work in rig %q: %w", rig, err)
	log.Printf("api: %v", l.err)
}

// failures is the failure a list response reports: the session lists'
// active_work_errors, the agent list's partial_errors.
func (l *activeWorkLookup) failures() []string {
	if l.err == nil {
		return nil
	}
	return []string{l.err.Error()}
}
