package sling

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// --- fake DepLister implementations ---

// fakeDepGraph maps a bead ID to the IDs it depends on ("down" direction).
// All edges are treated as "blocks" type.
type fakeDepGraph map[string][]string

func (g fakeDepGraph) DepList(id, direction string) ([]beads.Dep, error) {
	if direction != "down" {
		return nil, nil
	}
	var deps []beads.Dep
	for _, dep := range g[id] {
		deps = append(deps, beads.Dep{IssueID: id, DependsOnID: dep, Type: "blocks"})
	}
	return deps, nil
}

// fakeDepEdge is one labeled edge in a mixed-type dep graph.
type fakeDepEdge struct {
	to  string
	typ string
}

// fakeTypedDepGraph maps a bead ID to outgoing edges with explicit dep types.
type fakeTypedDepGraph map[string][]fakeDepEdge

func (g fakeTypedDepGraph) DepList(id, direction string) ([]beads.Dep, error) {
	if direction != "down" {
		return nil, nil
	}
	var deps []beads.Dep
	for _, e := range g[id] {
		deps = append(deps, beads.Dep{IssueID: id, DependsOnID: e.to, Type: e.typ})
	}
	return deps, nil
}

// errDepLister always returns an error from DepList.
type errDepLister struct{ err error }

func (e errDepLister) DepList(_, _ string) ([]beads.Dep, error) { return nil, e.err }

// --- DetectCycle unit tests ---

func TestDetectCycleNoCycle_Linear(t *testing.T) {
	// a → b → c (no cycle)
	g := fakeDepGraph{"a": {"b"}, "b": {"c"}, "c": nil}
	if err := DetectCycle("a", g); err != nil {
		t.Fatalf("expected no cycle, got %v", err)
	}
}

func TestDetectCycleNoCycle_Empty(t *testing.T) {
	g := fakeDepGraph{"a": nil}
	if err := DetectCycle("a", g); err != nil {
		t.Fatalf("expected no cycle for isolated node, got %v", err)
	}
}

func TestDetectCycleNoCycle_Diamond(t *testing.T) {
	// a → {b, c}, b → d, c → d — shared dep, no cycle
	g := fakeDepGraph{"a": {"b", "c"}, "b": {"d"}, "c": {"d"}, "d": nil}
	if err := DetectCycle("a", g); err != nil {
		t.Fatalf("expected no cycle in diamond graph, got %v", err)
	}
}

func TestDetectCycleDirect(t *testing.T) {
	// a → b → a
	g := fakeDepGraph{"a": {"b"}, "b": {"a"}}
	err := DetectCycle("a", g)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CycleError, got %T: %v", err, err)
	}
	if len(ce.Path) < 2 {
		t.Fatalf("cycle path too short: %v", ce.Path)
	}
	if ce.Path[0] != ce.Path[len(ce.Path)-1] {
		t.Errorf("cycle path should start and end at same node; got %v", ce.Path)
	}
}

func TestDetectCycleIndirect(t *testing.T) {
	// a → b → c → a
	g := fakeDepGraph{"a": {"b"}, "b": {"c"}, "c": {"a"}}
	err := DetectCycle("a", g)
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CycleError, got %T: %v", err, err)
	}
	if ce.Path[0] != ce.Path[len(ce.Path)-1] {
		t.Errorf("cycle path should close; got %v", ce.Path)
	}
}

func TestDetectCycleSelfLoop(t *testing.T) {
	// a → a
	g := fakeDepGraph{"a": {"a"}}
	err := DetectCycle("a", g)
	if err == nil {
		t.Fatal("expected cycle error for self-loop, got nil")
	}
	var ce *CycleError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *CycleError, got %T: %v", err, err)
	}
}

func TestDetectCycleSkipsNonSchedulingDeps(t *testing.T) {
	// a -relates-to-> b, b -blocks-> a.
	// The "relates-to" edge from a is skipped, so DFS from "a" never enters "b",
	// and the cycle b→a is never reachable from "a".
	g := fakeTypedDepGraph{
		"a": {{to: "b", typ: "relates-to"}},
		"b": {{to: "a", typ: "blocks"}},
	}
	if err := DetectCycle("a", g); err != nil {
		t.Fatalf("relates-to dep should not trigger cycle detection; got %v", err)
	}
}

func TestDetectCycleWaitsForType(t *testing.T) {
	// waits-for is a scheduling dep and must be cycle-sensitive
	g := fakeTypedDepGraph{
		"a": {{to: "b", typ: "waits-for"}},
		"b": {{to: "a", typ: "waits-for"}},
	}
	if err := DetectCycle("a", g); err == nil {
		t.Fatal("expected cycle error for waits-for dep cycle, got nil")
	}
}

func TestDetectCycleErrorPropagation(t *testing.T) {
	dl := errDepLister{err: errors.New("store down")}
	err := DetectCycle("a", dl)
	if err == nil {
		t.Fatal("expected error from DepList failure, got nil")
	}
}

func TestCycleErrorMessage(t *testing.T) {
	g := fakeDepGraph{"x": {"y"}, "y": {"x"}}
	err := DetectCycle("x", g)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "x") || !strings.Contains(msg, "y") {
		t.Errorf("error message %q should name the cycle nodes", msg)
	}
	if !strings.Contains(msg, "→") {
		t.Errorf("error message %q should use → separator", msg)
	}
}

func TestCycleErrorImplementsError(t *testing.T) {
	ce := &CycleError{Path: []string{"a", "b", "a"}}
	// Verify it satisfies the error interface.
	var _ error = ce
	if ce.Error() == "" {
		t.Error("CycleError.Error() should return non-empty string")
	}
}
