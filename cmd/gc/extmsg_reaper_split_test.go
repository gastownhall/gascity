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

// extmsgReaperSplit is a controller runtime whose messaging and sessions
// classes live in two different stores (the fixture
// TestCityExtMsgServicesRouteEachClassToItsOwnStore uses), the controller's own
// external-messaging services, and one session the test can respawn.
type extmsgReaperSplit struct {
	cr        *CityRuntime
	messaging beads.Store
	sessions  beads.Store
	svc       *extmsg.Services
	oldID     string
}

func newExtmsgReaperSplit(t *testing.T) *extmsgReaperSplit {
	t.Helper()
	work := splittest.NewWorkStore(t, "hq")
	f := &extmsgReaperSplit{
		messaging: splittest.NewClassStore(t, config.BeadClassMessaging),
		sessions:  splittest.NewClassStore(t, config.BeadClassSessions),
	}
	routes := &storageRoutes{stores: map[coordclass.Class]beads.Store{
		coordclass.ClassMessaging: f.messaging,
		coordclass.ClassSessions:  f.sessions,
	}}
	f.cr = &CityRuntime{cityName: "c", cityPath: t.TempDir(), storageRoutes: routes, standaloneCityStore: work}
	f.svc = newCityExtMsgServices(routes, work, f.cr.cityPath)
	f.oldID = f.newSession(t)
	return f
}

func (f *extmsgReaperSplit) newSession(t *testing.T) string {
	t.Helper()
	b, err := f.sessions.Create(beads.Bead{
		Title:    "session gc-pl",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "gc-pl"},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return b.ID
}

// respawn closes the old session and mints its replacement under the same
// name, returning the replacement's id.
func (f *extmsgReaperSplit) respawn(t *testing.T) string {
	t.Helper()
	if err := f.sessions.Close(f.oldID); err != nil {
		t.Fatalf("close old session: %v", err)
	}
	return f.newSession(t)
}

// wantSessionID asserts the messaging store's row id points at want. It reads
// the row itself: the services heal at read time, which would hide whether
// the reaper ran over the row.
func (f *extmsgReaperSplit) wantSessionID(t *testing.T, what, id, want string) {
	t.Helper()
	row, err := f.messaging.Get(id)
	if err != nil {
		t.Fatalf("get %s row: %v", what, err)
	}
	if got := row.Metadata["session_id"]; got != want {
		t.Fatalf("the reaper left the messaging store's %s on session %q, want the respawned %q", what, got, want)
	}
}

var extmsgReaperTestCaller = extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}

var extmsgReaperTestRef = extmsg.ConversationRef{ScopeID: "city-1", Provider: "slack", AccountID: "acct-1", ConversationID: "C0B25SS12CD", Kind: extmsg.ConversationRoom}

// bind binds the conversation to the old session and returns the binding id.
func (f *extmsgReaperSplit) bind(t *testing.T) string {
	t.Helper()
	bound, err := f.svc.Bindings.Bind(context.Background(), extmsgReaperTestCaller, extmsg.BindInput{
		Conversation: extmsgReaperTestRef,
		SessionID:    f.oldID,
		Now:          time.Date(2026, time.March, 23, 9, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return bound.ID
}

// TestControllerReapsExtmsgBindingsFromTheMessagingStore: the controller's
// binding reaper reads extmsg's bindings from the messaging store and session
// liveness from the sessions store. It used to hand the sessions store to
// both, so on a city whose two classes live in two stores it never saw the
// bindings and a respawned session's binding was never re-pointed.
func TestControllerReapsExtmsgBindingsFromTheMessagingStore(t *testing.T) {
	f := newExtmsgReaperSplit(t)
	bindingID := f.bind(t)
	respawnedID := f.respawn(t)

	f.cr.tickReapStaleExtmsgBindings(&tickPass{ctx: context.Background()})

	f.wantSessionID(t, "binding", bindingID, respawnedID)
}

// TestControllerReapsExtmsgBindingsFromTheMessagingStoreControl is the
// control: the same fixture, reaped directly with the messaging store for the
// bindings and the sessions directory for liveness, re-points the binding, so
// the test above fails only on the routing.
func TestControllerReapsExtmsgBindingsFromTheMessagingStoreControl(t *testing.T) {
	f := newExtmsgReaperSplit(t)
	bindingID := f.bind(t)
	respawnedID := f.respawn(t)
	stores := f.cr.cityStores()
	if _, err := extmsg.ReapStaleBindings(context.Background(), stores.Mail(), stores.Sessions(), time.Now()); err != nil {
		t.Fatalf("ReapStaleBindings: %v", err)
	}
	f.wantSessionID(t, "binding", bindingID, respawnedID)
}

// TestControllerReapsExtmsgParticipantsFromTheMessagingStore is the
// participant reaper's twin: a group participant on the old session, in the
// messaging store, follows the respawn when the controller ticks.
func TestControllerReapsExtmsgParticipantsFromTheMessagingStore(t *testing.T) {
	f := newExtmsgReaperSplit(t)
	ctx := context.Background()
	group, err := f.svc.Groups.EnsureGroup(ctx, extmsgReaperTestCaller, extmsg.EnsureGroupInput{
		RootConversation: extmsgReaperTestRef,
		Mode:             extmsg.GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	participant, err := f.svc.Groups.UpsertParticipant(ctx, extmsgReaperTestCaller, extmsg.UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: f.oldID,
	})
	if err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	respawnedID := f.respawn(t)

	f.cr.tickReapStaleExtmsgParticipants(&tickPass{ctx: ctx})

	f.wantSessionID(t, "participant", participant.ID, respawnedID)
}
