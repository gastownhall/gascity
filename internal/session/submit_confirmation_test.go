package session

import (
	"context"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

type unconfirmedSubmitRuntime struct {
	*runtime.Fake
	deliver func()
}

func (r *unconfirmedSubmitRuntime) Nudge(_ string, _ []runtime.ContentBlock) error {
	r.deliver()
	return runtime.ErrSubmitUnconfirmed
}

func TestSubmitConfirmationSharesDeliveryLock(t *testing.T) {
	sp := &unconfirmedSubmitRuntime{Fake: runtime.NewFake()}
	mgr := NewManagerWithOptions(beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "probe", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	var phases []string
	// The receipt must be captured and checked under the actual delivery lock;
	// otherwise a simultaneous identical request can supply its matching record.
	recordLocked := func(phase string) {
		t.Helper()
		sessionMutationLocksMu.Lock()
		lock := sessionMutationLocks[info.ID]
		sessionMutationLocksMu.Unlock()
		if lock == nil {
			t.Fatalf("%s ran outside session delivery serialization", phase)
		}
		if lock.mu.TryLock() {
			lock.mu.Unlock()
			t.Fatalf("%s ran without the delivery lock", phase)
		}
		phases = append(phases, phase)
	}
	sp.deliver = func() { recordLocked("deliver") }
	outcome, err := mgr.SubmitWithConfirmation(context.Background(), info.ID, "same request", "claude", runtime.Config{}, SubmitIntentDefault, func() func() bool {
		recordLocked("snapshot")
		return func() bool { recordLocked("confirm"); return true }
	})
	if err != nil || outcome.Queued {
		t.Fatalf("confirmed submit: outcome=%+v error=%v", outcome, err)
	}
	if !reflect.DeepEqual(phases, []string{"snapshot", "deliver", "confirm"}) {
		t.Fatalf("receipt/delivery ordering=%v", phases)
	}
}
