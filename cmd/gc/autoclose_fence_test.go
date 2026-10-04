package main

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// interleavedStore runs between once after the first live Get of id returns:
// a write that lands between autoclose's re-read and its close.
type interleavedStore struct {
	beads.Store
	id      string
	once    sync.Once
	between func()
}

func (s *interleavedStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if id == s.id && s.between != nil {
		s.once.Do(s.between)
	}
	return b, err
}

func (s *interleavedStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

func stampedMemStore(t *testing.T, store beads.Store) {
	t.Helper()
	if err := beads.StampOpenedStore(store, "MemStore", gate.Require, nil, nil); err != nil {
		t.Fatalf("stamp: %v", err)
	}
}

// TestAutocloseCloseIfStill pins the fenced close: a live re-check always, a
// CAS on the re-read revision where conditional writes resolve.
func TestAutocloseCloseIfStill(t *testing.T) {
	open := func(b beads.Bead) bool { return b.Status == "open" }
	for _, tc := range []struct {
		name       string
		store      func(t *testing.T) beads.Store
		still      func(beads.Bead) bool
		interleave bool
		wantClosed bool
		wantReason bool
	}{
		{name: "unconditional, premise holds", store: func(*testing.T) beads.Store { return beads.NewMemStore() }, still: open, wantClosed: true},
		{name: "unconditional, premise moved", store: func(*testing.T) beads.Store { return beads.NewMemStore() }, still: func(beads.Bead) bool { return false }},
		{name: "CAS, nothing between", store: func(t *testing.T) beads.Store {
			m := beads.NewMemStore()
			stampedMemStore(t, m)
			return m
		}, still: open, wantClosed: true, wantReason: true},
		// The live re-check passed; a write landed before the close. Only the
		// CAS can refuse this.
		{name: "CAS, write between re-read and close", store: func(t *testing.T) beads.Store {
			m := beads.NewMemStore()
			stampedMemStore(t, m)
			return m
		}, still: open, interleave: true},
		{name: "atomic closer commits the reason", store: func(t *testing.T) beads.Store {
			m := beads.NewAtomicCloseMemStore()
			stampedMemStore(t, m)
			return m
		}, still: open, wantClosed: true, wantReason: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := tc.store(t)
			target, err := inner.Create(beads.Bead{Title: "target"})
			if err != nil {
				t.Fatal(err)
			}
			store := &interleavedStore{Store: inner, id: target.ID}
			if tc.interleave {
				store.between = func() { _ = inner.SetMetadata(target.ID, "touched", "yes") }
			}
			unconditional := 0
			closed, err := autocloseCloseIfStill(store, target.ID, "test-reason", tc.still, func() error {
				unconditional++
				return inner.Close(target.ID)
			})
			if err != nil {
				t.Fatalf("autocloseCloseIfStill: %v", err)
			}
			got, _ := inner.Get(target.ID)
			if closed != tc.wantClosed || (got.Status == "closed") != tc.wantClosed {
				t.Fatalf("closed = %v, status %q; want closed %v", closed, got.Status, tc.wantClosed)
			}
			if tc.wantReason && got.Metadata["close_reason"] != "test-reason" {
				t.Fatalf("close_reason = %q", got.Metadata["close_reason"])
			}
			if tc.wantReason && unconditional != 0 {
				t.Fatal("a conditional store took the unconditional close")
			}
		})
	}
}

// TestConvoyAutocloseReadsTheConvoyLiveBeforeClosing: the decision read the
// convoy from the cache, which lacks the owned label an out-of-process writer
// added. The live re-check sees it and leaves the convoy open
// (STALE-READ-RESILIENCE L5).
func TestConvoyAutocloseReadsTheConvoyLiveBeforeClosing(t *testing.T) {
	backing := beads.NewMemStore()
	convoy, err := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	member, err := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
	if err != nil {
		t.Fatal(err)
	}
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := backing.Update(convoy.ID, beads.UpdateOpts{Labels: []string{"owned"}}); err != nil {
		t.Fatal(err)
	}
	if err := cached.Close(member.ID); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	var stdout bytes.Buffer
	doConvoyAutocloseWith(cached, rec, member.ID, &stdout, &stdout)

	if got := statusOf(t, backing, convoy.ID); got != "open" {
		t.Fatalf("convoy %s; the live row is owned", got)
	}
	for _, evt := range rec.Events {
		if evt.Type == events.ConvoyClosed {
			t.Fatalf("recorded %s for a convoy left open", evt.Type)
		}
	}
}

// TestMoleculeAutocloseLeavesARootClosedBehindTheCache: the root was closed
// out of process; the cache still holds it open. The fenced close refuses, so
// no second close, close_reason or resolution record is written.
func TestMoleculeAutocloseLeavesARootClosedBehindTheCache(t *testing.T) {
	backing := beads.NewMemStore()
	root, err := backing.Create(beads.Bead{Title: "mol", Type: "molecule"})
	if err != nil {
		t.Fatal(err)
	}
	step, err := backing.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID, Metadata: map[string]string{"gc.root_bead_id": root.ID}})
	if err != nil {
		t.Fatal(err)
	}
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := backing.Close(root.ID); err != nil {
		t.Fatal(err)
	}
	if err := cached.Close(step.ID); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	var stdout bytes.Buffer
	doMoleculeAutocloseWith(cached, "", rec, step.ID, &stdout, beads.GraphStore{Store: cached})

	for _, evt := range rec.Events {
		if evt.Subject == root.ID {
			t.Fatalf("recorded %s for a root another writer already closed", evt.Type)
		}
	}
	if got, _ := backing.Get(root.ID); got.Metadata["close_reason"] != "" {
		t.Fatalf("close_reason stamped %q on a root this autoclose did not close", got.Metadata["close_reason"])
	}
}

// reopeningStore answers the first live Get of id closed, later ones open: a
// parent reopened while wisp autoclose collected its attachments.
type reopeningStore struct {
	*beads.MemStore
	id    string
	calls int
}

func (s *reopeningStore) Get(id string) (beads.Bead, error) {
	b, err := s.MemStore.Get(id)
	if id == s.id {
		s.calls++
		if s.calls == 1 {
			b.Status = "closed"
		}
	}
	return b, err
}

func TestWispAutocloseRechecksTheParentBeforeClosing(t *testing.T) {
	mem := beads.NewMemStore()
	parent, _ := mem.Create(beads.Bead{Title: "work item"})
	wisp, _ := mem.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: parent.ID})
	store := &reopeningStore{MemStore: mem, id: parent.ID}

	var stdout bytes.Buffer
	doWispAutocloseWith(store, parent.ID, &stdout, beads.GraphStore{Store: store})

	if got := statusOf(t, mem, wisp.ID); got != "open" {
		t.Fatalf("wisp %s under a parent reopened before the close", got)
	}
}

// TestWispAutocloseRechecksAReapedRootBeforeClosing: the attachment root read
// terminal (an orphan to reap), then was reopened before the close. A reopened
// root may be a parked checkpoint again, so it is left alone.
func TestWispAutocloseRechecksAReapedRootBeforeClosing(t *testing.T) {
	mem := beads.NewMemStore()
	wisp, _ := mem.Create(beads.Bead{Title: "wisp", Type: "molecule"})
	parent, _ := mem.Create(beads.Bead{Title: "work item", Metadata: map[string]string{"molecule_id": wisp.ID}})
	if err := mem.Close(parent.ID); err != nil {
		t.Fatal(err)
	}
	store := &reopeningStore{MemStore: mem, id: wisp.ID}

	var stdout bytes.Buffer
	doWispAutocloseWith(store, parent.ID, &stdout, beads.GraphStore{Store: store})

	if got := statusOf(t, mem, wisp.ID); got != "open" {
		t.Fatalf("wisp %s after it was reopened before the close", got)
	}
}
