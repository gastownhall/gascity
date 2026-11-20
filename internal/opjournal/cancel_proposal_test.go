package opjournal

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCancelProposalReservesSameFrozenMessageWithoutRunnableWindow(t *testing.T) {
	h := newLaneHarness(t)
	req := msgReq("public-cancel", "frozen")
	result, canceled, err := h.store.CancelProposal(context.Background(), req, "gc:extmsg", "operator requested cancellation")
	if err != nil || result != CancelClosed || canceled.Status != StatusClosed || canceled.Outcome.Arm != ArmCanceled {
		t.Fatalf("cancel=%s %+v %v", result, canceled, err)
	}
	replay, created, err := h.store.Propose(req)
	if err != nil || created || replay.ID != canceled.ID || replay.Status != StatusClosed || replay.Outcome.Arm != ArmCanceled {
		t.Fatalf("proposal resurrected: %+v %v %v", replay, created, err)
	}
	changed := req
	changed.Payload = "changed"
	if _, _, err := h.store.CancelProposal(context.Background(), changed, "gc:extmsg", "cancel"); !errors.Is(err, ErrKeyReuse) {
		t.Fatalf("changed frozen request accepted: %v", err)
	}
}

func TestCancelProposalConcurrentWithAdmissionKeepsOneOperation(t *testing.T) {
	h := newLaneHarness(t)
	req := msgReq("public-race", "frozen")
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for n := range 16 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			if n%2 == 0 {
				_, _, err := h.store.Propose(req)
				errs <- err
			} else {
				_, _, err := h.store.CancelProposal(context.Background(), req, "gc:extmsg", "cancel")
				errs <- err
			}
		}(n)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	op, _, err := h.store.Propose(req)
	if err != nil || op.Status != StatusClosed || op.Outcome.Arm != ArmCanceled {
		t.Fatalf("race=%+v %v", op, err)
	}
}

func TestCancelProposalKeepsAlreadyTerminalOutcome(t *testing.T) {
	h := newLaneHarness(t)
	req := msgReq("public-finished", "frozen")
	original := h.propose(t, req)
	if _, err := h.store.Close(original.ID, Outcome{Arm: ArmSucceeded}, "executor"); err != nil {
		t.Fatal(err)
	}
	result, op, err := h.store.CancelProposal(context.Background(), req, "gc:extmsg", "late cancel")
	if err != nil || result != CancelAlreadyTerminal || op.Outcome.Arm != ArmSucceeded {
		t.Fatalf("late cancel=%s %+v %v", result, op, err)
	}
}

func TestCancelProposalLostAcknowledgmentRecoversSameClosedKey(t *testing.T) {
	h := newLaneHarness(t)
	h.batcher.setFault(func(_ int, b beads.GuardedBatch) (bool, error) {
		if b.Reason == "reserve canceled public message proposal" {
			return true, errLinkDropped
		}
		return false, nil
	})
	req := msgReq("lost-cancel-ack", "frozen")
	_, canceled, err := h.store.CancelProposal(context.Background(), req, "gc:extmsg", "cancel")
	if err != nil || canceled.Status != StatusClosed || canceled.Outcome.Arm != ArmCanceled {
		t.Fatalf("unknown ack recovery=%+v %v", canceled, err)
	}
	retry, created, err := h.store.Propose(req)
	if err != nil || created || retry.ID != canceled.ID || retry.Status != StatusClosed {
		t.Fatalf("replay=%+v %v %v", retry, created, err)
	}
}

func TestCancelProposalRequiresGuardedWriterBeforeAnyCreate(t *testing.T) {
	h := newHarness(t) // recorder deliberately has no batch capability
	_, _, err := h.store.CancelProposal(context.Background(), msgReq("unsupported-cancel", "frozen"), "gc:extmsg", "cancel")
	if !errors.Is(err, beads.ErrGuardedBatchUnsupported) {
		t.Fatalf("writer refusal=%v", err)
	}
	rows, err := h.mem.List(beads.ListQuery{Label: LabelOp, IncludeClosed: true})
	if err != nil || len(rows) != 0 {
		t.Fatalf("refused cancellation wrote %v error=%v", rows, err)
	}
}
