package extmsg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBindingServiceReplaceRebindsActiveBinding(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	svc := fabric.Bindings
	ref := testConversationRef()

	first, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/frontdesk",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(frontdesk): %v", err)
	}

	// Plain bind still conflicts — handoff is explicitly opt-in.
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/specialist",
		Now:          testNow(),
	}); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("Bind(no replace) error = %v, want ErrBindingConflict", err)
	}

	second, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/specialist",
		Replace:      true,
		Now:          testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(replace): %v", err)
	}
	if second.AgentName != "myrig/specialist" {
		t.Fatalf("rebound AgentName = %q, want myrig/specialist", second.AgentName)
	}
	if second.BindingGeneration != first.BindingGeneration+1 {
		t.Fatalf("BindingGeneration = %d, want %d", second.BindingGeneration, first.BindingGeneration+1)
	}

	active, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if active == nil || active.ID != second.ID {
		t.Fatalf("active = %#v, want the rebound binding", active)
	}

	// Membership followed the handoff: the old agent's membership is gone,
	// the new agent's exists.
	members, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 1 || members[0].SessionID != "myrig/specialist" {
		t.Fatalf("memberships = %#v, want one keyed myrig/specialist", members)
	}
}

func TestBindingServiceReplaceAcrossKindsAndIdempotence(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()

	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(session): %v", err)
	}

	// Session binding -> agent binding via replace.
	rebound, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/helper",
		Replace:      true,
		Now:          testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(replace session->agent): %v", err)
	}
	if rebound.AgentName != "myrig/helper" || rebound.SessionID != "" {
		t.Fatalf("rebound = %#v, want agent binding", rebound)
	}

	// Replace to the same target is idempotent — keeps the record.
	same, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "myrig/helper",
		Replace:      true,
		Now:          testNow().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(replace same target): %v", err)
	}
	if same.ID != rebound.ID || same.BindingGeneration != rebound.BindingGeneration {
		t.Fatalf("idempotent replace changed record: %#v vs %#v", same, rebound)
	}

	// Replace with no active binding behaves like a plain bind.
	if _, err := svc.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		Conversation: &ref,
		Now:          testNow().Add(3 * time.Minute),
	}); err != nil {
		t.Fatalf("Unbind: %v", err)
	}
	fresh, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-b",
		Replace:      true,
		Now:          testNow().Add(4 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(replace on unbound): %v", err)
	}
	if fresh.SessionID != "sess-b" {
		t.Fatalf("fresh = %#v, want sess-b session binding", fresh)
	}
}
