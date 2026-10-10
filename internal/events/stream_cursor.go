package events

import "sync"

// maxDepartedCities bounds how many departed cities a StreamCursor remembers
// for an in-stream return. A long-lived stream on a supervisor whose cities
// churn (test cities, ephemeral cities) would otherwise grow without bound.
const maxDepartedCities = 64

// StreamCursor is the per-city resume position of one long-lived merged event
// stream whose city set changes while it is open (#6861). It is the composite
// SSE id's source of truth: Format renders the cities currently in the
// provider set, each at the last seq the stream delivered (or attached at).
//
// A city that leaves the provider set is pruned from the composite cursor, so
// cursors for cities that are gone do not accumulate in every SSE id. Its last
// position is remembered (up to maxDepartedCities, oldest forgotten first) so
// that if it comes back while the stream is open it resumes there, with no gap
// and no duplicate. Advance rejects a seq at or below the city's known
// position, which drops the events a re-attached watcher replays that the
// stream already delivered.
//
// It is safe for concurrent use: the stream loop advances it while a
// background membership sync reads and reconciles it.
type StreamCursor struct {
	mu       sync.Mutex
	live     map[string]uint64
	departed map[string]uint64
	order    []string // departed cities, oldest first
}

// NewStreamCursor returns a cursor starting at initial, typically the resolved
// per-city start positions of the stream's initial Watch.
func NewStreamCursor(initial map[string]uint64) *StreamCursor {
	live := make(map[string]uint64, len(initial))
	for city, seq := range initial {
		live[city] = seq
	}
	return &StreamCursor{live: live, departed: make(map[string]uint64)}
}

// Advance records that the stream is delivering city's event seq. It reports
// false, recording nothing, when seq is at or below the city's known position:
// the stream already delivered it and must drop it. An event from a departed
// city (still buffered when it left) advances its remembered position without
// returning it to the composite cursor.
func (c *StreamCursor) Advance(city string, seq uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pos, ok := c.departed[city]; ok {
		if seq <= pos {
			return false
		}
		c.departed[city] = seq
		return true
	}
	if pos, ok := c.live[city]; ok && seq <= pos {
		return false
	}
	c.live[city] = seq
	return true
}

// Resume returns the position a city attaching to the stream resumes after:
// its position in the composite cursor, or its remembered position if it
// departed earlier in this stream. ok is false for a city the stream has never
// known, which the caller starts at its head (or from zero on request).
func (c *StreamCursor) Resume(city string) (uint64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if seq, ok := c.live[city]; ok {
		return seq, true
	}
	seq, ok := c.departed[city]
	return seq, ok
}

// Retain prunes the composite cursor to the cities in providers, the provider
// set a membership sync is about to reconcile the watcher with. Cities absent
// from it depart: they leave the composite cursor but their position is
// remembered (bounded by maxDepartedCities, oldest forgotten first) for an
// in-stream return. Call it before the sync detaches their watchers, so no
// frame sent after a detach still names a departed city. A city still in
// providers whose watcher ended keeps its position, so the sync resumes it.
func (c *StreamCursor) Retain(providers map[string]Provider) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for city, seq := range c.live {
		if _, ok := providers[city]; ok {
			continue
		}
		delete(c.live, city)
		c.departed[city] = seq
		c.order = append(c.order, city)
	}
	for len(c.order) > maxDepartedCities {
		delete(c.departed, c.order[0])
		c.order = c.order[1:]
	}
}

// Attach records the cities a membership sync attached and the seq each
// resumes after. They join the composite cursor (a city already in it keeps
// its position); a departed city that returned resumes at the later of its
// remembered position and its attach seq.
func (c *StreamCursor) Attach(started map[string]uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for city, seq := range started {
		if pos, ok := c.departed[city]; ok {
			delete(c.departed, city)
			c.dropOrderLocked(city)
			if pos > seq {
				seq = pos
			}
		}
		if _, ok := c.live[city]; !ok {
			c.live[city] = seq
		}
	}
}

func (c *StreamCursor) dropOrderLocked(city string) {
	for i, name := range c.order {
		if name == city {
			c.order = append(c.order[:i], c.order[i+1:]...)
			return
		}
	}
}

// Format renders the composite cursor ("city1:5,city2:12") of the cities in
// the stream's provider set.
func (c *StreamCursor) Format() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return FormatCursor(c.live)
}

// departedLen reports how many departed cities are remembered (tests).
func (c *StreamCursor) departedLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.departed)
}
