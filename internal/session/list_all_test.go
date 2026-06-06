package session

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestListAllSessionBeads exercises the type+label union helper across the
// matrix of bead shapes the production fleet has been observed to carry.
// The motivating bug: canonical configured_named_session beads can lose
// their gc:session label after a crash, but retain issue_type=session.
// A label-only query strands them invisible. The union here makes both
// type-only and label-only beads visible while still filtering out beads
// that satisfy neither predicate.
func TestListAllSessionBeads(t *testing.T) {
	store := beads.NewMemStore()

	// Bead with both Type and Label — the canonical, healthy shape.
	healthy, err := store.Create(beads.Bead{
		Title:    "healthy",
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"session_name": "healthy"},
	})
	if err != nil {
		t.Fatalf("create healthy session bead: %v", err)
	}

	// Bead with Type only and NO gc:session label — the production
	// failure mode this whole change exists to fix.
	typeOnly, err := store.Create(beads.Bead{
		Title:    "type-only",
		Type:     BeadType,
		Metadata: map[string]string{"session_name": "type-only"},
	})
	if err != nil {
		t.Fatalf("create type-only session bead: %v", err)
	}

	// Bead with Label only and empty Type — the legacy repairable shape
	// IsSessionBeadOrRepairable was originally written for.
	labelOnly, err := store.Create(beads.Bead{
		Title:    "label-only",
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"session_name": "label-only"},
	})
	if err != nil {
		t.Fatalf("create label-only session bead: %v", err)
	}
	// MemStore.Create defaults empty Type to "task"; rewrite to empty so
	// the repairable shape is preserved.
	empty := ""
	if err := store.Update(labelOnly.ID, beads.UpdateOpts{Type: &empty}); err != nil {
		t.Fatalf("clear type on label-only bead: %v", err)
	}

	// Bead with neither — must not surface.
	if _, err := store.Create(beads.Bead{
		Title: "unrelated",
		Type:  "task",
	}); err != nil {
		t.Fatalf("create unrelated bead: %v", err)
	}

	got, err := ListAllSessionBeads(store, beads.ListQuery{})
	if err != nil {
		t.Fatalf("ListAllSessionBeads: %v", err)
	}
	gotIDs := make(map[string]bool, len(got))
	for _, b := range got {
		if gotIDs[b.ID] {
			t.Errorf("duplicate bead ID %q in result", b.ID)
		}
		gotIDs[b.ID] = true
	}
	if !gotIDs[healthy.ID] {
		t.Errorf("expected healthy bead %s in result", healthy.ID)
	}
	if !gotIDs[typeOnly.ID] {
		t.Errorf("expected type-only bead %s in result (production failure mode)", typeOnly.ID)
	}
	if !gotIDs[labelOnly.ID] {
		t.Errorf("expected label-only repairable bead %s in result", labelOnly.ID)
	}
	if len(got) != 3 {
		t.Errorf("expected exactly 3 session beads, got %d", len(got))
	}
}

// TestListAllSessionBeads_IncludeClosed verifies the caller's
// IncludeClosed filter is preserved across both legs of the union.
func TestListAllSessionBeads_IncludeClosed(t *testing.T) {
	store := beads.NewMemStore()

	open, err := store.Create(beads.Bead{
		Title:  "open",
		Type:   BeadType,
		Labels: []string{LabelSession},
	})
	if err != nil {
		t.Fatalf("create open: %v", err)
	}
	closed, err := store.Create(beads.Bead{
		Title:  "closed",
		Type:   BeadType,
		Labels: []string{LabelSession},
	})
	if err != nil {
		t.Fatalf("create closed: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Default (IncludeClosed=false): only the open bead.
	got, err := ListAllSessionBeads(store, beads.ListQuery{})
	if err != nil {
		t.Fatalf("ListAllSessionBeads open-only: %v", err)
	}
	if len(got) != 1 || got[0].ID != open.ID {
		t.Errorf("open-only query: got %d beads, want exactly open %s; got=%+v", len(got), open.ID, got)
	}

	// IncludeClosed=true: both.
	got, err = ListAllSessionBeads(store, beads.ListQuery{IncludeClosed: true})
	if err != nil {
		t.Fatalf("ListAllSessionBeads include-closed: %v", err)
	}
	ids := map[string]bool{}
	for _, b := range got {
		ids[b.ID] = true
	}
	if !ids[open.ID] || !ids[closed.ID] {
		t.Errorf("include-closed: missing open=%v closed=%v in %+v", ids[open.ID], ids[closed.ID], got)
	}
}

// TestListAllSessionBeads_DedupedAcrossLegs verifies a bead that matches
// both queries (the healthy Type+Label shape) is returned exactly once.
func TestListAllSessionBeads_DedupedAcrossLegs(t *testing.T) {
	store := beads.NewMemStore()
	healthy, err := store.Create(beads.Bead{
		Title:  "healthy",
		Type:   BeadType,
		Labels: []string{LabelSession},
	})
	if err != nil {
		t.Fatalf("create healthy: %v", err)
	}
	got, err := ListAllSessionBeads(store, beads.ListQuery{})
	if err != nil {
		t.Fatalf("ListAllSessionBeads: %v", err)
	}
	count := 0
	for _, b := range got {
		if b.ID == healthy.ID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("healthy bead %s returned %d times, want 1", healthy.ID, count)
	}
}

// TestListAllSessionBeads_SortRespected verifies the caller's Sort field
// is propagated to both legs of the union.
func TestListAllSessionBeads_SortRespected(t *testing.T) {
	store := beads.NewMemStore()

	// Create three Type+Label beads with distinct creation times so
	// sort order is deterministic. MemStore.Create stamps CreatedAt at
	// time.Now(), so we space them with tiny sleeps to keep the test
	// fast but the ordering reliable.
	var ids []string
	for i := 0; i < 3; i++ {
		b, err := store.Create(beads.Bead{
			Title:  "s",
			Type:   BeadType,
			Labels: []string{LabelSession},
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, b.ID)
		time.Sleep(2 * time.Millisecond)
	}

	asc, err := ListAllSessionBeads(store, beads.ListQuery{Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatalf("asc: %v", err)
	}
	if len(asc) != 3 {
		t.Fatalf("asc len = %d, want 3", len(asc))
	}
	for i, want := range ids {
		if asc[i].ID != want {
			t.Errorf("asc[%d] = %s, want %s", i, asc[i].ID, want)
		}
	}

	desc, err := ListAllSessionBeads(store, beads.ListQuery{Sort: beads.SortCreatedDesc})
	if err != nil {
		t.Fatalf("desc: %v", err)
	}
	if len(desc) != 3 {
		t.Fatalf("desc len = %d, want 3", len(desc))
	}
	for i, want := range []string{ids[2], ids[1], ids[0]} {
		if desc[i].ID != want {
			t.Errorf("desc[%d] = %s, want %s", i, desc[i].ID, want)
		}
	}
}

// TestListAllSessionBeads_GlobalSortAcrossLegs guards against the
// regression where each leg of the union was sorted independently but
// the merged result was not. Creating beads with alternating shapes
// (type-only, label-only, type-only) means a leg-local sort would
// emit type-only rows before label-only rows regardless of CreatedAt;
// only a global sort yields the expected creation order.
func TestListAllSessionBeads_GlobalSortAcrossLegs(t *testing.T) {
	store := beads.NewMemStore()
	empty := ""

	// First bead: type-only.
	b0, err := store.Create(beads.Bead{Title: "t0", Type: BeadType})
	if err != nil {
		t.Fatalf("create b0: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	// Second bead: label-only (Type cleared after create).
	b1, err := store.Create(beads.Bead{Title: "t1", Labels: []string{LabelSession}})
	if err != nil {
		t.Fatalf("create b1: %v", err)
	}
	if err := store.Update(b1.ID, beads.UpdateOpts{Type: &empty}); err != nil {
		t.Fatalf("clear type on b1: %v", err)
	}
	time.Sleep(2 * time.Millisecond)

	// Third bead: type-only.
	b2, err := store.Create(beads.Bead{Title: "t2", Type: BeadType})
	if err != nil {
		t.Fatalf("create b2: %v", err)
	}

	asc, err := ListAllSessionBeads(store, beads.ListQuery{Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatalf("asc: %v", err)
	}
	wantAsc := []string{b0.ID, b1.ID, b2.ID}
	if len(asc) != len(wantAsc) {
		t.Fatalf("asc len = %d, want %d (%v)", len(asc), len(wantAsc), asc)
	}
	for i, want := range wantAsc {
		if asc[i].ID != want {
			t.Errorf("asc[%d] = %s, want %s (full order: %+v)", i, asc[i].ID, want, asc)
		}
	}

	desc, err := ListAllSessionBeads(store, beads.ListQuery{Sort: beads.SortCreatedDesc})
	if err != nil {
		t.Fatalf("desc: %v", err)
	}
	wantDesc := []string{b2.ID, b1.ID, b0.ID}
	if len(desc) != len(wantDesc) {
		t.Fatalf("desc len = %d, want %d (%v)", len(desc), len(wantDesc), desc)
	}
	for i, want := range wantDesc {
		if desc[i].ID != want {
			t.Errorf("desc[%d] = %s, want %s (full order: %+v)", i, desc[i].ID, want, desc)
		}
	}
}

// TestListAllSessionBeads_LimitAppliedOnce verifies Limit is applied
// to the merged result, not independently to each underlying query
// (which could return up to 2× the requested rows).
func TestListAllSessionBeads_LimitAppliedOnce(t *testing.T) {
	store := beads.NewMemStore()
	empty := ""

	var ids []string
	for i := 0; i < 4; i++ {
		var b beads.Bead
		var err error
		if i%2 == 0 {
			b, err = store.Create(beads.Bead{Title: "type-only", Type: BeadType})
		} else {
			b, err = store.Create(beads.Bead{Title: "label-only", Labels: []string{LabelSession}})
			if err == nil {
				err = store.Update(b.ID, beads.UpdateOpts{Type: &empty})
			}
		}
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ids = append(ids, b.ID)
		time.Sleep(2 * time.Millisecond)
	}

	got, err := ListAllSessionBeads(store, beads.ListQuery{Limit: 2, Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatalf("ListAllSessionBeads: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (Limit must be applied to the union, not per leg)", len(got))
	}
	if got[0].ID != ids[0] || got[1].ID != ids[1] {
		t.Errorf("got IDs [%s, %s], want [%s, %s] (oldest two)", got[0].ID, got[1].ID, ids[0], ids[1])
	}
}

// TestListAllSessionBeads_NilStore returns empty without error.
func TestListAllSessionBeads_NilStore(t *testing.T) {
	got, err := ListAllSessionBeads(nil, beads.ListQuery{})
	if err != nil {
		t.Errorf("nil store should not error, got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("nil store should return empty, got %d beads", len(got))
	}
}
