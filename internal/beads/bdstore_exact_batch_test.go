package beads_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdStoreGetExactBatchReadsAllIDsInOneShow(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json gc-1 gc-wisp-2 gc-3`: {
			// gc-3 is missing (bd reports it on stderr and exits 0).
			out: []byte(`[{"id":"gc-1","title":"one","status":"open","issue_type":"task"},{"id":"gc-wisp-2","title":"two","status":"open","issue_type":"task","ephemeral":true}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	found, unresolved, err := s.GetExactBatch([]string{"gc-1", "gc-wisp-2", "gc-3"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 || found["gc-1"].Title != "one" || found["gc-wisp-2"].Title != "two" {
		t.Fatalf("found = %+v", found)
	}
	if !reflect.DeepEqual(unresolved, []string{"gc-3"}) {
		t.Fatalf("unresolved = %v, want [gc-3]", unresolved)
	}
}

// A substring collision (bd answered a different bead for a requested id)
// must leave that id unresolved so the caller's exact Get can refuse it.
func TestBdStoreGetExactBatchLeavesCollisionsUnresolved(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json gcy-realbead gcy-dv7`: {
			out: []byte(`[{"id":"gcy-realbead","title":"real","status":"open","issue_type":"task"},{"id":"gcy-wisp-dv78","title":"other","status":"open","issue_type":"task"}]`),
		},
	})
	s := beads.NewBdStore("/city", runner)
	found, unresolved, err := s.GetExactBatch([]string{"gcy-realbead", "gcy-dv7"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := found["gcy-wisp-dv78"]; ok || len(found) != 1 {
		t.Fatalf("found = %+v, want only the exact match", found)
	}
	if !reflect.DeepEqual(unresolved, []string{"gcy-dv7"}) {
		t.Fatalf("unresolved = %v, want [gcy-dv7]", unresolved)
	}
}

func TestBdStoreGetExactBatchAllMissingIsNotAnError(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json gc-a gc-b`: {err: fmt.Errorf("issue gc-a not found")},
	})
	s := beads.NewBdStore("/city", runner)
	found, unresolved, err := s.GetExactBatch([]string{"gc-a", "gc-b"})
	if err != nil || len(found) != 0 || !reflect.DeepEqual(unresolved, []string{"gc-a", "gc-b"}) {
		t.Fatalf("GetExactBatch = %v, %v, %v; want empty, [gc-a gc-b], nil", found, unresolved, err)
	}
}

func TestBdStoreGetExactBatchReturnsTransportErrors(t *testing.T) {
	runner := fakeRunner(map[string]struct {
		out []byte
		err error
	}{
		`bd show --json gc-a gc-b`: {err: fmt.Errorf("dial tcp 127.0.0.1:1: connection refused")},
	})
	s := beads.NewBdStore("/city", runner)
	if _, _, err := s.GetExactBatch([]string{"gc-a", "gc-b"}); err == nil {
		t.Fatal("GetExactBatch() error = nil, want the transport error")
	}
}
