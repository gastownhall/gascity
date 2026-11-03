package extmsg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

type publicNoBatchStore struct{ beads.Store }

type publicFaultStore struct {
	*beads.MemStore
	mu            sync.Mutex
	lostAdmission bool
	failHandoff   bool
}

func (s *publicFaultStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) { return s, true }
func (s *publicFaultStore) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failHandoff && b.Reason == "record public recipient lane handoff" && b.Items[0].Update != nil && b.Items[0].Update.Opts.Status != nil {
		s.failHandoff = false
		return beads.GuardedResult{}, errors.New("process stopped after lane accepted and before outbox marked")
	}
	r, err := s.MemStore.ApplyGuarded(ctx, b)
	if err == nil && s.lostAdmission && b.Reason == "admit public message and recipient intents" {
		s.lostAdmission = false
		return beads.GuardedResult{}, errors.New("admission committed; response connection lost")
	}
	return r, err
}

func publicTestStore() *beads.MemStore {
	return &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
}

func publicTestInput() PublicAdmissionInput {
	return PublicAdmissionInput{Caller: testControllerCaller(), RequestKey: "req-one", Message: ExternalInboundMessage{ProviderMessageID: "public-one", Conversation: testConversationRef(), Actor: ExternalActor{ID: "human-42", DisplayName: "person"}, Text: "please inspect", ReceivedAt: testNow()}, Recipients: []PublicRecipient{{SessionID: "session-one", Policy: PublicMessagePolicy{When: opjournal.WhenBoundary}}, {AgentIdentity: "project/claude", Policy: PublicMessagePolicy{When: opjournal.WhenBoundary}}}}
}

func publicTestAdmission(t *testing.T, store beads.Store, lanes ...PublicDeliveryLane) *PublicAdmissionStore {
	t.Helper()
	var lane PublicDeliveryLane = &publicTestLane{}
	if len(lanes) > 0 {
		lane = lanes[0]
	}
	s, err := NewPublicAdmissionStore(beads.MailStore{Store: store}, "gc", lane)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestPublicAdmissionRequiresGuardedMessagingWriterBeforeAppend(t *testing.T) {
	mem := publicTestStore()
	_, err := NewPublicAdmissionStore(beads.MailStore{Store: publicNoBatchStore{Store: mem}}, "gc", &publicTestLane{})
	if !errors.Is(err, beads.ErrGuardedBatchUnsupported) {
		t.Fatalf("readiness=%v", err)
	}
	rows, err := mem.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil || len(rows) != 0 {
		t.Fatalf("readiness wrote rows=%v err=%v", rows, err)
	}
}

func TestPublicAdmissionBindsPayloadAndRecipientSelection(t *testing.T) {
	mutations := map[string]func(*PublicAdmissionInput){
		"text":   func(i *PublicAdmissionInput) { i.Message.Text = "changed" },
		"actor":  func(i *PublicAdmissionInput) { i.Message.Actor.DisplayName = "other" },
		"target": func(i *PublicAdmissionInput) { i.Message.ExplicitTarget = "owl" },
		"attachment": func(i *PublicAdmissionInput) {
			i.Message.Attachments = []ExternalAttachment{{URL: "https://example.test/image.png"}}
		},
		"recipient":   func(i *PublicAdmissionInput) { i.Recipients[0].AgentIdentity = "other" },
		"intent":      func(i *PublicAdmissionInput) { i.Recipients[0].Policy.When = opjournal.WhenNow },
		"unsupported": func(i *PublicAdmissionInput) { i.Recipients[0].Policy.OnUnsupported = opjournal.FallbackFail },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			mem := publicTestStore()
			s := publicTestAdmission(t, mem)
			input := publicTestInput()
			first, err := s.Admit(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			recreated := publicTestAdmission(t, mem)
			input.Message.ReceivedAt = input.Message.ReceivedAt.Add(time.Hour)
			input.Recipients[0], input.Recipients[1] = input.Recipients[1], input.Recipients[0]
			retry, err := recreated.Admit(context.Background(), input)
			if err != nil || retry.ID != first.ID {
				t.Fatalf("same retry=%+v err=%v", retry, err)
			}
			mutate(&input)
			if _, err := recreated.Admit(context.Background(), input); !errors.Is(err, ErrMessageConflict) {
				t.Fatalf("changed retry=%v", err)
			}
			rows, err := NewServices(mem).Transcript.List(context.Background(), ListTranscriptInput{Caller: testControllerCaller(), Conversation: input.Message.Conversation})
			if err != nil || len(rows) != 1 || rows[0].ID != first.TranscriptID {
				t.Fatalf("transcript=%+v err=%v", rows, err)
			}
		})
	}
}

func TestPublicAdmissionConcurrentLostAckAndProcessRecreation(t *testing.T) {
	mem := publicTestStore()
	fault := &publicFaultStore{MemStore: mem, lostAdmission: true}
	var wg sync.WaitGroup
	results := make(chan PublicReceipt, 12)
	errs := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := publicTestAdmission(t, fault)
			r, err := s.Admit(context.Background(), publicTestInput())
			results <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	id := ""
	for receipt := range results {
		if id == "" {
			id = receipt.ID
		}
		if receipt.ID != id {
			t.Fatalf("different receipts %s/%s", id, receipt.ID)
		}
	}
	s := publicTestAdmission(t, mem)
	pending, err := s.Pending(context.Background(), 100)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	rows, err := NewServices(mem).Transcript.List(context.Background(), ListTranscriptInput{Caller: testControllerCaller(), Conversation: testConversationRef()})
	if err != nil || len(rows) != 1 {
		t.Fatalf("public append count=%d err=%v", len(rows), err)
	}
	// Provider event identity remains deduplicated even if its transport supplies
	// a new request key. It recovers the original receipt rather than re-notifying.
	input := publicTestInput()
	input.RequestKey = "new-http-transport-key"
	retry, err := s.Admit(context.Background(), input)
	if err != nil || retry.ID != id {
		t.Fatalf("provider retry=%+v err=%v", retry, err)
	}
}

// This lane models the REQUIRED idempotent Sessions handoff boundary. These
// tests establish Messaging crash recovery, not native provider execution.
type publicTestLane struct {
	mu             sync.Mutex
	ops            map[string]string
	resolveCalls   int
	admissionCalls int
	lostAck        bool
	unavailable    bool
	readinessError error
}

func (l *publicTestLane) Ready(context.Context) error { return l.readinessError }
func (l *publicTestLane) ResolveRecipient(_ context.Context, _ PublicRecipient) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.resolveCalls++
	if l.unavailable {
		return "", beads.ErrStoreUnavailable
	}
	return "resolved-session", nil
}

func (l *publicTestLane) Prepare(_ context.Context, _ PublicDelivery, r ConversationTranscriptRecord) (string, error) {
	return r.Text, nil
}

func (l *publicTestLane) Admit(_ context.Context, d PublicDelivery, _ ConversationTranscriptRecord) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.admissionCalls++
	if l.ops == nil {
		l.ops = make(map[string]string)
	}
	if id := l.ops[d.OperationKey]; id != "" {
		return id, nil
	}
	id := fmt.Sprintf("op-%d", len(l.ops)+1)
	l.ops[d.OperationKey] = id
	if l.lostAck {
		l.lostAck = false
		return "", errors.New("lane committed; acknowledgement lost")
	}
	return id, nil
}

func TestPublicOutboxCrashBeforeLaneAfterLaneAndLostAck(t *testing.T) {
	for _, mode := range []string{"before-lane", "lost-lane-ack", "after-lane-before-mark"} {
		t.Run(mode, func(t *testing.T) {
			mem := publicTestStore()
			fault := &publicFaultStore{MemStore: mem, failHandoff: mode == "after-lane-before-mark"}
			lane := &publicTestLane{lostAck: mode == "lost-lane-ack"}
			s := publicTestAdmission(t, fault, lane)
			input := publicTestInput()
			input.Recipients = input.Recipients[:1]
			receipt, err := s.Admit(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "before-lane" {
				if _, err := s.Relay(context.Background(), receipt.DeliveryIDs[0]); err == nil {
					t.Fatal("expected interrupted relay")
				}
			}
			// New service handle after process loss sees the same committed intent.
			recreated := publicTestAdmission(t, fault, lane)
			delivery, err := recreated.Relay(context.Background(), receipt.DeliveryIDs[0])
			if err != nil || delivery.OperationID == "" {
				t.Fatalf("recovery=%+v err=%v", delivery, err)
			}
			if _, err := recreated.Relay(context.Background(), delivery.ID); err != nil {
				t.Fatal(err)
			}
			if len(lane.ops) != 1 {
				t.Fatalf("operations=%v", lane.ops)
			}
			pending, err := recreated.Pending(context.Background(), 100)
			if err != nil || len(pending) != 0 {
				t.Fatalf("pending=%v err=%v", pending, err)
			}
			rows, err := NewServices(mem).Transcript.List(context.Background(), ListTranscriptInput{Caller: testControllerCaller(), Conversation: input.Message.Conversation})
			if err != nil || len(rows) != 1 {
				t.Fatalf("append count=%d err=%v", len(rows), err)
			}
		})
	}
}

func TestPublicOutboxColdTargetRemainsDurableAndBindsBeforeLane(t *testing.T) {
	mem := publicTestStore()
	lane := &publicTestLane{unavailable: true}
	s := publicTestAdmission(t, mem, lane)
	input := publicTestInput()
	input.Recipients = input.Recipients[1:]
	receipt, err := s.Admit(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Relay(context.Background(), receipt.DeliveryIDs[0]); !errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("cold target=%v", err)
	}
	pending, err := s.Pending(context.Background(), 100)
	if err != nil || len(pending) != 1 || len(lane.ops) != 0 {
		t.Fatalf("pending=%v ops=%v err=%v", pending, lane.ops, err)
	}
	lane.unavailable = false
	lane.lostAck = true
	if _, err := s.Relay(context.Background(), receipt.DeliveryIDs[0]); err == nil {
		t.Fatal("expected lost lane ack")
	}
	recreated := publicTestAdmission(t, mem, lane)
	d, err := recreated.Relay(context.Background(), receipt.DeliveryIDs[0])
	if err != nil || d.ResolvedSessionID != "resolved-session" || d.OperationID == "" {
		t.Fatalf("recovered=%+v err=%v", d, err)
	}
	if lane.resolveCalls != 2 || len(lane.ops) != 1 {
		t.Fatalf("resolved=%d ops=%v", lane.resolveCalls, lane.ops)
	}
}

func TestPublicAdmissionDifferentMessagesSerializeTranscriptSequence(t *testing.T) {
	mem := publicTestStore()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for n := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			input := publicTestInput()
			input.RequestKey = fmt.Sprintf("key-%d", n)
			input.Message.ProviderMessageID = fmt.Sprintf("provider-%d", n)
			_, err := publicTestAdmission(t, mem).Admit(context.Background(), input)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := NewServices(mem).Transcript.List(context.Background(), ListTranscriptInput{Caller: testControllerCaller(), Conversation: testConversationRef()})
	if err != nil || len(rows) != 12 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	seen := make(map[int64]bool)
	for _, row := range rows {
		if seen[row.Sequence] {
			t.Fatalf("duplicate sequence %d", row.Sequence)
		}
		seen[row.Sequence] = true
	}
}

func TestPublicAdmissionRequiresReadySessionsWriterAndRecoversReceiptDuringOutage(t *testing.T) {
	mem := publicTestStore()
	lane := &publicTestLane{readinessError: beads.ErrGuardedBatchUnsupported}
	s := publicTestAdmission(t, mem, lane)
	if _, err := s.Admit(context.Background(), publicTestInput()); !errors.Is(err, beads.ErrGuardedBatchUnsupported) {
		t.Fatalf("readiness=%v", err)
	}
	rows, err := mem.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil || len(rows) != 0 {
		t.Fatalf("unready admission wrote %d rows err=%v", len(rows), err)
	}
	lane.readinessError = nil
	first, err := s.Admit(context.Background(), publicTestInput())
	if err != nil {
		t.Fatal(err)
	}
	lane.readinessError = beads.ErrGuardedBatchUnsupported
	retry, err := s.Admit(context.Background(), publicTestInput())
	if err != nil || retry.ID != first.ID {
		t.Fatalf("receipt recovery=%+v err=%v", retry, err)
	}
}

func TestPublicOutboxPreservesRecipientAdmissionOrder(t *testing.T) {
	mem := publicTestStore()
	lane := &publicTestLane{}
	store := publicTestAdmission(t, mem, lane)
	firstInput := publicTestInput()
	firstInput.Recipients = firstInput.Recipients[:1]
	first, err := store.Admit(context.Background(), firstInput)
	if err != nil {
		t.Fatal(err)
	}
	secondInput := firstInput
	secondInput.RequestKey = "request-two"
	secondInput.Message.ProviderMessageID = "public-two"
	second, err := store.Admit(context.Background(), secondInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Relay(context.Background(), second.DeliveryIDs[0]); !errors.Is(err, beads.ErrStoreUnavailable) {
		t.Fatalf("later admission bypassed predecessor: %v", err)
	}
	if len(lane.ops) != 0 {
		t.Fatalf("later op admitted: %v", lane.ops)
	}
	if _, err := store.Relay(context.Background(), first.DeliveryIDs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Relay(context.Background(), second.DeliveryIDs[0]); err != nil {
		t.Fatal(err)
	}
	if len(lane.ops) != 2 {
		t.Fatalf("ops=%v", lane.ops)
	}
}

type publicChangingFormatter struct {
	publicTestLane
	prepared int
	received []string
}

func (l *publicChangingFormatter) Prepare(context.Context, PublicDelivery, ConversationTranscriptRecord) (string, error) {
	l.prepared++
	return fmt.Sprintf("format-%d", l.prepared), nil
}

func (l *publicChangingFormatter) Admit(ctx context.Context, d PublicDelivery, r ConversationTranscriptRecord) (string, error) {
	l.received = append(l.received, d.NativePayload)
	return l.publicTestLane.Admit(ctx, d, r)
}

func TestPublicOutboxFreezesPayloadBeforeUnknownLaneAcknowledgment(t *testing.T) {
	mem := publicTestStore()
	lane := &publicChangingFormatter{publicTestLane: publicTestLane{lostAck: true}}
	store := publicTestAdmission(t, mem, lane)
	input := publicTestInput()
	input.Recipients = input.Recipients[:1]
	receipt, err := store.Admit(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Relay(context.Background(), receipt.DeliveryIDs[0]); err == nil {
		t.Fatal("expected lost acknowledgment")
	}
	recreated := publicTestAdmission(t, mem, lane)
	if _, err = recreated.Relay(context.Background(), receipt.DeliveryIDs[0]); err != nil {
		t.Fatal(err)
	}
	if lane.prepared != 1 || len(lane.received) != 2 || lane.received[0] != lane.received[1] {
		t.Fatalf("formatter=%d native payloads=%v", lane.prepared, lane.received)
	}
}

func TestPublicInvalidationRecoversLostAckWithoutReadmittingMessage(t *testing.T) {
	mem := publicTestStore()
	lane := &publicTestLane{}
	store := publicTestAdmission(t, mem, lane)
	input := publicTestInput()
	input.Recipients = input.Recipients[:1]
	receipt, err := store.Admit(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	var observed []string
	lose := true
	emit := func(r PublicReceipt, record ConversationTranscriptRecord) error {
		observed = append(observed, r.ID+"/"+record.ID)
		if lose {
			lose = false
			return errors.New("event appended; acknowledgment lost")
		}
		return nil
	}
	if err := store.Invalidate(context.Background(), receipt.ID, emit); err == nil {
		t.Fatal("expected lost invalidation acknowledgment")
	}
	recreated := publicTestAdmission(t, mem, lane)
	pending, _, err := recreated.PendingInvalidations(context.Background(), 10, nil)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%v error=%v", pending, err)
	}
	if err := recreated.Invalidate(context.Background(), receipt.ID, emit); err != nil {
		t.Fatal(err)
	}
	if err := recreated.Invalidate(context.Background(), receipt.ID, emit); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || observed[0] != observed[1] {
		t.Fatalf("invalidation identities=%v", observed)
	}
	if _, err := recreated.Relay(context.Background(), receipt.DeliveryIDs[0]); err != nil {
		t.Fatal(err)
	}
	if len(lane.ops) != 1 {
		t.Fatalf("operations=%v", lane.ops)
	}
	records, err := NewServices(mem).Transcript.List(context.Background(), ListTranscriptInput{Caller: testControllerCaller(), Conversation: input.Message.Conversation})
	if err != nil || len(records) != 1 {
		t.Fatalf("records=%v error=%v", records, err)
	}
	pending, _, err = recreated.PendingInvalidations(context.Background(), 10, nil)
	if err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged pending=%v error=%v", pending, err)
	}
}
