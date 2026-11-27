package extmsg

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/session"
)

// cancelOnWriteStore cancels a context right after the first Close or
// metadata batch that lands on a watched bead, so a test can end a teardown's
// context in the middle of one item.
type cancelOnWriteStore struct {
	*beads.MemStore
	watch    map[string]bool
	cancel   context.CancelFunc
	canceled bool
}

func (s *cancelOnWriteStore) trip(id string) {
	if s.watch[id] && !s.canceled {
		s.canceled = true
		s.cancel()
	}
}

func (s *cancelOnWriteStore) Close(id string) error {
	err := s.MemStore.Close(id)
	s.trip(id)
	return err
}

func (s *cancelOnWriteStore) SetMetadataBatch(id string, kvs map[string]string) error {
	err := s.MemStore.SetMetadataBatch(id, kvs)
	s.trip(id)
	return err
}

func teardownConversation(i int) ConversationRef {
	ref := testConversationRef()
	ref.ConversationID = fmt.Sprintf("thread-teardown-%d", i)
	return ref
}

func beadStatus(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b.Status
}

func openDeliveryContexts(t *testing.T, store beads.Store, sessionID string, ref ConversationRef) int {
	t.Helper()
	items, err := store.List(beads.ListQuery{Label: deliveryRouteLabel(ref, sessionID)})
	if err != nil {
		t.Fatalf("list delivery contexts: %v", err)
	}
	open := 0
	for _, item := range items {
		if hasLabel(item, "gc:extmsg-delivery") && item.Status != "closed" {
			open++
		}
	}
	return open
}

// TestCloseSessionBindingsUnbindsAStartedConversationWhole: a context canceled
// after the teardown has closed one conversation's binding still lets that
// conversation's delivery context clear, and stops the teardown before the
// next conversation starts.
func TestCloseSessionBindingsUnbindsAStartedConversationWhole(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	fabric := NewServices(beadstest.Mail(mem), beadstest.Sessions(mem))
	ctx := context.Background()
	byID := map[string]ConversationRef{}
	watch := map[string]bool{}
	for i := 1; i <= 2; i++ {
		ref := teardownConversation(i)
		binding, err := fabric.Bindings.Bind(ctx, testControllerCaller(), BindInput{Conversation: ref, SessionID: "sess-a", Now: testNow()})
		if err != nil {
			t.Fatalf("Bind: %v", err)
		}
		if err := fabric.Delivery.Record(ctx, testControllerCaller(), DeliveryContextRecord{
			SessionID:         "sess-a",
			Conversation:      ref,
			BindingGeneration: binding.BindingGeneration,
		}); err != nil {
			t.Fatalf("Delivery.Record: %v", err)
		}
		byID[binding.ID] = ref
		watch[binding.ID] = true
	}
	teardownCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &cancelOnWriteStore{MemStore: mem, watch: watch, cancel: cancel}

	err := CloseSessionBindings(teardownCtx, beadstest.Mail(store), beadstest.Sessions(store), "sess-a", testNow())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CloseSessionBindings error = %v, want one matching context.Canceled", err)
	}
	ended := 0
	for id, ref := range byID {
		closedBinding := beadStatus(t, mem, id) == "closed"
		delivery := openDeliveryContexts(t, mem, "sess-a", ref)
		if closedBinding {
			ended++
		}
		if closedBinding == (delivery != 0) {
			t.Fatalf("binding %s closed = %v with %d delivery contexts open, want the binding and its delivery context ended together", id, closedBinding, delivery)
		}
	}
	if ended != 1 {
		t.Fatalf("bindings ended = %d, want the 1 started before the cancellation", ended)
	}
}

// TestCloseSessionBindingsRemovesAStartedParticipantWhole: a context canceled
// after a participant bead closes still removes that participant's
// group-owned membership, and the teardown stops before the next participant.
func TestCloseSessionBindingsRemovesAStartedParticipantWhole(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	fabric := NewServices(beadstest.Mail(mem), beadstest.Sessions(mem))
	ctx := context.Background()
	watch := map[string]bool{}
	groups := map[string]ConversationRef{}
	for i := 0; i < 2; i++ {
		ref := teardownConversation(i)
		group, err := fabric.Groups.EnsureGroup(ctx, testControllerCaller(), EnsureGroupInput{RootConversation: ref, Mode: GroupModeLauncher})
		if err != nil {
			t.Fatalf("EnsureGroup: %v", err)
		}
		participant, err := fabric.Groups.UpsertParticipant(ctx, testControllerCaller(), UpsertParticipantInput{
			GroupID:   group.ID,
			Handle:    fmt.Sprintf("worker%d", i),
			SessionID: "sess-a",
		})
		if err != nil {
			t.Fatalf("UpsertParticipant: %v", err)
		}
		watch[participant.ID] = true
		groups[participant.ID] = ref
	}
	teardownCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &cancelOnWriteStore{MemStore: mem, watch: watch, cancel: cancel}

	err := CloseSessionBindings(teardownCtx, beadstest.Mail(store), beadstest.Sessions(store), "sess-a", testNow())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CloseSessionBindings error = %v, want one matching context.Canceled", err)
	}
	removed := 0
	for id, ref := range groups {
		closedParticipant := beadStatus(t, mem, id) == "closed"
		members := membershipSessionIDs(t, fabric.Transcript, ref)
		if closedParticipant {
			removed++
		}
		if closedParticipant == (len(members) != 0) {
			t.Fatalf("participant %s closed = %v with memberships %v, want the participant and its membership removed together", id, closedParticipant, members)
		}
	}
	if removed != 1 {
		t.Fatalf("participants removed = %d, want the 1 started before the cancellation", removed)
	}
}

// TestCloseSessionBindingsRemovesAStartedMembershipWhole: a context canceled
// after the first of a membership's owners is removed still removes the
// others, so the membership closes rather than staying open with fewer owners.
func TestCloseSessionBindingsRemovesAStartedMembershipWhole(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	fabric := NewServices(beadstest.Mail(mem), beadstest.Sessions(mem))
	ctx := context.Background()
	ref := teardownConversation(1)
	var membershipID string
	for _, owner := range []MembershipOwner{MembershipOwnerBinding, MembershipOwnerManual} {
		m, err := fabric.Transcript.EnsureMembership(ctx, EnsureMembershipInput{
			Caller:       testControllerCaller(),
			Conversation: ref,
			SessionID:    "sess-a",
			Owner:        owner,
			Now:          testNow(),
		})
		if err != nil {
			t.Fatalf("EnsureMembership(%s): %v", owner, err)
		}
		membershipID = m.ID
	}
	if got := membershipRecordBySession(t, fabric.Transcript, ref, "sess-a"); len(got.Owners) != 2 {
		t.Fatalf("membership owners = %v, want 2", got.Owners)
	}
	teardownCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &cancelOnWriteStore{MemStore: mem, watch: map[string]bool{membershipID: true}, cancel: cancel}

	err := CloseSessionBindings(teardownCtx, beadstest.Mail(store), beadstest.Sessions(store), "sess-a", testNow())
	if err != nil {
		t.Fatalf("CloseSessionBindings: %v (the membership was the last item)", err)
	}
	if !store.canceled {
		t.Fatal("the context was never canceled; the test did not cut the membership's removal")
	}
	if status := beadStatus(t, mem, membershipID); status != "closed" {
		t.Fatalf("membership status = %q, want closed: every owner removed once its removal started", status)
	}
}

func createTeardownSession(t *testing.T, store beads.Store, state string, closed bool) string {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title:    "session",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"state": state},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if closed {
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("close session: %v", err)
		}
	}
	return b.ID
}

func seedTeardownParticipant(t *testing.T, fabric Services, sessionID string, i int) ConversationRef {
	t.Helper()
	ctx := context.Background()
	ref := teardownConversation(i)
	group, err := fabric.Groups.EnsureGroup(ctx, testControllerCaller(), EnsureGroupInput{RootConversation: ref, Mode: GroupModeLauncher})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(ctx, testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    fmt.Sprintf("worker%d", i),
		SessionID: sessionID,
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	return ref
}

func openTeardownRecords(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	items, err := store.List(beads.ListQuery{Label: labelSessionTeardownBase})
	if err != nil {
		t.Fatalf("list teardown records: %v", err)
	}
	return items
}

// TestRecordPendingSessionTeardownIsIdempotent: recording a session twice
// leaves one open record naming it, and blank input records nothing.
func TestRecordPendingSessionTeardownIsIdempotent(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	for i := 0; i < 2; i++ {
		if err := RecordPendingSessionTeardown(beadstest.Mail(mem), "sess-a", testNow()); err != nil {
			t.Fatalf("RecordPendingSessionTeardown: %v", err)
		}
	}
	if err := RecordPendingSessionTeardown(beadstest.Mail(mem), " ", testNow()); err != nil {
		t.Fatalf("RecordPendingSessionTeardown(blank): %v", err)
	}
	if err := RecordPendingSessionTeardown(beads.MailStore{}, "sess-a", testNow()); err != nil {
		t.Fatalf("RecordPendingSessionTeardown(no store): %v", err)
	}
	records := openTeardownRecords(t, mem)
	if len(records) != 1 {
		t.Fatalf("open teardown records = %d, want 1", len(records))
	}
	if got := records[0].Metadata["session_id"]; got != "sess-a" {
		t.Fatalf("record session_id = %q, want sess-a", got)
	}
	if !hasLabel(records[0], sessionTeardownSessionLabel("sess-a")) {
		t.Fatalf("record labels = %v, want the per-session label", records[0].Labels)
	}
}

// TestResumePendingSessionTeardowns: a recorded teardown of a closed or
// archived session is run again and its record closed; one of a session that
// is live again is closed with the session's state untouched; one whose
// teardown fails stays open while the sweep goes on to the others.
func TestResumePendingSessionTeardowns(t *testing.T) {
	freezeTestClock(t)

	t.Run("FinishesRetiredSessions", func(t *testing.T) {
		mem := beads.NewMemStore()
		fabric := NewServices(beadstest.Mail(mem), beadstest.Sessions(mem))
		closedID := createTeardownSession(t, mem, "awake", true)
		archivedID := createTeardownSession(t, mem, string(session.StateArchived), false)
		refs := map[string]ConversationRef{
			closedID:   seedTeardownParticipant(t, fabric, closedID, 1),
			archivedID: seedTeardownParticipant(t, fabric, archivedID, 2),
		}
		for id := range refs {
			if err := RecordPendingSessionTeardown(beadstest.Mail(mem), id, testNow()); err != nil {
				t.Fatalf("RecordPendingSessionTeardown: %v", err)
			}
		}

		stats, err := ResumePendingSessionTeardowns(context.Background(), beadstest.Mail(mem), beadstest.Sessions(mem), testNow())
		if err != nil {
			t.Fatalf("ResumePendingSessionTeardowns: %v", err)
		}
		if stats != (SessionTeardownStats{Pending: 2, Finished: 2}) {
			t.Fatalf("stats = %+v, want 2 pending, 2 finished", stats)
		}
		handles := map[string]string{closedID: "worker1", archivedID: "worker2"}
		for id, ref := range refs {
			if got := membershipSessionIDs(t, fabric.Transcript, ref); len(got) != 0 {
				t.Fatalf("session %s memberships = %v, want none", id, got)
			}
			decision, err := fabric.Groups.ResolveInbound(context.Background(), ExternalInboundMessage{Conversation: ref, ExplicitTarget: handles[id]})
			if err != nil {
				t.Fatalf("ResolveInbound: %v", err)
			}
			if decision.Match != GroupRouteNoMatch || decision.TargetSessionID == id {
				t.Fatalf("inbound to %s's handle = %+v, want no match once its teardown finished", id, decision)
			}
		}
		if got := openTeardownRecords(t, mem); len(got) != 0 {
			t.Fatalf("open teardown records = %d, want 0", len(got))
		}
	})

	t.Run("DropsLiveSession", func(t *testing.T) {
		mem := beads.NewMemStore()
		fabric := NewServices(beadstest.Mail(mem), beadstest.Sessions(mem))
		liveID := createTeardownSession(t, mem, "awake", false)
		ref := seedTeardownParticipant(t, fabric, liveID, 1)
		if err := RecordPendingSessionTeardown(beadstest.Mail(mem), liveID, testNow()); err != nil {
			t.Fatalf("RecordPendingSessionTeardown: %v", err)
		}

		stats, err := ResumePendingSessionTeardowns(context.Background(), beadstest.Mail(mem), beadstest.Sessions(mem), testNow())
		if err != nil {
			t.Fatalf("ResumePendingSessionTeardowns: %v", err)
		}
		if stats != (SessionTeardownStats{Pending: 1, Dropped: 1}) {
			t.Fatalf("stats = %+v, want 1 pending, 1 dropped", stats)
		}
		if got := membershipSessionIDs(t, fabric.Transcript, ref); len(got) != 1 || got[0] != liveID {
			t.Fatalf("live session memberships = %v, want [%s] untouched", got, liveID)
		}
		if got := openTeardownRecords(t, mem); len(got) != 0 {
			t.Fatalf("open teardown records = %d, want 0", len(got))
		}
	})

	t.Run("KeepsFailedRecordAndGoesOn", func(t *testing.T) {
		mem := beads.NewMemStore()
		fabric := NewServices(beadstest.Mail(mem), beadstest.Sessions(mem))
		failingID := createTeardownSession(t, mem, "awake", true)
		okID := createTeardownSession(t, mem, "awake", true)
		seedTeardownParticipant(t, fabric, failingID, 1)
		okRef := seedTeardownParticipant(t, fabric, okID, 2)
		for _, id := range []string{failingID, okID} {
			if err := RecordPendingSessionTeardown(beadstest.Mail(mem), id, testNow()); err != nil {
				t.Fatalf("RecordPendingSessionTeardown: %v", err)
			}
		}
		store := &failingListStore{MemStore: mem, failLabel: groupParticipantSessionLabel(failingID)}

		stats, err := ResumePendingSessionTeardowns(context.Background(), beadstest.Mail(store), beadstest.Sessions(store), testNow())
		if err == nil || !errors.Is(err, errTeardownListFailed) {
			t.Fatalf("ResumePendingSessionTeardowns error = %v, want the failing teardown's", err)
		}
		if stats != (SessionTeardownStats{Pending: 2, Finished: 1}) {
			t.Fatalf("stats = %+v, want 2 pending, 1 finished", stats)
		}
		if got := membershipSessionIDs(t, fabric.Transcript, okRef); len(got) != 0 {
			t.Fatalf("memberships of the session after the failing one = %v, want none", got)
		}
		records := openTeardownRecords(t, mem)
		if len(records) != 1 || records[0].Metadata["session_id"] != failingID {
			t.Fatalf("open teardown records = %v, want only the failing session's", records)
		}
	})

	t.Run("StopsOnEndedContext", func(t *testing.T) {
		mem := beads.NewMemStore()
		if err := RecordPendingSessionTeardown(beadstest.Mail(mem), createTeardownSession(t, mem, "awake", true), testNow()); err != nil {
			t.Fatalf("RecordPendingSessionTeardown: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := ResumePendingSessionTeardowns(ctx, beadstest.Mail(mem), beadstest.Sessions(mem), testNow()); !errors.Is(err, context.Canceled) {
			t.Fatalf("ResumePendingSessionTeardowns error = %v, want one matching context.Canceled", err)
		}
		if got := openTeardownRecords(t, mem); len(got) != 1 {
			t.Fatalf("open teardown records = %d, want the 1 left for the next sweep", len(got))
		}
	})
}

var errTeardownListFailed = errors.New("teardown list failed")

// failingListStore fails every List of one label.
type failingListStore struct {
	*beads.MemStore
	failLabel string
}

func (s *failingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == s.failLabel {
		return nil, errTeardownListFailed
	}
	return s.MemStore.List(query)
}
