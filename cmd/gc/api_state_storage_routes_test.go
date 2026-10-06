package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// messagingSplitRoutes builds the routes a converged split city runs on: work
// keeps its own ledger and every infrastructure class is served by one shared
// binding, which is the arrangement storageSplitWhole names and
// openStorageRoutes produces.
func messagingSplitRoutes(infra beads.Store) *storageRoutes {
	routes := &storageRoutes{stores: make(map[coordclass.Class]beads.Store), binding: "infra"}
	for _, class := range coordclass.Classes() {
		if class.IsInfrastructure() {
			routes.stores[class] = infra
		}
	}
	return routes
}

// stubControllerCityStore points newControllerState's city-store opener at an
// in-memory store for the duration of a test.
func stubControllerCityStore(t *testing.T, store beads.Store) {
	t.Helper()
	prev := newControllerStateOpenCityStore
	newControllerStateOpenCityStore = func(string, gate.Mode) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: store}, nil
	}
	t.Cleanup(func() { newControllerStateOpenCityStore = prev })
}

func newRoutedControllerStateForTest(t *testing.T, routes *storageRoutes, work beads.Store) *controllerState {
	t.Helper()
	stubControllerCityStore(t, work)
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	return newControllerStateWithRoutes(context.Background(), routes, cfg, nil, nil, "test-city", cityPath)
}

func appendTestTranscript(t *testing.T, svc *extmsg.Services) extmsg.ConversationTranscriptRecord {
	t.Helper()
	rec, err := svc.Transcript.Append(context.Background(), extmsg.AppendTranscriptInput{
		Caller: extmsg.Caller{
			Kind:      extmsg.CallerAdapter,
			ID:        "adapter-1",
			Provider:  "discord",
			AccountID: "acct-1",
		},
		Conversation: extmsg.ConversationRef{
			ScopeID:        "city-1",
			Provider:       "discord",
			AccountID:      "acct-1",
			ConversationID: "thread-1",
			Kind:           extmsg.ConversationThread,
		},
		Kind:              extmsg.TranscriptMessageInbound,
		Provenance:        extmsg.TranscriptProvenanceLive,
		ProviderMessageID: "msg-1",
		Text:              "hello",
		CreatedAt:         time.Date(2026, time.March, 23, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("transcript append: %v", err)
	}
	return rec
}

// TestControllerStateRoutesMessagingClassAtConstruction pins the boot-ordering
// bug. The mail provider and the external-messaging services are BUILT during
// newControllerState, from the class resolvers. Installing the storage routes
// after construction — which is what CityRuntime.setControllerState used to do —
// leaves both of them resolved against the work store forever, so on a split
// city every message bead and every extmsg record is written to the ledger the
// messaging class was moved off.
func TestControllerStateRoutesMessagingClassAtConstruction(t *testing.T) {
	workStore := beads.NewMemStore()
	infraStore := beads.NewMemStore()

	cs := newRoutedControllerStateForTest(t, messagingSplitRoutes(infraStore), workStore)

	if cs.cityMailProv == nil {
		t.Fatal("no city mail provider built")
	}
	msg, err := cs.cityMailProv.Send("mayor", "worker", "subject", "body")
	if err != nil {
		t.Fatalf("mail send: %v", err)
	}
	if _, err := infraStore.Get(msg.ID); err != nil {
		t.Fatalf("message %q not in the messaging binding: %v", msg.ID, err)
	}
	if _, err := workStore.Get(msg.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("message %q resolves in the work store (err = %v); the mail provider was built before the routes arrived", msg.ID, err)
	}

	if cs.extmsgSvc == nil {
		t.Fatal("no extmsg services built")
	}
	rec := appendTestTranscript(t, cs.extmsgSvc)
	if _, err := infraStore.Get(rec.ID); err != nil {
		t.Fatalf("transcript %q not in the messaging binding: %v", rec.ID, err)
	}
	if _, err := workStore.Get(rec.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("transcript %q resolves in the work store (err = %v); extmsg was never routed", rec.ID, err)
	}
}

// TestControllerStateSingleStoreKeepsMessagingOnTheWorkStore is the
// compatibility guarantee: a city that relocates nothing routes nothing, so
// mail and extmsg keep writing to the one store they always did. Green before
// and after by design — the value it carries is that it goes red if the class
// resolvers ever stop being identity for an unrelocated class.
func TestControllerStateSingleStoreKeepsMessagingOnTheWorkStore(t *testing.T) {
	store := beads.NewMemStore()
	cs := newRoutedControllerStateForTest(t, nil, store)

	msg, err := cs.cityMailProv.Send("mayor", "worker", "subject", "body")
	if err != nil {
		t.Fatalf("mail send: %v", err)
	}
	if _, err := store.Get(msg.ID); err != nil {
		t.Fatalf("message %q not in the single store: %v", msg.ID, err)
	}
	rec := appendTestTranscript(t, cs.extmsgSvc)
	if _, err := store.Get(rec.ID); err != nil {
		t.Fatalf("transcript %q not in the single store: %v", rec.ID, err)
	}
	if got := cs.mailBeadStore().Store; got != cs.cityBeadStore {
		t.Fatalf("mailBeadStore = %p, want the identical city store %p", got, cs.cityBeadStore)
	}
	if got := cs.SessionsBeadStore().Store; got != cs.cityBeadStore {
		t.Fatalf("SessionsBeadStore = %p, want the identical city store %p", got, cs.cityBeadStore)
	}
}

// TestSetControllerStateDoesNotRaceClassAccessors pins the second half of the
// same bug. The routes used to be written by setControllerState with no lock
// held, while every class accessor on the API surface reads them under
// cs.mu.RLock(). Run under -race this fails on the pre-fix code; it passes now
// because the field is a constructor input, written once and never reassigned.
func TestSetControllerStateDoesNotRaceClassAccessors(t *testing.T) {
	store := beads.NewMemStore()
	stubControllerCityStore(t, store)
	cityPath := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	cs := newControllerState(context.Background(), cfg, nil, nil, "test-city", cityPath)
	cr := &CityRuntime{storageRoutes: messagingSplitRoutes(beads.NewMemStore())}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		cr.setControllerState(cs)
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			_ = cs.GraphBeadStore()
			_ = cs.NudgesBeadStore()
			_ = cs.SessionsBeadStore()
			_ = cs.mailBeadStore()
			_ = cs.ordersBeadStore("")
		}
	}()
	wg.Wait()
}
