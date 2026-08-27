package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestCanonicalBeadEventIdentity(t *testing.T) {
	payload, err := json.Marshal(beads.Bead{ID: "dependency-d", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name      string
		event     events.Event
		wantID    string
		wantErr   bool
		unchanged bool
	}{
		{
			name:   "recovers subjectless valid bead snapshot",
			event:  events.Event{Type: events.BeadClosed, Payload: payload},
			wantID: "dependency-d",
		},
		{
			name:   "accepts matching subject",
			event:  events.Event{Type: events.BeadClosed, Subject: "dependency-d", Payload: payload},
			wantID: "dependency-d",
		},
		{
			name:    "rejects mismatched subject",
			event:   events.Event{Type: events.BeadClosed, Subject: "other-dependency", Payload: payload},
			wantErr: true,
		},
		{
			name:      "keeps malformed payload behavior",
			event:     events.Event{Type: events.BeadClosed, Subject: "dependency-d", Payload: []byte(`{`)},
			wantID:    "dependency-d",
			unchanged: true,
		},
		{
			name:      "keeps missing payload behavior",
			event:     events.Event{Type: events.BeadClosed, Subject: "dependency-d"},
			wantID:    "dependency-d",
			unchanged: true,
		},
		{
			name:      "leaves unrelated event unchanged",
			event:     events.Event{Type: "custom.event", Subject: "other-dependency", Payload: payload},
			wantID:    "other-dependency",
			unchanged: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := canonicalBeadEventIdentity(test.event)
			if test.wantErr {
				if err == nil {
					t.Fatal("canonicalBeadEventIdentity error = nil, want identity mismatch")
				}
				return
			}
			if err != nil {
				t.Fatalf("canonicalBeadEventIdentity: %v", err)
			}
			if got.Subject != test.wantID {
				t.Fatalf("canonical subject = %q, want %q", got.Subject, test.wantID)
			}
			if test.unchanged && !reflect.DeepEqual(got, test.event) {
				t.Fatalf("canonical event = %#v, want unchanged %#v", got, test.event)
			}
		})
	}
}

func TestControllerStateCloseEventCanonicalIdentityFencesAndReservesDependency(t *testing.T) {
	for _, test := range []struct {
		name        string
		subjectless bool
	}{
		{name: "subjectless valid snapshot", subjectless: true},
		{name: "matching subject"},
	} {
		t.Run(test.name, func(t *testing.T) {
			backing := beads.NewMemStore()
			dependency, err := backing.Create(beads.Bead{ID: "dependency-d", Type: "task", Status: "open"})
			if err != nil {
				t.Fatalf("create dependency: %v", err)
			}
			cache := beads.NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatalf("prime cache: %v", err)
			}
			if err := backing.Close(dependency.ID); err != nil {
				t.Fatalf("close backing dependency: %v", err)
			}
			closed, err := backing.Get(dependency.ID)
			if err != nil {
				t.Fatalf("get closed dependency: %v", err)
			}
			payload, err := json.Marshal(closed)
			if err != nil {
				t.Fatalf("marshal close snapshot: %v", err)
			}

			cs := &controllerState{cityBeadStore: cache, pokeCh: make(chan struct{}, 1)}
			reserved := make(chan events.Event, 1)
			releaseReservation := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseReservation) }) }
			if err := cs.installSessionWaitDependencyPrePokeAdmission(func(evt events.Event) {
				reserved <- evt
				<-releaseReservation
			}); err != nil {
				t.Fatalf("install pre-poke admission: %v", err)
			}
			t.Cleanup(func() {
				release()
				cs.stopSessionWaitDependencyShadowAdmission()
			})

			done := make(chan struct{})
			subject := dependency.ID
			if test.subjectless {
				subject = ""
			}
			go func() {
				cs.applyBeadEventToStores(events.Event{Type: events.BeadClosed, Subject: subject, Payload: payload})
				close(done)
			}()

			select {
			case evt := <-reserved:
				if evt.Subject != dependency.ID {
					t.Fatalf("reserved dependency = %q, want %q", evt.Subject, dependency.ID)
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("close event did not reserve dependency")
			}
			if got, err := cache.Get(dependency.ID); err != nil || got.Status != "closed" {
				t.Fatalf("cache dependency after close = (%+v, %v), want closed", got, err)
			}
			if cs.sessionWaitDependencyVisibilityMu.TryRLock() {
				cs.sessionWaitDependencyVisibilityMu.RUnlock()
				t.Fatal("cache close became visible before dependency reservation completed")
			}
			release()
			select {
			case <-done:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("close event did not finish after dependency reservation")
			}
		})
	}
}

func TestControllerStateDropsMismatchedBeadEventIdentity(t *testing.T) {
	backing := beads.NewMemStore()
	dependency, err := backing.Create(beads.Bead{ID: "dependency-d", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create dependency: %v", err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("prime cache: %v", err)
	}
	if err := backing.Close(dependency.ID); err != nil {
		t.Fatalf("close backing dependency: %v", err)
	}
	closed, err := backing.Get(dependency.ID)
	if err != nil {
		t.Fatalf("get closed dependency: %v", err)
	}
	payload, err := json.Marshal(closed)
	if err != nil {
		t.Fatalf("marshal close snapshot: %v", err)
	}

	cs := &controllerState{cityBeadStore: cache, pokeCh: make(chan struct{}, 1)}
	reserved := make(chan events.Event, 1)
	if err := cs.installSessionWaitDependencyPrePokeAdmission(func(evt events.Event) { reserved <- evt }); err != nil {
		t.Fatalf("install pre-poke admission: %v", err)
	}
	t.Cleanup(cs.stopSessionWaitDependencyShadowAdmission)

	cs.applyBeadEventToStores(events.Event{
		Type:    events.BeadClosed,
		Subject: "other-dependency",
		Payload: payload,
	})

	got, err := cache.Get(dependency.ID)
	if err != nil {
		t.Fatalf("get cached dependency: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("cache dependency status = %q, want original open after mismatched event", got.Status)
	}
	select {
	case evt := <-reserved:
		t.Fatalf("mismatched event reserved %q, want no reservation", evt.Subject)
	default:
	}
	select {
	case <-cs.pokeCh:
		t.Fatal("mismatched event poked controller")
	default:
	}
	if _, err := canonicalBeadEventIdentity(events.Event{Type: events.BeadClosed, Subject: "other-dependency", Payload: payload}); !errors.Is(err, errBeadEventIdentityMismatch) {
		t.Fatalf("mismatch error = %v, want errBeadEventIdentityMismatch", err)
	}
}
