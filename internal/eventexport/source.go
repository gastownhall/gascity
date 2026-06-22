package eventexport

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

var errNoProviders = errors.New("eventexport: no city providers")

// MuxSource adapts the supervisor's per-city event providers into a Source by
// building an events.Multiplexer and watching it. It rebuilds periodically so
// cities that start or stop after launch are picked up, resuming each city from
// the exporter's acked cursor (or, for a city never seen before, from its head
// so launch does not backfill the whole history).
type MuxSource struct {
	providers    func() map[string]events.Provider
	cursors      func() map[string]uint64
	rebuildEvery time.Duration
	logf         func(string, ...any)

	mu      sync.Mutex
	watcher *events.MuxWatcher
	cancel  context.CancelFunc
	floor   map[string]uint64 // city -> head-floor first set for a never-acked city
}

// NewMuxSource builds a MuxSource. providers returns the current city providers;
// cursors returns the exporter's acked per-city seq (the resume points).
func NewMuxSource(providers func() map[string]events.Provider, cursors func() map[string]uint64, rebuildEvery time.Duration, logf func(string, ...any)) *MuxSource {
	if rebuildEvery <= 0 {
		rebuildEvery = 60 * time.Second
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &MuxSource{providers: providers, cursors: cursors, rebuildEvery: rebuildEvery, logf: logf, floor: map[string]uint64{}}
}

// Next yields the next tagged event, transparently rebuilding the multiplexer on
// the rebuild interval or when the current watcher ends.
func (s *MuxSource) Next(ctx context.Context) (events.TaggedEvent, error) {
	for {
		if err := ctx.Err(); err != nil {
			return events.TaggedEvent{}, err
		}
		s.mu.Lock()
		w := s.watcher
		s.mu.Unlock()
		if w == nil {
			if err := s.rebuild(ctx); err != nil {
				if !sleepCtx(ctx, 500*time.Millisecond) {
					return events.TaggedEvent{}, ctx.Err()
				}
				continue
			}
			continue
		}
		te, err := w.Next()
		if err != nil {
			s.closeWatcher() // rebuild-due (child ctx timeout), city drop, or shutdown
			continue
		}
		return te, nil
	}
}

func (s *MuxSource) rebuild(ctx context.Context) error {
	provs := s.providers()
	if len(provs) == 0 {
		return errNoProviders
	}
	cur := s.cursors()
	resume := make(map[string]uint64, len(provs))
	s.mu.Lock()
	for city, p := range provs {
		switch {
		case cur[city] > 0:
			resume[city] = cur[city] // resume from acked
		case s.floor[city] > 0:
			resume[city] = s.floor[city] // keep the floor; never re-floor to a newer head
		default:
			head, err := p.LatestSeq()
			if err != nil {
				// Do not floor at 0 on a transient error: that could backfill the
				// whole history if Watch later succeeds. Skip this city; the next
				// rebuild floors it once LatestSeq is reliable.
				continue
			}
			s.floor[city] = head
			resume[city] = head // forward-from-now; no backfill
		}
	}
	s.mu.Unlock()

	mux := events.NewMultiplexer()
	for city, p := range provs {
		if _, ok := resume[city]; ok {
			mux.Add(city, p)
		}
	}
	childCtx, cancel := context.WithTimeout(ctx, s.rebuildEvery)
	w, err := mux.Watch(childCtx, resume)
	if err != nil {
		cancel()
		return err
	}
	s.mu.Lock()
	s.watcher = w
	s.cancel = cancel
	s.mu.Unlock()
	return nil
}

func (s *MuxSource) closeWatcher() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.watcher != nil {
		_ = s.watcher.Close()
		s.watcher = nil
	}
	s.mu.Unlock()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
