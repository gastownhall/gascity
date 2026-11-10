package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/opjournal"
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
// (mc-zndi7.12), and each one woke the allocator. It drops a bead.created
// identical to the last bead.created recorded for the bead too: a refresh
// that installs a new row between the backing create and the cache's absorb
// announces it, and the create announces it again with the same row.
//
// Operation-journal rows (type "op") record no bead.* event (DES-O1 D18): an
// op row's change is recorded as op.changed and a fact row as fact.recorded,
// each derived from the row's post-image (journalRowEvent) and dropped like a
// bead.updated when it repeats the last one recorded for the row; a closed
// op's op.changed and a closed fact's fact.recorded forget the row, as a
// bead.closed does. A one-shot CLI's
// write to a journal row reaches this bridge through the controller's
// doorbell, which refreshes the row through the cache
// (class_store_emit_oprows.go). Every other
// journal row, the runtime row and DES-BX's event-fact and projector rows
// included, records nothing.
func cacheChangeRecorder(recorder events.Recorder) func(source beads.ChangeSource, eventType, beadID, runID, sessionID, stepID string, dependsOnStepIDs *[]string, payload json.RawMessage) {
	dedup := newBeadUpdateDedup(beadUpdateDedupCap)
	return func(source beads.ChangeSource, eventType, beadID, runID, sessionID, stepID string, dependsOnStepIDs *[]string, payload json.RawMessage) {
		if recorder == nil {
			return
		}
		if derived, journal := journalRowEvent(eventType, payload); journal {
			switch {
			case derived.event != nil:
				derived.event.Actor = cacheNotificationActor(source)
				dedup.record(recorder, *derived.event)
				if derived.closed {
					dedup.forget(beadID)
				}
			case eventType == events.BeadDeleted:
				dedup.forget(beadID)
			}
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

// journalTypeMarker is how a journal row's notification payload spells its
// type; a payload without it is not a journal row, which spares every other
// bead's notification a decode.
var journalTypeMarker = []byte(`"issue_type":"` + opjournal.BeadType + `"`)

// journalRowDerived is the event a journal row's post-image derives, nil
// when it derives none; closed says the row is a closed op or a recorded
// (closed) fact, whose last event this is, so the dedup forgets it as it
// forgets a bead.closed's bead.
type journalRowDerived struct {
	event  *events.Event
	closed bool
}

// journalRowEvent reports whether a cache notification is an operation
// journal row's (a bead of type "op") and, for an op or fact row that was not
// deleted, the event its post-image derives: op.changed or fact.recorded,
// whose subject is the row id and session id the conversation. A row that
// does not decode records nothing and is logged: the lane quarantines it and
// a reader reports it.
func journalRowEvent(eventType string, payload json.RawMessage) (journalRowDerived, bool) {
	if !bytes.Contains(payload, journalTypeMarker) {
		return journalRowDerived{}, false
	}
	b, ok := beads.DecodeBeadEventPayload(payload)
	if !ok || b.Type != opjournal.BeadType {
		return journalRowDerived{}, false
	}
	if eventType == events.BeadDeleted {
		return journalRowDerived{}, true
	}
	var (
		e      events.Event
		p      events.Payload
		closed bool
		err    error
	)
	switch {
	case slices.Contains(b.Labels, opjournal.LabelOp):
		var op opjournal.Op
		if op, err = opjournal.DecodeOp(b); err == nil {
			e = events.Event{Type: events.OpChanged, Subject: op.ID, SessionID: op.Conversation}
			p = api.OpChangedPayloadFor(op)
			closed = op.Status == opjournal.StatusClosed
		}
	case slices.Contains(b.Labels, opjournal.LabelFact):
		var f opjournal.Fact
		if f, err = opjournal.DecodeFact(b); err == nil {
			e = events.Event{Type: events.FactRecorded, Subject: f.ID, SessionID: f.Conversation}
			p = api.FactRecordedPayloadFor(f)
			// A fact row is closed once recorded and never changes again.
			closed = b.Status == "closed"
		}
	default:
		return journalRowDerived{}, true
	}
	if err == nil {
		e.Payload, err = json.Marshal(p)
	}
	if err != nil {
		log.Printf("caching-store: journal row %s: no %s event: %v", b.ID, journalEventName(b), err)
		return journalRowDerived{}, true
	}
	return journalRowDerived{event: &e, closed: closed}, true
}

// journalEventName names the event a journal row would derive, for a log line.
func journalEventName(b beads.Bead) string {
	if slices.Contains(b.Labels, opjournal.LabelFact) {
		return events.FactRecorded
	}
	return events.OpChanged
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
// notification recorded per bead id, and when it was recorded. It drops
// repeated notifications, not writes: the cache hands every write to its
// backing on purpose, a same-value one included (see CachingStore.Update),
// and must not regain a cached-row skip to spare a pass-through backing (bd,
// a proxied or HTTP store) the call.
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
	// eventType is the remembered notification's type: a bead.created
	// repeats only a bead.created.
	eventType string
}

func newBeadUpdateDedup(capacity int) *beadUpdateDedup {
	return &beadUpdateDedup{last: map[string]dedupEntry{}, cap: capacity, ttl: beadUpdateDedupTTL, now: time.Now}
}

// record records e unless, within the TTL, it is a bead.updated, op.changed or
// fact.recorded repeating the payload last recorded for its subject, or a
// bead.created repeating the bead.created last recorded for it. Closes and deletes are never suppressed,
// and forget the id, so a create after one is recorded. A payload is
// remembered only once the recorder acknowledged it (events.AckRecorder): a
// recorder that cannot acknowledge, or a dropped append, leaves nothing to
// suppress the next copy. The check, the record and the remember share the
// lock, so one bead's notifications reach the recorder in order.
func (d *beadUpdateDedup) record(recorder events.Recorder, e events.Event) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch e.Type {
	case events.BeadClosed, events.BeadDeleted:
		delete(d.last, e.Subject)
		recorder.Record(e)
		return
	case events.BeadCreated, events.BeadUpdated, events.OpChanged, events.FactRecorded:
	default:
		recorder.Record(e)
		return
	}
	sum := sha256.Sum256(e.Payload)
	now := d.now()
	if prev, ok := d.last[e.Subject]; ok && prev.sum == sum && now.Sub(prev.at) < d.ttl &&
		(e.Type != events.BeadCreated || prev.eventType == events.BeadCreated) {
		if n := d.suppressed.Add(1); n == 1 || n%10000 == 0 {
			log.Printf("caching-store: suppressed %d repeated bead.created/bead.updated notification(s), latest %s %s", n, e.Type, e.Subject)
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
	d.last[e.Subject] = dedupEntry{sum: sum, at: now, eventType: e.Type}
}

// forget drops what d remembers for id, as a close or delete does.
func (d *beadUpdateDedup) forget(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.last, id)
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
