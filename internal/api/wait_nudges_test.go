package api

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// proposeWaitNotice proposes a wait's lane notice to conversation, as cmd/gc's
// wait dispatch does.
func proposeWaitNotice(t *testing.T, st *lanePokeState, conversation string) opjournal.Op {
	t.Helper()
	op, _, err := st.ops.Propose(opjournal.Request{
		Conversation: conversation, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane, ClientKey: "wait:wait-gc-9-1-1",
		Producer: opjournal.Producer{Principal: waitNoticePrincipal, Actor: opjournal.ActorBackground}, Payload: "Wait satisfied",
		Causation: "gc-9",
		Policy:    opjournal.Policy{Message: opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeLiveOnly}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// The API's wait withdrawal cancels a wait's lane notice by its op id and
// sends any other id to the nudge queue.
func TestWaitWithdrawCancelsItsNotice(t *testing.T) {
	e, st := newLaneOpsEnv(t, "message")
	queue := &recordingNudgeQueue{}
	e.fs.nudgeQueue = queue
	op := proposeWaitNotice(t, st, e.session(t, "Waiting").ID)
	if err := e.server(t).withdrawQueuedWaitNudges([]string{op.ID, "wait-gc-8-1-1"}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	got, err := st.ops.Get(op.ID)
	if err != nil || got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != waitNoticePrincipal {
		t.Fatalf("notice after withdraw = %+v, %v", got, err)
	}
	if len(queue.withdrawn) != 1 || !slices.Equal(queue.withdrawn[0], []string{"wait-gc-8-1-1"}) {
		t.Fatalf("queue withdrawals = %v, want only the queue id", queue.withdrawn)
	}
}

// A notice already being delivered cannot be withdrawn: the API records the
// cancel, leaves the lane's outcome to say whether the delivery landed, and
// logs that.
func TestWaitWithdrawReportsANoticeBeingDelivered(t *testing.T) {
	var logs bytes.Buffer
	defer captureLog(t, &logs)()
	e, st := newLaneOpsEnv(t, "message")
	e.fs.nudgeQueue = &recordingNudgeQueue{}
	op := proposeWaitNotice(t, st, e.session(t, "Waiting").ID)
	if _, err := st.ops.Transition(op.ID, opjournal.StatusSubmitted, opjournal.StatusExecuting, "lane:test"); err != nil {
		t.Fatal(err)
	}
	if err := e.server(t).withdrawQueuedWaitNudges([]string{op.ID}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	got, err := st.ops.Get(op.ID)
	if err != nil || got.Status != opjournal.StatusExecuting || got.Cancel.By != waitNoticePrincipal {
		t.Fatalf("notice after withdraw = %+v, %v; want it executing with gc:wait's cancel recorded", got, err)
	}
	if !strings.Contains(logs.String(), "wait notice "+op.ID+" is being delivered") {
		t.Fatalf("log = %q, want the withdrawal that could not stop the delivery reported", logs.String())
	}
}

// A notice this build cannot read is left as it is: the lane never delivers
// such a notice, so the API logs it rather than failing the withdrawal of a
// wait it has already ended.
func TestWaitWithdrawLeavesAnUnreadableNotice(t *testing.T) {
	var logs bytes.Buffer
	defer captureLog(t, &logs)()
	e, st := newLaneOpsEnv(t, "message")
	e.fs.nudgeQueue = &recordingNudgeQueue{}
	op := proposeWaitNotice(t, st, e.session(t, "Waiting").ID)
	if err := e.store.SetMetadata(op.ID, "gc.op.schema", "99"); err != nil {
		t.Fatalf("rewriting the notice's schema revision: %v", err)
	}
	if _, err := st.ops.Get(op.ID); !errors.Is(err, opjournal.ErrUnsupportedSchema) {
		t.Fatalf("Get(%s) = %v, want it unreadable", op.ID, err)
	}
	if err := e.server(t).withdrawQueuedWaitNudges([]string{op.ID}); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	if !strings.Contains(logs.String(), "wait notice "+op.ID+" is unreadable") {
		t.Fatalf("log = %q, want the unreadable notice named", logs.String())
	}
}
