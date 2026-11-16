// Package fanouttest holds test helpers for code that runs calls through
// fanout.Bounded.
package fanouttest

import (
	"slices"
	"sync"
	"sync/atomic"
)

// Probe counts how many calls are running at once. Every call parks on a
// release channel until the test opens it, so inside a testing/synctest bubble
// synctest.Wait returns only once every goroutine is either parked here or
// waiting for a slot: the running count at that moment is exactly the fan-out
// the caller allowed, with no timing involved.
type Probe struct {
	release chan struct{}
	once    sync.Once
	running atomic.Int64
	peak    atomic.Int64
	mu      sync.Mutex
	visited []int
}

// NewProbe returns a closed-gate probe: every Call parks until Open.
func NewProbe() *Probe {
	return &Probe{release: make(chan struct{})}
}

// Call records call i running, parks until Open, then records it done.
func (p *Probe) Call(i int) {
	n := p.running.Add(1)
	defer p.running.Add(-1)
	for {
		peak := p.peak.Load()
		if n <= peak || p.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	<-p.release
	p.mu.Lock()
	p.visited = append(p.visited, i)
	p.mu.Unlock()
}

// Open lets every parked call return. It is idempotent so a test can both call
// it and defer it: the deferred call frees parked goroutines when a failed
// assertion ends the test early, which would otherwise deadlock the bubble.
func (p *Probe) Open() { p.once.Do(func() { close(p.release) }) }

// Running is how many calls are parked in the probe right now.
func (p *Probe) Running() int64 { return p.running.Load() }

// Peak is the most calls that were ever parked in the probe at once.
func (p *Probe) Peak() int64 { return p.peak.Load() }

// SortedVisits is every call index that has returned, sorted.
func (p *Probe) SortedVisits() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := slices.Clone(p.visited)
	slices.Sort(out)
	return out
}
