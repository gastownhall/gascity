package orders

import (
	"log"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

var runtimeHelpersLogf = log.Printf

// LastRunRequest describes one order whose latest persisted run must be read
// across one or more stores.
type LastRunRequest struct {
	Name   string
	Stores []beads.Store
}

// LastRunResult is the result corresponding to one LastRunRequest.
type LastRunResult struct {
	Name    string
	LastRun time.Time
	Err     error
}

// LoadLastRuns resolves latest-run requests concurrently while bounding the
// number of requests in flight. Results preserve request order. A non-positive
// maxConcurrent value uses one worker.
func LoadLastRuns(requests []LastRunRequest, maxConcurrent int) []LastRunResult {
	results := make([]LastRunResult, len(requests))
	if len(requests) == 0 {
		return results
	}
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	if maxConcurrent > len(requests) {
		maxConcurrent = len(requests)
	}

	jobs := make(chan int)
	var workers sync.WaitGroup
	workers.Add(maxConcurrent)
	for range maxConcurrent {
		go func() {
			defer workers.Done()
			for index := range jobs {
				request := requests[index]
				lastRun, err := LastRunAcrossStores(request.Stores...)(request.Name)
				results[index] = LastRunResult{
					Name:    request.Name,
					LastRun: lastRun,
					Err:     err,
				}
			}
		}()
	}
	for index := range requests {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	return results
}

// LastRunFuncForStore returns the latest order-run bead time for one store.
func LastRunFuncForStore(store beads.Store) LastRunFunc {
	return func(name string) (time.Time, error) {
		if store == nil {
			return time.Time{}, nil
		}
		label := "order-run:" + name
		// Order-run beads land in either tier: the ephemeral tracking bead
		// (wisps) created by the dispatcher and the molecule root (issues)
		// labeled after instantiation. Both carry the order-run label.
		results, err := store.List(beads.ListQuery{
			Label:         label,
			Limit:         1,
			IncludeClosed: true,
			Sort:          beads.SortCreatedDesc,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			if len(results) == 0 {
				return time.Time{}, err
			}
			runtimeHelpersLogf("orders: last-run lookup partially failed for %s: %v", name, err)
		}
		if len(results) == 0 {
			return time.Time{}, nil
		}
		return results[0].CreatedAt, nil
	}
}

// LastRunAcrossStores returns the most recent run time across a set of stores
// for a single order name.
func LastRunAcrossStores(stores ...beads.Store) LastRunFunc {
	return func(name string) (time.Time, error) {
		var latest time.Time
		for _, store := range stores {
			if store == nil {
				continue
			}
			last, err := LastRunFuncForStore(store)(name)
			if err != nil {
				return time.Time{}, err
			}
			if last.After(latest) {
				latest = last
			}
		}
		return latest, nil
	}
}

// CursorFuncForStore returns the max order-run seq for one store.
func CursorFuncForStore(store beads.Store) CursorFunc {
	return func(name string) uint64 {
		if store == nil {
			return 0
		}
		label := "order-run:" + name
		results, err := store.List(beads.ListQuery{
			Label:         label,
			Limit:         10,
			IncludeClosed: true,
			Sort:          beads.SortCreatedDesc,
			TierMode:      beads.TierBoth,
		})
		if err != nil {
			if len(results) == 0 {
				runtimeHelpersLogf("orders: cursor lookup failed for %s: %v", name, err)
				return 0
			}
			runtimeHelpersLogf("orders: cursor lookup partially failed for %s: %v", name, err)
		}
		if len(results) == 0 {
			return 0
		}
		labelSets := make([][]string, 0, len(results))
		for _, b := range results {
			labelSets = append(labelSets, b.Labels)
		}
		return MaxSeqFromLabels(labelSets)
	}
}

// CursorAcrossStores merges seq cursors from multiple stores.
func CursorAcrossStores(stores ...beads.Store) CursorFunc {
	fns := make([]CursorFunc, 0, len(stores))
	for _, store := range stores {
		if store != nil {
			fns = append(fns, CursorFuncForStore(store))
		}
	}
	return func(name string) uint64 {
		var latest uint64
		for _, fn := range fns {
			if seq := fn(name); seq > latest {
				latest = seq
			}
		}
		return latest
	}
}
