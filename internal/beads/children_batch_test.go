package beads_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdStoreChildrenOfAnyReadsAFrontierInTwoCalls(t *testing.T) {
	var calls [][]string
	runner := func(_, _ string, args ...string) ([]byte, error) {
		calls = append(calls, slices.Clone(args))
		switch {
		case args[0] == "dep" && args[1] == "list":
			// bd prints the dependents' rows; ephemeral rows are wisps, which
			// `bd list --parent` (Children) never returns.
			return []byte(`[
  {"id":"gc-c1","status":"closed","dependency_type":"parent-child"},
  {"id":"gc-w1","status":"open","ephemeral":true,"dependency_type":"parent-child"},
  {"id":"gc-c2","status":"open","dependency_type":"parent-child"}
]`), nil
		case args[0] == "show" && len(args) == 4:
			// The exact batch answers gc-c1 only; gc-c2 is resolved by Get.
			return []byte(`[{"id":"gc-c1","status":"closed","issue_type":"task","parent":"gc-p1"}]`), nil
		case args[0] == "show" && len(args) == 3 && args[2] == "gc-c2":
			return []byte(`[{"id":"gc-c2","status":"open","issue_type":"task","parent":"gc-p2"}]`), nil
		}
		return nil, fmt.Errorf("unexpected bd %v", args)
	}
	s := beads.NewBdStore("/city", runner)
	children, err := s.ChildrenOfAny([]string{"gc-p1", "gc-p2"})
	if err != nil {
		t.Fatalf("ChildrenOfAny: %v", err)
	}
	var got []string
	for _, c := range children {
		got = append(got, c.ID+"<"+c.ParentID)
	}
	if want := []string{"gc-c1<gc-p1", "gc-c2<gc-p2"}; !slices.Equal(got, want) {
		t.Fatalf("children = %v, want %v (full rows, ephemeral dropped)", got, want)
	}
	if want := "dep list gc-p1 gc-p2 --direction=up --type=parent-child --json"; strings.Join(calls[0], " ") != want {
		t.Fatalf("first call = %q, want %q", strings.Join(calls[0], " "), want)
	}
	if want := "show --json gc-c1 gc-c2"; strings.Join(calls[1], " ") != want {
		t.Fatalf("second call = %q, want %q", strings.Join(calls[1], " "), want)
	}
	if len(calls) != 3 {
		t.Fatalf("calls = %v, want dep list, batched show, and one show for the unresolved child", calls)
	}
}

func TestBdStoreChildrenOfAnyWithNoChildrenIsOneCall(t *testing.T) {
	var calls int
	s := beads.NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
		calls++
		return []byte(`[]`), nil
	})
	children, err := s.ChildrenOfAny([]string{"gc-p1"})
	if err != nil || len(children) != 0 || calls != 1 {
		t.Fatalf("ChildrenOfAny = (%v, %v) after %d calls, want no children after 1", children, err, calls)
	}
}

func TestChildrenOfAnyWithoutTheCapabilityReadsEachParent(t *testing.T) {
	mem := beads.NewMemStore()
	p1, _ := mem.Create(beads.Bead{Title: "p1"})
	p2, _ := mem.Create(beads.Bead{Title: "p2"})
	c1, _ := mem.Create(beads.Bead{Title: "c1", ParentID: p1.ID})
	c2, _ := mem.Create(beads.Bead{Title: "c2", ParentID: p2.ID})
	if err := mem.Close(c2.ID); err != nil {
		t.Fatal(err)
	}
	children, err := beads.ChildrenOfAny(mem, []string{p1.ID, p2.ID})
	if err != nil {
		t.Fatalf("ChildrenOfAny: %v", err)
	}
	var got []string
	for _, c := range children {
		got = append(got, c.ID)
	}
	if want := []string{c1.ID, c2.ID}; !slices.Equal(got, want) {
		t.Fatalf("children = %v, want %v (closed children included)", got, want)
	}
}
