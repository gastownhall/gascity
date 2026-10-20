package main

import (
	"strings"
	"time"

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
// row and announces the change, and the bridge derives the event, exactly as
// for a write the controller made itself. With no controller to answer, the
// cache's next reconcile finds the row changed and announces it then.

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
// controller for them once per conversation, and returns the rest. A journal
// row that names no conversation is dropped without a ring: it is a row the
// lane does not read (a DES-BX event-fact or projector row).
func (s *emittingClassStore) ringJournalEmissions(emissions []classStoreEmission) []classStoreEmission {
	rest := emissions[:0:0]
	var convs []string
	rows := map[string][]string{}
	seen := map[[2]string]bool{}
	for _, e := range emissions {
		if e.bead.Type != opjournal.BeadType {
			rest = append(rest, e)
			continue
		}
		conv := journalRowConversation(e.bead.Labels)
		key := [2]string{conv, e.bead.ID}
		if conv == "" || strings.TrimSpace(e.bead.ID) == "" || seen[key] {
			continue
		}
		seen[key] = true
		if _, ok := rows[conv]; !ok {
			convs = append(convs, conv)
		}
		rows[conv] = append(rows[conv], e.bead.ID)
	}
	for _, conv := range convs {
		classStoreJournalRing(s.cityPath, conv, rows[conv])
	}
	return rest
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
