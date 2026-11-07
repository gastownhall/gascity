package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// activeWorkLookup bounds the active-work enrichment of one list response
// (GET /sessions, GET /agents). Every row's work-store reads draw on one
// statusStoreReadTimeout budget, charged only for the time reads take, so a
// stalled store costs the response the budget at most, not the backend's own
// timeout once per row, and the runtime probes between rows spend none of
// it. A clean cache answers at once, so it needs no budget and answers even
// once the budget is spent. The first failed read fails the lookup, as do a
// cache refresh the budget ran out waiting for and a read the spent budget
// refuses, and no store is read after it: a row whose search reaches a store
// its cache cannot answer resolves no work-store bead, since that unread store
// may hold the winning one, while a row the caches answer still resolves. The
// response reports the failure: the session lists in active_work_errors, the
// agent list as partial. One request's row loop uses it sequentially.
type activeWorkLookup struct {
	ctx    context.Context
	budget time.Duration
	left   time.Duration
	err    error
}

// newActiveWorkLookup starts a lookup whose reads end with ctx, the request's
// context, or with the budget, whichever comes first.
func newActiveWorkLookup(ctx context.Context) *activeWorkLookup {
	return &activeWorkLookup{ctx: ctx, budget: statusStoreReadTimeout, left: statusStoreReadTimeout}
}

// list reads query from store, the store of rig, for one row under what is
// left of the budget, and charges the read's time to it. A store with a cache
// (beads.RefreshingCachedLister) answers from it when it can: a clean cache
// at once, a dirty one once the cache's own refresh of its dirty rows ends.
// That refresh runs on past the budget when it must, so a later row or
// response finds the rows refreshed. Any other read goes through
// statusListStoreWithTimeout. Once the lookup has failed, or the budget is
// spent, only a clean cache answers, and nothing is read or refreshed. A read
// that fails, a refresh the budget ran out waiting for, or a read the spent
// budget refuses fails the lookup; list then reports false.
func (l *activeWorkLookup) list(state State, rig string, store beads.WorkStore, query beads.ListQuery) ([]beads.Bead, bool) {
	spent := l.budget - l.left
	wait := l.left
	if l.err != nil {
		wait = 0
	}
	ctx, cancel := context.WithTimeout(l.ctx, wait)
	defer cancel()
	start := time.Now()
	defer func() { l.left -= time.Since(start) }()
	if cache, ok := beads.As[beads.RefreshingCachedLister](store); ok {
		waiting := ctx.Err() == nil
		if rows, ok := cache.CachedListRefreshing(ctx, query); ok {
			return rows, true
		}
		if waiting && ctx.Err() != nil {
			l.fail(rig, l.noteSpent(fmt.Errorf("waiting for its cache to refresh its dirty rows: %w", ctx.Err()), spent))
			return nil, false
		}
	}
	if l.err != nil {
		return nil, false
	}
	if l.left <= 0 {
		l.fail(rig, fmt.Errorf("not read: earlier reads spent the %s active-work budget", l.budget))
		return nil, false
	}
	rows, err := statusListStoreWithTimeout(ctx, state, store, query)
	if err != nil {
		l.fail(rig, l.noteSpent(err, spent))
		return nil, false
	}
	return rows, true
}

// noteSpent adds to err, a read's failure, how much of the budget the reads
// before it had spent when the budget ended it, so the timeout does not read
// as a stall of the rig it ended on.
func (l *activeWorkLookup) noteSpent(err error, spent time.Duration) error {
	if spent > 0 && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (earlier reads had spent %s of the %s active-work budget)", err, spent.Round(time.Millisecond), l.budget)
	}
	return err
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
