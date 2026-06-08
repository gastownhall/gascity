package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

func TestReapStaleExtmsgBindingsRepointsRespawnedSession(t *testing.T) {
	store := beads.NewMemStore()
	now := time.Date(2026, time.March, 23, 9, 0, 0, 0, time.UTC)

	oldSession, err := store.Create(beads.Bead{
		Title:    "session gc-pl",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "gc-pl"},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	ref := extmsg.ConversationRef{
		ScopeID:        "city-1",
		Provider:       "slack",
		AccountID:      "acct-1",
		ConversationID: "C0B25SS12CD",
		Kind:           extmsg.ConversationRoom,
	}
	svc := extmsg.NewServices(store).Bindings
	if _, err := svc.Bind(context.Background(), extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}, extmsg.BindInput{
		Conversation: ref,
		SessionID:    oldSession.ID,
		Now:          now,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// Respawn: close the old bead, mint a fresh one under the same name.
	if err := store.Close(oldSession.ID); err != nil {
		t.Fatalf("close old session: %v", err)
	}
	newSession, err := store.Create(beads.Bead{
		Title:    "session gc-pl",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "gc-pl"},
	})
	if err != nil {
		t.Fatalf("recreate session bead: %v", err)
	}

	var stderr bytes.Buffer
	reapStaleExtmsgBindings(context.Background(), store, now, &stderr)

	got, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if got == nil || got.SessionID != newSession.ID {
		t.Fatalf("binding not re-pointed at respawned session: got %+v want SessionID=%s", got, newSession.ID)
	}
}

func TestReapStaleExtmsgBindingsNilStoreNoPanic(_ *testing.T) {
	// Defensive: a tick before the bead store is wired must be a no-op.
	reapStaleExtmsgBindings(context.Background(), nil, time.Now(), nil)
}
