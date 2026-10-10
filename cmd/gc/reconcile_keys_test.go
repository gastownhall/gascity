package main

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

// Shared fixtures for the v2 tests, kept from the deleted per-key router and
// runtime tests (C3).

const routerTestLeg = "city:test"

func routerSessionBead(id string, meta map[string]string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Type:     session.BeadType,
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	}
}

func routerWorkBead(id, status, assignee string) beads.Bead {
	return beads.Bead{ID: id, Type: "task", Status: status, Assignee: assignee}
}

func routedWorkBead(id, status, assignee, route string) beads.Bead {
	b := routerWorkBead(id, status, assignee)
	if route != "" {
		b.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: route}
	}
	return b
}

func beadEvent(t *testing.T, eventType string, b beads.Bead) events.Event {
	t.Helper()
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		t.Fatalf("encoding %s: %v", b.ID, err)
	}
	return events.Event{Type: eventType, Subject: b.ID, Payload: payload}
}

// advance moves a synctest bubble's clock by d and lets it settle.
func advance(d time.Duration) {
	<-time.After(d)
	synctest.Wait()
}
