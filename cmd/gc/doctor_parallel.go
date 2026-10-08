package main

import (
	"sync"
	"sync/atomic"
)

// doctorStoreReadConcurrency bounds how many independent store reads one
// doctor check issues at once. Each read on a bd-backed scope is a bd
// subprocess (and two git forks under it), so the bound keeps a check a good
// citizen against the data plane while still overlapping the per-fork latency
// that otherwise dominates a check making a dozen targeted reads.
const doctorStoreReadConcurrency = 4

// doctorParallelMap calls fn for every item, starting them in item order with
// at most doctorStoreReadConcurrency calls in flight, and returns the results
// in item order.
//
// stop, when non-nil, marks a result that ends the scan the way an error ends
// a serial loop: once a call's result satisfies stop, no further item is
// started. Calls already in flight finish, and every item before the stopping
// one has run, so a caller that folds the results in order and returns at the
// first stopping result reports what the serial loop would, with the one
// exception for panics below; the results of items never started are left
// zero, and that fold never reads them.
//
// A panic in fn also stops the scan, and is re-raised on the calling goroutine
// once every started call has returned (the lowest item's, when several
// panic). Raised on a worker goroutine it would escape doctor's per-check
// panic fence and crash the whole run; re-raised here, it fails only the check.
// The re-raise comes before any fold, so if an item past the first stopping
// one had already started and its call panics, the check still fails with
// that panic, where the serial loop would have returned at the stopping
// result without ever making the call.
func doctorParallelMap[T, R any](items []T, fn func(T) R, stop func(R) bool) []R {
	out := make([]R, len(items))
	if len(items) == 0 {
		return out
	}
	panics := make([]any, len(items))
	var stopped atomic.Bool
	sem := make(chan struct{}, min(doctorStoreReadConcurrency, len(items)))
	var wg sync.WaitGroup
	for i, item := range items {
		sem <- struct{}{}
		if stopped.Load() {
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					panics[i] = r
					stopped.Store(true)
				}
			}()
			out[i] = fn(item)
			if stop != nil && stop(out[i]) {
				stopped.Store(true)
			}
		}()
	}
	wg.Wait()
	for _, r := range panics {
		if r != nil {
			panic(r)
		}
	}
	return out
}

// doctorListResult is one store read's outcome.
type doctorListResult[T any] struct {
	value T
	err   error
}
