package orders

import "github.com/gastownhall/gascity/internal/beads"

// Frozen copies of the order reads the T7b.2 leg forms stand beside, taken
// from origin/main 8d2fb3120c (internal/orders/store.go, store_reads.go and
// runtime_helpers.go): each method became a function of its *Store and the
// comments were trimmed; the code is unchanged. store_legs_test.go runs them
// against ListTrackingOn, LatestOpenRunOn and CursorAcrossLegs, and against
// today's methods, whose bodies T7b.2 factored. NewestRunRecords,
// RunFromTrackingBead, decodeRun and MaxSeqFromLabels are called live: T7b.2
// left them byte-identical, and none reads a store.

func frozenListTracking(s *Store, limit int) ([]OrderRun, error) {
	if s.store == (beads.OrdersStore{}) {
		return nil, nil
	}
	list, err := s.store.List(beads.ListQuery{
		Label:                    labelOrderTracking,
		Limit:                    limit,
		IncludeClosed:            true,
		Sort:                     beads.SortCreatedDesc,
		TierMode:                 beads.TierBoth,
		AllowBackingCreatedLimit: true,
	})
	runs := make([]OrderRun, 0, len(list))
	for _, b := range list {
		if run, ok := RunFromTrackingBead(b); ok {
			runs = append(runs, run)
		}
	}
	return runs, err
}

func frozenLatestOpenRun(s *Store, scoped string) (OrderRun, bool, error) {
	if s.store == (beads.OrdersStore{}) {
		return OrderRun{}, false, nil
	}
	list, err := s.store.List(beads.ListQuery{
		Label:    labelOrderRunPrefix + scoped,
		Limit:    1,
		Sort:     beads.SortCreatedDesc,
		TierMode: beads.TierBoth,
	})
	if len(list) == 0 {
		return OrderRun{}, false, err
	}
	return decodeRun(scoped, list[0]), true, err
}

func frozenMixedLegStores(s *Store) []beads.Store {
	var stores []beads.Store
	if s.store.Store != nil {
		stores = append(stores, s.store.Store)
	}
	if s.graph.Store != nil && s.graph.Store != s.store.Store {
		stores = append(stores, s.graph.Store)
	}
	return stores
}

func frozenLastRunRecords(store beads.Store, label string, n int) ([]beads.Bead, error) {
	return NewestRunRecords(func(limit int) ([]beads.Bead, error) {
		return beads.HandlesFor(store).Live.List(beads.ListQuery{
			Label:                    label,
			Limit:                    limit,
			IncludeClosed:            true,
			Sort:                     beads.SortCreatedDesc,
			TierMode:                 beads.TierBoth,
			AllowBackingCreatedLimit: n == 1,
		})
	}, n)
}

func frozenCursor(s *Store, name string) EventCursor {
	label := labelOrderRunPrefix + name
	var latest uint64
	for _, store := range frozenMixedLegStores(s) {
		results, err := frozenLastRunRecords(store, label, 10)
		if err != nil {
			if len(results) == 0 {
				runtimeHelpersLogf("orders: cursor lookup failed for %s: %v", name, err)
				continue
			}
			runtimeHelpersLogf("orders: cursor lookup partially failed for %s: %v", name, err)
		}
		if len(results) == 0 {
			continue
		}
		labelSets := make([][]string, 0, len(results))
		for _, b := range results {
			labelSets = append(labelSets, b.Labels)
		}
		if seq := MaxSeqFromLabels(labelSets); seq > latest {
			latest = seq
		}
	}
	return EventCursor(latest)
}

func frozenCursorAcross(stores []*Store) CursorFunc {
	return func(name string) uint64 {
		var latest uint64
		for _, s := range stores {
			if s == nil {
				continue
			}
			if seq := uint64(frozenCursor(s, name)); seq > latest {
				latest = seq
			}
		}
		return latest
	}
}
