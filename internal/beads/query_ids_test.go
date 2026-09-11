package beads

import "testing"

// TestListQueryIDsSelectsExactlyTheNamedBeads pins the IN-list selector a lane
// that already knows which beads it needs uses instead of one Get per bead.
//
// The negative is "the beads nobody named are absent". Its control is that the
// named ones are all present — a filter that matched nothing would satisfy the
// negative and say nothing about the selector.
func TestListQueryIDsSelectsExactlyTheNamedBeads(t *testing.T) {
	store := NewMemStore()
	var ids []string
	for range 5 {
		b, err := store.Create(Bead{Title: "work"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		ids = append(ids, b.ID)
	}
	want := []string{ids[1], ids[3]}

	got, err := store.List(ListQuery{IDs: want})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("List(IDs=%v) returned %d bead(s), want %d", want, len(got), len(want))
	}
	found := map[string]bool{}
	for _, b := range got {
		found[b.ID] = true
	}
	for _, id := range want {
		if !found[id] {
			t.Fatalf("List(IDs=%v) missing %s; the selector is matching nothing rather than selecting", want, id)
		}
	}
}

// TestListQueryIDsIsAFilterNotAScan pins that an IDs query needs no AllowScan:
// naming the rows IS the filter, and requiring the scan opt-in would push the
// batched form back into the population-read vocabulary it exists to leave.
func TestListQueryIDsIsAFilterNotAScan(t *testing.T) {
	if !(ListQuery{IDs: []string{"ga-1"}}).HasFilter() {
		t.Fatal("ListQuery{IDs} reports no filter, so it would require AllowScan")
	}
	if (ListQuery{}).HasFilter() {
		t.Fatal("the empty query reports a filter; HasFilter is not discriminating")
	}
}

// TestListQueryIDsComposesWithStatus pins that IDs narrows conjunctively like
// every other selector — the route-repair re-verify asks "these ids, still
// open", and an IDs clause that overrode Status would hand it closed rows.
func TestListQueryIDsComposesWithStatus(t *testing.T) {
	store := NewMemStore()
	open, err := store.Create(Bead{Title: "open"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	closed, err := store.Create(Bead{Title: "closed"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("close: %v", err)
	}

	got, err := store.List(ListQuery{IDs: []string{open.ID, closed.ID}, Status: "open"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != open.ID {
		t.Fatalf("List(IDs+open) = %v, want only %s", beadIDsOf(got), open.ID)
	}
	// Control: without the status clause both ids come back, so the assertion
	// above measured the composition and not an empty id set.
	both, err := store.List(ListQuery{IDs: []string{open.ID, closed.ID}, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(both) != 2 {
		t.Fatalf("List(IDs, IncludeClosed) = %v, want both ids", beadIDsOf(both))
	}
}

func beadIDsOf(items []Bead) []string {
	out := make([]string, 0, len(items))
	for _, b := range items {
		out = append(out, b.ID)
	}
	return out
}
