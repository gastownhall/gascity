package extmsg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBindingServiceBindEnforcesOwnershipAndConflict(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	svc := fabric.Bindings
	ref := testConversationRef()

	first, err := svc.Bind(context.Background(), testAdapterCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
		Metadata:     map[string]string{"source": "discord"},
	})
	if err != nil {
		t.Fatalf("Bind(first): %v", err)
	}
	if first.BindingGeneration != 1 {
		t.Fatalf("BindingGeneration = %d, want 1", first.BindingGeneration)
	}

	second, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(idempotent): %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("idempotent bind changed ID: got %s want %s", second.ID, first.ID)
	}
	if second.BindingGeneration != first.BindingGeneration {
		t.Fatalf("idempotent bind changed generation: got %d want %d", second.BindingGeneration, first.BindingGeneration)
	}

	_, err = svc.Bind(context.Background(), Caller{
		Kind:      CallerAdapter,
		ID:        "adapter-2",
		Provider:  "slack",
		AccountID: "acct-1",
	}, BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Bind(foreign adapter) error = %v, want ErrUnauthorized", err)
	}

	_, err = svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-b",
		Now:          testNow(),
	})
	if !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("Bind(conflict) error = %v, want ErrBindingConflict", err)
	}

	got, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if got == nil || got.SessionID != "sess-a" {
		t.Fatalf("ResolveByConversation session = %#v, want sess-a", got)
	}
}

func TestBindingServiceExpiredBindingIsMissAndRebinds(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()
	expiredAt := testNow().Add(-time.Minute)

	first, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		ExpiresAt:    &expiredAt,
		Now:          testNow().Add(-2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(expired): %v", err)
	}
	if first.BindingGeneration != 1 {
		t.Fatalf("expired BindingGeneration = %d, want 1", first.BindingGeneration)
	}

	got, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation(expired): %v", err)
	}
	if got != nil {
		t.Fatalf("ResolveByConversation(expired) = %#v, want nil", got)
	}

	second, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-b",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(rebind): %v", err)
	}
	if second.BindingGeneration != 2 {
		t.Fatalf("rebind BindingGeneration = %d, want 2", second.BindingGeneration)
	}
}

func TestBindingServiceBindSeparatesConversationVariants(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings

	threadRef := testConversationRef()
	roomRef := testConversationRef()
	roomRef.Kind = ConversationRoom
	roomRef.ParentConversationID = "parent-1"

	if bindingConversationLabel(threadRef) == bindingConversationLabel(roomRef) {
		t.Fatal("bindingConversationLabel should differ for distinct ConversationRef variants")
	}

	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: threadRef,
		SessionID:    "sess-thread",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(thread): %v", err)
	}
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: roomRef,
		SessionID:    "sess-room",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(room): %v", err)
	}

	threadBinding, err := svc.ResolveByConversation(context.Background(), threadRef)
	if err != nil {
		t.Fatalf("ResolveByConversation(thread): %v", err)
	}
	roomBinding, err := svc.ResolveByConversation(context.Background(), roomRef)
	if err != nil {
		t.Fatalf("ResolveByConversation(room): %v", err)
	}
	if threadBinding == nil || threadBinding.SessionID != "sess-thread" {
		t.Fatalf("thread binding = %#v, want sess-thread", threadBinding)
	}
	if roomBinding == nil || roomBinding.SessionID != "sess-room" {
		t.Fatalf("room binding = %#v, want sess-room", roomBinding)
	}
}

func TestBindingServiceConcurrentBindConflicts(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, sessionID := range []string{"sess-a", "sess-b"} {
		wg.Add(1)
		go func(sessionID string) {
			defer wg.Done()
			_, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
				Conversation: ref,
				SessionID:    sessionID,
				Now:          testNow(),
			})
			errs <- err
		}(sessionID)
	}
	wg.Wait()
	close(errs)

	var successes int
	var conflicts int
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrBindingConflict):
			conflicts++
		default:
			t.Fatalf("Bind(concurrent) unexpected error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent bind results = successes:%d conflicts:%d, want 1/1", successes, conflicts)
	}
}

func TestBindingServiceConcurrentBindConflictsAcrossBundles(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabricA := NewServices(store)
	fabricB := NewServices(store)
	ref := testConversationRef()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, svc := range []BindingService{fabricA.Bindings, fabricB.Bindings} {
		wg.Add(1)
		go func(i int, svc BindingService) {
			defer wg.Done()
			_, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
				Conversation: ref,
				SessionID:    "sess-" + strconv.Itoa(i),
				Now:          testNow(),
			})
			errs <- err
		}(i, svc)
	}
	wg.Wait()
	close(errs)

	var successes int
	var conflicts int
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrBindingConflict):
			conflicts++
		default:
			t.Fatalf("Bind(across bundles) unexpected error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("across-bundle bind results = successes:%d conflicts:%d, want 1/1", successes, conflicts)
	}
}

func TestBindingServiceUnbindClearsDeliveryContext(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	delivery := fabric.Delivery
	svc := fabric.Bindings
	ref := testConversationRef()

	binding, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := delivery.Record(context.Background(), testControllerCaller(), DeliveryContextRecord{
		SessionID:         "sess-a",
		Conversation:      ref,
		BindingGeneration: binding.BindingGeneration,
		LastPublishedAt:   testNow(),
		LastMessageID:     "msg-1",
		SourceSessionID:   "sess-a",
		Metadata:          map[string]string{"route": "thread-reply"},
	}); err != nil {
		t.Fatalf("Record(delivery): %v", err)
	}

	got, err := delivery.Resolve(context.Background(), "sess-a", ref)
	if err != nil {
		t.Fatalf("Resolve(delivery before unbind): %v", err)
	}
	if got == nil {
		t.Fatal("Resolve(delivery before unbind) = nil, want record")
	}
	if got.SessionID != "sess-a" || got.BindingGeneration != binding.BindingGeneration {
		t.Fatalf("Resolve(delivery before unbind) identity = %#v, want sess-a generation %d", got, binding.BindingGeneration)
	}
	if !got.LastPublishedAt.Equal(testNow()) || got.LastMessageID != "msg-1" || got.SourceSessionID != "sess-a" {
		t.Fatalf("Resolve(delivery before unbind) fields = %#v, want published/msg/source values", got)
	}
	if got.Metadata["route"] != "thread-reply" {
		t.Fatalf("Resolve(delivery before unbind) metadata = %#v, want route=thread-reply", got.Metadata)
	}

	unbound, err := svc.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		Conversation: &ref,
		Now:          testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Unbind: %v", err)
	}
	if len(unbound) != 1 || unbound[0].Status != BindingEnded {
		t.Fatalf("Unbind() = %#v, want one ended binding", unbound)
	}

	got, err = delivery.Resolve(context.Background(), "sess-a", ref)
	if err != nil {
		t.Fatalf("Resolve(delivery after unbind): %v", err)
	}
	if got != nil {
		t.Fatalf("Resolve(delivery after unbind) = %#v, want nil", got)
	}

	items, err := store.ListByLabel(deliveryRouteLabel(ref, "sess-a"), 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel(delivery route): %v", err)
	}
	if len(items) != 1 || items[0].Status != "closed" {
		t.Fatalf("delivery bead status = %#v, want one closed bead", items)
	}
}

func TestDeliveryContextResolveKeepsValidRouteWhileClosingStaleRoute(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	delivery := fabric.Delivery
	svc := fabric.Bindings
	ref := testConversationRef()

	firstBinding, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(first): %v", err)
	}
	if _, err := svc.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		Conversation: &ref,
		Now:          testNow().Add(time.Minute),
	}); err != nil {
		t.Fatalf("Unbind(first): %v", err)
	}
	secondBinding, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(second): %v", err)
	}
	if secondBinding.BindingGeneration != firstBinding.BindingGeneration+1 {
		t.Fatalf("BindingGeneration(second) = %d, want %d", secondBinding.BindingGeneration, firstBinding.BindingGeneration+1)
	}
	if err := delivery.Record(context.Background(), testControllerCaller(), DeliveryContextRecord{
		SessionID:         "sess-a",
		Conversation:      ref,
		BindingGeneration: secondBinding.BindingGeneration,
		LastPublishedAt:   testNow().Add(2 * time.Minute),
		LastMessageID:     "msg-2",
	}); err != nil {
		t.Fatalf("Record(valid delivery): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  "stale delivery",
		Type:   "task",
		Labels: []string{"gc:extmsg-delivery", labelDeliveryBase, deliveryRouteLabel(ref, "sess-a"), deliverySessionLabel("sess-a")},
		Metadata: encodeMetadataFields(nil, map[string]string{
			"schema_version":         strconv.Itoa(schemaVersion),
			"session_id":             "sess-a",
			"scope_id":               ref.ScopeID,
			"provider":               ref.Provider,
			"account_id":             ref.AccountID,
			"conversation_id":        ref.ConversationID,
			"parent_conversation_id": ref.ParentConversationID,
			"conversation_kind":      string(ref.Kind),
			"binding_generation":     strconv.FormatInt(firstBinding.BindingGeneration, 10),
			"last_published_at":      formatTime(testNow()),
			"last_message_id":        "msg-stale",
		}),
	}); err != nil {
		t.Fatalf("Create(stale delivery): %v", err)
	}

	got, err := delivery.Resolve(context.Background(), "sess-a", ref)
	if err != nil {
		t.Fatalf("Resolve(delivery): %v", err)
	}
	if got == nil || got.BindingGeneration != secondBinding.BindingGeneration || got.LastMessageID != "msg-2" {
		t.Fatalf("Resolve(delivery) = %#v, want valid generation %d msg-2", got, secondBinding.BindingGeneration)
	}

	items, err := store.ListByLabel(deliveryRouteLabel(ref, "sess-a"), 0, beads.IncludeClosed)
	if err != nil {
		t.Fatalf("ListByLabel(delivery route): %v", err)
	}
	var openCount int
	var closedCount int
	for _, item := range items {
		switch item.Status {
		case "open":
			openCount++
		case "closed":
			closedCount++
		}
	}
	if openCount != 1 || closedCount != 1 {
		t.Fatalf("delivery route statuses = %#v, want one open and one closed", items)
	}
}

func TestBindingServiceUnbindBySessionReturnsPartialClosedOnFailure(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	refFirst := testConversationRef()
	refFirst.ConversationID = "thread-a"
	refSecond := testConversationRef()
	refSecond.ConversationID = "thread-b"
	if conversationLockKey(refFirst) > conversationLockKey(refSecond) {
		refFirst, refSecond = refSecond, refFirst
	}
	delivery := &selectiveFailingDeliveryContextService{
		failConversationIDs: map[string]bool{refFirst.ConversationID: true},
		err:                 errors.New("boom"),
	}
	svc := newBindingService(store, delivery, nil, newBindingLockPool())

	bindingFirst, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: refFirst,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(first): %v", err)
	}
	bindingSecond, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: refSecond,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(second): %v", err)
	}

	closed, err := svc.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		SessionID: "sess-a",
		Now:       testNow().Add(time.Minute),
	})
	if err == nil {
		t.Fatal("Unbind(session-wide) error = nil, want delivery clear failure")
	}
	if len(closed) != 1 || closed[0].ID != bindingFirst.ID || closed[0].Status != BindingEnded {
		t.Fatalf("Unbind(session-wide) partial closed = %#v, want only first binding ended", closed)
	}

	itemFirst, err := store.Get(bindingFirst.ID)
	if err != nil {
		t.Fatalf("Get(bindingFirst): %v", err)
	}
	if itemFirst.Status != "closed" {
		t.Fatalf("bindingFirst status = %q, want closed", itemFirst.Status)
	}
	itemSecond, err := store.Get(bindingSecond.ID)
	if err != nil {
		t.Fatalf("Get(bindingSecond): %v", err)
	}
	if itemSecond.Status != "open" {
		t.Fatalf("bindingSecond status = %q, want open", itemSecond.Status)
	}
}

func TestBindingServiceUnbindKeepsClosedBindingWhenDeliveryClearFails(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	delivery := &failingDeliveryContextService{err: errors.New("boom")}
	svc := newBindingService(store, delivery, nil, newBindingLockPool())
	ref := testConversationRef()

	binding, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}

	closed, err := svc.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		Conversation: &ref,
		Now:          testNow().Add(time.Minute),
	})
	if err == nil {
		t.Fatal("Unbind() error = nil, want delivery clear failure")
	}
	if len(closed) != 1 || closed[0].ID != binding.ID || closed[0].Status != BindingEnded {
		t.Fatalf("Unbind() closed = %#v, want one ended binding", closed)
	}

	item, err := store.Get(binding.ID)
	if err != nil {
		t.Fatalf("Get(binding): %v", err)
	}
	if item.Status != "closed" {
		t.Fatalf("binding status after failed clear = %q, want closed", item.Status)
	}
}

func TestBindingServiceListBySessionReturnsOnlyBindings(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	delivery := fabric.Delivery
	bindings := fabric.Bindings
	groups := fabric.Groups
	ref := testConversationRef()

	binding, err := bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if err := delivery.Record(context.Background(), testControllerCaller(), DeliveryContextRecord{
		SessionID:         "sess-a",
		Conversation:      ref,
		BindingGeneration: binding.BindingGeneration,
		LastPublishedAt:   testNow(),
	}); err != nil {
		t.Fatalf("Record(delivery): %v", err)
	}
	group, err := groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	got, err := bindings.ListBySession(context.Background(), "sess-a")
	if err != nil {
		t.Fatalf("ListBySession: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListBySession len = %d, want 1", len(got))
	}
	if got[0].ID != binding.ID {
		t.Fatalf("ListBySession binding ID = %s, want %s", got[0].ID, binding.ID)
	}
}

func TestEmptyMetadataRecordsEncodeAsObjects(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	binding, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if binding.Metadata == nil {
		t.Fatal("binding Metadata = nil, want empty object map")
	}

	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if group.Metadata == nil {
		t.Fatal("group Metadata = nil, want empty object map")
	}

	participant, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	})
	if err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	if participant.Metadata == nil {
		t.Fatal("participant Metadata = nil, want empty object map")
	}
}

func TestBindingServiceTouchDebouncesMetadataWrites(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := newBindingService(store, nil, nil, newBindingLockPool(), WithBindingTouchDebounce(time.Hour))
	ref := testConversationRef()
	start := testNow()

	binding, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          start,
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if err := svc.Touch(context.Background(), testControllerCaller(), binding.ID, start.Add(10*time.Minute)); err != nil {
		t.Fatalf("Touch(debounced): %v", err)
	}
	item, err := store.Get(binding.ID)
	if err != nil {
		t.Fatalf("Get(binding): %v", err)
	}
	lastTouched, err := parseTime(item.Metadata, "last_touched_at")
	if err != nil {
		t.Fatalf("parseTime(last_touched_at): %v", err)
	}
	if !lastTouched.Equal(start) {
		t.Fatalf("last_touched_at after debounced touch = %s, want %s", lastTouched, start)
	}

	if err := svc.Touch(context.Background(), testControllerCaller(), binding.ID, start.Add(2*time.Hour)); err != nil {
		t.Fatalf("Touch(applied): %v", err)
	}
	item, err = store.Get(binding.ID)
	if err != nil {
		t.Fatalf("Get(binding after applied touch): %v", err)
	}
	lastTouched, err = parseTime(item.Metadata, "last_touched_at")
	if err != nil {
		t.Fatalf("parseTime(last_touched_at after apply): %v", err)
	}
	want := start.Add(2 * time.Hour)
	if !lastTouched.Equal(want) {
		t.Fatalf("last_touched_at after applied touch = %s, want %s", lastTouched, want)
	}
}

func TestDeliveryContextRecordRejectsBindingMismatch(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	delivery := fabric.Delivery
	bindings := fabric.Bindings
	ref := testConversationRef()

	if _, err := bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	err := delivery.Record(context.Background(), testControllerCaller(), DeliveryContextRecord{
		SessionID:         "sess-a",
		Conversation:      ref,
		BindingGeneration: 2,
		LastPublishedAt:   testNow(),
	})
	if !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("Record(binding mismatch) error = %v, want ErrBindingMismatch", err)
	}
}

func TestGroupServiceRoutesExplicitAndImplicitTargets(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewGroupService(store)
	ref := testConversationRef()

	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
		Public:    true,
	}); err != nil {
		t.Fatalf("UpsertParticipant(alpha): %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "beta",
		SessionID: "sess-b",
	}); err != nil {
		t.Fatalf("UpsertParticipant(beta): %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "gamma",
		SessionID: "sess-c",
	}); err != nil {
		t.Fatalf("UpsertParticipant(gamma): %v", err)
	}

	defaultDecision, err := svc.ResolveInbound(context.Background(), ExternalInboundMessage{
		Conversation: ref,
	})
	if err != nil {
		t.Fatalf("ResolveInbound(default): %v", err)
	}
	if defaultDecision.Match != GroupRouteDefault || defaultDecision.TargetSessionID != "sess-a" {
		t.Fatalf("ResolveInbound(default) = %#v, want default->sess-a", defaultDecision)
	}

	explicitDecision, err := svc.ResolveInbound(context.Background(), ExternalInboundMessage{
		Conversation:   ref,
		ExplicitTarget: "beta",
	})
	if err != nil {
		t.Fatalf("ResolveInbound(explicit): %v", err)
	}
	if explicitDecision.Match != GroupRouteExplicitTarget || explicitDecision.TargetSessionID != "sess-b" || !explicitDecision.UpdateCursor {
		t.Fatalf("ResolveInbound(explicit) = %#v, want explicit->sess-b with cursor update", explicitDecision)
	}

	if err := svc.UpdateCursor(context.Background(), testControllerCaller(), UpdateCursorInput{
		RootConversation: ref,
		Handle:           "beta",
	}); err != nil {
		t.Fatalf("UpdateCursor(beta): %v", err)
	}

	implicitDecision, err := svc.ResolveInbound(context.Background(), ExternalInboundMessage{
		Conversation: ref,
	})
	if err != nil {
		t.Fatalf("ResolveInbound(last addressed): %v", err)
	}
	if implicitDecision.Match != GroupRouteLastAddressed || implicitDecision.TargetSessionID != "sess-b" {
		t.Fatalf("ResolveInbound(last addressed) = %#v, want last_addressed->sess-b", implicitDecision)
	}

	if err := svc.UpdateCursor(context.Background(), testControllerCaller(), UpdateCursorInput{
		RootConversation: ref,
		Handle:           "",
	}); err != nil {
		t.Fatalf("UpdateCursor(clear): %v", err)
	}
	clearedDecision, err := svc.ResolveInbound(context.Background(), ExternalInboundMessage{
		Conversation: ref,
	})
	if err != nil {
		t.Fatalf("ResolveInbound(cleared cursor): %v", err)
	}
	if clearedDecision.Match != GroupRouteDefault || clearedDecision.TargetSessionID != "sess-a" {
		t.Fatalf("ResolveInbound(cleared cursor) = %#v, want default->sess-a", clearedDecision)
	}
}

func TestGroupServiceEnsureGroupPreservesLastAddressedHandle(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewGroupService(store)
	ref := testConversationRef()

	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup(create): %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant(alpha): %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "beta",
		SessionID: "sess-b",
	}); err != nil {
		t.Fatalf("UpsertParticipant(beta): %v", err)
	}
	if err := svc.UpdateCursor(context.Background(), testControllerCaller(), UpdateCursorInput{
		RootConversation: ref,
		Handle:           "beta",
	}); err != nil {
		t.Fatalf("UpdateCursor(beta): %v", err)
	}

	updated, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup(update): %v", err)
	}
	if updated.LastAddressedHandle != "beta" {
		t.Fatalf("EnsureGroup(update) last addressed = %q, want beta", updated.LastAddressedHandle)
	}
}

func TestBindingServiceResolveByConversationRejectsDuplicateActiveBindings(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := store.Create(beads.Bead{
		Title:  conversationTitle(ref),
		Type:   "task",
		Labels: []string{"gc:extmsg-binding", labelBindingBase, bindingConversationLabel(ref), bindingSessionLabel("sess-a")},
		Metadata: encodeMetadataFields(nil, map[string]string{
			"schema_version":         strconv.Itoa(schemaVersion),
			"scope_id":               ref.ScopeID,
			"provider":               ref.Provider,
			"account_id":             ref.AccountID,
			"conversation_id":        ref.ConversationID,
			"parent_conversation_id": ref.ParentConversationID,
			"conversation_kind":      string(ref.Kind),
			"session_id":             "sess-a",
			"binding_generation":     "1",
			"bound_at":               formatTime(testNow()),
			"last_touched_at":        formatTime(testNow()),
			"created_by_kind":        string(CallerController),
			"created_by_id":          "controller-1",
		}),
	}); err != nil {
		t.Fatalf("Create(binding a): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  conversationTitle(ref),
		Type:   "task",
		Labels: []string{"gc:extmsg-binding", labelBindingBase, bindingConversationLabel(ref), bindingSessionLabel("sess-b")},
		Metadata: encodeMetadataFields(nil, map[string]string{
			"schema_version":         strconv.Itoa(schemaVersion),
			"scope_id":               ref.ScopeID,
			"provider":               ref.Provider,
			"account_id":             ref.AccountID,
			"conversation_id":        ref.ConversationID,
			"parent_conversation_id": ref.ParentConversationID,
			"conversation_kind":      string(ref.Kind),
			"session_id":             "sess-b",
			"binding_generation":     "2",
			"bound_at":               formatTime(testNow().Add(time.Minute)),
			"last_touched_at":        formatTime(testNow().Add(time.Minute)),
			"created_by_kind":        string(CallerController),
			"created_by_id":          "controller-1",
		}),
	}); err != nil {
		t.Fatalf("Create(binding b): %v", err)
	}

	_, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if !errors.Is(err, ErrInvariantViolation) {
		t.Fatalf("ResolveByConversation(duplicate active) error = %v, want ErrInvariantViolation", err)
	}
}

func TestGroupServiceResolveInboundRejectsDuplicateParticipants(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewGroupService(store)
	ref := testConversationRef()

	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  group.ID + "/alpha-a",
		Type:   "task",
		Labels: []string{"gc:extmsg-participant", labelGroupParticipantBase, groupParticipantLabel(group.ID), groupParticipantSessionLabel("sess-a")},
		Metadata: encodeMetadataFields(nil, map[string]string{
			"schema_version": strconv.Itoa(schemaVersion),
			"group_id":       group.ID,
			"handle":         "alpha",
			"session_id":     "sess-a",
			"public":         "false",
		}),
	}); err != nil {
		t.Fatalf("Create(participant a): %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Title:  group.ID + "/alpha-b",
		Type:   "task",
		Labels: []string{"gc:extmsg-participant", labelGroupParticipantBase, groupParticipantLabel(group.ID), groupParticipantSessionLabel("sess-b")},
		Metadata: encodeMetadataFields(nil, map[string]string{
			"schema_version": strconv.Itoa(schemaVersion),
			"group_id":       group.ID,
			"handle":         "alpha",
			"session_id":     "sess-b",
			"public":         "false",
		}),
	}); err != nil {
		t.Fatalf("Create(participant b): %v", err)
	}

	_, err = svc.ResolveInbound(context.Background(), ExternalInboundMessage{Conversation: ref})
	if !errors.Is(err, ErrInvariantViolation) {
		t.Fatalf("ResolveInbound(duplicate participants) error = %v, want ErrInvariantViolation", err)
	}
}

func TestGroupServiceUpsertParticipantPreservesSessionLabelOnIdempotentUpdate(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewGroupService(store)
	ref := testConversationRef()

	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	participant, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	})
	if err != nil {
		t.Fatalf("UpsertParticipant(create): %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
		Metadata:  map[string]string{"note": "updated"},
	}); err != nil {
		t.Fatalf("UpsertParticipant(idempotent): %v", err)
	}

	items, err := store.ListByLabel(groupParticipantSessionLabel("sess-a"), 0)
	if err != nil {
		t.Fatalf("ListByLabel(groupParticipantSessionLabel): %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("participant session label count = %d, want 1", len(items))
	}
	if items[0].ID != participant.ID {
		t.Fatalf("participant session label ID = %s, want %s", items[0].ID, participant.ID)
	}
}

func TestGroupServiceParticipantMutationsEnforceOwnership(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewGroupService(store)
	ref := testConversationRef()

	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	_, err = svc.UpsertParticipant(context.Background(), Caller{
		Kind:      CallerAdapter,
		ID:        "adapter-2",
		Provider:  "slack",
		AccountID: "acct-1",
	}, UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("UpsertParticipant(foreign adapter) error = %v, want ErrUnauthorized", err)
	}
}

func TestGroupServiceParticipantMutationsAllowSameScopeAdapterAndSyncTranscript(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	groups := fabric.Groups
	transcript := fabric.Transcript
	ref := testConversationRef()

	group, err := groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := groups.UpsertParticipant(context.Background(), testAdapterCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant(same-scope adapter): %v", err)
	}
	membership := membershipRecordBySession(t, transcript, ref, "sess-a")
	if !sameMembershipOwners(membership.Owners, []MembershipOwner{MembershipOwnerGroup}) {
		t.Fatalf("membership owners = %#v, want [group]", membership.Owners)
	}

	if err := groups.RemoveParticipant(context.Background(), testAdapterCaller(), RemoveParticipantInput{
		GroupID: group.ID,
		Handle:  "alpha",
	}); err != nil {
		t.Fatalf("RemoveParticipant(same-scope adapter): %v", err)
	}
	if got := membershipSessionIDs(t, transcript, ref); len(got) != 0 {
		t.Fatalf("memberships(after adapter removal) = %#v, want []", got)
	}
}

func TestTranscriptServiceAppendDedupeAndList(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	first, err := svc.Append(context.Background(), AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-1",
		Text:              "hello",
		CreatedAt:         testNow(),
	})
	if err != nil {
		t.Fatalf("Append(first): %v", err)
	}
	dup, err := svc.Append(context.Background(), AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-1",
		Text:              "hello again",
		CreatedAt:         testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Append(duplicate): %v", err)
	}
	if dup.ID != first.ID || dup.Sequence != first.Sequence {
		t.Fatalf("Append(duplicate) = %#v, want same record as first %#v", dup, first)
	}
	second, err := svc.Append(context.Background(), AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-2",
		Text:              "second",
		CreatedAt:         testNow().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Append(second): %v", err)
	}
	if second.Sequence != first.Sequence+1 {
		t.Fatalf("second.Sequence = %d, want %d", second.Sequence, first.Sequence+1)
	}

	got, err := svc.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List len = %d, want 2", len(got))
	}
	if got[0].ProviderMessageID != "msg-1" || got[1].ProviderMessageID != "msg-2" {
		t.Fatalf("List provider_message_ids = %#v, want msg-1,msg-2", []string{got[0].ProviderMessageID, got[1].ProviderMessageID})
	}
}

func TestTranscriptServiceListOrderLimitAndCursor(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	const total = 5
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("msg-%d", i+1)
		if _, err := svc.Append(context.Background(), AppendTranscriptInput{
			Caller:            testAdapterCaller(),
			Conversation:      ref,
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: id,
			Text:              id,
			CreatedAt:         testNow().Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("Append(%s): %v", id, err)
		}
	}

	providerIDs := func(records []ConversationTranscriptRecord) []string {
		ids := make([]string, len(records))
		for i, r := range records {
			ids[i] = r.ProviderMessageID
		}
		return ids
	}

	// Descending + limit returns the newest entries, newest-first.
	desc, err := svc.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		Limit:        2,
		Order:        TranscriptOrderDesc,
	})
	if err != nil {
		t.Fatalf("List(desc): %v", err)
	}
	if got := providerIDs(desc); len(got) != 2 || got[0] != "msg-5" || got[1] != "msg-4" {
		t.Fatalf("List(desc) = %#v, want [msg-5 msg-4]", got)
	}

	// Empty order defaults to ascending (backwards compatible).
	asc, err := svc.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		Limit:        2,
	})
	if err != nil {
		t.Fatalf("List(default asc): %v", err)
	}
	if got := providerIDs(asc); len(got) != 2 || got[0] != "msg-1" || got[1] != "msg-2" {
		t.Fatalf("List(default asc) = %#v, want [msg-1 msg-2]", got)
	}

	// AfterSequence cursor is an exclusive lower bound under both orders.
	descAfter, err := svc.List(context.Background(), ListTranscriptInput{
		Caller:        testControllerCaller(),
		Conversation:  ref,
		AfterSequence: 3,
		Order:         TranscriptOrderDesc,
	})
	if err != nil {
		t.Fatalf("List(desc, after=3): %v", err)
	}
	if got := providerIDs(descAfter); len(got) != 2 || got[0] != "msg-5" || got[1] != "msg-4" {
		t.Fatalf("List(desc, after=3) = %#v, want [msg-5 msg-4]", got)
	}

	// Invalid order is rejected.
	if _, err := svc.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		Order:        TranscriptOrder("sideways"),
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("List(invalid order) err = %v, want ErrInvalidInput", err)
	}
}

func TestTranscriptServiceListOrderCrossBucket(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	// 130 entries guarantee the stream spans more than one transcript bucket
	// regardless of the exact bucket-boundary arithmetic, so the descending
	// walk must cross at least one boundary to collect the newest entries.
	const total = 130
	for i := 0; i < total; i++ {
		id := fmt.Sprintf("msg-%d", i+1)
		if _, err := svc.Append(context.Background(), AppendTranscriptInput{
			Caller:            testAdapterCaller(),
			Conversation:      ref,
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: id,
			Text:              id,
			CreatedAt:         testNow().Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("Append(%s): %v", id, err)
		}
	}

	providerIDs := func(records []ConversationTranscriptRecord) []string {
		ids := make([]string, len(records))
		for i, r := range records {
			ids[i] = r.ProviderMessageID
		}
		return ids
	}

	// Full ascending read establishes the ground-truth ordering without
	// hardcoding which sequence lands in which bucket.
	all, err := svc.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		Limit:        total,
	})
	if err != nil {
		t.Fatalf("List(asc full): %v", err)
	}
	if len(all) != total {
		t.Fatalf("List(asc full) returned %d records, want %d", len(all), total)
	}
	highestSeq := all[len(all)-1].Sequence

	// Descending + limit returns the highest sequences, newest-first, even
	// though the newest entries live in a later bucket than the oldest.
	const topN = 10
	desc, err := svc.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		Limit:        topN,
		Order:        TranscriptOrderDesc,
	})
	if err != nil {
		t.Fatalf("List(desc): %v", err)
	}
	if len(desc) != topN {
		t.Fatalf("List(desc) returned %d records, want %d", len(desc), topN)
	}
	// Strictly descending by sequence.
	for i := 1; i < len(desc); i++ {
		if desc[i-1].Sequence <= desc[i].Sequence {
			t.Fatalf("List(desc) not strictly descending at %d: %d <= %d",
				i, desc[i-1].Sequence, desc[i].Sequence)
		}
	}
	// Derive the expectation: the newest topN entries from the ascending read,
	// reversed. This proves the descending walk pulled the highest sequences
	// across the bucket boundary.
	wantDesc := make([]string, 0, topN)
	for i := 0; i < topN; i++ {
		wantDesc = append(wantDesc, all[len(all)-1-i].ProviderMessageID)
	}
	if got := providerIDs(desc); !slices.Equal(got, wantDesc) {
		t.Fatalf("List(desc) = %#v, want %#v", got, wantDesc)
	}
	// Sanity: the returned window genuinely spans more than one bucket.
	if transcriptBucket(desc[0].Sequence) == transcriptBucket(desc[len(desc)-1].Sequence) {
		t.Fatalf("List(desc) window did not cross a bucket boundary: seq %d..%d in bucket %d",
			desc[len(desc)-1].Sequence, desc[0].Sequence, transcriptBucket(desc[0].Sequence))
	}

	// AfterSequence as an exclusive lower bound near the top of the stream:
	// the top 5 entries, descending.
	const tail = 5
	descAfter, err := svc.List(context.Background(), ListTranscriptInput{
		Caller:        testControllerCaller(),
		Conversation:  ref,
		AfterSequence: highestSeq - tail,
		Limit:         100,
		Order:         TranscriptOrderDesc,
	})
	if err != nil {
		t.Fatalf("List(desc, after=%d): %v", highestSeq-tail, err)
	}
	wantAfter := make([]string, 0, tail)
	for i := 0; i < tail; i++ {
		wantAfter = append(wantAfter, all[len(all)-1-i].ProviderMessageID)
	}
	if got := providerIDs(descAfter); !slices.Equal(got, wantAfter) {
		t.Fatalf("List(desc, after=%d) = %#v, want %#v", highestSeq-tail, got, wantAfter)
	}
}

func TestTranscriptServiceMembershipBackfillAndAck(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	for i, id := range []string{"msg-1", "msg-2"} {
		if _, err := svc.Append(context.Background(), AppendTranscriptInput{
			Caller:            testAdapterCaller(),
			Conversation:      ref,
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: id,
			Text:              id,
			CreatedAt:         testNow().Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("Append(%s): %v", id, err)
		}
	}

	membership, err := svc.EnsureMembership(context.Background(), EnsureMembershipInput{
		Caller:         testControllerCaller(),
		Conversation:   ref,
		SessionID:      "sess-a",
		BackfillPolicy: MembershipBackfillAll,
		Now:            testNow().Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatalf("EnsureMembership: %v", err)
	}
	if membership.JoinedSequence != 2 {
		t.Fatalf("JoinedSequence = %d, want 2", membership.JoinedSequence)
	}

	backfill, err := svc.ListBackfill(context.Background(), ListBackfillInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ListBackfill(initial): %v", err)
	}
	if len(backfill) != 2 {
		t.Fatalf("ListBackfill(initial) len = %d, want 2", len(backfill))
	}

	if err := svc.Ack(context.Background(), AckMembershipInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Sequence:     backfill[0].Sequence,
	}); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	backfill, err = svc.ListBackfill(context.Background(), ListBackfillInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ListBackfill(after ack): %v", err)
	}
	if len(backfill) != 1 || backfill[0].ProviderMessageID != "msg-2" {
		t.Fatalf("ListBackfill(after ack) = %#v, want only msg-2", backfill)
	}
}

func TestTranscriptServiceHydrationPendingRejectsLiveAppendAndReplay(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	if _, err := svc.BeginHydration(context.Background(), testAdapterCaller(), ref, nil); err != nil {
		t.Fatalf("BeginHydration: %v", err)
	}
	_, err := svc.Append(context.Background(), AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "live-1",
		Text:              "live",
		CreatedAt:         testNow(),
	})
	if !errors.Is(err, ErrHydrationPending) {
		t.Fatalf("Append(live while pending) error = %v, want ErrHydrationPending", err)
	}
	if _, err := svc.Append(context.Background(), AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceHydrated,
		ProviderMessageID: "hist-1",
		Text:              "history",
		CreatedAt:         testNow(),
	}); err != nil {
		t.Fatalf("Append(hydrated): %v", err)
	}
	if _, err := svc.EnsureMembership(context.Background(), EnsureMembershipInput{
		Caller:         testControllerCaller(),
		Conversation:   ref,
		SessionID:      "sess-a",
		BackfillPolicy: MembershipBackfillAll,
		Now:            testNow(),
	}); err != nil {
		t.Fatalf("EnsureMembership: %v", err)
	}
	_, err = svc.ListBackfill(context.Background(), ListBackfillInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Limit:        10,
	})
	if !errors.Is(err, ErrHydrationPending) {
		t.Fatalf("ListBackfill(pending) error = %v, want ErrHydrationPending", err)
	}
	if _, err := svc.CompleteHydration(context.Background(), testAdapterCaller(), ref); err != nil {
		t.Fatalf("CompleteHydration: %v", err)
	}
	backfill, err := svc.ListBackfill(context.Background(), ListBackfillInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ListBackfill(after complete): %v", err)
	}
	if len(backfill) != 1 || backfill[0].ProviderMessageID != "hist-1" {
		t.Fatalf("ListBackfill(after complete) = %#v, want only hist-1", backfill)
	}
}

func TestTranscriptServiceHydrationTransitionsRequirePending(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	if _, err := svc.CompleteHydration(context.Background(), testAdapterCaller(), ref); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("CompleteHydration(without pending) error = %v, want ErrInvalidInput", err)
	}
	if _, err := svc.BeginHydration(context.Background(), testAdapterCaller(), ref, nil); err != nil {
		t.Fatalf("BeginHydration: %v", err)
	}
	if _, err := svc.CompleteHydration(context.Background(), testAdapterCaller(), ref); err != nil {
		t.Fatalf("CompleteHydration(after pending): %v", err)
	}
	if _, err := svc.MarkHydrationFailed(context.Background(), testAdapterCaller(), ref, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("MarkHydrationFailed(after complete) error = %v, want ErrInvalidInput", err)
	}
}

func TestTranscriptServiceHydrationFailedStillAllowsBackfill(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Transcript
	ref := testConversationRef()

	if _, err := svc.BeginHydration(context.Background(), testAdapterCaller(), ref, nil); err != nil {
		t.Fatalf("BeginHydration: %v", err)
	}
	if _, err := svc.Append(context.Background(), AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceHydrated,
		ProviderMessageID: "hist-1",
		Text:              "history",
		CreatedAt:         testNow(),
	}); err != nil {
		t.Fatalf("Append(hydrated): %v", err)
	}
	if _, err := svc.EnsureMembership(context.Background(), EnsureMembershipInput{
		Caller:         testControllerCaller(),
		Conversation:   ref,
		SessionID:      "sess-a",
		BackfillPolicy: MembershipBackfillAll,
		Now:            testNow(),
	}); err != nil {
		t.Fatalf("EnsureMembership: %v", err)
	}
	if _, err := svc.MarkHydrationFailed(context.Background(), testAdapterCaller(), ref, nil); err != nil {
		t.Fatalf("MarkHydrationFailed: %v", err)
	}

	backfill, err := svc.ListBackfill(context.Background(), ListBackfillInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("ListBackfill(after failed hydration): %v", err)
	}
	if len(backfill) != 1 || backfill[0].ProviderMessageID != "hist-1" {
		t.Fatalf("ListBackfill(after failed hydration) = %#v, want only hist-1", backfill)
	}
}

func TestGroupServiceParticipantLifecycleSyncsTranscriptMembership(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	groups := fabric.Groups
	transcript := fabric.Transcript
	ref := testConversationRef()

	group, err := groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	memberships, err := transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships(after add): %v", err)
	}
	if len(memberships) != 1 || memberships[0].SessionID != "sess-a" {
		t.Fatalf("ListMemberships(after add) = %#v, want sess-a", memberships)
	}
	if err := groups.RemoveParticipant(context.Background(), testControllerCaller(), RemoveParticipantInput{
		GroupID: group.ID,
		Handle:  "alpha",
	}); err != nil {
		t.Fatalf("RemoveParticipant: %v", err)
	}
	memberships, err = transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships(after remove): %v", err)
	}
	if len(memberships) != 0 {
		t.Fatalf("ListMemberships(after remove) len = %d, want 0", len(memberships))
	}
}

func TestGroupServiceUpsertParticipantReassignsMembershipWhenLastHandleMoves(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	groups := fabric.Groups
	transcript := fabric.Transcript
	ref := testConversationRef()

	group, err := groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant(alpha=sess-a): %v", err)
	}
	if got := membershipSessionIDs(t, transcript, ref); !sameMembers(got, []string{"sess-a"}) {
		t.Fatalf("memberships(after initial add) = %#v, want [sess-a]", got)
	}

	if _, err := groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-b",
	}); err != nil {
		t.Fatalf("UpsertParticipant(alpha=sess-b): %v", err)
	}
	if got := membershipSessionIDs(t, transcript, ref); !sameMembers(got, []string{"sess-b"}) {
		t.Fatalf("memberships(after reassignment) = %#v, want [sess-b]", got)
	}
}

func TestGroupServiceUpsertParticipantReassignKeepsMembershipWhenSessionHasAnotherHandle(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	groups := fabric.Groups
	transcript := fabric.Transcript
	ref := testConversationRef()

	group, err := groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	for _, participant := range []UpsertParticipantInput{
		{GroupID: group.ID, Handle: "alpha", SessionID: "sess-a"},
		{GroupID: group.ID, Handle: "beta", SessionID: "sess-a"},
	} {
		if _, err := groups.UpsertParticipant(context.Background(), testControllerCaller(), participant); err != nil {
			t.Fatalf("UpsertParticipant(%s=%s): %v", participant.Handle, participant.SessionID, err)
		}
	}

	if _, err := groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-b",
	}); err != nil {
		t.Fatalf("UpsertParticipant(alpha=sess-b): %v", err)
	}
	if got := membershipSessionIDs(t, transcript, ref); !sameMembers(got, []string{"sess-a", "sess-b"}) {
		t.Fatalf("memberships(after reassignment with surviving handle) = %#v, want [sess-a sess-b]", got)
	}
}

func TestGroupServiceRemoveParticipantKeepsMembershipUntilLastHandle(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	groups := fabric.Groups
	transcript := fabric.Transcript
	ref := testConversationRef()

	group, err := groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	for _, participant := range []UpsertParticipantInput{
		{GroupID: group.ID, Handle: "alpha", SessionID: "sess-a"},
		{GroupID: group.ID, Handle: "beta", SessionID: "sess-a"},
	} {
		if _, err := groups.UpsertParticipant(context.Background(), testControllerCaller(), participant); err != nil {
			t.Fatalf("UpsertParticipant(%s=%s): %v", participant.Handle, participant.SessionID, err)
		}
	}

	if err := groups.RemoveParticipant(context.Background(), testControllerCaller(), RemoveParticipantInput{
		GroupID: group.ID,
		Handle:  "alpha",
	}); err != nil {
		t.Fatalf("RemoveParticipant(alpha): %v", err)
	}
	if got := membershipSessionIDs(t, transcript, ref); !sameMembers(got, []string{"sess-a"}) {
		t.Fatalf("memberships(after removing one handle) = %#v, want [sess-a]", got)
	}

	if err := groups.RemoveParticipant(context.Background(), testControllerCaller(), RemoveParticipantInput{
		GroupID: group.ID,
		Handle:  "beta",
	}); err != nil {
		t.Fatalf("RemoveParticipant(beta): %v", err)
	}
	if got := membershipSessionIDs(t, transcript, ref); len(got) != 0 {
		t.Fatalf("memberships(after removing last handle) = %#v, want []", got)
	}
}

func TestBindingServiceBindEnsuresTranscriptMembershipForController(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	memberships, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(memberships) != 1 {
		t.Fatalf("ListMemberships len = %d, want 1", len(memberships))
	}
	if memberships[0].SessionID != "sess-a" || memberships[0].BackfillPolicy != MembershipBackfillSinceJoin {
		t.Fatalf("membership = %#v, want sess-a since_join", memberships[0])
	}
	if !sameMembershipOwners(memberships[0].Owners, []MembershipOwner{MembershipOwnerBinding}) {
		t.Fatalf("membership owners = %#v, want [binding]", memberships[0].Owners)
	}
}

func TestTranscriptServiceUpdateMembershipAddsManualOwnerAndRecomputesPolicy(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	updated, err := fabric.Transcript.UpdateMembership(context.Background(), UpdateMembershipInput{
		Caller:         testControllerCaller(),
		Conversation:   ref,
		SessionID:      "sess-a",
		BackfillPolicy: MembershipBackfillAll,
	})
	if err != nil {
		t.Fatalf("UpdateMembership(binding+manual): %v", err)
	}
	if updated.BackfillPolicy != MembershipBackfillAll {
		t.Fatalf("binding+manual backfill policy = %q, want all", updated.BackfillPolicy)
	}
	if !sameMembershipOwners(updated.Owners, []MembershipOwner{MembershipOwnerBinding, MembershipOwnerManual}) {
		t.Fatalf("binding+manual owners = %#v, want [binding manual]", updated.Owners)
	}

	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	updated, err = fabric.Transcript.UpdateMembership(context.Background(), UpdateMembershipInput{
		Caller:         testControllerCaller(),
		Conversation:   ref,
		SessionID:      "sess-a",
		BackfillPolicy: MembershipBackfillSinceJoin,
	})
	if err != nil {
		t.Fatalf("UpdateMembership(group dominant): %v", err)
	}
	if updated.BackfillPolicy != MembershipBackfillAll {
		t.Fatalf("group-dominant backfill policy = %q, want all", updated.BackfillPolicy)
	}
	if !sameMembershipOwners(updated.Owners, []MembershipOwner{MembershipOwnerBinding, MembershipOwnerGroup, MembershipOwnerManual}) {
		t.Fatalf("group-dominant owners = %#v, want [binding group manual]", updated.Owners)
	}

	if err := fabric.Groups.RemoveParticipant(context.Background(), testControllerCaller(), RemoveParticipantInput{
		GroupID: group.ID,
		Handle:  "alpha",
	}); err != nil {
		t.Fatalf("RemoveParticipant: %v", err)
	}
	afterGroupRemoval := membershipRecordBySession(t, fabric.Transcript, ref, "sess-a")
	if afterGroupRemoval.BackfillPolicy != MembershipBackfillSinceJoin {
		t.Fatalf("after group removal backfill policy = %q, want since_join", afterGroupRemoval.BackfillPolicy)
	}
	if afterGroupRemoval.ManualBackfill != MembershipBackfillSinceJoin {
		t.Fatalf("after group removal manual backfill = %q, want since_join", afterGroupRemoval.ManualBackfill)
	}
	if !sameMembershipOwners(afterGroupRemoval.Owners, []MembershipOwner{MembershipOwnerBinding, MembershipOwnerManual}) {
		t.Fatalf("after group removal owners = %#v, want [binding manual]", afterGroupRemoval.Owners)
	}
}

func TestBindingServiceBindEnsuresTranscriptMembershipForAdapter(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testAdapterCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	membership := membershipRecordBySession(t, fabric.Transcript, ref, "sess-a")
	if membership.BackfillPolicy != MembershipBackfillSinceJoin {
		t.Fatalf("adapter bind backfill policy = %q, want since_join", membership.BackfillPolicy)
	}
	if !sameMembershipOwners(membership.Owners, []MembershipOwner{MembershipOwnerBinding}) {
		t.Fatalf("adapter bind owners = %#v, want [binding]", membership.Owners)
	}
}

func TestGroupServiceRemoveParticipantKeepsMembershipWhenBindingOwnsConversation(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	if err := fabric.Groups.RemoveParticipant(context.Background(), testControllerCaller(), RemoveParticipantInput{
		GroupID: group.ID,
		Handle:  "alpha",
	}); err != nil {
		t.Fatalf("RemoveParticipant: %v", err)
	}

	membership := membershipRecordBySession(t, fabric.Transcript, ref, "sess-a")
	if membership.SessionID != "sess-a" {
		t.Fatalf("membership session = %q, want sess-a", membership.SessionID)
	}
	if membership.BackfillPolicy != MembershipBackfillSinceJoin {
		t.Fatalf("membership backfill policy = %q, want since_join", membership.BackfillPolicy)
	}
	if !sameMembershipOwners(membership.Owners, []MembershipOwner{MembershipOwnerBinding}) {
		t.Fatalf("membership owners = %#v, want [binding]", membership.Owners)
	}
}

func TestBindingServiceUnbindKeepsMembershipWhenGroupOwnsConversation(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	membership := membershipRecordBySession(t, fabric.Transcript, ref, "sess-a")
	if membership.BackfillPolicy != MembershipBackfillAll {
		t.Fatalf("membership backfill policy before unbind = %q, want all", membership.BackfillPolicy)
	}

	if _, err := fabric.Bindings.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		Conversation: &ref,
		Now:          testNow().Add(time.Minute),
	}); err != nil {
		t.Fatalf("Unbind: %v", err)
	}

	membership = membershipRecordBySession(t, fabric.Transcript, ref, "sess-a")
	if membership.BackfillPolicy != MembershipBackfillAll {
		t.Fatalf("membership backfill policy after unbind = %q, want all", membership.BackfillPolicy)
	}
	if !sameMembershipOwners(membership.Owners, []MembershipOwner{MembershipOwnerGroup}) {
		t.Fatalf("membership owners = %#v, want [group]", membership.Owners)
	}
}

func TestBindingServiceUnbindRemovesMembershipWhenNoOtherOwnerRemains(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if _, err := fabric.Bindings.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		Conversation: &ref,
		Now:          testNow().Add(time.Minute),
	}); err != nil {
		t.Fatalf("Unbind: %v", err)
	}

	if got := membershipSessionIDs(t, fabric.Transcript, ref); len(got) != 0 {
		t.Fatalf("memberships(after unbind) = %#v, want []", got)
	}
}

func TestBindingServiceUnbindRetriesTranscriptCleanupWhenRemovalFails(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	transcript := &flakyTranscriptService{failRemoveCount: 1, err: errors.New("boom")}
	svc := newBindingService(store, nil, transcript, newBindingLockPool())
	ref := testConversationRef()

	binding, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}

	closed, err := svc.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		Conversation: &ref,
		Now:          testNow().Add(time.Minute),
	})
	if err == nil || !errors.Is(err, ErrTranscriptSyncFailed) {
		t.Fatalf("Unbind(first) error = %v, want ErrTranscriptSyncFailed", err)
	}
	if len(closed) != 0 {
		t.Fatalf("Unbind(first) closed = %#v, want none because cleanup failed before close", closed)
	}
	item, err := store.Get(binding.ID)
	if err != nil {
		t.Fatalf("Get(binding after failed unbind): %v", err)
	}
	if item.Status != "open" {
		t.Fatalf("binding status after failed unbind = %q, want open", item.Status)
	}

	closed, err = svc.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		Conversation: &ref,
		Now:          testNow().Add(2 * time.Minute),
	})
	if err != nil {
		t.Fatalf("Unbind(retry): %v", err)
	}
	if len(closed) != 1 || closed[0].ID != binding.ID || closed[0].Status != BindingEnded {
		t.Fatalf("Unbind(retry) closed = %#v, want ended binding %s", closed, binding.ID)
	}
	if transcript.removeCalls != 2 {
		t.Fatalf("RemoveMembership calls = %d, want 2", transcript.removeCalls)
	}
}

func TestBindingServiceResolveByConversationExpiresBindingAndRemovesMembership(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()
	expiredAt := testNow().Add(-time.Minute)

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		ExpiresAt:    &expiredAt,
		Now:          testNow().Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("Bind(expired): %v", err)
	}

	got, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if got != nil {
		t.Fatalf("ResolveByConversation = %#v, want nil for expired binding", got)
	}
	if members := membershipSessionIDs(t, fabric.Transcript, ref); len(members) != 0 {
		t.Fatalf("memberships(after expiry cleanup) = %#v, want []", members)
	}
}

func TestGroupServiceUpsertParticipantRetriesTranscriptCleanupAfterReassignment(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	transcript := &flakyTranscriptService{failRemoveCount: 1, err: errors.New("boom")}
	svc := newGroupService(store, sharedBindingLockPool(store), transcript)
	ref := testConversationRef()

	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant(initial): %v", err)
	}

	_, err = svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-b",
	})
	if err == nil || !errors.Is(err, ErrTranscriptSyncFailed) {
		t.Fatalf("UpsertParticipant(reassign first) error = %v, want ErrTranscriptSyncFailed", err)
	}
	items, err := store.ListByLabel(groupParticipantSessionLabel("sess-b"), 0)
	if err != nil {
		t.Fatalf("ListByLabel(sess-b): %v", err)
	}
	if len(items) != 1 || items[0].Metadata["previous_session_id_pending_cleanup"] != "sess-a" {
		t.Fatalf("participant pending cleanup after failed reassignment = %#v, want sess-a marker", items)
	}

	participant, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-b",
	})
	if err != nil {
		t.Fatalf("UpsertParticipant(reassign retry): %v", err)
	}
	item, err := store.Get(participant.ID)
	if err != nil {
		t.Fatalf("Get(participant): %v", err)
	}
	if item.Metadata["previous_session_id_pending_cleanup"] != "" {
		t.Fatalf("participant pending cleanup after retry = %q, want empty", item.Metadata["previous_session_id_pending_cleanup"])
	}
	if transcript.removeCalls != 2 {
		t.Fatalf("RemoveMembership calls = %d, want 2", transcript.removeCalls)
	}
}

func TestGroupServiceUpsertParticipantCarriesDeferredCleanupAcrossLaterReassignment(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	transcript := &flakyTranscriptService{failRemoveCount: 1, err: errors.New("boom")}
	svc := newGroupService(store, sharedBindingLockPool(store), transcript)
	ref := testConversationRef()

	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant(initial): %v", err)
	}

	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-b",
	}); err == nil || !errors.Is(err, ErrTranscriptSyncFailed) {
		t.Fatalf("UpsertParticipant(first reassignment) error = %v, want ErrTranscriptSyncFailed", err)
	}

	participant, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-c",
	})
	if err != nil {
		t.Fatalf("UpsertParticipant(second reassignment): %v", err)
	}
	item, err := store.Get(participant.ID)
	if err != nil {
		t.Fatalf("Get(participant): %v", err)
	}
	if item.Metadata["previous_session_id_pending_cleanup"] != "" {
		t.Fatalf("participant pending cleanup after second reassignment = %q, want empty", item.Metadata["previous_session_id_pending_cleanup"])
	}
	if transcript.removeCalls != 3 {
		t.Fatalf("RemoveMembership calls = %d, want 3", transcript.removeCalls)
	}
}

func TestBindingServiceBindRetriesTranscriptSyncOnRebind(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	transcript := &flakyTranscriptService{failEnsureCount: 1, err: errors.New("boom")}
	svc := newBindingService(store, nil, transcript, newBindingLockPool())
	ref := testConversationRef()

	_, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err == nil || !errors.Is(err, ErrTranscriptSyncFailed) {
		t.Fatalf("Bind(first) error = %v, want ErrTranscriptSyncFailed", err)
	}
	_, err = svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(retry): %v", err)
	}
	if transcript.ensureCalls != 2 {
		t.Fatalf("EnsureMembership calls = %d, want 2", transcript.ensureCalls)
	}
}

func TestGroupServiceUpsertParticipantRetriesTranscriptSync(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	transcript := &flakyTranscriptService{failEnsureCount: 1, err: errors.New("boom")}
	svc := newGroupService(store, sharedBindingLockPool(store), transcript)
	ref := testConversationRef()

	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	_, err = svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	})
	if err == nil || !errors.Is(err, ErrTranscriptSyncFailed) {
		t.Fatalf("UpsertParticipant(first) error = %v, want ErrTranscriptSyncFailed", err)
	}
	_, err = svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	})
	if err != nil {
		t.Fatalf("UpsertParticipant(retry): %v", err)
	}
	if transcript.ensureCalls != 2 {
		t.Fatalf("EnsureMembership calls = %d, want 2", transcript.ensureCalls)
	}
}

func TestGroupServiceRemoveParticipantRetriesTranscriptCleanup(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	transcript := &flakyTranscriptService{failRemoveCount: 1, err: errors.New("boom")}
	svc := newGroupService(store, sharedBindingLockPool(store), transcript)
	ref := testConversationRef()

	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	err = svc.RemoveParticipant(context.Background(), testControllerCaller(), RemoveParticipantInput{
		GroupID: group.ID,
		Handle:  "alpha",
	})
	if err == nil || !errors.Is(err, ErrTranscriptSyncFailed) {
		t.Fatalf("RemoveParticipant(first) error = %v, want ErrTranscriptSyncFailed", err)
	}
	err = svc.RemoveParticipant(context.Background(), testControllerCaller(), RemoveParticipantInput{
		GroupID: group.ID,
		Handle:  "alpha",
	})
	if err != nil {
		t.Fatalf("RemoveParticipant(retry): %v", err)
	}
	if transcript.removeCalls != 2 {
		t.Fatalf("RemoveMembership calls = %d, want 2", transcript.removeCalls)
	}
}

// overrideResolveLiveSessionID substitutes resolveLiveSessionID for the
// duration of the test, restoring the original in t.Cleanup (Theme 18).
func overrideResolveLiveSessionID(t *testing.T, fn func(beads.Store, string) (string, error)) {
	t.Helper()
	prev := resolveLiveSessionID
	resolveLiveSessionID = fn
	t.Cleanup(func() { resolveLiveSessionID = prev })
}

// overrideReassignmentTranscript substitutes the transcript syncer used by
// ReassignSessionParticipants so tests can inject a flaky transcript and drive
// the retry-idempotence paths after a membership-migration failure.
func overrideReassignmentTranscript(t *testing.T, transcript groupTranscriptSync) {
	t.Helper()
	prev := newReassignmentTranscript
	newReassignmentTranscript = func(beads.Store, *bindingLockPool) groupTranscriptSync { return transcript }
	t.Cleanup(func() { newReassignmentTranscript = prev })
}

func TestGroupServiceUpsertParticipantStoresSessionName(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	sessID := makeSessionBead(t, store, "pl-alpha")

	svc := NewGroupService(store)
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	participant, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessID,
	})
	if err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	if participant.SessionName != "pl-alpha" {
		t.Errorf("SessionName = %q, want %q", participant.SessionName, "pl-alpha")
	}

	bead, err := store.Get(participant.ID)
	if err != nil {
		t.Fatalf("get participant bead: %v", err)
	}
	wantLabel := groupParticipantSessionNameLabel("pl-alpha")
	if !hasLabel(bead, wantLabel) {
		t.Errorf("participant bead missing label %q; labels: %v", wantLabel, bead.Labels)
	}
}

func TestGroupServiceResolveInboundFollowsRespawnedSession(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	// Session A is the original (now dead) bead; session B is the live respawn.
	sessAID := makeSessionBead(t, store, "pl-alpha")
	sessBID := makeSessionBead(t, store, "pl-alpha-v2") // different name, same slot

	svc := NewGroupService(store)
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessAID,
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	// Simulate respawn: close the old session bead (as happens in production on
	// handover), then override the resolver to point "pl-alpha" at session B.
	if err := store.Close(sessAID); err != nil {
		t.Fatalf("close session A bead: %v", err)
	}
	overrideResolveLiveSessionID(t, func(_ beads.Store, name string) (string, error) {
		if name == "pl-alpha" {
			return sessBID, nil
		}
		return "", errors.New("not found")
	})

	decision, err := svc.ResolveInbound(context.Background(), ExternalInboundMessage{Conversation: ref})
	if err != nil {
		t.Fatalf("ResolveInbound: %v", err)
	}
	if decision.TargetSessionID != sessBID {
		t.Errorf("TargetSessionID = %q, want %q (live session B)", decision.TargetSessionID, sessBID)
	}
}

func TestGroupServiceResolveOutboundFollowsRespawnedSession(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	sessAID := makeSessionBead(t, store, "pl-alpha")
	sessBID := makeSessionBead(t, store, "pl-alpha-v2")

	svc := NewGroupService(store)
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessAID,
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	if err := store.Close(sessAID); err != nil {
		t.Fatalf("close session A bead: %v", err)
	}
	overrideResolveLiveSessionID(t, func(_ beads.Store, name string) (string, error) {
		if name == "pl-alpha" {
			return sessBID, nil
		}
		return "", errors.New("not found")
	})

	// Session B should now be authorized to publish (participant overlay matched it).
	decision, err := svc.ResolveOutbound(context.Background(), ref, sessBID)
	if err != nil {
		t.Fatalf("ResolveOutbound: %v", err)
	}
	if decision.Match != GroupRouteParticipantMatch {
		t.Errorf("ResolveOutbound(sessB).Match = %q, want %q", decision.Match, GroupRouteParticipantMatch)
	}

	// Original dead session A should not be authorized after respawn.
	deadDecision, err := svc.ResolveOutbound(context.Background(), ref, sessAID)
	if err != nil {
		t.Fatalf("ResolveOutbound(dead): %v", err)
	}
	if deadDecision.Match != GroupRouteNoMatch {
		t.Errorf("ResolveOutbound(sessA).Match = %q, want GroupRouteNoMatch", deadDecision.Match)
	}
}

func TestReassignSessionParticipants(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	sessAID := makeSessionBead(t, store, "pl-alpha")

	fabric := NewServices(store)
	svc := fabric.Groups
	transcript := fabric.Transcript
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	participant, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessAID,
	})
	if err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	// The participant upsert seeds a group-owned membership for session A.
	if got := membershipSessionIDs(t, transcript, ref); !sameMembers(got, []string{sessAID}) {
		t.Fatalf("memberships(after upsert) = %#v, want [%s]", got, sessAID)
	}

	sessBID := makeSessionBead(t, store, "pl-alpha-v2")
	// Reassignment runs on a retired (closed) old session in production
	// (session_beads.go and session_resolution.go both call it after the old
	// bead is superseded); close sessAID so the test reflects that invariant.
	if err := store.Close(sessAID); err != nil {
		t.Fatalf("close retired session A bead: %v", err)
	}
	if err := ReassignSessionParticipants(context.Background(), store, sessAID, sessBID); err != nil {
		t.Fatalf("ReassignSessionParticipants: %v", err)
	}

	bead, err := store.Get(participant.ID)
	if err != nil {
		t.Fatalf("get participant bead: %v", err)
	}
	if bead.Metadata["session_id"] != sessBID {
		t.Errorf("session_id = %q, want %q", bead.Metadata["session_id"], sessBID)
	}
	if hasLabel(bead, groupParticipantSessionLabel(sessAID)) {
		t.Errorf("participant still has old session label %q", groupParticipantSessionLabel(sessAID))
	}
	if !hasLabel(bead, groupParticipantSessionLabel(sessBID)) {
		t.Errorf("participant missing new session label %q; labels: %v", groupParticipantSessionLabel(sessBID), bead.Labels)
	}
	// session_name label must be preserved (stable across respawn).
	if !hasLabel(bead, groupParticipantSessionNameLabel("pl-alpha")) {
		t.Errorf("participant missing session-name label %q; labels: %v", groupParticipantSessionNameLabel("pl-alpha"), bead.Labels)
	}
	// The group-owned transcript membership must follow the participant to the
	// respawned session: session B gains it, retired session A loses it. Without
	// this, transcript discovery (keyed by session ID) misses binding-less group
	// participants and the retired session's membership leaks.
	if got := membershipSessionIDs(t, transcript, ref); !sameMembers(got, []string{sessBID}) {
		t.Fatalf("memberships(after reassign) = %#v, want [%s]", got, sessBID)
	}
	membership := membershipRecordBySession(t, transcript, ref, sessBID)
	if !sameMembershipOwners(membership.Owners, []MembershipOwner{MembershipOwnerGroup}) {
		t.Errorf("membership owners = %#v, want [group]", membership.Owners)
	}
}

// TestReassignSessionParticipantsLeavesOtherSessionMembershipsIntact proves the
// reassign only migrates the membership for the retired session and never
// disturbs a co-resident participant bound to a different live session.
func TestReassignSessionParticipantsLeavesOtherSessionMembershipsIntact(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	sessAID := makeSessionBead(t, store, "pl-alpha")
	sessZID := makeSessionBead(t, store, "pl-zeta")

	fabric := NewServices(store)
	svc := fabric.Groups
	transcript := fabric.Transcript
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessAID,
	}); err != nil {
		t.Fatalf("UpsertParticipant(alpha): %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "zeta",
		SessionID: sessZID,
	}); err != nil {
		t.Fatalf("UpsertParticipant(zeta): %v", err)
	}
	if got := membershipSessionIDs(t, transcript, ref); !sameMembers(got, []string{sessAID, sessZID}) {
		t.Fatalf("memberships(after upserts) = %#v, want [%s %s]", got, sessAID, sessZID)
	}

	sessBID := makeSessionBead(t, store, "pl-alpha-v2")
	if err := store.Close(sessAID); err != nil {
		t.Fatalf("close retired session A bead: %v", err)
	}
	if err := ReassignSessionParticipants(context.Background(), store, sessAID, sessBID); err != nil {
		t.Fatalf("ReassignSessionParticipants: %v", err)
	}

	// alpha's membership moved A->B; zeta's session-Z membership is untouched.
	if got := membershipSessionIDs(t, transcript, ref); !sameMembers(got, []string{sessBID, sessZID}) {
		t.Fatalf("memberships(after reassign) = %#v, want [%s %s]", got, sessBID, sessZID)
	}
}

// TestReassignSessionParticipantsRetriesAfterEnsureFailure proves the handover
// survives a transcript ensure failure: the retired-session lookup label must be
// retained until membership migration commits, so a retry can rediscover the
// partially migrated participant and finish the move instead of stranding it.
func TestReassignSessionParticipantsRetriesAfterEnsureFailure(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	sessAID := makeSessionBead(t, store, "pl-alpha")

	svc := NewServices(store).Groups
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	participant, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessAID,
	})
	if err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	sessBID := makeSessionBead(t, store, "pl-alpha-v2")
	if err := store.Close(sessAID); err != nil {
		t.Fatalf("close retired session A bead: %v", err)
	}

	// First handover attempt: the replacement-session membership ensure fails.
	flaky := &flakyTranscriptService{failEnsureCount: 1, err: errors.New("boom")}
	overrideReassignmentTranscript(t, flaky)
	if err := ReassignSessionParticipants(context.Background(), store, sessAID, sessBID); err == nil || !errors.Is(err, ErrTranscriptSyncFailed) {
		t.Fatalf("ReassignSessionParticipants(first) error = %v, want ErrTranscriptSyncFailed", err)
	}

	// Regression: the partially migrated participant must still be discoverable by
	// the retired-session label so a retry can finish the handover. Dropping that
	// label before membership migration committed is the bug this guards.
	stale, err := store.ListByLabel(groupParticipantSessionLabel(sessAID), 0)
	if err != nil {
		t.Fatalf("ListByLabel(retired session): %v", err)
	}
	if len(stale) != 1 || stale[0].ID != participant.ID {
		t.Fatalf("retired-session label dropped before migration committed: %#v", stale)
	}
	if got := stale[0].Metadata["session_id"]; got != sessBID {
		t.Errorf("session_id after failed handover = %q, want %q (already swapped to replacement)", got, sessBID)
	}
	if got := stale[0].Metadata["previous_session_id_pending_cleanup"]; got != sessAID {
		t.Errorf("pending cleanup after failed handover = %q, want %q", got, sessAID)
	}

	// Retry: ensure now succeeds and the handover completes.
	if err := ReassignSessionParticipants(context.Background(), store, sessAID, sessBID); err != nil {
		t.Fatalf("ReassignSessionParticipants(retry): %v", err)
	}
	if flaky.ensureCalls != 2 {
		t.Fatalf("EnsureMembership calls = %d, want 2 (initial failure + successful retry)", flaky.ensureCalls)
	}
	bead, err := store.Get(participant.ID)
	if err != nil {
		t.Fatalf("Get(participant): %v", err)
	}
	if hasLabel(bead, groupParticipantSessionLabel(sessAID)) {
		t.Errorf("retired-session label %q not cleared after successful retry", groupParticipantSessionLabel(sessAID))
	}
	if !hasLabel(bead, groupParticipantSessionLabel(sessBID)) {
		t.Errorf("replacement-session label %q missing; labels: %v", groupParticipantSessionLabel(sessBID), bead.Labels)
	}
	if got := bead.Metadata["previous_session_id_pending_cleanup"]; got != "" {
		t.Errorf("pending cleanup after retry = %q, want empty", got)
	}
}

// TestReassignSessionParticipantsRetriesAfterRemoveFailure proves the same
// retry idempotence when the retired session's membership removal fails after a
// successful ensure on the replacement.
func TestReassignSessionParticipantsRetriesAfterRemoveFailure(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	sessAID := makeSessionBead(t, store, "pl-alpha")

	svc := NewServices(store).Groups
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	participant, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessAID,
	})
	if err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	sessBID := makeSessionBead(t, store, "pl-alpha-v2")
	if err := store.Close(sessAID); err != nil {
		t.Fatalf("close retired session A bead: %v", err)
	}

	// First attempt: ensure(replacement) succeeds, remove(retired) fails.
	flaky := &flakyTranscriptService{failRemoveCount: 1, err: errors.New("boom")}
	overrideReassignmentTranscript(t, flaky)
	if err := ReassignSessionParticipants(context.Background(), store, sessAID, sessBID); err == nil || !errors.Is(err, ErrTranscriptSyncFailed) {
		t.Fatalf("ReassignSessionParticipants(first) error = %v, want ErrTranscriptSyncFailed", err)
	}
	stale, err := store.ListByLabel(groupParticipantSessionLabel(sessAID), 0)
	if err != nil {
		t.Fatalf("ListByLabel(retired session): %v", err)
	}
	if len(stale) != 1 || stale[0].ID != participant.ID {
		t.Fatalf("retired-session label dropped before cleanup committed: %#v", stale)
	}
	if got := stale[0].Metadata["previous_session_id_pending_cleanup"]; got != sessAID {
		t.Errorf("pending cleanup after failed remove = %q, want %q", got, sessAID)
	}

	// Retry: removal now succeeds and the handover completes.
	if err := ReassignSessionParticipants(context.Background(), store, sessAID, sessBID); err != nil {
		t.Fatalf("ReassignSessionParticipants(retry): %v", err)
	}
	if flaky.removeCalls != 2 {
		t.Fatalf("RemoveMembership calls = %d, want 2 (initial failure + successful retry)", flaky.removeCalls)
	}
	bead, err := store.Get(participant.ID)
	if err != nil {
		t.Fatalf("Get(participant): %v", err)
	}
	if hasLabel(bead, groupParticipantSessionLabel(sessAID)) {
		t.Errorf("retired-session label %q not cleared after successful retry", groupParticipantSessionLabel(sessAID))
	}
	if got := bead.Metadata["previous_session_id_pending_cleanup"]; got != "" {
		t.Errorf("pending cleanup after retry = %q, want empty", got)
	}
}

// TestReapStaleParticipants proves the background participant reaper heals a
// binding-less group participant whose session respawned: it follows the live
// bead and carries the group-owned transcript membership, on the same NDI
// cadence as the binding reaper, and is a no-op once converged.
func TestReapStaleParticipants(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	sessAID := makeSessionBead(t, store, "pl-alpha")

	fabric := NewServices(store)
	svc := fabric.Groups
	transcript := fabric.Transcript
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	participant, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessAID,
	})
	if err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	if got := membershipSessionIDs(t, transcript, ref); !sameMembers(got, []string{sessAID}) {
		t.Fatalf("memberships(after upsert) = %#v, want [%s]", got, sessAID)
	}

	// Respawn: retire A and mint a fresh bead under the same name. No binding
	// exists, so only the participant reaper can converge this session.
	sessBID := respawn(t, store, sessAID, "pl-alpha")

	stats, err := ReapStaleParticipants(context.Background(), store)
	if err != nil {
		t.Fatalf("ReapStaleParticipants: %v", err)
	}
	if stats.Reassigned != 1 || stats.Scanned != 1 {
		t.Fatalf("stats = %+v, want Reassigned=1 Scanned=1", stats)
	}
	bead, err := store.Get(participant.ID)
	if err != nil {
		t.Fatalf("Get(participant): %v", err)
	}
	if bead.Metadata["session_id"] != sessBID {
		t.Errorf("participant session_id = %q, want %q (respawned bead)", bead.Metadata["session_id"], sessBID)
	}
	if got := membershipSessionIDs(t, transcript, ref); !sameMembers(got, []string{sessBID}) {
		t.Fatalf("memberships(after reap) = %#v, want [%s]; group-owned membership must follow the respawn", got, sessBID)
	}

	// Idempotent: a second sweep finds nothing stale now that the participant
	// already points at the live bead.
	stats, err = ReapStaleParticipants(context.Background(), store)
	if err != nil {
		t.Fatalf("ReapStaleParticipants(second): %v", err)
	}
	if stats.Reassigned != 0 {
		t.Fatalf("second sweep Reassigned = %d, want 0 (already converged)", stats.Reassigned)
	}
}

// TestReapStaleParticipantsFinishesPendingCleanupAfterPartialHandover proves the
// participant reaper completes a handover that committed the session_id swap and
// then failed mid-migration. Such a participant already names the live bead
// (session_id == liveID), so the reaper's "live target already matches" fast
// path would otherwise skip it forever while its retired-session transcript
// membership stays stranded in previous_session_id_pending_cleanup. The reaper
// must instead re-drive the pending handover to the live bead.
func TestReapStaleParticipantsFinishesPendingCleanupAfterPartialHandover(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	sessAID := makeSessionBead(t, store, "pl-alpha")

	svc := NewServices(store).Groups
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	participant, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessAID,
	})
	if err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	// Respawn A -> B. No binding exists, so only the participant reaper can
	// converge this session.
	sessBID := respawn(t, store, sessAID, "pl-alpha")

	// First handover attempt fails mid-migration: the replacement-session
	// membership ensure fails after session_id was already swapped to B. This
	// leaves session_id == B (the live bead) with A still pending cleanup and the
	// retired-session label retained for retry discovery.
	failing := &flakyTranscriptService{failEnsureCount: 1, err: errors.New("boom")}
	overrideReassignmentTranscript(t, failing)
	if err := ReassignSessionParticipants(context.Background(), store, sessAID, sessBID); err == nil || !errors.Is(err, ErrTranscriptSyncFailed) {
		t.Fatalf("ReassignSessionParticipants(partial) error = %v, want ErrTranscriptSyncFailed", err)
	}
	stale, err := store.Get(participant.ID)
	if err != nil {
		t.Fatalf("Get(participant after partial handover): %v", err)
	}
	if got := stale.Metadata["session_id"]; got != sessBID {
		t.Fatalf("session_id after partial handover = %q, want %q (already swapped to live bead)", got, sessBID)
	}
	if got := stale.Metadata["previous_session_id_pending_cleanup"]; got != sessAID {
		t.Fatalf("pending cleanup after partial handover = %q, want %q", got, sessAID)
	}
	if !hasLabel(stale, groupParticipantSessionLabel(sessAID)) {
		t.Fatalf("retired-session label %q dropped before migration committed", groupParticipantSessionLabel(sessAID))
	}

	// The reaper now sees session_id == liveID == B, so the old "live target
	// already matches" skip would strand the pending cleanup forever. A healthy
	// transcript lets the reaper finish the handover.
	healthy := &flakyTranscriptService{}
	overrideReassignmentTranscript(t, healthy)
	stats, err := ReapStaleParticipants(context.Background(), store)
	if err != nil {
		t.Fatalf("ReapStaleParticipants: %v", err)
	}
	if stats.Reassigned != 1 || stats.Scanned != 1 {
		t.Fatalf("stats = %+v, want Reassigned=1 Scanned=1 (reaper must finish the pending handover)", stats)
	}
	if healthy.ensureCalls == 0 || healthy.removeCalls == 0 {
		t.Fatalf("reaper did not drive membership migration: ensureCalls=%d removeCalls=%d", healthy.ensureCalls, healthy.removeCalls)
	}
	bead, err := store.Get(participant.ID)
	if err != nil {
		t.Fatalf("Get(participant after reap): %v", err)
	}
	if got := bead.Metadata["session_id"]; got != sessBID {
		t.Errorf("session_id after reap = %q, want %q", got, sessBID)
	}
	if got := bead.Metadata["previous_session_id_pending_cleanup"]; got != "" {
		t.Errorf("pending cleanup after reap = %q, want empty (handover finished)", got)
	}
	if hasLabel(bead, groupParticipantSessionLabel(sessAID)) {
		t.Errorf("retired-session label %q not dropped after reaper finished the handover", groupParticipantSessionLabel(sessAID))
	}
	if !hasLabel(bead, groupParticipantSessionLabel(sessBID)) {
		t.Errorf("replacement-session label %q missing; labels: %v", groupParticipantSessionLabel(sessBID), bead.Labels)
	}

	// Idempotent: a second sweep finds nothing pending now.
	stats, err = ReapStaleParticipants(context.Background(), store)
	if err != nil {
		t.Fatalf("ReapStaleParticipants(second): %v", err)
	}
	if stats.Reassigned != 0 {
		t.Fatalf("second sweep Reassigned = %d, want 0 (already converged)", stats.Reassigned)
	}
}

func TestGroupServiceUpsertParticipantSessionNameLegacyFallback(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	// "legacy-sess" is a raw ID with no backing session bead — sessionNameForSelector returns "".
	svc := NewGroupService(store)
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}

	participant, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "legacy-sess",
	})
	if err != nil {
		t.Fatalf("UpsertParticipant with legacy session: %v", err)
	}
	if participant.SessionName != "" {
		t.Errorf("SessionName = %q, want empty for legacy session", participant.SessionName)
	}

	// ResolveInbound should still route to legacy-sess without panic.
	decision, err := svc.ResolveInbound(context.Background(), ExternalInboundMessage{Conversation: ref})
	if err != nil {
		t.Fatalf("ResolveInbound: %v", err)
	}
	if decision.TargetSessionID != "legacy-sess" {
		t.Errorf("TargetSessionID = %q, want legacy-sess", decision.TargetSessionID)
	}
}

func TestGroupServiceResolveInboundDeadSessionNameReturnsStaleID(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	sessAID := makeSessionBead(t, store, "pl-alpha")

	svc := NewGroupService(store)
	ref := testConversationRef()
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessAID,
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	// Simulate session gone: resolver returns error for any name.
	overrideResolveLiveSessionID(t, func(_ beads.Store, _ string) (string, error) {
		return "", errors.New("session not found")
	})

	// Overlay returns "" → routing falls back to stored (stale) session ID; no panic.
	decision, err := svc.ResolveInbound(context.Background(), ExternalInboundMessage{Conversation: ref})
	if err != nil {
		t.Fatalf("ResolveInbound: %v", err)
	}
	if decision.TargetSessionID != sessAID {
		t.Errorf("TargetSessionID = %q, want stale %q as fallback", decision.TargetSessionID, sessAID)
	}
}

func testConversationRef() ConversationRef {
	return ConversationRef{
		ScopeID:        "city-1",
		Provider:       "discord",
		AccountID:      "acct-1",
		ConversationID: "thread-1",
		Kind:           ConversationThread,
	}
}

func testControllerCaller() Caller {
	return Caller{Kind: CallerController, ID: "controller-1"}
}

func testAdapterCaller() Caller {
	return Caller{
		Kind:      CallerAdapter,
		ID:        "adapter-1",
		Provider:  "discord",
		AccountID: "acct-1",
	}
}

func testNow() time.Time {
	return time.Date(2026, time.March, 23, 9, 0, 0, 0, time.UTC)
}

func freezeTestClock(t *testing.T) {
	t.Helper()
	prev := timeNow
	timeNow = testNow
	t.Cleanup(func() {
		timeNow = prev
	})
}

func sameMembers(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	got = slices.Clone(got)
	want = slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

func sameMembershipOwners(got, want []MembershipOwner) bool {
	if len(got) != len(want) {
		return false
	}
	got = slices.Clone(got)
	want = slices.Clone(want)
	slices.Sort(got)
	slices.Sort(want)
	return slices.Equal(got, want)
}

func membershipSessionIDs(t *testing.T, transcript TranscriptService, ref ConversationRef) []string {
	t.Helper()
	memberships, err := transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	out := make([]string, 0, len(memberships))
	for _, membership := range memberships {
		out = append(out, membership.SessionID)
	}
	return out
}

//nolint:unparam // sessionID varies across future tests
func membershipRecordBySession(t *testing.T, transcript TranscriptService, ref ConversationRef, sessionID string) ConversationMembershipRecord {
	t.Helper()
	memberships, err := transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	for _, membership := range memberships {
		if membership.SessionID == sessionID {
			return membership
		}
	}
	t.Fatalf("membership for session %q not found in %#v", sessionID, memberships)
	return ConversationMembershipRecord{}
}

type failingDeliveryContextService struct {
	err error
}

func (f *failingDeliveryContextService) Record(context.Context, Caller, DeliveryContextRecord) error {
	return nil
}

func (f *failingDeliveryContextService) Resolve(context.Context, string, ConversationRef) (*DeliveryContextRecord, error) {
	return nil, nil
}

func (f *failingDeliveryContextService) ClearForConversation(context.Context, string, ConversationRef) error {
	return f.err
}

type selectiveFailingDeliveryContextService struct {
	failConversationIDs map[string]bool
	err                 error
}

func (f *selectiveFailingDeliveryContextService) Record(context.Context, Caller, DeliveryContextRecord) error {
	return nil
}

func (f *selectiveFailingDeliveryContextService) Resolve(context.Context, string, ConversationRef) (*DeliveryContextRecord, error) {
	return nil, nil
}

func (f *selectiveFailingDeliveryContextService) ClearForConversation(_ context.Context, _ string, ref ConversationRef) error {
	if f.failConversationIDs[ref.ConversationID] {
		return f.err
	}
	return nil
}

type flakyTranscriptService struct {
	failEnsureCount int
	failRemoveCount int
	ensureCalls     int
	removeCalls     int
	err             error
}

func (f *flakyTranscriptService) Append(context.Context, AppendTranscriptInput) (ConversationTranscriptRecord, error) {
	panic("unexpected Append call")
}

func (f *flakyTranscriptService) List(context.Context, ListTranscriptInput) ([]ConversationTranscriptRecord, error) {
	panic("unexpected List call")
}

func (f *flakyTranscriptService) EnsureMembership(_ context.Context, input EnsureMembershipInput) (ConversationMembershipRecord, error) {
	f.ensureCalls++
	if f.ensureCalls <= f.failEnsureCount {
		return ConversationMembershipRecord{}, f.err
	}
	return ConversationMembershipRecord{Conversation: input.Conversation, SessionID: input.SessionID, Owners: []MembershipOwner{input.Owner}}, nil
}

func (f *flakyTranscriptService) UpdateMembership(context.Context, UpdateMembershipInput) (ConversationMembershipRecord, error) {
	panic("unexpected UpdateMembership call")
}

func (f *flakyTranscriptService) ensureMembershipLocked(input EnsureMembershipInput) (ConversationMembershipRecord, error) {
	return f.EnsureMembership(context.Background(), input)
}

func (f *flakyTranscriptService) RemoveMembership(context.Context, RemoveMembershipInput) error {
	f.removeCalls++
	if f.removeCalls <= f.failRemoveCount {
		return f.err
	}
	return nil
}

func (f *flakyTranscriptService) removeMembershipLocked(input RemoveMembershipInput) error {
	return f.RemoveMembership(context.Background(), input)
}

func (f *flakyTranscriptService) ListMemberships(context.Context, Caller, ConversationRef) ([]ConversationMembershipRecord, error) {
	panic("unexpected ListMemberships call")
}

func (f *flakyTranscriptService) ListConversationsBySession(context.Context, Caller, string) ([]ConversationMembershipRecord, error) {
	panic("unexpected ListConversationsBySession call")
}

func (f *flakyTranscriptService) ListBackfill(context.Context, ListBackfillInput) ([]ConversationTranscriptRecord, error) {
	panic("unexpected ListBackfill call")
}

func (f *flakyTranscriptService) Ack(context.Context, AckMembershipInput) error {
	panic("unexpected Ack call")
}

func (f *flakyTranscriptService) BeginHydration(context.Context, Caller, ConversationRef, map[string]string) (ConversationTranscriptStateRecord, error) {
	panic("unexpected BeginHydration call")
}

func (f *flakyTranscriptService) CompleteHydration(context.Context, Caller, ConversationRef) (ConversationTranscriptStateRecord, error) {
	panic("unexpected CompleteHydration call")
}

func (f *flakyTranscriptService) MarkHydrationFailed(context.Context, Caller, ConversationRef, map[string]string) (ConversationTranscriptStateRecord, error) {
	panic("unexpected MarkHydrationFailed call")
}

func (f *flakyTranscriptService) State(context.Context, Caller, ConversationRef) (*ConversationTranscriptStateRecord, error) {
	panic("unexpected State call")
}

func TestCloseSessionBindingsClosesGroupOwnedMembership(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	if got := membershipSessionIDs(t, fabric.Transcript, ref); len(got) != 1 || got[0] != "sess-a" {
		t.Fatalf("memberships(after participant upsert) = %#v, want [sess-a]", got)
	}

	if err := CloseSessionBindings(context.Background(), store, "sess-a", testNow().Add(time.Minute)); err != nil {
		t.Fatalf("CloseSessionBindings: %v", err)
	}

	if got := membershipSessionIDs(t, fabric.Transcript, ref); len(got) != 0 {
		t.Fatalf("memberships(after CloseSessionBindings) = %#v, want []", got)
	}
}

func TestCloseSessionBindingsClosesGroupParticipants(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	openParticipants := func() []string {
		t.Helper()
		items, err := store.ListByLabel(groupParticipantSessionLabel("sess-a"), 0)
		if err != nil {
			t.Fatalf("ListByLabel: %v", err)
		}
		open := make([]string, 0, len(items))
		for _, item := range items {
			if hasLabel(item, "gc:extmsg-participant") && item.Status != "closed" {
				open = append(open, item.ID)
			}
		}
		return open
	}
	if got := openParticipants(); len(got) != 1 {
		t.Fatalf("open participants(after upsert) = %#v, want 1", got)
	}

	if err := CloseSessionBindings(context.Background(), store, "sess-a", testNow().Add(time.Minute)); err != nil {
		t.Fatalf("CloseSessionBindings: %v", err)
	}

	if got := openParticipants(); len(got) != 0 {
		t.Fatalf("open participants(after CloseSessionBindings) = %#v, want []", got)
	}
}
