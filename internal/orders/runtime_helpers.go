package orders

import (
	"log"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

var runtimeHelpersLogf = log.Printf

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

type openOrderRunStore interface {
	HasOpenOrderRun(name string) (bool, error)
}

// HasOpenRunFuncForStore returns true when a store has any non-closed
// order-run bead for the named order.
func HasOpenRunFuncForStore(store beads.Store) func(string) (bool, error) {
	return func(name string) (bool, error) {
		name = strings.TrimSpace(name)
		if store == nil || name == "" {
			return false, nil
		}
		if fastStore, ok := store.(openOrderRunStore); ok {
			return fastStore.HasOpenOrderRun(name)
		}
		results, err := store.List(beads.ListQuery{
			Label:    "order-run:" + name,
			Limit:    1,
			Live:     true,
			TierMode: beads.TierBoth,
		})
		if err != nil {
			return false, err
		}
		return len(results) > 0, nil
	}
}

// HasOpenRunAcrossStores returns true if any store has a non-closed order run.
func HasOpenRunAcrossStores(stores ...beads.Store) func(string) (bool, error) {
	return func(name string) (bool, error) {
		for _, store := range stores {
			open, err := HasOpenRunFuncForStore(store)(name)
			if err != nil {
				return false, err
			}
			if open {
				return true, nil
			}
		}
		return false, nil
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
