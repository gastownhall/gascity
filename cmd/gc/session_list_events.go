package main

import (
	"encoding/json"
	"errors"
	"log"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// sessionListBeadChange maps a bead.* event type onto the ledger mutation
// session.ListTransitions folds.
func sessionListBeadChange(eventType string) (session.BeadChange, bool) {
	switch eventType {
	case events.BeadCreated:
		return session.BeadChangeCreated, true
	case events.BeadUpdated:
		return session.BeadChangeUpdated, true
	case events.BeadClosed:
		return session.BeadChangeClosed, true
	case events.BeadDeleted:
		return session.BeadChangeDeleted, true
	}
	return 0, false
}

// sessionListTransitionsOf returns the controller's session list fold.
func (cs *controllerState) sessionListTransitionsOf() *session.ListTransitions {
	cs.sessionListOnce.Do(func() {
		cs.sessionList = session.NewListTransitions(session.DefaultListTransitionsCapacity, cs.servedSessionRow)
	})
	return cs.sessionList
}

// servedSessionRow reads id from the city's session store, the cache-first
// store GET /v0/city/{cityName}/sessions lists. The watcher reads it after
// the event was applied to the caches, so the row is what the cache kept: an
// event the cache refused (a stale close, a snapshot older than the cached
// row) does not show in it.
func (cs *controllerState) servedSessionRow(id string) (beads.Bead, bool, error) {
	sessions := cs.Stores().Sessions()
	if sessions == (beads.SessionStore{}) {
		return beads.Bead{}, false, errors.New("no session store")
	}
	b, err := sessions.Get(id)
	if errors.Is(err, beads.ErrNotFound) {
		return beads.Bead{}, false, nil
	}
	if err != nil {
		return beads.Bead{}, false, err
	}
	return b, true, nil
}

// recordSessionListEvent derives session.created and session.closed from a
// bead event the watcher consumed, after the event was applied to the
// caches. The watcher sees every writer's bead events once (the controller
// cache's own, a one-shot CLI's, a bd hook's, the cache's inferred ones), so
// the fold in session.ListTransitions is what makes each open-set transition
// one event however often the ledger repeats it, and what keeps a stale one
// from announcing a transition the session store does not show. id is the
// event's validated bead id.
func (cs *controllerState) recordSessionListEvent(evt events.Event, id string) {
	change, ok := sessionListBeadChange(evt.Type)
	if !ok {
		return
	}
	b := beads.Bead{ID: id}
	if change != session.BeadChangeDeleted {
		decoded, ok := beads.DecodeBeadEventPayload(evt.Payload)
		if !ok {
			return
		}
		b = decoded
	}
	transition, err := cs.sessionListTransitionsOf().Observe(change, b)
	if err != nil {
		log.Printf("session list: %s seq=%d for %s not confirmed: %v", evt.Type, evt.Seq, id, err)
		return
	}
	var (
		eventType string
		payload   events.Payload
	)
	switch transition.Change {
	case session.ListChangeCreated:
		eventType, payload = events.SessionCreated, api.SessionCreatedPayloadFor(transition.Session)
	case session.ListChangeClosed:
		eventType, payload = events.SessionClosed, api.SessionClosedPayloadFor(transition.Session, transition.CloseReason)
	default:
		return
	}
	ep := cs.EventProvider()
	if ep == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		log.Printf("session list: %s for %s not recorded: %v", eventType, id, err)
		return
	}
	ep.Record(events.Event{
		Type:      eventType,
		Actor:     evt.Actor,
		Subject:   id,
		SessionID: id,
		Payload:   raw,
	})
}
