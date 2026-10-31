package beads

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// workflowRootHoldFixture builds the reported shape on a MemStore: a workflow root
// held with an undated defer (bd's status-based deferral), a step and a ralph
// under it, and a step under a live root, all behind one closed blocker.
func workflowRootHoldFixture(t *testing.T) (*MemStore, Bead) {
	t.Helper()
	m := NewMemStore()
	held, err := m.Create(Bead{Title: "held root", Type: "task", Status: "open", IndefinitelyDeferred: true})
	if err != nil {
		t.Fatal(err)
	}
	live, err := m.Create(Bead{Title: "live root", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := m.Create(Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []Bead{
		{Title: "held step", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: held.ID}},
		{Title: "held ralph", Type: "task", Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: held.ID,
			beadmeta.KindMetadataKey:       beadmeta.KindRalph,
		}},
		{Title: "live step", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: live.ID}},
	} {
		created, err := m.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		if err := m.DepAdd(created.ID, blocker.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	return m, held
}

func readyTitles(t *testing.T, label string, got []Bead, err error) []string {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	titles := make([]string, 0, len(got))
	for _, b := range got {
		titles = append(titles, b.Title)
	}
	slices.Sort(titles)
	return titles
}

// TestReadyHoldsWorkflowMembersUnderUndatedDeferredRoot is the reported case:
// a run's root was deferred with no date, and when the run before it closed,
// its first step was offered to the pool anyway. The
// MemStore and every read of a CachingStore over it hold the root's members,
// its control included, and release them when the root is undeferred.
func TestReadyHoldsWorkflowMembersUnderUndatedDeferredRoot(t *testing.T) {
	m, held := workflowRootHoldFixture(t)
	wantHeld := []string{"live root", "live step"}

	got, err := m.Ready()
	if titles := readyTitles(t, "MemStore.Ready", got, err); !slices.Equal(titles, wantHeld) {
		t.Fatalf("MemStore.Ready = %v, want %v", titles, wantHeld)
	}

	cache := NewCachingStoreForTest(m, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	got, err = cache.Ready()
	if titles := readyTitles(t, "CachingStore.Ready", got, err); !slices.Equal(titles, wantHeld) {
		t.Fatalf("CachingStore.Ready = %v, want %v", titles, wantHeld)
	}
	cached, ok := cache.CachedReady()
	if !ok {
		t.Fatal("CachedReady reported the cache unavailable")
	}
	if titles := readyTitles(t, "CachedReady", cached, nil); !slices.Equal(titles, wantHeld) {
		t.Fatalf("CachedReady = %v, want %v", titles, wantHeld)
	}
	got, err = cache.ReadyContext(context.Background())
	if titles := readyTitles(t, "ReadyContext", got, err); !slices.Equal(titles, wantHeld) {
		t.Fatalf("ReadyContext = %v, want %v", titles, wantHeld)
	}

	// Undeferring the root releases its members.
	open := "open"
	if err := cache.Update(held.ID, UpdateOpts{Status: &open}); err != nil {
		t.Fatalf("Update root open: %v", err)
	}
	wantReleased := []string{"held ralph", "held root", "held step", "live root", "live step"}
	got, err = cache.Ready()
	if titles := readyTitles(t, "CachingStore.Ready after undefer", got, err); !slices.Equal(titles, wantReleased) {
		t.Fatalf("CachingStore.Ready after undefer = %v, want %v", titles, wantReleased)
	}
	got, err = m.Ready()
	if titles := readyTitles(t, "MemStore.Ready after undefer", got, err); !slices.Equal(titles, wantReleased) {
		t.Fatalf("MemStore.Ready after undefer = %v, want %v", titles, wantReleased)
	}
}

func TestFilterReadyByWorkflowRoot(t *testing.T) {
	now := time.Now().UTC()
	candidates := []Bead{
		{ID: "a", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "held"}},
		{ID: "b", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "live"}},
		{ID: "c", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "missing"}},
		{ID: "held", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "held"}},
		{ID: "d"},
	}
	var asked []string
	got, err := FilterReadyByWorkflowRoot(candidates, now, func(ids []string) ([]Bead, error) {
		asked = append(asked, ids...)
		return []Bead{{ID: "held", Status: "open", IndefinitelyDeferred: true}, {ID: "live", Status: "open"}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	// A root that names itself is not its own root; a missing root holds nothing.
	if want := []string{"b", "c", "held", "d"}; !slices.Equal(ids, want) {
		t.Fatalf("FilterReadyByWorkflowRoot = %v, want %v", ids, want)
	}
	if want := []string{"held", "live", "missing"}; !slices.Equal(asked, want) {
		t.Fatalf("roots asked = %v, want %v (each once, the self-root excluded)", asked, want)
	}

	calls := 0
	if _, err := FilterReadyByWorkflowRoot([]Bead{{ID: "x"}}, now, func([]string) ([]Bead, error) {
		calls++
		return nil, nil
	}); err != nil || calls != 0 {
		t.Fatalf("no candidate names a root: calls=%d err=%v, want no lookup", calls, err)
	}
}
