package events

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// TaggedEvent is an Event annotated with the city that produced it.
type TaggedEvent struct {
	Event
	City string `json:"city"`
}

// Multiplexer merges events from multiple city providers into one
// stream, tagging each event with its source city.
type Multiplexer struct {
	mu        sync.RWMutex
	providers map[string]Provider // city name -> provider
}

// NewMultiplexer creates a Multiplexer with no providers.
// Use Add/Remove to manage city providers dynamically.
func NewMultiplexer() *Multiplexer {
	return &Multiplexer{providers: make(map[string]Provider)}
}

// Add registers a city's event provider.
func (m *Multiplexer) Add(city string, p Provider) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.providers[city] = p
}

// Remove unregisters a city's event provider.
func (m *Multiplexer) Remove(city string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.providers, city)
}

// Len returns the number of registered city providers. Callers that
// need to surface "no providers available" before committing an SSE
// response use this to distinguish an empty mux from a populated one —
// Watch itself can't report that condition because it happens after
// headers commit.
func (m *Multiplexer) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.providers)
}

// snapshot returns a copy of the current providers map.
func (m *Multiplexer) snapshot() map[string]Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make(map[string]Provider, len(m.providers))
	for k, v := range m.providers {
		cp[k] = v
	}
	return cp
}

// ListAll returns events from all cities matching the filter, sorted by
// timestamp. Each event is tagged with its source city.
func (m *Multiplexer) ListAll(filter Filter) ([]TaggedEvent, error) {
	providers := m.snapshot()
	var all []TaggedEvent
	for city, p := range providers {
		evts, err := p.List(filter)
		if err != nil {
			continue // best-effort: skip cities with errors
		}
		for _, e := range evts {
			all = append(all, TaggedEvent{Event: e, City: city})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].Ts.Before(all[j].Ts)
	})
	return all, nil
}

// Watch returns a Watcher that merges events from all currently registered
// city providers. Events are yielded in approximate time order. The cursor
// is a map of city→seq positions (use ParseCursor/FormatCursor to persist).
func (m *Multiplexer) Watch(ctx context.Context, cursors map[string]uint64) (*MuxWatcher, error) {
	providers := m.snapshot()
	childCtx, cancel := context.WithCancel(ctx)
	w := &MuxWatcher{
		ctx:    childCtx,
		cancel: cancel,
		ch:     make(chan TaggedEvent, 16),
		done:   make(chan struct{}),
	}

	var wg sync.WaitGroup
	for city, p := range providers {
		afterSeq := cursors[city]
		watcher, err := p.Watch(childCtx, afterSeq)
		if err != nil {
			continue // skip cities whose watcher fails
		}
		wg.Add(1)
		go func(city string, watcher Watcher) {
			defer wg.Done()
			defer watcher.Close() //nolint:errcheck
			for {
				e, err := watcher.Next()
				if err != nil {
					return
				}
				te := TaggedEvent{Event: e, City: city}
				select {
				case w.ch <- te:
				case <-ctx.Done():
					return
				case <-w.done:
					return
				}
			}
		}(city, watcher)
	}

	// Close the channel when all watchers finish.
	go func() {
		wg.Wait()
		close(w.ch)
	}()

	return w, nil
}

// MuxWatcher yields tagged events from multiple cities. It implements
// a subset of Watcher but returns TaggedEvent instead of Event.
type MuxWatcher struct {
	ctx       context.Context
	cancel    context.CancelFunc
	ch        chan TaggedEvent
	done      chan struct{}
	closeOnce sync.Once
}

// Next blocks until the next tagged event is available or the context
// is canceled.
func (w *MuxWatcher) Next() (TaggedEvent, error) {
	select {
	case <-w.ctx.Done():
		return TaggedEvent{}, w.ctx.Err()
	case <-w.done:
		return TaggedEvent{}, fmt.Errorf("watcher closed")
	case te, ok := <-w.ch:
		if !ok {
			return TaggedEvent{}, fmt.Errorf("all watchers finished")
		}
		return te, nil
	}
}

// Close unblocks any pending Next call and stops all underlying watchers
// by canceling the child context, which causes blocked watcher.Next()
// calls to return.
func (w *MuxWatcher) Close() error {
	w.closeOnce.Do(func() {
		close(w.done)
		w.cancel()
	})
	return nil
}

// ParseCursor parses a cursor string like "city1:5,city2:12" into a map.
func ParseCursor(s string) map[string]uint64 {
	if s == "" {
		return nil
	}
	m := make(map[string]uint64)
	for _, part := range splitComma(s) {
		city, seqStr, ok := cutColon(part)
		if !ok || city == "" {
			continue
		}
		var seq uint64
		fmt.Sscanf(seqStr, "%d", &seq) //nolint:errcheck // best-effort parse
		m[city] = seq
	}
	return m
}

// FormatCursor formats a cursor map as "city1:5,city2:12".
func FormatCursor(cursors map[string]uint64) string {
	if len(cursors) == 0 {
		return ""
	}
	keys := make([]string, 0, len(cursors))
	for k := range cursors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b []byte
	for i, k := range keys {
		if i > 0 {
			b = append(b, ',')
		}
		b = fmt.Appendf(b, "%s:%d", k, cursors[k])
	}
	return string(b)
}

// splitComma splits s on commas.
func splitComma(s string) []string {
	var parts []string
	for s != "" {
		idx := -1
		for i, c := range s {
			if c == ',' {
				idx = i
				break
			}
		}
		if idx < 0 {
			parts = append(parts, s)
			break
		}
		parts = append(parts, s[:idx])
		s = s[idx+1:]
	}
	return parts
}

// cutColon splits s on the last colon.
func cutColon(s string) (string, string, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == ':' {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

// keepaliveWatcher wraps a MuxWatcher to satisfy the standard Watcher
// interface by converting TaggedEvent to Event (with City embedded in the
// Actor field as "city/actor"). This is a bridge for the existing SSE
// infrastructure which expects events.Watcher.
type keepaliveWatcher struct {
	mux *MuxWatcher
}

// WrapForSSE wraps a MuxWatcher as a standard events.Watcher for use with
// streamEventsWithWatcher. The City is prepended to the Actor field.
func WrapForSSE(mw *MuxWatcher) Watcher {
	return &keepaliveWatcher{mux: mw}
}

func (w *keepaliveWatcher) Next() (Event, error) {
	te, err := w.mux.Next()
	if err != nil {
		return Event{}, err
	}
	e := te.Event
	if te.City != "" {
		e.Actor = te.City + "/" + e.Actor
	}
	return e, nil
}

func (w *keepaliveWatcher) Close() error {
	return w.mux.Close()
}
