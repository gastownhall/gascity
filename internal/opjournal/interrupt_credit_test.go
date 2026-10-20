package opjournal

import (
	"fmt"
	"reflect"
	"testing"
)

// creditInterruptReq is a lane interrupt of conversation gc-1 in generation gen,
// scoped to turn when it is set.
func creditInterruptReq(key string, gen int64, turn string) Request {
	return Request{
		Conversation: "gc-1", Kind: KindInterrupt, Executor: ExecutorLane, ClientKey: key,
		Producer: Producer{Principal: "operator:alice", Actor: ActorOperator},
		Scope:    Scope{Kind: ScopeSession, Generation: gen, Turn: turn},
	}
}

// TestInterruptCreditsReadsTheOpsThatRan pins which interrupt ops credit a
// turn's end (X11 §5.4, D8): those whose effect may have reached the runtime,
// in the generation asked, by their result's turn or else their scope's,
// newest first; not one still waiting, one that never ran, one of another
// generation or conversation, nor a message.
func TestInterruptCreditsReadsTheOpsThatRan(t *testing.T) {
	h := newHarness(t)
	closeAs := func(op Op, outcome Outcome) {
		t.Helper()
		if _, err := h.store.Transition(op.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
			t.Fatal(err)
		}
		if _, err := h.store.Close(op.ID, outcome, "lane"); err != nil {
			t.Fatal(err)
		}
	}
	scoped := h.propose(t, creditInterruptReq("scoped", 2, "t1"))
	closeAs(scoped, Outcome{Arm: ArmSucceeded, Result: Result{Generation: 2, Turn: "t1"}})
	session := h.propose(t, creditInterruptReq("session", 2, ""))
	closeAs(session, Outcome{Arm: ArmSucceeded, Degradation: "unfenced_interrupt", Result: Result{Generation: 2, Turn: "t2"}})
	unsettled := h.propose(t, creditInterruptReq("unsettled", 2, "t3"))
	closeAs(unsettled, Outcome{Arm: ArmFailed, Reason: ReasonNoBoundary})
	stale := h.propose(t, creditInterruptReq("stale", 2, "t4"))
	closeAs(stale, Outcome{Arm: ArmSkipped, Reason: ReasonStale})
	unsupported := h.propose(t, creditInterruptReq("unsupported", 2, "t5"))
	closeAs(unsupported, Outcome{Arm: ArmFailed, Reason: ReasonUnsupported})
	running := h.propose(t, creditInterruptReq("running", 2, "t6"))
	if _, err := h.store.Transition(running.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
		t.Fatal(err)
	}
	h.propose(t, creditInterruptReq("waiting", 2, "t7"))
	h.propose(t, creditInterruptReq("bare", 2, ""))
	older := h.propose(t, creditInterruptReq("older", 1, "t8"))
	closeAs(older, Outcome{Arm: ArmSucceeded, Result: Result{Generation: 1, Turn: "t8"}})
	other := creditInterruptReq("other", 2, "t9")
	other.Conversation = "gc-2"
	h.propose(t, other)
	h.propose(t, msgReq("message", "hello"))

	got, err := h.store.InterruptCredits("gc-1", 2)
	if err != nil {
		t.Fatalf("InterruptCredits: %v", err)
	}
	want := []InterruptRef{
		{Op: running.ID, Turn: "t6"},
		{Op: unsettled.ID, Turn: "t3"},
		{Op: session.ID, Turn: "t2"},
		{Op: scoped.ID, Turn: "t1"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("credits = %+v, want %+v", got, want)
	}
	if got, err := h.store.InterruptCredits("gc-1", 3); err != nil || len(got) != 0 {
		t.Fatalf("credits in generation 3 = %+v, %v, want none", got, err)
	}
	if _, err := h.store.InterruptCredits("", 2); err == nil {
		t.Fatal("InterruptCredits accepted an empty conversation")
	}
}

// TestInterruptCreditsLooksPastInterruptsThatDidNotRun pins the lookback's
// unit: the newest interrupt ops that ran in the generation, so newer ones
// that never ran, or ran in another generation, do not hide an older one that
// ran; the read stops after interruptCreditPages pages of rows.
func TestInterruptCreditsLooksPastInterruptsThatDidNotRun(t *testing.T) {
	h := newHarness(t)
	ran := h.propose(t, creditInterruptReq("ran", 2, "t1"))
	if _, err := h.store.Transition(ran.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
		t.Fatal(err)
	}
	for i := range interruptCreditLookback + 1 {
		h.propose(t, creditInterruptReq(fmt.Sprintf("waiting-%d", i), 2, fmt.Sprintf("w%d", i)))
	}
	for i := range interruptCreditLookback + 1 {
		op := h.propose(t, creditInterruptReq(fmt.Sprintf("older-%d", i), 1, fmt.Sprintf("o%d", i)))
		if _, err := h.store.Transition(op.ID, StatusSubmitted, StatusExecuting, "lane"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := h.store.InterruptCredits("gc-1", 2)
	if err != nil {
		t.Fatalf("InterruptCredits: %v", err)
	}
	if want := []InterruptRef{{Op: ran.ID, Turn: "t1"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("credits = %+v, want %+v", got, want)
	}
	for i := range interruptCreditLookback*interruptCreditPages - 2*(interruptCreditLookback+1) {
		h.propose(t, creditInterruptReq(fmt.Sprintf("later-%d", i), 2, fmt.Sprintf("l%d", i)))
	}
	if got, err := h.store.InterruptCredits("gc-1", 2); err != nil || len(got) != 0 {
		t.Fatalf("credits behind %d newer rows = %+v, %v, want none read", interruptCreditLookback*interruptCreditPages, got, err)
	}
}
