package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// An intentional suspend is not an exit to classify: a real legacy tick over
// a seat suspended inside the rapid-crash or churn window after its wake must
// neither count a crash or churn nor discard the provider conversation key,
// so the next wake resumes the conversation. Covers every suspend intent:
// the operator's (hold), the chat idle auto-suspend (soft, no hold), and the
// city stop sweep.
func TestSuspendPreservesConversationThroughExitReconciliation(t *testing.T) {
	intents := []struct {
		name  string
		kind  session.ActorKind
		soft  bool
		sweep bool
	}{
		{name: "operator", kind: session.ActorOperator},
		{name: "idle", kind: session.ActorOperator, soft: true},
		{name: "shutdown sweep", sweep: true},
	}
	ages := []struct {
		name string
		age  time.Duration
	}{
		{name: "rapid-crash window", age: stabilityThreshold / 3},
		{name: "churn window", age: stabilityThreshold + time.Minute},
	}
	for _, in := range intents {
		for _, a := range ages {
			t.Run(in.name+"/"+a.name, func(t *testing.T) {
				e := newKilledSeatEnv(t, persistentWorker())
				work := e.addWork(t, "in_progress", e.seat.ID, nil)
				e.tick(t, []beads.Bead{work}, withReadyAssignedFlags([]bool{true}))
				name := e.reload(t, e.seat.ID).Metadata["session_name"]
				if !e.sp.IsRunning(name) {
					t.Fatal("seat with ready in_progress work was not woken")
				}
				if woke := e.reload(t, e.seat.ID).Metadata["last_woke_at"]; woke == "" {
					t.Fatal("woken seat carries no last_woke_at")
				}
				const key = "conversation-to-resume"
				if err := e.mem.SetMetadataBatch(e.seat.ID, map[string]string{
					"session_key": key, "wake_attempts": "0", "churn_count": "0",
				}); err != nil {
					t.Fatal(err)
				}
				e.advance(a.age)

				by := session.Actor{Kind: session.ActorSweep}
				if !in.sweep {
					by = testActorIn(t, in.kind, e.city)
				}
				mgr := session.NewManagerWithOptions(e.mem, e.sp, session.WithClock(e.clk), session.WithCityPath(e.city))
				if err := mgr.Suspend(context.Background(), by, e.seat.ID, in.soft); err != nil {
					t.Fatalf("Suspend: %v", err)
				}
				if e.sp.IsRunning(name) {
					t.Fatal("suspended runtime is still running")
				}

				for tick := 0; tick < 2; tick++ {
					e.advance(time.Second)
					e.tick(t, nil)
					m := e.reload(t, e.seat.ID).Metadata
					if m["session_key"] != key || m["continuation_reset_pending"] == "true" {
						t.Fatalf("tick %d lost the suspended conversation: session_key=%q continuation_reset_pending=%q", tick, m["session_key"], m["continuation_reset_pending"])
					}
					if w := m["wake_attempts"]; w != "" && w != "0" {
						t.Fatalf("tick %d counted the suspend as a crash: wake_attempts=%q", tick, w)
					}
					if c := m["churn_count"]; c != "" && c != "0" {
						t.Fatalf("tick %d counted the suspend as churn: churn_count=%q", tick, c)
					}
				}
			})
		}
	}
}
