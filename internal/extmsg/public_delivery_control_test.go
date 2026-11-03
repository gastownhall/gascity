package extmsg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/opjournal"
)

type publicJournalTestLane struct {
	store        *opjournal.Store
	backing      *beads.MemStore
	prepareError error
	onlyFirst    bool
	entered      chan struct{}
	release      chan struct{}
	once         sync.Once
}

func newPublicJournalTestLane(t *testing.T) *publicJournalTestLane {
	t.Helper()
	mem := publicTestStore()
	store, err := opjournal.NewLaneStore(beads.SessionStore{Store: mem}, clock.Real{}, opjournal.WithIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	return &publicJournalTestLane{store: store, backing: mem}
}
func (l *publicJournalTestLane) Ready(context.Context) error { return nil }
func (l *publicJournalTestLane) ResolveRecipient(context.Context, PublicRecipient) (string, error) {
	return "session-one", nil
}

func (l *publicJournalTestLane) Prepare(_ context.Context, d PublicDelivery, r ConversationTranscriptRecord) (string, error) {
	if l.prepareError != nil && (!l.onlyFirst || r.ProviderMessageID == "public-one") {
		return "", l.prepareError
	}
	req := opjournal.Request{
		Conversation: d.ResolvedSessionID, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane,
		ClientKey: d.OperationKey, Producer: opjournal.Producer{Principal: "gc:extmsg", Actor: opjournal.ActorBackground},
		Payload: r.Text, Policy: opjournal.Policy{Guarantee: opjournal.GuaranteeAtMostOnce, Message: opjournal.MessagePolicy(d.Recipient.Policy)},
		Causation: d.TranscriptID, Correlation: d.ReceiptID,
	}
	b, err := json.Marshal(req)
	return string(b), err
}

func (l *publicJournalTestLane) Admit(ctx context.Context, d PublicDelivery, _ ConversationTranscriptRecord) (string, error) {
	if l.entered != nil {
		l.once.Do(func() { close(l.entered) })
		select {
		case <-l.release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	var req opjournal.Request
	if err := json.Unmarshal([]byte(d.NativePayload), &req); err != nil {
		return "", err
	}
	op, _, err := l.store.Propose(req)
	return op.ID, err
}

func (l *publicJournalTestLane) Cancel(ctx context.Context, d PublicDelivery) (PublicCancelResult, error) {
	var req opjournal.Request
	if err := json.Unmarshal([]byte(d.NativePayload), &req); err != nil {
		return PublicCancelResult{}, err
	}
	_, op, err := l.store.CancelProposal(ctx, req, "gc:extmsg", "cancel")
	return PublicCancelResult{
		OperationID: op.ID, Status: string(op.Status), Outcome: string(op.Outcome.Arm), Reason: op.Outcome.Reason,
		Terminal: op.Status == opjournal.StatusClosed, Canceled: op.Status == opjournal.StatusClosed && op.Outcome.Arm == opjournal.ArmCanceled,
	}, err
}

func onePublicDelivery(t *testing.T, store *PublicAdmissionStore, receipt PublicReceipt) PublicDelivery {
	t.Helper()
	_, deliveries, err := store.ReadAdmission(context.Background(), receipt)
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries=%v err=%v", deliveries, err)
	}
	return deliveries[0]
}

func TestPublicPermanentFailureAndCancellationReleaseRecipientPredecessor(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprint("cancel=", cancel), func(t *testing.T) {
			mem := publicTestStore()
			lane := newPublicJournalTestLane(t)
			lane.prepareError = &PublicDeliveryError{Code: "invalid_native_payload", Cause: opjournal.ErrPayloadTooLarge}
			lane.onlyFirst = true
			store := publicTestAdmission(t, mem, lane)
			input := publicTestInput()
			input.Recipients = input.Recipients[:1]
			first, err := store.Admit(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			input.RequestKey = "second"
			input.Message.ProviderMessageID = "public-two"
			second, err := store.Admit(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			if cancel {
				if _, err := store.RequestCancel(context.Background(), first.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := store.Relay(context.Background(), first.DeliveryIDs[0]); err == nil {
					t.Fatal("permanent failure unreported")
				}
			}
			failed := onePublicDelivery(t, store, first)
			want := PublicDeliveryFailed
			if cancel {
				want = PublicDeliveryCanceled
			}
			if failed.State != want || failed.Retryable || failed.OperationID != "" {
				t.Fatalf("first=%+v", failed)
			}
			if _, err := store.Relay(context.Background(), second.DeliveryIDs[0]); err != nil {
				t.Fatalf("terminal predecessor blocked next intent: %v", err)
			}
			if next := onePublicDelivery(t, store, second); next.State != PublicDeliveryAdmitted || next.OperationID == "" {
				t.Fatalf("second=%+v", next)
			}
			ops, err := lane.backing.List(beads.ListQuery{Label: opjournal.LabelOp, IncludeClosed: true})
			if err != nil || len(ops) != 1 {
				t.Fatalf("native ops=%v err=%v", ops, err)
			}
		})
	}
}

func TestPublicTransientFailureIsDurableRetryableAndRecoversSameOperationKey(t *testing.T) {
	mem := publicTestStore()
	lane := newPublicJournalTestLane(t)
	lane.prepareError = beads.ErrStoreUnavailable
	store := publicTestAdmission(t, mem, lane)
	input := publicTestInput()
	input.Recipients = input.Recipients[:1]
	receipt, err := store.Admit(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Relay(context.Background(), receipt.DeliveryIDs[0]); err == nil {
		t.Fatal("outage unreported")
	}
	before := onePublicDelivery(t, store, receipt)
	if before.State != PublicDeliveryQueued || !before.Retryable || before.ErrorCode == "" {
		t.Fatalf("outage=%+v", before)
	}
	lane.prepareError = nil
	recreated := publicTestAdmission(t, mem, lane)
	if _, err := recreated.Relay(context.Background(), receipt.DeliveryIDs[0]); err != nil {
		t.Fatal(err)
	}
	after := onePublicDelivery(t, recreated, receipt)
	if after.State != PublicDeliveryAdmitted || after.OperationKey != before.OperationKey || after.ErrorCode != "" || after.Retryable {
		t.Fatalf("recovery=%+v", after)
	}
}

func TestPublicCancelPreparedRaceReservesTombstoneBeforePausedAdmission(t *testing.T) {
	mem := publicTestStore()
	lane := newPublicJournalTestLane(t)
	lane.entered = make(chan struct{})
	lane.release = make(chan struct{})
	var release sync.Once
	unblock := func() { release.Do(func() { close(lane.release) }) }
	defer unblock()
	store := publicTestAdmission(t, mem, lane)
	input := publicTestInput()
	input.Recipients = input.Recipients[:1]
	receipt, err := store.Admit(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	returned := make(chan error, 1)
	go func() { _, err := store.Relay(ctx, receipt.DeliveryIDs[0]); returned <- err }()
	select {
	case <-lane.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	canceled, err := store.RequestCancel(ctx, receipt.ID)
	if err != nil || !canceled.CancelRequested {
		t.Fatalf("cancel=%+v %v", canceled, err)
	}
	pending := onePublicDelivery(t, store, receipt)
	if pending.State != PublicDeliveryCancelRequested {
		t.Fatalf("prepared request falsely called canceled: %+v", pending)
	}
	// A successor worker resolves cancellation while the old admission is paused.
	successor := publicTestAdmission(t, mem, lane)
	if _, err := successor.Relay(ctx, pending.ID); err != nil {
		t.Fatal(err)
	}
	if got := onePublicDelivery(t, successor, receipt); got.State != PublicDeliveryCanceled || got.OperationID == "" {
		t.Fatalf("tombstone=%+v", got)
	}
	unblock()
	if err := <-returned; err != nil {
		t.Fatal(err)
	}
	ops, err := lane.backing.List(beads.ListQuery{Label: opjournal.LabelOp, IncludeClosed: true})
	if err != nil || len(ops) != 1 {
		t.Fatalf("ops=%v err=%v", ops, err)
	}
	op, err := lane.store.Get(ops[0].ID)
	if err != nil || op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmCanceled || op.Attempt != 0 {
		t.Fatalf("resurrected native op=%+v err=%v", op, err)
	}
}

type publicCancelLostAck struct {
	*beads.MemStore
	once sync.Once
}

func (s *publicCancelLostAck) GuardedBatcherHandle() (beads.GuardedBatcher, bool) { return s, true }
func (s *publicCancelLostAck) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	result, err := s.MemStore.ApplyGuarded(ctx, b)
	lose := false
	if err == nil && b.Reason == "request cancellation of public message recipients" {
		s.once.Do(func() { lose = true })
	}
	if lose {
		return result, errors.New("cancel committed; acknowledgment lost")
	}
	return result, err
}

func TestPublicCancelLostAcknowledgmentAndStateEventRecovery(t *testing.T) {
	mem := &publicCancelLostAck{MemStore: publicTestStore()}
	lane := newPublicJournalTestLane(t)
	store := publicTestAdmission(t, mem, lane)
	input := publicTestInput()
	input.Recipients = input.Recipients[:1]
	receipt, err := store.Admit(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestCancel(context.Background(), receipt.ID); err != nil {
		t.Fatal(err)
	}
	recreated := publicTestAdmission(t, mem, lane)
	if _, err := recreated.RequestCancel(context.Background(), receipt.ID); err != nil {
		t.Fatal(err)
	}
	delivery := onePublicDelivery(t, recreated, receipt)
	if delivery.State != PublicDeliveryCanceled || delivery.OperationID != "" {
		t.Fatalf("canceled=%+v", delivery)
	}
	var observed []string
	fail := true
	emit := func(d PublicDelivery) error {
		observed = append(observed, fmt.Sprintf("%s/%d", d.ID, d.StateVersion))
		if fail {
			fail = false
			return errors.New("state event appended; ack lost")
		}
		return nil
	}
	if err := recreated.InvalidateDelivery(context.Background(), delivery.ID, emit); err == nil {
		t.Fatal("expected event lost ack")
	}
	if err := recreated.InvalidateDelivery(context.Background(), delivery.ID, emit); err != nil {
		t.Fatal(err)
	}
	if err := recreated.InvalidateDelivery(context.Background(), delivery.ID, emit); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 2 || observed[0] != observed[1] {
		t.Fatalf("state identities=%v", observed)
	}
	pending, _, err := recreated.PendingDeliveryChanges(context.Background(), 100, nil)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending changes=%v err=%v", pending, err)
	}
	ops, err := lane.backing.List(beads.ListQuery{Label: opjournal.LabelOp, IncludeClosed: true})
	if err != nil || len(ops) != 0 {
		t.Fatalf("cancel created runnable work=%v err=%v", ops, err)
	}
}
