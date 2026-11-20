package beads_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// bdStoreWorkflowRootRunner answers "bd ready" with two steps whose blockers
// bd already saw closed, one under bd-held and one under bd-live, and answers
// the root lookup with bd-held at heldStatus, the raw status bd records.
func bdStoreWorkflowRootRunner(heldStatus string) (beads.CommandRunner, *[]string) {
	lookups := &[]string{}
	readyRows := []byte(`[
		{"id":"bd-held-step","title":"held step","status":"open","issue_type":"task","created_at":"2025-01-15T10:30:00Z","metadata":{"gc.root_bead_id":"bd-held"}},
		{"id":"bd-live-step","title":"live step","status":"open","issue_type":"task","created_at":"2025-01-15T10:31:00Z","metadata":{"gc.root_bead_id":"bd-live"}}
	]`)
	rootRows := []byte(`[
		{"id":"bd-held","title":"held root","status":"` + heldStatus + `","issue_type":"task","created_at":"2025-01-15T10:00:00Z"},
		{"id":"bd-live","title":"live root","status":"open","issue_type":"task","created_at":"2025-01-15T10:01:00Z"}
	]`)
	runner := func(_, name string, args ...string) ([]byte, error) {
		if len(args) == 0 {
			return nil, fmt.Errorf("unexpected command: %s", name)
		}
		switch args[0] {
		case "ready":
			return readyRows, nil
		case "list":
			*lookups = append(*lookups, strings.Join(args, " "))
			return rootRows, nil
		case "query":
			return []byte(`[]`), nil
		}
		return nil, fmt.Errorf("unexpected command: %s %s", name, strings.Join(args, " "))
	}
	return runner, lookups
}

// TestBdStoreReadyHoldsStepUnderDeferredWorkflowRoot pins the hold on the bd
// door: bd offers both steps (it holds only the parent-child children of a
// parent deferred to a future date), and gc withholds the step whose workflow
// root bd records as deferred, with no date.
func TestBdStoreReadyHoldsStepUnderDeferredWorkflowRoot(t *testing.T) {
	runner, lookups := bdStoreWorkflowRootRunner("deferred")
	got, err := beads.NewBdStore("/city", runner).Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "bd-live-step" {
		t.Fatalf("Ready() = %+v, want [bd-live-step]: a step under a deferred workflow root is held", got)
	}
	if len(*lookups) != 1 {
		t.Fatalf("root lookups = %q, want one list call", *lookups)
	}
}

func TestBdStoreReadyOffersStepUnderOpenWorkflowRoot(t *testing.T) {
	runner, _ := bdStoreWorkflowRootRunner("open")
	got, err := beads.NewBdStore("/city", runner).Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Ready() = %+v, want both steps: an open root holds nothing", got)
	}
}
