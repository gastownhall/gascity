package main

import "sync"

// doctorStoreReadConcurrency bounds how many independent store reads one
// doctor check issues at once. Each read on a bd-backed scope is a bd
// subprocess (and two git forks under it), so the bound keeps a check a good
// citizen against the data plane while still overlapping the per-fork latency
// that otherwise dominates a check making a dozen targeted reads.
const doctorStoreReadConcurrency = 4

// doctorParallelMap calls fn for every item with at most
// doctorStoreReadConcurrency calls in flight and returns the results in item
// order, so a caller that folds them in order behaves exactly as the serial
// loop it replaces.
func doctorParallelMap[T, R any](items []T, fn func(T) R) []R {
	out := make([]R, len(items))
	if len(items) == 0 {
		return out
	}
	limit := doctorStoreReadConcurrency
	if len(items) < limit {
		limit = len(items)
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, item T) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = fn(item)
		}(i, item)
	}
	wg.Wait()
	return out
}

// doctorListResult is one store read's outcome.
type doctorListResult[T any] struct {
	value T
	err   error
}
