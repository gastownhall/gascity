package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	if err := beads.StampOpenedStore(store, "MemStore", gate.Require, false, nil, nil); err != nil {
		t.Fatalf("stamp: %v", err)
	}
}

// zeroRevisionStore reads every row back at revision 0 while the store holds a
// real one.
type zeroRevisionStore struct{ beads.Store }

func (s zeroRevisionStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	b.Revision = 0
	return b, err
}

func (s zeroRevisionStore) ConditionalWritesResolveTarget() beads.Store { return s.Store }

// TestAutocloseCloseIfStill pins the fenced close: a live re-check always, a
// CAS on the re-read revision where conditional writes resolve, and never a
// close after a failed read or a require-mode refusal.
func TestAutocloseCloseIfStill(t *testing.T) {
	open := func(b beads.Bead) bool { return b.Status == "open" }
	stamped := func(t *testing.T) *beads.MemStore {
		m := beads.NewMemStore()
		stampedMemStore(t, m)
		return m
	}
	for _, tc := range []struct {
		name       string
		store      func(t *testing.T) beads.Store
		wrap       func(inner beads.Store, id string) beads.Store
		still      func(beads.Bead) bool
		interleave bool
		wantClosed bool
		wantReason bool
		wantErr    func(error) bool
	}{
		{name: "unconditional, premise holds", store: func(*testing.T) beads.Store { return beads.NewMemStore() }, still: open, wantClosed: true},
		{name: "unconditional, premise moved", store: func(*testing.T) beads.Store { return beads.NewMemStore() }, still: func(beads.Bead) bool { return false }},
		{name: "CAS, nothing between", store: func(t *testing.T) beads.Store { return stamped(t) }, still: open, wantClosed: true, wantReason: true},
		// The live re-check passed; a write landed before the close. Only the
		// CAS can refuse this, and the refusal is told apart from a moved
		// premise so the caller re-decides.
		{
			name: "CAS, write between re-read and close", store: func(t *testing.T) beads.Store { return stamped(t) }, still: open, interleave: true,
			wantErr: func(err error) bool { return errors.Is(err, errAutocloseRefused) },
		},
		{name: "atomic closer commits the reason", store: func(t *testing.T) beads.Store {
			m := beads.NewAtomicCloseMemStore()
			stampedMemStore(t, m)
			return m
		}, still: open, wantClosed: true, wantReason: true},
		// A re-read at revision 0 is CASed at 0 like any revision, never
		// closed blind: over a row whose real revision is not 0 the CAS
		// refuses.
		{
			name: "CAS store, row read back at 0", store: func(t *testing.T) beads.Store { return stamped(t) },
			wrap: func(inner beads.Store, _ string) beads.Store { return zeroRevisionStore{inner} }, still: open,
			wantErr: func(err error) bool { return errors.Is(err, errAutocloseRefused) },
		},
		{
			name: "re-read fails", store: func(*testing.T) beads.Store { return beads.NewMemStore() },
			wrap: func(inner beads.Store, _ string) beads.Store {
				return &getErrStore{Store: inner, err: errors.New("backing down")}
			}, still: open, wantErr: func(err error) bool { return err != nil },
		},
		{name: "require on a store that cannot fence", store: func(t *testing.T) beads.Store {
			m := stamped(t)
			m.DisableConditionalWrites = true
			return m
		}, still: open, wantErr: func(err error) bool {
			var required *beads.ConditionalWritesRequiredError
			return errors.As(err, &required)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inner := tc.store(t)
			target, err := inner.Create(beads.Bead{Title: "target"})
			if err != nil {
				t.Fatal(err)
			}
			var store beads.Store = &interleavedStore{Store: inner, id: target.ID}
			if tc.interleave {
				store.(*interleavedStore).between = func() { _ = inner.SetMetadata(target.ID, "touched", "yes") }
			}
			if tc.wrap != nil {
				store = tc.wrap(inner, target.ID)
			}
			unconditional := 0
			closed, err := autocloseCloseIfStill(store, target.ID, "test-reason", tc.still, func() error {
				unconditional++
				return inner.Close(target.ID)
			})
			if tc.wantErr == nil && err != nil || tc.wantErr != nil && !tc.wantErr(err) {
				t.Fatalf("autocloseCloseIfStill err = %v", err)
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

// legacyRowStore is a stamped MemStore holding one row at revision 0, the
// default a legacy native row (row_lock backfilled by migration 0054) keeps
// until its first write.
func legacyRowStore(t *testing.T) (*beads.MemStore, string) {
	t.Helper()
	m := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "gc-1", Title: "legacy", Status: "open", Type: "task"}}, nil)
	stampedMemStore(t, m)
	return m, "gc-1"
}

// Kills the revision-0 blind close: a legacy row re-read at its real revision
// 0 is closed by a CAS at 0, so a write landing between the re-read and the
// close refuses it rather than being closed over.
func TestAutocloseCloseIfStillFencesALegacyRow(t *testing.T) {
	open := func(b beads.Bead) bool { return b.Status == "open" }
	for _, interleave := range []bool{false, true} {
		inner, id := legacyRowStore(t)
		store := &interleavedStore{Store: inner, id: id}
		if interleave {
			store.between = func() { _ = inner.SetMetadata(id, "touched", "yes") }
		}
		unconditional := 0
		closed, err := autocloseCloseIfStill(store, id, "legacy-reason", open, func() error {
			unconditional++
			return inner.Close(id)
		})
		got, _ := inner.Get(id)
		if unconditional != 0 {
			t.Fatalf("interleave=%v: a legacy row at revision 0 took the unconditional close", interleave)
		}
		if interleave {
			if !errors.Is(err, errAutocloseRefused) || closed || got.Status == "closed" {
				t.Fatalf("interleave: (closed %v, err %v, status %q), want the CAS at 0 refused and the row open", closed, err, got.Status)
			}
			continue
		}
		if err != nil || !closed || got.Status != "closed" || got.Metadata["close_reason"] != "legacy-reason" {
			t.Fatalf("(closed %v, err %v, row %+v), want the legacy row closed by the CAS at 0 with its reason", closed, err, got)
		}
	}
}

func TestAutocloseRunNote(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		wantFinished bool
		wantRefused  bool
	}{
		{"nil", nil, true, false},
		{"gone is an answer", fmt.Errorf("get: %w", beads.ErrNotFound), true, false},
		{"require refusal is configuration", &beads.ConditionalWritesRequiredError{StoreKind: "BdStore"}, true, false},
		{"refused CAS", errAutocloseRefused, false, true},
		{"read failure", errors.New("backing down"), false, false},
	} {
		var run autocloseRun
		run.note(tc.err)
		if run.finished() != tc.wantFinished || run.refused != tc.wantRefused {
			t.Errorf("%s: run = %+v, want finished %v refused %v", tc.name, run, tc.wantFinished, tc.wantRefused)
		}
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
	doConvoyAutocloseWith(cached, convoyAutocloseHomes{}, rec, member.ID, &stdout, &stdout)

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

// TestConvoyAutocloseRechecksTheConvoyIsStillOpen: the cache still holds the
// convoy open, but another writer closed it. The live re-check sees the
// terminal status, so autoclose neither closes it again nor announces it.
func TestConvoyAutocloseRechecksTheConvoyIsStillOpen(t *testing.T) {
	backing := beads.NewMemStore()
	convoy, _ := backing.Create(beads.Bead{Title: "batch", Type: "convoy"})
	member, _ := backing.Create(beads.Bead{Title: "task", ParentID: convoy.ID})
	cached := beads.NewCachingStoreForTest(backing, nil)
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := backing.Close(convoy.ID); err != nil {
		t.Fatal(err)
	}
	if err := cached.Close(member.ID); err != nil {
		t.Fatal(err)
	}
	rec := events.NewFake()
	var stdout bytes.Buffer
	doConvoyAutocloseWith(cached, convoyAutocloseHomes{}, rec, member.ID, &stdout, &stdout)
	for _, evt := range rec.Events {
		if evt.Type == events.ConvoyClosed {
			t.Fatalf("recorded %s for a convoy another writer already closed", evt.Type)
		}
	}
}

// flakyLiveStore answers the first live Get of id closed and fails every
// later one: the parent read fine, then the store went down before the
// per-attachment re-check.
type flakyLiveStore struct {
	*beads.MemStore
	id    string
	calls int
}

func (s *flakyLiveStore) Get(id string) (beads.Bead, error) {
	if id == s.id {
		s.calls++
		if s.calls > 1 {
			return beads.Bead{}, errors.New("backing down")
		}
	}
	return s.MemStore.Get(id)
}

// TestWispAutocloseRecheckFailsClosedOnAReadError: a re-check that cannot
// read the parent is a refusal, and the run reports the failure so the
// trigger is retried.
func TestWispAutocloseRecheckFailsClosedOnAReadError(t *testing.T) {
	mem := beads.NewMemStore()
	parent, _ := mem.Create(beads.Bead{Title: "work item"})
	wisp, _ := mem.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: parent.ID})
	if err := mem.Close(parent.ID); err != nil {
		t.Fatal(err)
	}
	store := &flakyLiveStore{MemStore: mem, id: parent.ID}

	var stdout bytes.Buffer
	run := doWispAutocloseWith(store, parent.ID, &stdout, beads.GraphStore{Store: store})

	if got := statusOf(t, mem, wisp.ID); got != "open" {
		t.Fatalf("wisp %s after the parent re-check failed", got)
	}
	if run.finished() {
		t.Fatal("the run hid the failed re-check")
	}
}

// staleHandlesStore serves cached reads from one store and live reads from
// another: a cache that lags the backing.
type staleHandlesStore struct {
	beads.Store
	live beads.Store
}

func (s *staleHandlesStore) Handles() beads.StoreHandles {
	h := beads.HandlesFor(s.Store)
	h.Live = beads.HandlesFor(s.live).Live
	return h
}

// closedViewStore reads row id as closed whatever the backing holds.
type closedViewStore struct {
	beads.Store
	id string
}

func (s closedViewStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if id == s.id {
		b.Status = "closed"
	}
	return b, err
}

// TestWispAutocloseRecheckReadsTheParentLive: the cache still says the
// parent is closed, but it was reopened after the guard read. The re-check
// reads live, sees it open, and leaves the attachment alone.
func TestWispAutocloseRecheckReadsTheParentLive(t *testing.T) {
	mem := beads.NewMemStore()
	parent, _ := mem.Create(beads.Bead{Title: "work item"})
	wisp, _ := mem.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: parent.ID})
	store := &staleHandlesStore{
		Store: closedViewStore{Store: mem, id: parent.ID},
		live:  &reopeningStore{MemStore: mem, id: parent.ID},
	}

	var stdout bytes.Buffer
	doWispAutocloseWith(store, parent.ID, &stdout, beads.GraphStore{Store: store})

	if got := statusOf(t, mem, wisp.ID); got != "open" {
		t.Fatalf("wisp %s; the live parent was reopened before the close", got)
	}
}
