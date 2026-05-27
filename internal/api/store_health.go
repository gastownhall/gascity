package api

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storehealth"
)

// storeHealthCacheTTL is the refresh interval for the /v0/status
// StoreHealth block. The underlying inputs include a directory size walk
// and bead row count, so expired snapshots are refreshed in the background
// instead of blocking every operator status poll.
const storeHealthCacheTTL = 30 * time.Second

const storeHealthRowCountTimeout = 3 * time.Second

type indexedRowCounter interface {
	CountIndexed(context.Context, beads.ListQuery) (int, error)
}

// cachedStoreHealth returns the memoized StoreHealth block, refreshing
// when the TTL has elapsed. Safe for concurrent callers.
func (s *Server) cachedStoreHealth(now time.Time) *StatusStoreHealth {
	s.storeHealthMu.Lock()
	if s.storeHealthEntry != nil {
		entry := s.storeHealthEntry
		if now.Before(s.storeHealthExpires) {
			s.storeHealthMu.Unlock()
			return entry
		}
		if !s.storeHealthRefreshing {
			compute := s.storeHealthComputeFunc()
			s.storeHealthRefreshing = true
			go s.refreshStoreHealth(compute)
		}
		s.storeHealthMu.Unlock()
		return entry
	}
	compute := s.storeHealthComputeFunc()
	s.storeHealthMu.Unlock()

	h := compute()

	s.storeHealthMu.Lock()
	defer s.storeHealthMu.Unlock()
	if s.storeHealthEntry != nil && now.Before(s.storeHealthExpires) {
		return s.storeHealthEntry
	}
	s.storeHealthEntry = h
	s.storeHealthExpires = now.Add(storeHealthCacheTTL)
	return h
}

func (s *Server) storeHealthComputeFunc() func() *StatusStoreHealth {
	if s.storeHealthComputer != nil {
		return s.storeHealthComputer
	}
	return s.computeStoreHealth
}

func (s *Server) refreshStoreHealth(compute func() *StatusStoreHealth) {
	defer func() {
		if recover() != nil {
			s.storeHealthMu.Lock()
			s.storeHealthRefreshing = false
			s.storeHealthMu.Unlock()
		}
	}()
	h := compute()

	s.storeHealthMu.Lock()
	s.storeHealthEntry = h
	s.storeHealthExpires = time.Now().Add(storeHealthCacheTTL)
	s.storeHealthRefreshing = false
	s.storeHealthMu.Unlock()
}

// computeStoreHealth measures the Dolt store on disk and the latest
// gc.store.maintenance event via the server's State. Returns nil when
// the city path is empty (no state to measure against).
func (s *Server) computeStoreHealth() *StatusStoreHealth {
	cityPath := s.state.CityPath()
	if cityPath == "" {
		return nil
	}
	size := storehealth.WalkSize(storehealth.StorePath(cityPath))
	rows := countBeadStoreRows(s.state.CityBeadStore())
	lastAt, lastStatus := storehealth.LastMaintenance(s.state.EventProvider())
	h := storehealth.Compute(cityPath, size, rows, lastAt, lastStatus)
	return statusStoreHealthFromDomain(h)
}

// statusStoreHealthFromDomain adapts storehealth.Health to the wire
// type StatusStoreHealth, serializing LastGCAt to RFC3339 UTC.
func statusStoreHealthFromDomain(h storehealth.Health) *StatusStoreHealth {
	out := &StatusStoreHealth{
		Path:        h.Path,
		SizeBytes:   h.SizeBytes,
		LiveRows:    h.LiveRows,
		RatioMB:     h.RatioMB,
		Warning:     h.Warning,
		ThresholdMB: h.ThresholdMB,
	}
	if !h.LastGCAt.IsZero() {
		out.LastGCAt = h.LastGCAt.UTC().Format(time.RFC3339)
		out.LastGCStatus = h.LastGCStatus
	}
	return out
}

// countBeadStoreRows returns the number of beads in store. Zero when
// store is nil or the scan fails — the ratio is best-effort.
func countBeadStoreRows(store beads.Store) int {
	if store == nil {
		return 0
	}
	query := beads.ListQuery{AllowScan: true, IncludeClosed: true}
	if counter, ok := store.(indexedRowCounter); ok {
		ctx, cancel := context.WithTimeout(context.Background(), storeHealthRowCountTimeout)
		defer cancel()
		count, err := counter.CountIndexed(ctx, query)
		if err == nil {
			return count
		}
		return 0
	}
	list, err := store.List(query)
	if err != nil {
		return 0
	}
	return len(list)
}
