package opjournal

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/clock"
)

func TestNewLaneStoreRefusesStoresWithoutGuardedBatches(t *testing.T) {
	clk := &clock.Fake{Time: t0}
	calls := 0
	bd := beads.NewBdStore(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		calls++
		return nil, errors.New("bd must not run")
	})
	for name, store := range map[string]beads.Store{
		"bd":            bd,
		"cache over bd": beads.NewCachingStoreForTest(bd, nil),
		"recorder":      beadstest.NewOpRecordingStore(beads.NewMemStore()),
		"fake bd":       beadstest.NewFakeBd().NewBdStore(),
	} {
		_, err := NewLaneStore(beads.SessionStore{Store: store}, clk, WithIDPrefix("gc"))
		if !errors.Is(err, beads.ErrGuardedBatchUnsupported) {
			t.Errorf("%s: NewLaneStore = %v, want ErrGuardedBatchUnsupported", name, err)
		}
	}
	if calls != 0 {
		t.Fatalf("NewLaneStore ran bd %d times", calls)
	}
	if _, err := NewLaneStore(beads.SessionStore{}, clk, WithIDPrefix("gc")); err == nil {
		t.Fatal("accepted a nil store")
	}
}

func TestNewLaneStoreAcceptsMemStore(t *testing.T) {
	s, err := NewLaneStore(beads.SessionStore{Store: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}, &clock.Fake{Time: t0}, WithIDPrefix("gc"))
	if err != nil || s.batcher == nil {
		t.Fatalf("NewLaneStore(mem) = %v", err)
	}
}

func openSQLiteLaneStore(t *testing.T) (*Store, beads.Store, *clock.Fake) {
	t.Helper()
	sq, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if c, ok := sq.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore()
		}
	})
	clk := &clock.Fake{Time: t0}
	s, err := NewLaneStore(beads.SessionStore{Store: sq}, clk)
	if err != nil {
		t.Fatalf("NewLaneStore(sqlite): %v", err)
	}
	return s, sq, clk
}

func TestLanePassOverSQLite(t *testing.T) {
	s, _, clk := openSQLiteLaneStore(t)
	n, _, err := s.Propose(laneMsg("n", RoleNotice, "fyi"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := s.Propose(laneMsg("p", RolePrompt, "go"))
	if err != nil {
		t.Fatal(err)
	}
	l, err := NewLane(s, "ctl", clk)
	if err != nil {
		t.Fatal(err)
	}
	exec := newFakeExec(clk)
	exec.deliver = func(Delivery) DeliveryResult { return NotDelivered{Transient: true} }
	res, err := l.Pass(context.Background(), view(), exec)
	if err != nil || res.Done != DoneReleased {
		t.Fatalf("first pass %+v, %v", res, err)
	}
	clk.Advance(TransientBackoffBase + 1)
	exec.deliver = nil
	// The runtime row now exists: the second Take guards on its revision.
	res, err = l.Pass(context.Background(), view(), exec)
	if err != nil || res.Done != DoneAccepted {
		t.Fatalf("second pass %+v, %v", res, err)
	}
	for _, id := range []string{n.ID, p.ID} {
		got, err := s.Get(id)
		if err != nil || got.Outcome.Arm != ArmSucceeded || got.Attempt != 2 {
			t.Fatalf("op %+v, %v", got, err)
		}
	}
	rt, _, found, err := s.Runtime("gc-1")
	if err != nil || !found || rt.Seq != 4 || !rt.Lease.IsZero() {
		t.Fatalf("runtime %+v found %v, %v", rt, found, err)
	}
}

func TestStoreRuntime(t *testing.T) {
	h := newLaneHarness(t)
	if _, _, found, err := h.store.Runtime("gc-1"); found || err != nil {
		t.Fatalf("missing row: found %v, %v", found, err)
	}
	b, _ := EncodeRuntimeRow("gc", RuntimeRow{Conversation: "gc-1", Generation: 2, Seq: 9})
	created, err := h.mem.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	rt, rev, found, err := h.store.Runtime("gc-1")
	if err != nil || !found || rt.Seq != 9 || rev != created.Revision {
		t.Fatalf("Runtime = %+v rev %d found %v, %v", rt, rev, found, err)
	}
	if err := h.mem.SetMetadata(b.ID, keyRTSeq, "nine"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.store.Runtime("gc-1"); !errors.Is(err, ErrCorruptOp) {
		t.Fatalf("corrupt row: %v", err)
	}
}
