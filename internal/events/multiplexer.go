package events

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
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

// ErrMuxWatcherFinished reports that MuxWatcher.Sync was called on a watcher
// that was closed, or whose city watchers have all ended. Such a watcher can
// no longer deliver events, so it refuses to attach more cities.
var ErrMuxWatcherFinished = errors.New("events: mux watcher is closed or finished")

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
// Use MuxWatcher.Sync to attach or detach cities after Watch returns.
//
// Returns ErrNoWatchers when providers are registered but none of them
// could attach a watcher — callers use this to fail fast with 503
// before committing SSE response headers.
func (m *Multiplexer) Watch(ctx context.Context, cursors map[string]uint64) (*MuxWatcher, error) {
	providers := m.snapshot()
	childCtx, cancel := context.WithCancel(ctx)
	w := &MuxWatcher{
		ctx:     childCtx,
		cancel:  cancel,
		ch:      make(chan TaggedEvent, 16),
		done:    make(chan struct{}),
		timeout: m.providerOperationTimeout(),
		cities:  make(map[string]*muxCity),
		holds:   1, // keep ch open until the initial attach completes
	}

	attached := 0
	start := func(city string, _ Provider) (uint64, error) { return cursors[city], nil }
	attachResults, timedOut := collectWatchAttachResults(childCtx, providers, start, w.timeout)
	for _, city := range timedOut {
		log.Printf("events: mux watcher attach timed out for city %q after %s", city, w.timeout)
	}
	for _, result := range attachResults {
		if result.err != nil {
			// Log so operators can diagnose one-bad-city scenarios.
			// Previously silent; the SSE endpoint would commit headers
			// and immediately EOF when every watcher dropped out.
			log.Printf("events: mux watcher attach failed for city %q: %v", result.city, result.err)
			continue
		}
		if err := w.startCity(result); err != nil {
			log.Printf("events: mux watcher attach failed for city %q: %v", result.city, err)
			continue
		}
		attached++
	}

	if len(providers) > 0 && attached == 0 {
		cancel()
		w.release()
		return nil, ErrNoWatchers
	}
	w.release()
	return w, nil
}

type watchAttachResult struct {
	city    string
	seq     uint64 // the watcher resumes after this seq
	ctx     context.Context
	cancel  context.CancelFunc
	watcher Watcher
	err     error
}

// collectWatchAttachResults attaches a watcher to every provider in parallel,
// each under its own child context of ctx so it can be detached on its own.
// start picks the seq each watcher resumes after. Providers that do not answer
// within timeout are reported by name; their late watchers are closed.
func collectWatchAttachResults(
	ctx context.Context,
	providers map[string]Provider,
	start func(city string, p Provider) (uint64, error),
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
			cityCtx, cityCancel := context.WithCancel(ctx)
			result := watchAttachResult{city: city, ctx: cityCtx, cancel: cityCancel}
			seq, err := start(city, p)
			if err != nil {
				result.err = fmt.Errorf("resolving start cursor: %w", err)
			} else {
				result.seq = seq
				result.watcher, result.err = p.Watch(cityCtx, seq)
				// Defensive: a Provider returning (nil, nil) would panic the
				// fan-in goroutine on Next().
				if result.err == nil && result.watcher == nil {
					result.err = errors.New("nil watcher")
				}
			}
			if result.err != nil {
				if result.watcher != nil {
					_ = result.watcher.Close()
					result.watcher = nil
				}
				cityCancel()
			}
			select {
			case ch <- result:
			case <-abandoned:
				if result.watcher != nil {
					_ = result.watcher.Close()
				}
				cityCancel()
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
//
// The set of watched cities is not fixed: Sync attaches cities that appeared
// after Watch and detaches cities that went away. Next reports that all
// watchers finished once no city is attached and no Sync is in progress.
type MuxWatcher struct {
	ctx       context.Context
	cancel    context.CancelFunc
	ch        chan TaggedEvent
	done      chan struct{}
	closeOnce sync.Once
	timeout   time.Duration

	mu       sync.Mutex
	cities   map[string]*muxCity // attached cities by name
	active   int                 // running fan-in goroutines
	holds    int                 // in-progress attaches that keep ch open
	finished bool                // ch is closed
}

// muxCity is one attached city's fan-in goroutine.
type muxCity struct {
	cancel context.CancelFunc
}

// startCity registers an attached city and starts its fan-in goroutine. It
// takes ownership of result's watcher and context, releasing them when the
// city cannot be started.
func (w *MuxWatcher) startCity(result watchAttachResult) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var err error
	switch {
	case w.finished || w.isClosed():
		err = ErrMuxWatcherFinished
	case w.cities[result.city] != nil:
		err = fmt.Errorf("city %q is already attached", result.city)
	}
	if err != nil {
		_ = result.watcher.Close()
		result.cancel()
		return err
	}
	c := &muxCity{cancel: result.cancel}
	w.cities[result.city] = c
	w.active++
	go w.pump(result.ctx, result.city, c, result.watcher)
	return nil
}

// pump forwards one city's events into the merged channel until its watcher
// ends, the city is detached, or the MuxWatcher is closed.
func (w *MuxWatcher) pump(ctx context.Context, city string, c *muxCity, watcher Watcher) {
	defer w.cityDone(city, c)
	defer watcher.Close() //nolint:errcheck
	for {
		e, err := watcher.Next()
		if err != nil {
			if ctx.Err() == nil && !w.isClosed() {
				log.Printf("events: mux watcher for city %q ended: %v", city, err)
			}
			return
		}
		select {
		case w.ch <- TaggedEvent{Event: e, City: city}:
		case <-ctx.Done():
			return
		case <-w.done:
			return
		}
	}
}

func (w *MuxWatcher) cityDone(city string, c *muxCity) {
	c.cancel()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cities[city] == c {
		delete(w.cities, city)
	}
	w.active--
	w.finishIfIdleLocked()
}

// release drops an attach hold taken by Watch or Sync.
func (w *MuxWatcher) release() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.holds--
	w.finishIfIdleLocked()
}

// finishIfIdleLocked closes the merged channel once no fan-in goroutine is
// running and no attach is in progress, so Next reports that all watchers
// finished. PRECONDITION: w.mu is held.
func (w *MuxWatcher) finishIfIdleLocked() {
	if w.active == 0 && w.holds == 0 && !w.finished {
		w.finished = true
		close(w.ch)
	}
}

func (w *MuxWatcher) isClosed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// Cities returns the names of the currently attached cities, sorted.
func (w *MuxWatcher) Cities() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	names := make([]string, 0, len(w.cities))
	for name := range w.cities {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Sync reconciles the watched cities with providers. Attached cities missing
// from providers are detached: their watcher is closed and their goroutine
// exits. Providers whose city is not attached — new cities, or cities whose
// watcher ended — are attached, each resuming after the seq start returns for
// it. Cities already attached are left alone.
//
// Sync returns the start seq of every city it attached. Cities that could not
// be attached are reported in the joined error and stay detached, so a later
// Sync retries them; the cities that did attach are unaffected. It returns
// ErrMuxWatcherFinished, attaching nothing, when the watcher was closed or all
// its city watchers had already ended.
func (w *MuxWatcher) Sync(providers map[string]Provider, start func(city string, p Provider) (uint64, error)) (map[string]uint64, error) {
	w.mu.Lock()
	if w.finished || w.isClosed() {
		w.mu.Unlock()
		return nil, ErrMuxWatcherFinished
	}
	w.holds++
	for city, c := range w.cities {
		if _, ok := providers[city]; !ok {
			delete(w.cities, city)
			c.cancel()
		}
	}
	missing := make(map[string]Provider)
	for city, p := range providers {
		if _, ok := w.cities[city]; !ok {
			missing[city] = p
		}
	}
	w.mu.Unlock()
	defer w.release()

	var errs []error
	results, timedOut := collectWatchAttachResults(w.ctx, missing, start, w.timeout)
	for _, city := range timedOut {
		errs = append(errs, fmt.Errorf("%s: events watcher attach timed out after %s", city, w.timeout))
	}
	started := make(map[string]uint64, len(results))
	for _, result := range results {
		if result.err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.city, result.err))
			continue
		}
		if err := w.startCity(result); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", result.city, err))
			continue
		}
		started[result.city] = result.seq
	}
	return started, errors.Join(errs...)
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
		seq, err := strconv.ParseUint(seqStr, 10, 64)
		if err != nil {
			continue
		}
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
