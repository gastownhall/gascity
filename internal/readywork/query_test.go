package readywork

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func intRef(v int) *int { return &v }

func readyRow(id string, priority int, created time.Time, mutate ...func(*beads.Bead)) beads.Bead {
	b := beads.Bead{ID: id, Title: id, Status: "open", Type: "task", Priority: intRef(priority), CreatedAt: created}
	for _, m := range mutate {
		m(&b)
	}
	return b
}

func readIDs(t *testing.T, legs []Leg, q Query) []string {
	t.Helper()
	rows, err := Read(legs, q)
	if err != nil {
		t.Fatalf("Read(%+v): %v", q, err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestReadAssigneeMatchesTheStoredAssigneeExactly pins --assignee and
// --unassigned to `bd ready`'s comparison: the stored assignee exactly. A
// stored assignee with surrounding whitespace is a different identity, and a
// whitespace-only one is an assignee, not the absence of one. Exactness is also
// what lets a store narrow by these predicates and still return every row they
// keep (the advisory pushdown of beads.ReadyQuery).
func TestReadAssigneeMatchesTheStoredAssigneeExactly(t *testing.T) {
	now := time.Now().UTC()
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		readyRow("a-1", 1, now, func(b *beads.Bead) { b.Assignee = "worker" }),
		readyRow("a-2", 2, now.Add(time.Second), func(b *beads.Bead) { b.Assignee = " worker" }),
		readyRow("a-3", 3, now.Add(2*time.Second), func(b *beads.Bead) { b.Assignee = "  " }),
		readyRow("a-4", 2, now.Add(3*time.Second)),
	}, nil)
	legs := []Leg{testLeg("city", store)}
	if got := readIDs(t, legs, Query{Assignee: " worker "}); !slices.Equal(got, []string{"a-1"}) {
		t.Fatalf("--assignee worker = %v, want only the exact match a-1", got)
	}
	if got := readIDs(t, legs, Query{Unassigned: true}); !slices.Equal(got, []string{"a-4"}) {
		t.Fatalf("--unassigned = %v, want only the empty assignee a-4 (a whitespace assignee is an assignee, as bd matches it)", got)
	}
}

// TestReadRefusesANegativeLimitBeforeReadingAnyStore pins the limit's floor at
// the reader, where the CLI and the API meet: zero is unlimited and a negative
// limit is refused, as the API refuses it, so the two doors cannot disagree on
// it. The refusal costs no store read.
func TestReadRefusesANegativeLimitBeforeReadingAnyStore(t *testing.T) {
	store := newNarrowingStore(readyRow("a-1", 1, time.Now().UTC()))
	_, err := Read([]Leg{testLeg("city", store)}, Query{Limit: -1})
	if err == nil || !strings.Contains(err.Error(), "--limit -1") {
		t.Fatalf("Read(--limit -1) error = %v, want a refusal naming the limit", err)
	}
	if got := len(store.readyQueries()) + len(store.orderedCalls()); got != 0 {
		t.Fatalf("a refused query read the store %d times, want none", got)
	}
	if got := readIDs(t, []Leg{testLeg("city", store)}, Query{Limit: 0}); !slices.Equal(got, []string{"a-1"}) {
		t.Fatalf("--limit 0 = %v, want every row (0 is unlimited)", got)
	}
}
