package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// cacheNotificationActor maps a CachingStore notification's source to the
// actor the controller records it under (see cacheLocalActor).
func cacheNotificationActor(source beads.ChangeSource) string {
	if source.Inferred() {
		return cacheReconcileActor
	}
	return cacheLocalActor
}

// isCacheActor reports whether an event is a CachingStore's own notification,
// whatever its source.
func isCacheActor(actor string) bool {
	return actor == cacheLocalActor || actor == cacheReconcileActor
}

// cacheChangeRecorder returns the onChange a controller CachingStore records
// its notifications through. It drops a bead.updated identical to the last
// notification recorded for the bead: on bd cities 98.9-99.8% of them were
// (mc-zndi7.12), and each one woke the allocator.
func cacheChangeRecorder(recorder events.Recorder) func(source beads.ChangeSource, eventType, beadID, runID, sessionID, stepID string, dependsOnStepIDs *[]string, payload json.RawMessage) {
	dedup := newBeadUpdateDedup(beadUpdateDedupCap)
	return func(source beads.ChangeSource, eventType, beadID, runID, sessionID, stepID string, dependsOnStepIDs *[]string, payload json.RawMessage) {
		if recorder == nil || dedup.suppress(eventType, beadID, payload) {
			return
		}
		recorder.Record(events.Event{
			Type:             eventType,
			Actor:            cacheNotificationActor(source),
			Subject:          beadID,
			RunID:            runID,
			SessionID:        sessionID,
			StepID:           stepID,
			DependsOnStepIDs: dependsOnStepIDs,
			Payload:          payload,
		})
	}
}

// beadUpdateDedupCap bounds the ids one cache's dedup remembers; a close or a
// delete forgets its id. Overflow forgets them all, which costs one repeat
// per id.
const beadUpdateDedupCap = 1 << 16

// beadUpdateDedup remembers the payload hash of the last created or updated
// notification per bead id.
type beadUpdateDedup struct {
	mu   sync.Mutex
	last map[string][sha256.Size]byte
	cap  int
	// suppressed counts dropped repeats, logged so the flapping field behind
	// mc-zndi7.12 can still be chased.
	suppressed atomic.Uint64
}

func newBeadUpdateDedup(capacity int) *beadUpdateDedup {
	return &beadUpdateDedup{last: map[string][sha256.Size]byte{}, cap: capacity}
}

// suppress reports whether a notification repeats the last one recorded for
// beadID, and remembers it otherwise. Only bead.updated is ever suppressed.
func (d *beadUpdateDedup) suppress(eventType, beadID string, payload []byte) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch eventType {
	case events.BeadClosed, events.BeadDeleted:
		delete(d.last, beadID)
		return false
	case events.BeadCreated, events.BeadUpdated:
	default:
		return false
	}
	sum := sha256.Sum256(payload)
	if prev, ok := d.last[beadID]; ok && prev == sum && eventType == events.BeadUpdated {
		if n := d.suppressed.Add(1); n == 1 || n%10000 == 0 {
			log.Printf("caching-store: suppressed %d repeated bead.updated notification(s), latest %s", n, beadID)
		}
		return true
	}
	if _, ok := d.last[beadID]; !ok && len(d.last) >= d.cap {
		d.last = map[string][sha256.Size]byte{}
	}
	d.last[beadID] = sum
	return false
}

func (d *beadUpdateDedup) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.last)
}

// closeVerdict is what a live read says about a close inferred from the cache.
type closeVerdict int

const (
	// closeConfirmed: the read returned the row closed, a committed close.
	closeConfirmed closeVerdict = iota
	// closeRefuted: the row is open (the inferred close was false) or gone
	// (deleted, not completed; every autoclose would find nothing to read).
	closeRefuted
	// closeUnconfirmed: the read failed.
	closeUnconfirmed
)

// confirmInferredClose judges a live Get of a row the cache inferred closed.
func confirmInferredClose(b beads.Bead, err error) closeVerdict {
	switch {
	case errors.Is(err, beads.ErrNotFound):
		return closeRefuted
	case err != nil:
		return closeUnconfirmed
	case b.Status == "closed":
		return closeConfirmed
	default:
		return closeRefuted
	}
}
