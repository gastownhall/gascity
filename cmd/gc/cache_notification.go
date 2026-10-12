package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

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
		if recorder == nil {
			return
		}
		dedup.record(recorder, events.Event{
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

const (
	// beadUpdateDedupCap bounds the ids one cache's dedup remembers; a close
	// or a delete forgets its id. Overflow forgets them all, which costs one
	// repeat per id.
	beadUpdateDedupCap = 1 << 16
	// beadUpdateDedupTTL bounds how long a remembered payload suppresses its
	// repeats. A consumer that missed the one recorded copy (a dropped
	// append, a watcher that started late) sees the state again within this
	// age, the heal-by-repeat the duplicates used to give, at one repeat per
	// changed bead per 20 minutes instead of one per scan.
	beadUpdateDedupTTL = 20 * time.Minute
)

// beadUpdateDedup remembers the payload hash of the last created or updated
// notification recorded per bead id, and when it was recorded.
type beadUpdateDedup struct {
	mu   sync.Mutex
	last map[string]dedupEntry
	cap  int
	ttl  time.Duration
	now  func() time.Time
	// suppressed counts dropped repeats, logged so the flapping field behind
	// mc-zndi7.12 can still be chased.
	suppressed atomic.Uint64
}

type dedupEntry struct {
	sum [sha256.Size]byte
	at  time.Time
}

func newBeadUpdateDedup(capacity int) *beadUpdateDedup {
	return &beadUpdateDedup{last: map[string]dedupEntry{}, cap: capacity, ttl: beadUpdateDedupTTL, now: time.Now}
}

// record records e unless it is a bead.updated repeating the payload last
// recorded for its bead within the TTL. Only bead.updated is ever suppressed;
// a close or a delete forgets the id. A payload is remembered only once the
// recorder acknowledged it (events.AckRecorder): a recorder that cannot
// acknowledge, or a dropped append, leaves nothing to suppress the next copy.
// The check, the record and the remember share the lock, so one bead's
// notifications reach the recorder in order.
func (d *beadUpdateDedup) record(recorder events.Recorder, e events.Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch e.Type {
	case events.BeadClosed, events.BeadDeleted:
		delete(d.last, e.Subject)
		recorder.Record(e)
		return
	case events.BeadCreated, events.BeadUpdated:
	default:
		recorder.Record(e)
		return
	}
	sum := sha256.Sum256(e.Payload)
	now := d.now()
	if prev, ok := d.last[e.Subject]; ok && prev.sum == sum && now.Sub(prev.at) < d.ttl && e.Type == events.BeadUpdated {
		if n := d.suppressed.Add(1); n == 1 || n%10000 == 0 {
			log.Printf("caching-store: suppressed %d repeated bead.updated notification(s), latest %s", n, e.Subject)
		}
		return
	}
	acker, ok := recorder.(events.AckRecorder)
	if !ok {
		delete(d.last, e.Subject)
		recorder.Record(e)
		return
	}
	if err := acker.RecordAck(e); err != nil {
		delete(d.last, e.Subject)
		return
	}
	if _, ok := d.last[e.Subject]; !ok && len(d.last) >= d.cap {
		d.last = map[string]dedupEntry{}
	}
	d.last[e.Subject] = dedupEntry{sum: sum, at: now}
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
