package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

// extmsgReaperSplitFixture is a controller runtime whose messaging and
// sessions classes live in two different stores (the fixture
// TestCityExtMsgServicesRouteEachClassToItsOwnStore uses), with a binding the
// controller's own services wrote to the messaging store for a session that
// then respawned. It returns the runtime, the messaging store, the binding id
// and the respawned session's id.
func extmsgReaperSplitFixture(t *testing.T) (cr *CityRuntime, messaging beads.Store, bindingID, respawnedID string) {
	t.Helper()
	work := splittest.NewWorkStore(t, "hq")
	messaging = splittest.NewClassStore(t, config.BeadClassMessaging)
	sessions := splittest.NewClassStore(t, config.BeadClassSessions)
	routes := &storageRoutes{stores: map[coordclass.Class]beads.Store{
		coordclass.ClassMessaging: messaging,
		coordclass.ClassSessions:  sessions,
	}}
	cr = &CityRuntime{cityName: "c", cityPath: t.TempDir(), storageRoutes: routes, standaloneCityStore: work}
	now := time.Date(2026, time.March, 23, 9, 0, 0, 0, time.UTC)
	newSession := func() beads.Bead {
		b, err := sessions.Create(beads.Bead{
			Title:    "session gc-pl",
			Type:     session.BeadType,
			Labels:   []string{session.LabelSession},
			Metadata: map[string]string{"session_name": "gc-pl"},
		})
		if err != nil {
			t.Fatalf("create session bead: %v", err)
		}
		return b
	}
	old := newSession()
	svc := newCityExtMsgServices(routes, work, cr.cityPath)
	ref := extmsg.ConversationRef{ScopeID: "city-1", Provider: "slack", AccountID: "acct-1", ConversationID: "C0B25SS12CD", Kind: extmsg.ConversationRoom}
	bound, err := svc.Bindings.Bind(context.Background(), extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, extmsg.BindInput{Conversation: ref, SessionID: old.ID, Now: now})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := sessions.Close(old.ID); err != nil {
		t.Fatalf("close old session: %v", err)
	}
	return cr, messaging, bound.ID, newSession().ID
}

// TestControllerReapsExtmsgBindingsFromTheMessagingStore: the controller's
// binding reaper reads extmsg's bindings from the messaging store and session
// liveness from the sessions store. It used to hand the sessions store to
// both, so on a city whose two classes live in two stores it never saw the
// bindings and a respawned session's binding was never re-pointed.
func TestControllerReapsExtmsgBindingsFromTheMessagingStore(t *testing.T) {
	cr, messaging, bindingID, respawnedID := extmsgReaperSplitFixture(t)

	cr.tickReapStaleExtmsgBindings(&tickPass{ctx: context.Background()})

	// Read the binding row itself: ResolveByConversation heals at read time,
	// which would hide whether the reaper ran over the row.
	row, err := messaging.Get(bindingID)
	if err != nil {
		t.Fatalf("get binding row: %v", err)
	}
	if got := row.Metadata["session_id"]; got != respawnedID {
		t.Fatalf("the reaper left the messaging store's binding on session %q, want the respawned %q", got, respawnedID)
	}
}

// TestControllerReapsExtmsgBindingsFromTheMessagingStoreControl is the
// control: the same fixture, reaped directly with the messaging store for the
// bindings and the sessions directory for liveness, re-points the binding, so
// the test above fails only on the routing.
func TestControllerReapsExtmsgBindingsFromTheMessagingStoreControl(t *testing.T) {
	cr, messaging, bindingID, respawnedID := extmsgReaperSplitFixture(t)
	if _, err := extmsg.ReapStaleBindings(context.Background(), cr.mailBeadStore(), cr.sessionsBeadStore(), time.Now()); err != nil {
		t.Fatalf("ReapStaleBindings: %v", err)
	}
	row, err := messaging.Get(bindingID)
	if err != nil {
		t.Fatalf("get binding row: %v", err)
	}
	if got := row.Metadata["session_id"]; got != respawnedID {
		t.Fatalf("control: binding on %q, want %q", got, respawnedID)
	}
}
