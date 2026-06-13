package main

import (
	"bytes"
	"context"
	"slices"
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

func TestReapStaleExtmsgParticipantsMigratesMembershipToRespawn(t *testing.T) {
	store := beads.NewMemStore()

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
	fabric := extmsg.NewServices(store)
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	group, err := fabric.Groups.EnsureGroup(context.Background(), caller, extmsg.EnsureGroupInput{
		RootConversation: ref,
		Mode:             extmsg.GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	participant, err := fabric.Groups.UpsertParticipant(context.Background(), caller, extmsg.UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: oldSession.ID,
	})
	if err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	// Respawn: close the old bead, mint a fresh one under the same name. No
	// binding exists, so the binding reaper never observes this session — only
	// the participant reaper can converge it.
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
	reapStaleExtmsgParticipants(context.Background(), store, &stderr)

	// Persistent heal: the participant now points at the respawned bead.
	bead, err := store.Get(participant.ID)
	if err != nil {
		t.Fatalf("get participant bead: %v", err)
	}
	if bead.Metadata["session_id"] != newSession.ID {
		t.Fatalf("participant session_id = %q, want %q (respawned bead)", bead.Metadata["session_id"], newSession.ID)
	}

	// The group-owned transcript membership followed the respawn. Routing already
	// self-heals at read time, so membership migration is the reaper's unique
	// contribution and the thing finding-3 guards against stranding.
	memberships, err := fabric.Transcript.ListMemberships(context.Background(), caller, ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	var sessionIDs []string
	for _, m := range memberships {
		sessionIDs = append(sessionIDs, m.SessionID)
	}
	if slices.Contains(sessionIDs, oldSession.ID) {
		t.Errorf("group membership still stranded on retired session %s: %v", oldSession.ID, sessionIDs)
	}
	if !slices.Contains(sessionIDs, newSession.ID) {
		t.Errorf("group membership did not follow respawn to %s: %v", newSession.ID, sessionIDs)
	}
}

func TestReapStaleExtmsgParticipantsNilStoreNoPanic(_ *testing.T) {
	// Defensive: a tick before the bead store is wired must be a no-op.
	reapStaleExtmsgParticipants(context.Background(), nil, nil)
}
