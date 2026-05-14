package main

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

type listCountingStore struct {
	beads.Store
	calls atomic.Int64
}

func (s *listCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.calls.Add(1)
	return s.Store.List(q)
}

func newSessionCacheTestStore(t *testing.T) (*listCountingStore, beads.Bead) {
	t.Helper()
	mem := beads.NewMemStore()
	openBead, err := mem.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "open-session-1",
			"alias":        "rig/open",
		},
	})
	if err != nil {
		t.Fatalf("create open bead: %v", err)
	}
	closedBead, err := mem.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "closed-session-1",
			"alias":        "rig/closed",
		},
	})
	if err != nil {
		t.Fatalf("create closed bead: %v", err)
	}
	if err := mem.Close(closedBead.ID); err != nil {
		t.Fatalf("close closed bead: %v", err)
	}
	return &listCountingStore{Store: mem}, openBead
}

func TestSessionListCacheCoalescesCanonicalQueries(t *testing.T) {
	store, openBead := newSessionCacheTestStore(t)
	cache := wrapSessionListCache(store)

	// Two redundant open-only queries (the resolveSessionID shape).
	for i := 0; i < 2; i++ {
		got, err := cache.List(beads.ListQuery{Label: session.LabelSession})
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if len(got) != 1 || got[0].ID != openBead.ID {
			t.Fatalf("call %d: got %+v, want only open bead %s", i, got, openBead.ID)
		}
	}

	// One include-closed query (the loadSessionBeadSnapshot shape).
	got, err := cache.List(beads.ListQuery{Label: session.LabelSession, IncludeClosed: true})
	if err != nil {
		t.Fatalf("include-closed: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("include-closed: got %d beads, want 2", len(got))
	}

	// One sort-desc query (the Manager.ListFull shape).
	got, err = cache.List(beads.ListQuery{Label: session.LabelSession, Sort: beads.SortCreatedDesc})
	if err != nil {
		t.Fatalf("sort-desc: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("sort-desc: got %d open beads, want 1", len(got))
	}

	if got := store.calls.Load(); got != 1 {
		t.Fatalf("backing List call count = %d, want 1 (queries should coalesce)", got)
	}
}

func TestSessionListCacheBypassesForNonCanonicalQueries(t *testing.T) {
	store, _ := newSessionCacheTestStore(t)
	cache := wrapSessionListCache(store)

	cases := []struct {
		name string
		q    beads.ListQuery
	}{
		{"different label", beads.ListQuery{Label: "other-label"}},
		{"with status filter", beads.ListQuery{Label: session.LabelSession, Status: "open"}},
		{"with metadata", beads.ListQuery{Label: session.LabelSession, Metadata: map[string]string{"foo": "bar"}}},
		{"with type", beads.ListQuery{Label: session.LabelSession, Type: "task"}},
		{"with assignee", beads.ListQuery{Label: session.LabelSession, Assignee: "x"}},
		{"with parent", beads.ListQuery{Label: session.LabelSession, ParentID: "x"}},
		{"live bypass", beads.ListQuery{Label: session.LabelSession, Live: true}},
	}

	before := store.calls.Load()
	for _, tc := range cases {
		if _, err := cache.List(tc.q); err != nil && !errors.Is(err, beads.ErrQueryRequiresScan) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	delta := store.calls.Load() - before
	if delta != int64(len(cases)) {
		t.Fatalf("non-canonical queries hit backing %d times, want %d", delta, len(cases))
	}
}

func TestSessionListCachePropagatesErrors(t *testing.T) {
	mem := beads.NewMemStore()
	failing := &failingListStore{Store: mem, err: errors.New("dolt offline")}
	cache := wrapSessionListCache(failing)

	if _, err := cache.List(beads.ListQuery{Label: session.LabelSession}); err == nil {
		t.Fatalf("expected error, got nil")
	}

	// Second call should not re-hit backing (cached error).
	if _, err := cache.List(beads.ListQuery{Label: session.LabelSession}); err == nil {
		t.Fatalf("expected cached error, got nil")
	}
	if failing.calls != 1 {
		t.Fatalf("backing call count = %d, want 1 (error must be cached)", failing.calls)
	}
}

func TestSessionListCacheNilStore(t *testing.T) {
	if got := wrapSessionListCache(nil); got != nil {
		t.Fatalf("wrapSessionListCache(nil) = %#v, want nil", got)
	}
}

type failingListStore struct {
	beads.Store
	err   error
	calls int
}

func (s *failingListStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	s.calls++
	return nil, s.err
}
