package orders

import (
	"errors"
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

// LastRunIndexAcross returns the newest tracking-bead CreatedAt per scoped
// order across a federation of order front doors. It is the bulk counterpart to
// LastRunAcross for diagnostics that need to inspect many orders at once.
func LastRunIndexAcross(stores []*Store) (map[string]time.Time, error) {
	latest := make(map[string]time.Time)
	var errs []error
	for _, s := range stores {
		if s == nil {
			continue
		}
		index, err := s.LastRunIndex()
		if err != nil {
			errs = append(errs, err)
		}
		for scoped, last := range index {
			if scoped == "" {
				continue
			}
			if last.After(latest[scoped]) {
				latest[scoped] = last
			}
		}
	}
	return latest, errors.Join(errs...)
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
