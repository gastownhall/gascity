package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The operator start's re-read under its runtime lease (#7638 review r2,
// mc-0403x): ensureRunning decides on the row as read under the lease, not
// on the read before it.

// getHookStore runs afterGet once, right after the first Get once armed.
type getHookStore struct {
	*beads.MemStore
	armed    bool
	afterGet func()
}

func (s *getHookStore) Get(id string) (beads.Bead, error) {
	b, err := s.MemStore.Get(id)
	if s.armed && s.afterGet != nil {
		s.armed = false
		f := s.afterGet
		s.afterGet = nil
		f()
	}
	return b, err
}

// Kills the re-read moved ahead of the lease (D4): a plain signal that lands
// right after the operator's first row read past IsRunning. In order, that
// read is the lease's own record read, under the name's flock, so the signal
// refuses busy; with the re-read ahead of the lease it is that re-read, the
// signal lands, and the start proceeds on a row it read before the signal,
// leaving a started runtime on a signaled row.
func TestOperatorStartRereadsTheRowUnderItsLease(t *testing.T) {
	p, it, store, _ := admittedBeginOn(t, append([]string{"provider", "claude"}, intentBegunAt(drainOrphaned, time.Hour)...), undesired(drainOrphaned))
	if it.Kind != intentSignal {
		t.Fatalf("decided %+v, want the plain signal", it)
	}
	saved := withRowMutationLock
	withRowMutationLock = func(_ string, fn func() error) error { return fn() }
	t.Cleanup(func() { withRowMutationLock = saved })
	ran := false
	hs := &getHookStore{MemStore: store.(*beads.MemStore)}
	hs.afterGet = func() { ran = true; runTx(context.Background(), p, it, effectSpecs[it.Kind], nil) }
	sp := &attachRaceSP{Fake: runtime.NewFake(), onIsRunning: func() { hs.armed = true }}
	city := p.World.CityPath
	err := session.NewManagerWithOptions(hs, sp, session.WithCityPath(city)).Attach(context.Background(), testActorIn(t, session.ActorOperator, city), it.Key.ID, "claude", runtime.Config{})
	got, gerr := store.Get(it.Key.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	req, active := activeStop(censusRowOf(t, got))
	if !ran {
		t.Fatal("the signal never ran")
	}
	if sp.Fake.IsRunning("s-gc-1") && active && req.Phase == stopSignaled {
		t.Fatalf("attach err %v: a started runtime on a signaled row %v", err, got.Metadata)
	}
}

// Kills the kill-fence check dropped from the re-read (D3): a kill fence
// written between the operator's row read and its lease (a kill whose
// post-kill sync failed) refuses the start under the lease.
func TestOperatorStartRefusesAKillFenceWrittenBeforeItsLease(t *testing.T) {
	store, _ := stampedMem(t, gate.Require)
	b, err := store.Create(drainRow("provider", "claude"))
	if err != nil {
		t.Fatal(err)
	}
	sp := &attachRaceSP{Fake: runtime.NewFake(), onIsRunning: func() {
		if _, err := session.WriteKillFence(store, b.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}}
	city := t.TempDir()
	err = session.NewManagerWithOptions(store, sp, session.WithCityPath(city)).Attach(context.Background(), testActorIn(t, session.ActorOperator, city), b.ID, "claude", runtime.Config{})
	if !errors.Is(err, session.ErrSessionKillPending) || sp.Fake.IsRunning("s-gc-1") {
		t.Fatalf("attach err %v, running %t: want ErrSessionKillPending and nothing started", err, sp.Fake.IsRunning("s-gc-1"))
	}
}

// Kills the re-read skipped under a lent lease (D6): an interrupt's restart
// runs under the lease the interrupt took and lent it (D8D-5), but on the row
// the submit read before that lease. A kill fence written in between must
// still refuse the restart under the lease.
func TestInterruptRestartRereadsTheRowUnderTheLentLease(t *testing.T) {
	store, _ := stampedMem(t, gate.Require)
	b, err := store.Create(drainRow("provider", "pi"))
	if err != nil {
		t.Fatal(err)
	}
	sp := &attachRaceSP{Fake: runtime.NewFake()}
	if err := sp.Fake.Start(context.Background(), "s-gc-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	sp.onIsRunning = func() {
		if _, err := session.WriteKillFence(store, b.ID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	city := t.TempDir()
	m := session.NewManagerWithOptions(store, sp, session.WithCityPath(city))
	_, err = m.Submit(context.Background(), testActorIn(t, session.ActorOperator, city), b.ID, "hello", "pi", runtime.Config{}, session.SubmitIntentInterruptNow)
	if !errors.Is(err, session.ErrSessionKillPending) || sp.Fake.IsRunning("s-gc-1") {
		t.Fatalf("submit err %v, running %t: want the restart refused under the lent lease (ErrSessionKillPending) and nothing restarted", err, sp.Fake.IsRunning("s-gc-1"))
	}
}

// failNthGetStore fails the nth Get once armed.
type failNthGetStore struct {
	*beads.MemStore
	armed bool
	n     int
}

func (s *failNthGetStore) Get(id string) (beads.Bead, error) {
	if s.armed {
		if s.n--; s.n == 0 {
			s.armed = false
			return beads.Bead{}, errors.New("store unreachable")
		}
	}
	return s.MemStore.Get(id)
}

// Kills a failed re-read under the lease reported as a state-sync failure
// (ErrStateSync: the runtime reached its state but the row did not follow):
// nothing has started there, so it is a plain error, and nothing starts.
func TestOperatorStartFailedRereadIsAPlainError(t *testing.T) {
	store, _ := stampedMem(t, gate.Require)
	b, err := store.Create(drainRow("provider", "claude"))
	if err != nil {
		t.Fatal(err)
	}
	fs := &failNthGetStore{MemStore: store, n: 2} // the lease's record read, then the re-read
	sp := &attachRaceSP{Fake: runtime.NewFake(), onIsRunning: func() { fs.armed = true }}
	city := t.TempDir()
	err = session.NewManagerWithOptions(fs, sp, session.WithCityPath(city)).Attach(context.Background(), testActorIn(t, session.ActorOperator, city), b.ID, "claude", runtime.Config{})
	if err == nil || !strings.Contains(err.Error(), "under its lease") {
		t.Fatalf("attach err %v, want the failed re-read under the lease", err)
	}
	if errors.Is(err, session.ErrStateSync) || sp.Fake.IsRunning("s-gc-1") {
		t.Fatalf("attach err %v, running %t: want a plain error and nothing started", err, sp.Fake.IsRunning("s-gc-1"))
	}
}
