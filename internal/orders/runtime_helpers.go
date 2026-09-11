package orders

import (
	"log"
	"time"
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

// LastRunAllAcross merges each front door's whole-city last-run index into one
// map, keeping the newest time per order, and reports whether every front door
// answered. nil entries are skipped. One incomplete leg makes the whole index
// unusable as an authority on absence, so ok is the AND. See Store.LastRunAll.
func LastRunAllAcross(stores []*Store) (map[string]time.Time, bool) {
	out := map[string]time.Time{}
	complete := true
	for _, s := range stores {
		if s == nil {
			continue
		}
		index, ok := s.LastRunAll()
		if !ok {
			complete = false
		}
		for scoped, at := range index {
			if at.After(out[scoped]) {
				out[scoped] = at
			}
		}
	}
	return out, complete
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
