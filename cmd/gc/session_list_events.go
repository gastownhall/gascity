package main

import (
	"encoding/json"
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
		cs.sessionList = session.NewListTransitions(session.DefaultListTransitionsCapacity)
	})
	return cs.sessionList
}

// recordSessionListEvent derives session.created and session.closed from a
// bead event the watcher consumed. The watcher sees every writer's bead
// events once (the controller cache's own, a one-shot CLI's, a bd hook's, the
// cache's inferred ones), so the fold in session.ListTransitions is what
// makes each open-set transition one event however often the ledger repeats
// it. id is the event's validated bead id.
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
	listChange, info := cs.sessionListTransitionsOf().Observe(change, b)
	var (
		eventType string
		payload   events.Payload
	)
	switch listChange {
	case session.ListChangeCreated:
		eventType, payload = events.SessionCreated, api.SessionCreatedPayloadFor(info)
	case session.ListChangeClosed:
		eventType, payload = events.SessionClosed, api.SessionClosedPayloadFor(info, session.CloseReasonOf(b))
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
