package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestWorkflowExpandedBackfillCheckWarnsOnLegacyExpandedRoot is AC-4: a
// graph.v2 workflow root minted without gc.workflow_expanded whose store
// holds an open workflow-finalize bead naming it must be flagged by Run and
// named in the details, in every scope the check walks. A root already
// stamped, a root-only root, a root that only grew attached fragment steps (a
// fragment member carries gc.root_bead_id but is not a workflow-finalize
// bead), and a workflow root without the graph.v2 contract must all be left
// out. A root already held by a worker is named with its assignee.
func TestWorkflowExpandedBackfillCheckWarnsOnLegacyExpandedRoot(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "repo", Path: rigDir}}}

	// workflowRoot and finalizeBead build the two rows of one graph.v2 run.
	workflowRoot := func(id string, extra map[string]string) beads.Bead {
		metadata := map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "mayor"}
		for k, v := range extra {
			metadata[k] = v
		}
		return beads.Bead{ID: id, Title: "root", Type: "task", Status: "open", Metadata: metadata}
	}
	finalizeBead := func(id, rootID string) beads.Bead {
		return beads.Bead{ID: id, Title: "finalize", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow-finalize", "gc.root_bead_id": rootID,
		}}
	}

	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		// Needs backfill: unstamped, named by an open finalize child.
		workflowRoot("WR-1", nil),
		finalizeBead("FIN-1", "WR-1"),
		// Healthy: already stamped — must be left alone.
		workflowRoot("WR-2", map[string]string{"gc.workflow_expanded": "true"}),
		finalizeBead("FIN-2", "WR-2"),
		// Root-only: no finalize names it, even though it is routed.
		workflowRoot("WR-3", nil),
		// Root with an attached fragment step: the member names the root but
		// is regular work, not a workflow-finalize bead.
		workflowRoot("WR-4", nil),
		{ID: "ST-4", Title: "fragment member", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.root_bead_id": "WR-4", "gc.scope_role": "member",
		}},
		// Assigned legacy expanded root: named with its assignee.
		{
			ID: "WR-5", Title: "held root", Type: "task", Status: "open", Assignee: "worker-7",
			Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": "mayor"},
		},
		finalizeBead("FIN-5", "WR-5"),
		// v1 root: no graph.v2 contract, so never expanded by this backfill.
		{ID: "WR-6", Title: "v1 root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind": "workflow", "gc.routed_to": "mayor",
		}},
		finalizeBead("FIN-6", "WR-6"),
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		workflowRoot("RR-1", nil),
		finalizeBead("FIN-R1", "RR-1"),
	}, nil)
	stores := map[string]beads.Store{cityDir: cityStore, rigDir: rigStore}
	factory := func(path string) (beads.Store, error) {
		store, ok := stores[path]
		if !ok {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	}

	check := newWorkflowExpandedBackfillCheck(cfg, cityDir, factory)

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Run status = %v, want warning: %#v", res.Status, res)
	}
	details := strings.Join(res.Details, "\n")
	for _, want := range []string{"WR-1", "WR-5", "(assigned to worker-7)", "RR-1"} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	// WR-3/WR-4 are prefixes of nothing else; the exclusion rows must not appear.
	for _, notWant := range []string{"WR-2", "WR-3", "WR-4", "WR-6", "ST-4", "FIN-"} {
		if strings.Contains(details, notWant) {
			t.Fatalf("details should not mention %q:\n%s", notWant, details)
		}
	}
}
