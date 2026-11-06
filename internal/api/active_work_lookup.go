package api

import (
	"context"
	"fmt"
	"log"
)

// activeWorkLookup is the request-wide read budget a list response's
// active-work enrichment shares: one context with the
// statusStoreReadTimeout deadline covers every uncached work-store read the
// /sessions and /agents lists perform, so a stalled backend read bounds the
// response instead of holding it for the backend's own timeout. The first
// read failure is recorded and reported through errors(); a request owns
// exactly one lookup and its row loops use it sequentially, so it needs no
// mutex.
type activeWorkLookup struct {
	ctx    context.Context
	cancel context.CancelFunc
	err    error // first read failure; nil until one fails
}

// newActiveWorkLookup starts the shared read budget for one list response.
// The caller owns the returned lookup and defers its cancel.
func newActiveWorkLookup(requestCtx context.Context) *activeWorkLookup {
	ctx, cancel := context.WithTimeout(requestCtx, statusStoreReadTimeout)
	return &activeWorkLookup{ctx: ctx, cancel: cancel}
}

// fail records the lookup's first read failure, naming the rig the failed
// read reached, and logs that failure once. A request's later rows see the
// failure through errors() instead of re-reading.
func (l *activeWorkLookup) fail(rig string, err error) {
	if l.err != nil {
		return
	}
	l.err = fmt.Errorf("looking up active work in rig %q: %w", rig, err)
	log.Printf("api: %v", l.err)
}

// errors returns the recorded read failures for the response's
// partial_errors field.
func (l *activeWorkLookup) errors() []string {
	if l.err == nil {
		return nil
	}
	return []string{l.err.Error()}
}
