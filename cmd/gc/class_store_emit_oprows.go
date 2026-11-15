package main

import (
	"errors"
	"fmt"
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
// made itself. A refresh that a local write of the cache holds off, or that
// fails, the controller retries for up to a minute (refreshRow), and one a
// quiescent city skips until it resumes, so a ring needs no retry of its
// own. A write whose ring no controller answers is
// announced only partly, later: a running controller's next reconcile
// announces a row it finds open and changed, but no close of a row it did
// not hold (until a later ring names the row), and a controller that starts
// afterwards primes its cache silently. The row stays authoritative: a
// consumer reads it. The in-band hook pass defers its rings to the end of
// the drain (deferJournalRings).

// classStoreJournalRing rings the controller of cityPath to refresh the
// journal rows ids of conversation conv, and reports whether the doorbell
// poked conv; a seam so tests can record it.
var classStoreJournalRing = ringJournalRows

// ringJournalRows rings the lane doorbell for conv's rows ids, best effort,
// with the doorbell's leg (hookLanePokeLeg: inside a hook, cut to the time
// the hook has left, and skipped when it has too little). It reports whether
// a doorbell answered that it poked conv (laneReplyPoked). A ring the hook's
// deadline leaves no leg for, and one a controller takes but does not answer,
// is a missed ring, warned of once per process; a controller that is not
// running is not warned of.
func ringJournalRows(cityPath, conv string, ids []string) bool {
	leg := hookLanePokeLeg(time.Now())
	if leg <= 0 {
		warnClassStoreEmit(fmt.Errorf("not ringing the controller for the journal rows of conversation %s: the hook's deadline leaves the doorbell too little time, or does not parse", conv))
		return false
	}
	poked := false
	for start := 0; start < len(ids); start += laneSocketMaxRows {
		chunk := ids[start:min(start+laneSocketMaxRows, len(ids))]
		reply, err := sendControllerCommandWithTimeouts(cityPath, laneSocketPrefix+conv+":"+strings.Join(chunk, ","), leg, leg, leg)
		switch {
		case err == nil:
			poked = poked || laneReplyPoked(string(reply))
		case !errors.Is(err, errControllerUnavailable):
			warnClassStoreEmit(fmt.Errorf("ringing the controller for the journal rows of conversation %s: %w", conv, err))
		}
	}
	return poked
}

// ringJournalEmissions takes the journal rows out of emissions, rings the
// controller for them once per conversation (or, when the store defers its
// rings, queues them for its caller to ring: deferJournalRings), and returns
// the rest. A journal row is a bead of the journal's type, or one with a
// pinned id (opjournal.IsPinnedID): the row a write's read-miss fallback
// carries has its id alone. A journal row that names no conversation is
// dropped without a ring: it is a row the lane does not read (a DES-BX
// event-fact or projector row). One known by its id alone is dropped with a
// warning: its conversation could not be read, and the controller hears of
// the write only from its cache's next reconcile.
func (s *emittingClassStore) ringJournalEmissions(emissions []classStoreEmission) []classStoreEmission {
	rest := emissions[:0:0]
	q := s.deferredRings
	if q == nil {
		q = &journalRingQueue{}
	}
	for _, e := range emissions {
		switch {
		case e.bead.Type == opjournal.BeadType:
			q.add(journalRowConversation(e.bead.Labels), e.bead.ID)
		case opjournal.IsPinnedID(e.bead.ID):
			warnClassStoreEmit(fmt.Errorf("%s of journal row %s neither journaled nor rung: the row could not be read after the write", e.eventType, e.bead.ID))
		default:
			rest = append(rest, e)
		}
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
// whose doorbell answered that it poked them. A nil queue rings nothing.
func (q *journalRingQueue) ring(cityPath string) map[string]bool {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	convs, rows := q.convs, q.rows
	q.convs, q.rows, q.seen = nil, nil, nil
	q.mu.Unlock()
	poked := make(map[string]bool, len(convs))
	for _, conv := range convs {
		if classStoreJournalRing(cityPath, conv, rows[conv]) {
			poked[conv] = true
		}
	}
	return poked
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
