package events

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"
)

const defaultMultiplexerProviderTimeout = 2 * time.Second

// ErrNoWatchers reports that Multiplexer.Watch was called against a
// non-empty set of city providers but none of them could attach a
// watcher. Callers (notably the supervisor SSE endpoint) dispatch on
// this sentinel before committing response headers so the client sees
// 503 instead of 200 followed by an immediate EOF.
var ErrNoWatchers = errors.New("events: no city watchers could be attached")

// TaggedEvent is an Event annotated with the city that produced it.
type TaggedEvent struct {
	Event
	City string `json:"city"`
}

// Multiplexer merges events from multiple city providers into one
// stream, tagging each event with its source city.
type Multiplexer struct {
	mu              sync.RWMutex
	providers       map[string]Provider // city name -> provider
	providerTimeout time.Duration
}

// NewMultiplexer creates a Multiplexer with no providers.
// Use Add/Remove to manage city providers dynamically.
func NewMultiplexer() *Multiplexer {
	return &Multiplexer{
		providers:       make(map[string]Provider),
		providerTimeout: defaultMultiplexerProviderTimeout,
	}
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

func (m *Multiplexer) providerOperationTimeout() time.Duration {
	if m.providerTimeout > 0 {
		return m.providerTimeout
	}
	return defaultMultiplexerProviderTimeout
}

type providerCallResult[T any] struct {
	city  string
	value T
	err   error
}

func collectProviderCallResults[T any](
	providers map[string]Provider,
	timeout time.Duration,
	call func(city string, p Provider) (T, error),
) ([]providerCallResult[T], []string) {
	if len(providers) == 0 {
		return nil, nil
	}

	ch := make(chan providerCallResult[T], len(providers))
	pending := make(map[string]struct{}, len(providers))
	for city, p := range providers {
		pending[city] = struct{}{}
		go func(city string, p Provider) {
			value, err := call(city, p)
			ch <- providerCallResult[T]{city: city, value: value, err: err}
		}(city, p)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	results := make([]providerCallResult[T], 0, len(providers))
	for len(pending) > 0 {
		select {
		case result := <-ch:
			if _, ok := pending[result.city]; !ok {
				continue
			}
			delete(pending, result.city)
			results = append(results, result)
		case <-timer.C:
			return results, sortedProviderNames(pending)
		}
	}
	return results, nil
}

func sortedProviderNames(providers map[string]struct{}) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ListAll returns events from all cities matching the filter, sorted by
// timestamp, city, and sequence. Each event is tagged with its source city.
// A positive filter Limit returns the earliest matching events after that
// global sort; callers needing the latest matching events should use ListTail.
func (m *Multiplexer) ListAll(filter Filter) ([]TaggedEvent, error) {
	providers := m.snapshot()
	providerFilter := filter
	providerFilter.Limit = 0
	var all []TaggedEvent
	timeout := m.providerOperationTimeout()
	results, timedOut := collectProviderCallResults(providers, timeout, func(_ string, p Provider) ([]Event, error) {
		return p.List(providerFilter)
	})
	for _, city := range timedOut {
		log.Printf("events: list all timed out for city %q after %s", city, timeout)
	}
	for _, result := range results {
		if result.err != nil {
			continue // best-effort: skip cities with errors
		}
		city := result.city
		evts := result.value
		for _, e := range evts {
			all = append(all, TaggedEvent{Event: e, City: city})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return taggedEventLess(all[i], all[j])
	})
	if filter.Limit > 0 && len(all) > filter.Limit {
		all = all[:filter.Limit]
	}
	return all, nil
}

// ListTail returns the trailing matching events across all cities. It asks
// tail-capable providers for only their local tail, then trims the merged
// result to the requested global limit.
func (m *Multiplexer) ListTail(filter Filter, limit int) ([]TaggedEvent, error) {
	if limit <= 0 {
		return m.ListAll(filter)
	}
	providers := m.snapshot()
	providerFilter := filter
	providerFilter.Limit = 0
	var all []TaggedEvent
	timeout := m.providerOperationTimeout()
	results, timedOut := collectProviderCallResults(providers, timeout, func(_ string, p Provider) ([]Event, error) {
		var evts []Event
		var err error
		if tail, ok := p.(TailProvider); ok {
			evts, err = tail.ListTail(providerFilter, limit)
		} else {
			evts, err = p.List(providerFilter)
			if limit < len(evts) {
				evts = evts[len(evts)-limit:]
			}
		}
		return evts, err
	})
	for _, city := range timedOut {
		log.Printf("events: list tail timed out for city %q after %s", city, timeout)
	}
	for _, result := range results {
		if result.err != nil {
			log.Printf("events: list tail failed for city %q: %v", result.city, result.err)
			continue // best-effort: skip cities with errors
		}
		city := result.city
		evts := result.value
		for _, e := range evts {
			all = append(all, TaggedEvent{Event: e, City: city})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return taggedEventLess(all[i], all[j])
	})
	if limit < len(all) {
		all = all[len(all)-limit:]
	}
	return all, nil
}

func taggedEventLess(left, right TaggedEvent) bool {
	if !left.Ts.Equal(right.Ts) {
		return left.Ts.Before(right.Ts)
	}
	if left.City != right.City {
		return left.City < right.City
	}
	return left.Seq < right.Seq
}

// LatestCursor returns the current highest sequence number for each provider.
// Providers that fail are skipped, matching ListAll's best-effort aggregation.
func (m *Multiplexer) LatestCursor() (map[string]uint64, error) {
	providers := m.snapshot()
	cursors := make(map[string]uint64, len(providers))
	var errs []error
	timeout := m.providerOperationTimeout()
	results, timedOut := collectProviderCallResults(providers, timeout, func(_ string, p Provider) (uint64, error) {
		return p.LatestSeq()
	})
	for _, city := range timedOut {
		err := fmt.Errorf("%s: events provider timed out after %s", city, timeout)
		log.Printf("events: latest cursor failed for city %q: %v", city, err)
		errs = append(errs, err)
	}
	for _, result := range results {
		if result.err != nil {
			log.Printf("events: latest cursor failed for city %q: %v", result.city, result.err)
			errs = append(errs, fmt.Errorf("%s: %w", result.city, result.err))
			continue
		}
		cursors[result.city] = result.value
	}
	return cursors, errors.Join(errs...)
}

// Watch returns a Watcher that merges events from all currently registered
// city providers. Events are yielded in approximate time order. The cursor
// is a map of city→seq positions (use ParseCursor/FormatCursor to persist).
//
// Returns ErrNoWatchers when providers are registered but none of them
// could attach a watcher — callers use this to fail fast with 503
// before committing SSE response headers.
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
	attached := 0
	timeout := m.providerOperationTimeout()
	attachResults, timedOut := collectWatchAttachResults(childCtx, providers, cursors, timeout)
	for _, city := range timedOut {
		log.Printf("events: mux watcher attach timed out for city %q after %s", city, timeout)
	}
	for _, result := range attachResults {
		if result.err != nil {
			// Log so operators can diagnose one-bad-city scenarios.
			// Previously silent; the SSE endpoint would commit headers
			// and immediately EOF when every watcher dropped out.
			log.Printf("events: mux watcher attach failed for city %q: %v", result.city, result.err)
			continue
		}
		// Defensive: a Provider returning (nil, nil) would panic the goroutine below on Next().
		if result.watcher == nil {
			log.Printf("events: mux watcher attach failed for city %q: nil watcher", result.city)
			continue
		}
		attached++
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
		}(result.city, result.watcher)
	}

	if len(providers) > 0 && attached == 0 {
		cancel()
		close(w.ch)
		return nil, ErrNoWatchers
	}

	// Close the channel when all watchers finish.
	go func() {
		wg.Wait()
		close(w.ch)
	}()

	return w, nil
}

type watchAttachResult struct {
	city    string
	watcher Watcher
	err     error
}

func collectWatchAttachResults(
	ctx context.Context,
	providers map[string]Provider,
	cursors map[string]uint64,
	timeout time.Duration,
) ([]watchAttachResult, []string) {
	if len(providers) == 0 {
		return nil, nil
	}

	ch := make(chan watchAttachResult)
	abandoned := make(chan struct{})
	defer close(abandoned)

	pending := make(map[string]struct{}, len(providers))
	for city, p := range providers {
		pending[city] = struct{}{}
		go func(city string, p Provider) {
			watcher, err := p.Watch(ctx, cursors[city])
			result := watchAttachResult{city: city, watcher: watcher, err: err}
			select {
			case ch <- result:
			case <-abandoned:
				if watcher != nil {
					_ = watcher.Close()
				}
			}
		}(city, p)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	results := make([]watchAttachResult, 0, len(providers))
	for len(pending) > 0 {
		select {
		case result := <-ch:
			if _, ok := pending[result.city]; !ok {
				continue
			}
			delete(pending, result.city)
			results = append(results, result)
		case <-timer.C:
			return results, sortedProviderNames(pending)
		}
	}
	return results, nil
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
