package api

import (
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storehealth"
)

// storeHealthCacheTTL is the refresh interval for the /v0/status
// StoreHealth block. The underlying inputs (directory size walk,
// maintenance-log read) are cheap enough to run every minute but
// running them on every dashboard poll is wasteful.
const storeHealthCacheTTL = 30 * time.Second

// cachedStoreHealth returns the memoized StoreHealth block, refreshing
// when the TTL has elapsed. Safe for concurrent callers.
func (s *Server) cachedStoreHealth(now time.Time) *StatusStoreHealth {
	s.storeHealthMu.Lock()
	if s.storeHealthEntry != nil && now.Before(s.storeHealthExpires) {
		entry := s.storeHealthEntry
		s.storeHealthMu.Unlock()
		return entry
	}
	compute := s.storeHealthComputer
	if compute == nil {
		compute = s.computeStoreHealth
	}
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
	list, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		return 0
	}
	return len(list)
}
