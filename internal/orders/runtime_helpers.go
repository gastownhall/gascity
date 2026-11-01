package orders

import (
	"log"
	"slices"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

var runtimeHelpersLogf = log.Printf

// LastRunAcross returns a LastRunFunc reporting the most recent run time for a
// named order across a federation of order front doors (the dispatcher/CLI
// city + rig scopes). Each *Store performs its own MIXED orders+graph LastRun
// read (unioning its orders leg with its graph leg); the max across scopes wins.
// A per-scope error aborts and propagates. nil entries are skipped.
func LastRunAcross(stores []*Store) LastRunFunc {
	return func(name string) (time.Time, error) {
		var latest time.Time
		for _, s := range stores {
			if s == nil {
				continue
			}
			last, err := s.LastRun(name)
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

// LastRunFuncWithEventFallback wraps fn with an events.Provider fallback.
// When fn returns a zero time (no tracking beads found — e.g. after the
// tracking beads were purged), it queries the event bus for the most recent
// order.fired event whose Subject matches the order's scoped name and
// returns its timestamp. A nil ep disables the fallback and the wrapped
// fn's result is returned unchanged. Event bus errors are non-fatal:
// when the provider fails the zero time is returned so the order appears
// unrun rather than permanently blocked.
func LastRunFuncWithEventFallback(fn LastRunFunc, ep events.Provider) LastRunFunc {
	if ep == nil {
		return fn
	}
	return func(name string) (time.Time, error) {
		last, err := fn(name)
		if err != nil {
			return time.Time{}, err
		}
		if !last.IsZero() {
			return last, nil
		}
		evts, listErr := ep.List(events.Filter{
			Type:    events.OrderFired,
			Subject: name,
		})
		if listErr != nil {
			return time.Time{}, nil
		}
		var latest time.Time
		for _, e := range evts {
			if e.Ts.After(latest) {
				latest = e.Ts
			}
		}
		return latest, nil
	}
}

// CursorAcross returns a CursorFunc merging the event seq cursor for a named
// order across a federation of order front doors. Each *Store performs its own
// MIXED orders+graph Cursor read; the max seq across scopes wins. nil entries
// are skipped.
func CursorAcross(stores []*Store) CursorFunc {
	return func(name string) uint64 {
		var latest uint64
		for _, s := range stores {
			if s == nil {
				continue
			}
			if seq := uint64(s.Cursor(name)); seq > latest {
				latest = seq
			}
		}
		return latest
	}
}

// RecentRunsAcross returns a reader of the newest runs of a named order across a
// federation of order front doors, newest first, at most limit of them. Each
// *Store lists its own newest limit runs; the merge is by CreatedAt descending,
// so the result is the true newest limit across every scope. A per-scope error
// aborts and propagates. nil entries are skipped.
func RecentRunsAcross(stores []*Store) func(scoped string, limit int) ([]OrderRun, error) {
	return func(scoped string, limit int) ([]OrderRun, error) {
		var merged []OrderRun
		for _, s := range stores {
			if s == nil {
				continue
			}
			runs, err := s.RecentRuns(scoped, limit)
			if err != nil {
				return nil, err
			}
			merged = append(merged, runs...)
		}
		slices.SortStableFunc(merged, func(a, b OrderRun) int { return b.CreatedAt.Compare(a.CreatedAt) })
		if limit > 0 && len(merged) > limit {
			merged = merged[:limit]
		}
		return merged, nil
	}
}
