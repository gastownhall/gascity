package session

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

// suspendIntents are Suspend's three callers: an operator's suspend, the chat
// idle auto-suspend (soft) and the city stop sweep.
var suspendIntents = []struct {
	name string
	kind ActorKind
	soft bool
}{
	{name: "operator", kind: ActorOperator},
	{name: "idle", kind: ActorOperator, soft: true},
	{name: "shutdown sweep", kind: ActorSweep},
}

// TestSuspendClearsWakeStampBeforeTheStop: every suspend intent clears
// last_woke_at before the runtime is torn down, so a tick that sees the dead
// runtime never classifies the deliberate stop as a rapid crash or churn of
// that wake (which would discard the conversation the next wake resumes). A
// failed stop restores the stamp: the runtime is still live under that wake.
func TestSuspendClearsWakeStampBeforeTheStop(t *testing.T) {
	const woke = "2026-10-08T11:59:50Z"
	for _, in := range suspendIntents {
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

// moveStampOnFirstGet returns the row as read, then writes a newer
// last_woke_at: a wake stamped between Suspend's read and its clear.
type moveStampOnFirstGet struct {
	beads.Store
	stamp string
	done  bool
}

func (s *moveStampOnFirstGet) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if !s.done {
		s.done = true
		_ = s.SetMetadata(id, "last_woke_at", s.stamp)
	}
	return b, err
}

func (s *moveStampOnFirstGet) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// TestSuspendClearsAStampWrittenSinceItsRead: a wake stamp written after
// Suspend read the row is the one cleared, so the stop never runs with a
// stamp set; a failed stop restores that newer stamp.
func TestSuspendClearsAStampWrittenSinceItsRead(t *testing.T) {
	const older, newer = "2026-10-08T11:59:00Z", "2026-10-08T11:59:50Z"
	for _, in := range suspendIntents {
		for _, fail := range []bool{false, true} {
			mem := beads.NewMemStore()
			b := wakeRow(t, mem)
			if err := mem.SetMetadata(b.ID, "last_woke_at", older); err != nil {
				t.Fatal(err)
			}
			sp := &stopSeesProvider{Fake: runtime.NewFake(), store: mem, id: b.ID, fail: fail}
			if err := sp.Start(context.Background(), "s-wake", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			m := newTestManager(t, &moveStampOnFirstGet{Store: mem, stamp: newer}, sp)
			if err := m.Suspend(context.Background(), testActor(m, in.kind), b.ID, in.soft); (err != nil) != fail {
				t.Fatalf("%s fail=%v: Suspend err=%v", in.name, fail, err)
			}
			if got := sp.seen["last_woke_at"]; got != "" {
				t.Errorf("%s fail=%v: the stop found last_woke_at=%q, want the newer stamp cleared first", in.name, fail, got)
			}
			want := ""
			if fail {
				want = newer
			}
			if got, _ := mem.Get(b.ID); got.Metadata["last_woke_at"] != want {
				t.Errorf("%s fail=%v: after Suspend last_woke_at=%q, want %q", in.name, fail, got.Metadata["last_woke_at"], want)
			}
		}
	}
}

// TestSuspendUnderContentionStopsNothing: with conditional writes on
// (gate.Auto) and a concurrent writer winning every fence, Suspend's guarded
// write is lost on every attempt: it returns an error and leaves the runtime
// running and the wake stamp and state as they were.
func TestSuspendUnderContentionStopsNothing(t *testing.T) {
	const woke = "2026-10-08T11:59:50Z"
	for _, in := range suspendIntents {
		mem := beads.NewMemStore()
		b := wakeRow(t, mem)
		if err := mem.SetMetadata(b.ID, "last_woke_at", woke); err != nil {
			t.Fatal(err)
		}
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), "s-wake", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		m := newTestManager(t, &everyGetWrites{Store: stampedMemStore(t, gate.Auto, mem)}, sp)
		err := m.Suspend(context.Background(), testActor(m, in.kind), b.ID, in.soft)
		if err == nil || !strings.Contains(err.Error(), "lost to concurrent writes") {
			t.Fatalf("%s: Suspend under a writer winning every fence = %v, want its guarded write lost", in.name, err)
		}
		if !sp.IsRunning("s-wake") {
			t.Errorf("%s: the runtime was stopped though the suspend's write was lost", in.name)
		}
		got, _ := mem.Get(b.ID)
		if got.Metadata["last_woke_at"] != woke || got.Metadata["state"] != string(StateActive) {
			t.Errorf("%s: last_woke_at=%q state=%q, want %q and active", in.name, got.Metadata["last_woke_at"], got.Metadata["state"], woke)
		}
	}
}
