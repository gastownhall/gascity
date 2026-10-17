package session

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestSuspendClearsWakeStampBeforeTheStop: every suspend intent clears
// last_woke_at before the runtime is torn down, so a tick that sees the dead
// runtime never classifies the deliberate stop as a rapid crash or churn of
// that wake (which would discard the conversation the next wake resumes). A
// failed stop restores the stamp: the runtime is still live under that wake.
func TestSuspendClearsWakeStampBeforeTheStop(t *testing.T) {
	const woke = "2026-10-08T11:59:50Z"
	for _, in := range []struct {
		name string
		kind ActorKind
		soft bool
	}{
		{name: "operator", kind: ActorOperator},
		{name: "idle", kind: ActorOperator, soft: true},
		{name: "shutdown sweep", kind: ActorSweep},
	} {
		for _, fail := range []bool{false, true} {
			store := beads.NewMemStore()
			b := wakeRow(t, store)
			if err := store.SetMetadata(b.ID, "last_woke_at", woke); err != nil {
				t.Fatal(err)
			}
			sp := &stopSeesProvider{Fake: runtime.NewFake(), store: store, id: b.ID, fail: fail}
			if err := sp.Start(context.Background(), "s-wake", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			m := newTestManager(t, store, sp)
			err := m.Suspend(context.Background(), testActor(m, in.kind), b.ID, in.soft)
			if (err != nil) != fail {
				t.Fatalf("%s fail=%v: Suspend err=%v", in.name, fail, err)
			}
			if got := sp.seen["last_woke_at"]; got != "" {
				t.Errorf("%s fail=%v: the stop found last_woke_at=%q, want it cleared first", in.name, fail, got)
			}
			want := ""
			if fail {
				want = woke
			}
			if got, _ := store.Get(b.ID); got.Metadata["last_woke_at"] != want {
				t.Errorf("%s fail=%v: after Suspend last_woke_at=%q, want %q", in.name, fail, got.Metadata["last_woke_at"], want)
			}
		}
	}
}
