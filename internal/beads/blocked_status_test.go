package beads

import (
	"testing"
	"time"
)

// TestBdBlockedStatusSurvivesTheReadModel pins sc-bpn7w: bd's blocked status
// must reach a reader instead of decoding as "open". Before the fix the
// supervisor bead API reported all 48 of a city's blocked beads as open, so the
// dashboard counted finished-but-blocked work as needing a human.
//
// review and testing stay collapsed on purpose: no Gas City consumer reads
// them, so widening them would add states nothing acts on.
func TestBdBlockedStatusSurvivesTheReadModel(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		bd   string
		want string
	}{
		{"blocked", "blocked"},
		{"open", "open"},
		{"in_progress", "in_progress"},
		{"closed", "closed"},
		{"review", "open"},
		{"testing", "open"},
		{"deferred", "open"},
	} {
		if got := mapBdStatus(tt.bd); got != tt.want {
			t.Errorf("mapBdStatus(%q) = %q, want %q", tt.bd, got, tt.want)
		}
	}

	// A blocked bead is not indefinitely deferred: only bd's deferred status
	// with no defer_until sets that hold.
	status, hold := normalizedBdReadState("blocked", nil)
	if status != "blocked" || hold {
		t.Errorf("normalizedBdReadState(blocked) = (%q, %v), want (\"blocked\", false)", status, hold)
	}
}

// TestBlockedBeadStaysInTheOpenSet pins the compatibility half of sc-bpn7w: the
// widening is reporting-only, so a blocked bead must still satisfy every
// demand, claim, dispatch, and mail guard that means "still open", and must
// still appear in a Status:"open" list. Exact equality in ListQuery.Matches
// would have dropped it from every such list.
func TestBlockedBeadStaysInTheOpenSet(t *testing.T) {
	t.Parallel()

	if !IsOpenStatus("blocked") || !IsOpenStatus("open") {
		t.Fatal("IsOpenStatus must accept both open and blocked")
	}
	if IsOpenStatus("in_progress") || IsOpenStatus("closed") {
		t.Fatal("IsOpenStatus must reject in_progress and closed")
	}

	rows := []Bead{
		{ID: "b-open", Status: "open", Type: "task"},
		{ID: "b-blocked", Status: "blocked", Type: "task"},
		{ID: "b-active", Status: "in_progress", Type: "task"},
		{ID: "b-closed", Status: "closed", Type: "task"},
	}
	got := map[string]bool{}
	for _, bead := range ApplyListQuery(rows, ListQuery{Status: "open"}) {
		got[bead.ID] = true
	}
	if !got["b-open"] || !got["b-blocked"] || len(got) != 2 {
		t.Fatalf("Status:\"open\" list = %v, want b-open and b-blocked", got)
	}

	// An explicit blocked filter still matches exactly.
	exact := ApplyListQuery(rows, ListQuery{Status: "blocked"})
	if len(exact) != 1 || exact[0].ID != "b-blocked" {
		t.Fatalf("Status:\"blocked\" list = %+v, want only b-blocked", exact)
	}

	// Ready is deliberately narrower: bd's ready semantics exclude blocked
	// (ga-3mv5d3), and nativeDoltOpenReadyStatuses never queries for it.
	if IsReadyCandidate(Bead{Status: "blocked", Type: "task"}, time.Now()) {
		t.Fatal("a blocked bead must not be a Ready candidate")
	}
	if !IsReadyCandidate(Bead{Status: "open", Type: "task"}, time.Now()) {
		t.Fatal("an open bead must remain a Ready candidate")
	}
}
