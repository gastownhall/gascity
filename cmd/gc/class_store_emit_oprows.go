package main

import (
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// Operation-journal rows written by a one-shot CLI (DES-O1 D18).
//
// A journal row (a bead of type "op": an op, fact or runtime row) records no
// bead.* event: its change is announced as op.changed or fact.recorded,
// derived once from the row's post-image by the controller's
// cache-notification bridge (cacheChangeRecorder), and a bead.* event would
// carry a message's text into the event log. So the CLI emitter journals
// nothing for such a row. It rings the controller's lane doorbell instead,
// lane:<conversation>:<row id>[,<row id>...], whose handler refreshes each
// named row through the controller's sessions cache: the cache installs the
// row and announces the change, a close of a row it did not hold included,
// and the bridge derives the event, exactly as for a write the controller
// made itself. A write whose ring no controller answers is announced only
// partly, later: a running controller's next reconcile announces a row it
// finds open and changed, but no close of a row it did not hold (until a
// later ring names the row), and a controller that starts afterwards primes
// its cache silently. The row stays authoritative: a consumer reads it.
// The in-band hook pass defers its rings to the end of the drain
// (deferJournalRings).

// classStoreJournalRing rings the controller of cityPath to refresh the
// journal rows ids of conversation conv; a seam so tests can record it.
var classStoreJournalRing = ringJournalRows

// ringJournalRows rings the lane doorbell for conv's rows ids, best effort,
// with the doorbell's leg (hookLanePokeLeg: inside a hook, cut to the time
// the hook has left, and skipped when it has too little).
func ringJournalRows(cityPath, conv string, ids []string) {
	leg := hookLanePokeLeg(time.Now())
	if leg <= 0 {
		return
	}
	for start := 0; start < len(ids); start += laneSocketMaxRows {
		chunk := ids[start:min(start+laneSocketMaxRows, len(ids))]
		_, _ = sendControllerCommandWithTimeouts(cityPath, laneSocketPrefix+conv+":"+strings.Join(chunk, ","), leg, leg, leg)
	}
}

// ringJournalEmissions takes the journal rows out of emissions, rings the
// controller for them once per conversation (or, when the store defers its
// rings, queues them for its caller to ring: deferJournalRings), and returns
// the rest. A journal row that names no conversation is dropped without a
// ring: it is a row the lane does not read (a DES-BX event-fact or projector
// row).
func (s *emittingClassStore) ringJournalEmissions(emissions []classStoreEmission) []classStoreEmission {
	rest := emissions[:0:0]
	q := s.deferredRings
	if q == nil {
		q = &journalRingQueue{}
	}
	for _, e := range emissions {
		if e.bead.Type != opjournal.BeadType {
			rest = append(rest, e)
			continue
		}
		q.add(journalRowConversation(e.bead.Labels), e.bead.ID)
	}
	if s.deferredRings == nil {
		q.ring(s.cityPath)
	}
	return rest
}

// journalRingQueue collects the journal rows a store's writes changed, per
// conversation in the order first written, each row once, until they are
// rung. It is safe for concurrent use.
type journalRingQueue struct {
	mu    sync.Mutex
	convs []string
	rows  map[string][]string
	seen  map[[2]string]bool
}

// add queues row id of conversation conv; an empty conversation or id, or a
// row already queued, adds nothing.
func (q *journalRingQueue) add(conv, id string) {
	if conv == "" || strings.TrimSpace(id) == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	key := [2]string{conv, id}
	if q.seen[key] {
		return
	}
	if q.seen == nil {
		q.seen, q.rows = map[[2]string]bool{}, map[string][]string{}
	}
	q.seen[key] = true
	if _, ok := q.rows[conv]; !ok {
		q.convs = append(q.convs, conv)
	}
	q.rows[conv] = append(q.rows[conv], id)
}

// ring rings the controller of cityPath once per queued conversation
// (classStoreJournalRing), empties the queue, and returns the conversations
// it rang. A nil queue rings nothing.
func (q *journalRingQueue) ring(cityPath string) map[string]bool {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	convs, rows := q.convs, q.rows
	q.convs, q.rows, q.seen = nil, nil, nil
	q.mu.Unlock()
	rung := make(map[string]bool, len(convs))
	for _, conv := range convs {
		classStoreJournalRing(cityPath, conv, rows[conv])
		rung[conv] = true
	}
	return rung
}

// deferJournalRings makes the CLI emitter under store, when there is one,
// queue its journal-row rings instead of ringing as it writes, and returns
// the queue its caller rings once it is done (journalRingQueue.ring); nil
// when store has no emitter. The in-band hook pass defers them so its
// writes never wait on the controller before the hook's output and
// bookkeeping are done (hookLaneRing).
func deferJournalRings(store beads.Store) *journalRingQueue {
	if inner, _, ok := unwrapBeadPolicyStore(store); ok {
		store = inner
	}
	emitting, ok := store.(*emittingClassStore)
	if !ok {
		return nil
	}
	if emitting.deferredRings == nil {
		emitting.deferredRings = &journalRingQueue{}
	}
	return emitting.deferredRings
}

// journalRowConversation is the conversation a journal row's labels name:
// an op's, a fact's or a runtime row's conversation label. Empty when none.
func journalRowConversation(labels []string) string {
	prefixes := []string{opjournal.ConversationLabel(""), opjournal.FactConversationLabel(""), opjournal.RuntimeConversationLabel("")}
	for _, label := range labels {
		for _, prefix := range prefixes {
			if conv, ok := strings.CutPrefix(label, prefix); ok && conv != "" {
				return conv
			}
		}
	}
	return ""
}
