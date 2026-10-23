package main

import (
	"crypto/sha256"
	"encoding/json"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// cacheChangeRecorder returns the onChange a controller CachingStore records
// its notifications through. It drops a bead.updated identical to the last
// notification recorded for the bead: on bd cities 98.9-99.8% of them were
// (mc-zndi7.12), and each one woke the allocator.
func cacheChangeRecorder(recorder events.Recorder) func(eventType, beadID, runID, sessionID, stepID string, dependsOnStepIDs *[]string, payload json.RawMessage) {
	dedup := newBeadUpdateDedup(beadUpdateDedupCap)
	return func(eventType, beadID, runID, sessionID, stepID string, dependsOnStepIDs *[]string, payload json.RawMessage) {
		if recorder == nil {
			return
		}
		dedup.record(recorder, events.Event{
			Type:             eventType,
			Actor:            cacheReconcileActor,
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
		// RecordAck hands back the error Record would have logged, so log it
		// here: a dropped append must not go unseen. Nothing is remembered,
		// so the next copy of this payload is recorded.
		log.Printf("caching-store: recording %s %s: %v", e.Type, e.Subject, err)
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
