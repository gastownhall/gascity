package session

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// controllerTickBurst is what one legacy reconciler tick writes to a row
// whose runtime an operator's close has just stopped (captured live,
// ga-u0fqtrq): the state heal (healStateWithRollbackInfo), then
// checkStability's recordWakeFailure and clearLastWokeAt. None moves the
// incarnation the close was decided on; each moves the row's revision.
func controllerTickBurst() []map[string]string {
	return []map[string]string{
		{
			"state":                      string(StateAsleep),
			"sleep_reason":               "runtime-missing",
			"continuation_reset_pending": "true",
			"session_key":                "",
			"started_config_hash":        "",
			"primed_at":                  "",
			"priming_attempted_at":       "",
			"prompt_hash":                "",
		},
		{"wake_attempts": "1"},
		{"last_woke_at": ""},
	}
}

func newBurstCloseManager(t *testing.T) (*Manager, *closeInterferenceStore, beads.Store, string) {
	t.Helper()
	backing := beads.NewAtomicCloseMemStore()
	tracing := &closeInterferenceStore{Store: backing}
	mgr := newTestManager(t, tracing, runtime.NewFake())
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		ExplicitName: "burst", Template: "helper", Title: "burst", Command: "claude", WorkDir: "/tmp",
		Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return mgr, tracing, backing, info.ID
}

// ga-u0fqtrq: an operator's close stops the runtime, then commits one write
// fenced on the row's revision. The tick after the stop writes the row three
// times, and with three attempts the close lost every fence: POST
// session/{id}/close?delete=true answered 500 "lost the revision fence 3
// times". An operator's close must outlast one tick's burst, as the
// operator's suspend does.
func TestOperatorCloseOutlastsAControllerTickBurst(t *testing.T) {
	mgr, tracing, backing, id := newBurstCloseManager(t)
	burst := controllerTickBurst()
	tracing.interfere = func(id string) error {
		if len(burst) == 0 {
			return nil
		}
		kvs := burst[0]
		burst = burst[1:]
		return backing.SetMetadataBatch(id, kvs)
	}

	if err := mgr.Close(context.Background(), testActor(mgr, ActorOperator), id); err != nil {
		t.Fatalf("operator close against one controller tick's writes: %v", err)
	}
	got, err := backing.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("status = %q, want closed", got.Status)
	}
	if tracing.atomicCalls != len(controllerTickBurst())+1 || tracing.plainCloseCalls != 0 {
		t.Fatalf("atomic/plain close calls = %d/%d, want %d/0 (one lost fence per burst write, then the close)", tracing.atomicCalls, tracing.plainCloseCalls, len(controllerTickBurst())+1)
	}
}

// The longer bound is still a bound: a writer that wins every fence cannot
// livelock an operator's close, which reports the lost fence, never closes
// with a split write, and leaves the row open.
func TestOperatorCloseStillBoundsAWriterThatWinsEveryFence(t *testing.T) {
	mgr, tracing, backing, id := newBurstCloseManager(t)
	tracing.interfere = func(id string) error {
		return backing.SetMetadata(id, unrelatedKey, strings.Repeat("x", tracing.atomicCalls))
	}

	err := mgr.Close(context.Background(), testActor(mgr, ActorOperator), id)
	if err == nil || !strings.Contains(err.Error(), "lost the revision fence") {
		t.Fatalf("operator close error = %v, want the bounded fence failure", err)
	}
	if tracing.atomicCalls != operatorSuspendAttempts || tracing.plainCloseCalls != 0 {
		t.Fatalf("atomic/plain close calls = %d/%d, want %d/0", tracing.atomicCalls, tracing.plainCloseCalls, operatorSuspendAttempts)
	}
	got, err := backing.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" || got.Metadata["close_reason"] != "" {
		t.Fatalf("row = status %q close_reason %q, want open with no terminal metadata", got.Status, got.Metadata["close_reason"])
	}
}
