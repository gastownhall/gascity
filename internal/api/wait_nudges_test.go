package api

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// The API's wait withdrawal cancels a wait's lane notice by its op id and
// sends any other id to the nudge queue.
func TestWaitWithdrawCancelsItsNotice(t *testing.T) {
	e, st := newLaneOpsEnv(t, "message")
	queue := &recordingNudgeQueue{}
	e.fs.nudgeQueue = queue
	info := e.session(t, "Waiting")
	op, _, err := st.ops.Propose(opjournal.Request{
		Conversation: info.ID, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane, ClientKey: "wait:wait-gc-9-1-1",
		Producer: opjournal.Producer{Principal: waitNoticePrincipal, Actor: opjournal.ActorBackground}, Payload: "Wait satisfied",
		Causation: "gc-9",
		Policy:    opjournal.Policy{Message: opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeLiveOnly}},
	})
	if err != nil {
		t.Fatal(err)
	}
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
