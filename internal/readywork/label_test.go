package readywork

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file pins --label (every label must be on the bead) and --label-any (at
// least one must be) on the federated reader. They are the flags a route_label /
// route_label_any agent's generated queries pass, and `bd ready` has them, so a
// federated `gc ready` without them is a reader that cannot be swapped in for it
// (ga-878prh.1).

func labeledRow(id string, n int, labels ...string) beads.Bead {
	return readyRow(id, 2, time.Date(2026, 10, 1, 0, 0, n, 0, time.UTC), func(b *beads.Bead) { b.Labels = labels })
}

func labelFixture() []beads.Bead {
	return []beads.Bead{
		labeledRow("l-both", 1, "review", "urgent"),
		labeledRow("l-review", 2, "review"),
		labeledRow("l-urgent", 3, "urgent", "extra"),
		labeledRow("l-none", 4),
	}
}

func TestReadLabelRequiresEveryLabelAndLabelAnyRequiresOne(t *testing.T) {
	legs := []Leg{testLeg("city", beads.NewMemStoreFrom(0, labelFixture(), nil))}
	for _, tc := range []struct {
		name  string
		query Query
		want  []string
	}{
		{"no labels keeps everything", Query{}, []string{"l-both", "l-review", "l-urgent", "l-none"}},
		{"one label", Query{Labels: []string{"review"}}, []string{"l-both", "l-review"}},
		{"labels are an AND", Query{Labels: []string{"review", "urgent"}}, []string{"l-both"}},
		{"label-any is an OR", Query{AnyLabels: []string{"urgent", "absent"}}, []string{"l-both", "l-urgent"}},
		{"both combine with AND", Query{Labels: []string{"review"}, AnyLabels: []string{"urgent", "absent"}}, []string{"l-both"}},
		{"unmatched label serves nothing", Query{Labels: []string{"absent"}}, []string{}},
		{"unmatched label-any serves nothing", Query{AnyLabels: []string{"absent"}}, []string{}},
		{"surrounding whitespace is trimmed", Query{Labels: []string{" review "}}, []string{"l-both", "l-review"}},
		{"a comma separates labels as bd does (AND)", Query{Labels: []string{"review,urgent"}}, []string{"l-both"}},
		{"a comma separates labels as bd does (OR)", Query{AnyLabels: []string{"absent,extra"}}, []string{"l-urgent"}},
		{"blank entries are dropped as bd drops them", Query{Labels: []string{"", " "}, AnyLabels: []string{","}}, []string{"l-both", "l-review", "l-urgent", "l-none"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := readIDs(t, legs, tc.query)
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Fatalf("Read(%+v) = %v, want %v", tc.query, got, want)
			}
		})
	}
}

// The label predicates travel with the controller's census predicate: Matcher
// is what the demand count holds a row to, so it must agree with Read.
func TestMatcherAppliesTheLabelPredicates(t *testing.T) {
	q := Query{Labels: []string{"review"}, AnyLabels: []string{"urgent", "vip"}}
	match, err := q.Matcher()
	if err != nil {
		t.Fatalf("Matcher: %v", err)
	}
	var got []string
	for _, b := range labelFixture() {
		if match(b) {
			got = append(got, b.ID)
		}
	}
	if !slices.Equal(got, []string{"l-both"}) {
		t.Fatalf("Matcher kept %v, want only l-both", got)
	}
}

// A label predicate is applied to the merged set, never pushed to a leg: bd
// normalizes labels, and a store that compared them differently could drop a
// row the Go filter keeps. The bounded read must still return the city's top N
// when most of a leg's rows fail the label predicate.
func TestReadNeverPushesLabelsAndStaysBoundedCorrect(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	var rows []beads.Bead
	for i := range 90 {
		var labels []string
		if i%9 == 0 {
			labels = []string{"review"}
		}
		rows = append(rows, readyRow(fmt.Sprintf("n-%02d", i), 2, base.Add(time.Duration(i)*time.Minute), func(b *beads.Bead) { b.Labels = labels }))
	}
	q := Query{Labels: []string{"review"}, Limit: 4}

	store := newNarrowingStore(rows...)
	got := readIDs(t, []Leg{testLeg("city", store)}, q)
	want := readIDs(t, []Leg{testLeg("city", beads.NewMemStoreFrom(0, rows, nil))}, q)
	if !slices.Equal(got, want) || len(got) != 4 {
		t.Fatalf("bounded labeled read = %v, want the unbounded top 4 %v", got, want)
	}
	for _, asked := range store.readyQueries() {
		if len(asked.MetadataEquals) != 0 || asked.Unassigned || asked.Assignee != "" {
			t.Fatalf("a label predicate narrowed the leg's read: %+v", asked)
		}
	}
}
