package workqueue

import "sync"

// Coverage completes when every tracked key has finished a reconcile whose
// item carried LastSeq >= that key's tracked seq. A key that was already
// processing when its tracked Add landed therefore counts only once its
// dirty requeue has run.
//
// The zero value is an open coverage: Track keys as their Adds are accepted,
// then Seal. Until Seal, Observe remembers the highest LastSeq seen per key,
// so a key a live worker reconciled before its Track caught up is covered at
// once. Done can close only after Seal.
type Coverage[K comparable] struct {
	mu      sync.Mutex
	pending map[K]uint64
	seen    map[K]uint64 // highest observed LastSeq per key, until Seal
	sealed  bool
	done    chan struct{}
}

// NewCoverage returns a sealed coverage over each key and seq in tracked. The
// map is copied. An empty map completes at once.
func NewCoverage[K comparable](tracked map[K]uint64) *Coverage[K] {
	c := &Coverage[K]{}
	for k, seq := range tracked {
		c.Track(k, seq)
	}
	c.Seal()
	return c
}

// Track adds k at seq, the seq its Add returned. A key tracked twice keeps the
// higher seq. Track after Seal is ignored.
func (c *Coverage[K]) Track(k K, seq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sealed || c.seen[k] >= seq {
		return
	}
	if c.pending == nil {
		c.pending = make(map[K]uint64)
	}
	c.pending[k] = max(c.pending[k], seq)
}

// Seal ends tracking. Done closes once every tracked key is covered.
func (c *Coverage[K]) Seal() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sealed {
		return
	}
	c.sealed, c.seen = true, nil
	c.closeIfDoneLocked()
}

// Observe records that k finished a reconcile of an item with lastSeq.
// After Seal, untracked keys and earlier seqs are ignored.
func (c *Coverage[K]) Observe(k K, lastSeq uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.sealed {
		if c.seen == nil {
			c.seen = make(map[K]uint64)
		}
		c.seen[k] = max(c.seen[k], lastSeq)
	}
	if seq, ok := c.pending[k]; !ok || lastSeq < seq {
		return
	}
	delete(c.pending, k)
	c.closeIfDoneLocked()
}

// Done is closed once Seal has been called and every tracked key has been
// covered.
func (c *Coverage[K]) Done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.doneLocked()
}

// Pending reports how many tracked keys are not yet covered.
func (c *Coverage[K]) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.pending)
}

func (c *Coverage[K]) doneLocked() chan struct{} {
	if c.done == nil {
		c.done = make(chan struct{})
	}
	return c.done
}

// closeIfDoneLocked closes done once sealed with nothing pending. That state
// is reached once: Seal runs once, and after it Track adds nothing.
func (c *Coverage[K]) closeIfDoneLocked() {
	if c.sealed && len(c.pending) == 0 {
		close(c.doneLocked())
	}
}
