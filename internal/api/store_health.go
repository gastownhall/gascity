package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storehealth"
)

// storeHealthCacheTTL is the refresh interval for the /v0/status
// StoreHealth block. Its inputs include a full closed-history row scan
// whose cost grows with store history and can exceed a minute on a
// long-lived city, so keep the interval above the worst observed scan.
const storeHealthCacheTTL = 3 * time.Minute

// cachedStoreHealth returns the memoized StoreHealth block, refreshing
// when the TTL has elapsed. Concurrent refreshes are coalesced through a
// singleflight group so a single scan serves every waiting caller. Failed
// refreshes are returned to the caller and are not cached. Safe for
// concurrent callers.
func (s *Server) cachedStoreHealth(ctx context.Context, now time.Time) (*StatusStoreHealth, error) {
	if entry := s.cachedStoreHealthEntry(now); entry != nil {
		return entry, nil
	}

	value, err, _ := s.storeHealthFlight.Do("refresh", func() (any, error) {
		// Another refresh may have completed between this caller's initial
		// miss and its election into the singleflight group.
		if entry := s.cachedStoreHealthEntry(time.Now()); entry != nil {
			return entry, nil
		}

		s.storeHealthMu.Lock()
		compute := s.storeHealthComputer
		if compute == nil {
			compute = s.computeStoreHealth
		}
		s.storeHealthMu.Unlock()

		// The refresh is shared by every concurrent status request, so its
		// lifetime must not depend on whichever request won the flight. The
		// store read applies its own bounded timeout downstream.
		health, err := compute(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}
		completedAt := time.Now()

		s.storeHealthMu.Lock()
		s.storeHealthEntry = health
		s.storeHealthExpires = completedAt.Add(storeHealthCacheTTL)
		s.storeHealthMu.Unlock()
		return health, nil
	})
	if err != nil {
		return nil, err
	}
	return value.(*StatusStoreHealth), nil
}

func (s *Server) cachedStoreHealthEntry(now time.Time) *StatusStoreHealth {
	s.storeHealthMu.Lock()
	defer s.storeHealthMu.Unlock()
	if s.storeHealthEntry != nil && now.Before(s.storeHealthExpires) {
		return s.storeHealthEntry
	}
	return nil
}

// computeStoreHealth measures the Dolt store on disk and the latest
// gc.store.maintenance event via the server's State. Returns nil when
// the city path is empty (no state to measure against).
func (s *Server) computeStoreHealth(ctx context.Context) (*StatusStoreHealth, error) {
	cityPath := s.state.CityPath()
	if cityPath == "" {
		return nil, nil
	}
	// WalkSize is a synchronous, uncancellable disk walk; the
	// storeHealthCacheTTL cache bounds how often it runs. Plumbing
	// context/timeout through WalkSize is deferred until it shows up
	// in profiles.
	size := storehealth.WalkSize(storehealth.StorePath(cityPath))
	rows, err := countBeadStoreRows(ctx, s.state, s.state.CityBeadStore())
	if err != nil {
		return nil, err
	}
	lastAt, lastStatus := storehealth.LastMaintenance(s.state.EventProvider())
	h := storehealth.Compute(cityPath, size, rows, lastAt, lastStatus)
	return statusStoreHealthFromDomain(h), nil
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

// countBeadStoreRows returns the number of retained beads in store, including
// open and closed beads. A nil store and measurement failures are returned as
// errors so callers do not mistake an unavailable denominator for zero.
// The closed-inclusive query is never answerable from the in-memory cache, so
// the count prefers the hydration-free beads.Counter path (the #1896
// follow-up): hydrating tens of thousands of closed rows cannot finish inside
// statusStoreReadTimeout on a long-lived city, which left store_health
// permanently absent. The hydrating List fallback remains for stores without
// a Counter and for shapes Count reports as unsupported; that path is the
// store-health block's exposure to ga-cdmx6x's bd-child leak, covered by
// statusListStoreWithTimeout's state.ScopedStoreLike wiring the same way as
// the work-count fallback.
func countBeadStoreRows(ctx context.Context, state State, store beads.Store) (int, error) {
	if store == nil {
		return 0, errors.New("counting retained bead rows: store unavailable")
	}
	query := beads.ListQuery{AllowScan: true, IncludeClosed: true}
	if counter, ok := store.(beads.Counter); ok {
		// cachedStoreHealth strips the request deadline, so bound the count
		// here the same way the status handler bounds its store reads.
		reqCtx, cancel := context.WithTimeout(ctx, statusStoreReadTimeout)
		n, err := counter.Count(reqCtx, query)
		cancel()
		if err == nil {
			return n, nil
		}
		if !errors.Is(err, beads.ErrCountUnsupported) {
			return 0, fmt.Errorf("counting retained bead rows: %w", err)
		}
	}
	list, err := statusListStoreWithTimeout(ctx, state, store, query)
	if err != nil {
		return 0, fmt.Errorf("counting retained bead rows: %w", err)
	}
	return len(list), nil
}
