package beads

import (
	"context"
	"testing"
)

// The native applier shares validateGraphApplyPlan with every other
// in-process graph applier. Validation must run before any storage is
// acquired, so a zero store proves the ordering: an invalid plan returns the
// contract error without touching a backend.
func TestNativeApplyGraphPlanValidatesBeforeAcquiringStorage(t *testing.T) {
	store := &NativeDoltStore{}
	for _, test := range []struct {
		name string
		plan *GraphApplyPlan
	}{
		{name: "nil plan", plan: nil},
		{name: "empty plan", plan: &GraphApplyPlan{}},
		{name: "empty node title", plan: &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "root"}}}},
		{
			name: "unknown edge endpoint",
			plan: &GraphApplyPlan{
				Nodes: []GraphApplyNode{{Key: "root", Title: "root"}},
				Edges: []GraphApplyEdge{{FromKey: "root", ToKey: "missing"}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := store.ApplyGraphPlanWithStorage(context.Background(), test.plan, StorageDefault); err == nil {
				t.Fatal("invalid plan was accepted")
			}
		})
	}
}
